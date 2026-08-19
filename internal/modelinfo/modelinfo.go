// Package modelinfo resolves a model's context window (max input tokens)
// DYNAMICALLY, so context-guru's triggers can scale with the model rather than
// hard-coding thresholds. The window is obtained without maintaining our own model
// list: the primary source is LiteLLM's community-maintained
// model_prices_and_context_window.json (fetched once and cached); an optional
// gateway /model/info probe is tried first for deployments that expose it; a tiny
// embedded table is the last-resort fallback.
//
// Every lookup fails OPEN: an unknown model returns ok=false and callers fall back
// to absolute thresholds. Nothing here ever blocks a request — fetches are cached,
// single-flighted, and time-bounded, and any error just leaves the window unknown.
package modelinfo

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Resolver returns a model's context window in tokens. ok=false means "unknown".
type Resolver interface {
	Window(ctx context.Context, model string) (tokens int, ok bool)
}

// Price is a model's per-token USD rates, in the four tiers a prompt-caching
// provider bills. Zero rates mean "unknown" — a caller must treat a Price it did
// not get an ok=true for as "no pricing", never as free.
type Price struct {
	Input      float64 `json:"input"`       // fresh (uncached) input per token
	Output     float64 `json:"output"`      // completion per token
	CacheRead  float64 `json:"cache_read"`  // cache-hit input per token
	CacheWrite float64 `json:"cache_write"` // cache-creation input per token
}

// Cost prices one request's four token tiers in USD.
func (p Price) Cost(fresh, cacheRead, cacheWrite, output int64) float64 {
	return float64(fresh)*p.Input + float64(cacheRead)*p.CacheRead +
		float64(cacheWrite)*p.CacheWrite + float64(output)*p.Output
}

// Zero reports whether no rate at all is known (so a cost figure would be a lie).
func (p Price) Zero() bool {
	return p.Input == 0 && p.Output == 0 && p.CacheRead == 0 && p.CacheWrite == 0
}

// Pricer resolves a model's per-token rates. ok=false means "unknown"; callers
// must then report cost as unavailable rather than zero.
type Pricer interface {
	Price(ctx context.Context, model string) (Price, bool)
}

// LiteLLMPricesURL is the community-maintained map of model -> {max_input_tokens,…}.
// Overridable (air-gapped mirrors) via NewLiteLLM.
const LiteLLMPricesURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// normalize lowercases a model id and strips a provider prefix so gateway route
// names (aws/claude-sonnet-5, anthropic/…, bedrock/…, us.anthropic.…) match the
// keys LiteLLM uses. Returns the full normalized id and its last path segment.
func normalize(model string) (full, tail string) {
	full = strings.ToLower(strings.TrimSpace(model))
	tail = full
	if i := strings.LastIndexAny(tail, "/"); i >= 0 {
		tail = tail[i+1:]
	}
	return full, tail
}

// LiteLLM fetches and caches the LiteLLM prices map, serving per-model windows.
type LiteLLM struct {
	URL    string
	Client *http.Client
	TTL    time.Duration

	mu       sync.Mutex
	byKey    map[string]int   // normalized key -> max_input_tokens
	priceBy  map[string]Price // normalized key -> per-token USD rates
	fetched  time.Time        // last fetch ATTEMPT (success or failure)
	fetching bool             // a background fetch is in flight (single-flight guard)
}

// negTTL is how long to wait before retrying after a failed/empty fetch when no map
// has ever been loaded — short enough to recover quickly, long enough not to hammer an
// unreachable source on every request.
const negTTL = 30 * time.Second

// NewLiteLLM builds a resolver. url="" uses LiteLLMPricesURL; ttl<=0 uses 6h.
func NewLiteLLM(url string, client *http.Client, ttl time.Duration) *LiteLLM {
	if url == "" {
		url = LiteLLMPricesURL
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	return &LiteLLM{URL: url, Client: client, TTL: ttl}
}

// refreshIfStale kicks off a BACKGROUND fetch when the cache is stale — it never blocks
// the caller. Window() is on the request hot path, so blocking on a (up-to-10s) GitHub
// GET would inflate every request's latency; instead the first calls return "unknown"
// (window 0 ⇒ fraction triggers ignored, absolutes apply — the safe default) until the
// fetch lands. Single-flighted via `fetching`; on failure the attempt time is recorded
// so we don't refetch until negTTL passes (negative caching, no thundering herd).
func (l *LiteLLM) refreshIfStale(context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	haveData := l.byKey != nil
	fresh := haveData && time.Since(l.fetched) < l.TTL
	retryGap := l.TTL
	if !haveData {
		retryGap = negTTL
	}
	if fresh || l.fetching || (!l.fetched.IsZero() && time.Since(l.fetched) < retryGap) {
		return
	}
	l.fetching = true
	go func() {
		// Detached context: the fetch outlives the triggering request; its own Client
		// timeout bounds it. Record the attempt time regardless of outcome (negative cache).
		m, pm, err := l.fetch(context.Background())
		l.mu.Lock()
		l.fetching = false
		l.fetched = time.Now()
		if err == nil && len(m) > 0 {
			l.byKey, l.priceBy = m, pm // on failure keep any prior map (fail open)
		}
		l.mu.Unlock()
	}()
}

func (l *LiteLLM) fetch(ctx context.Context) (map[string]int, map[string]Price, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	// Per-entry decode (see the package note): the upstream document is not
	// schema-clean, so one typed whole-map unmarshal yields nothing. This loop also
	// collects the per-token PRICES the dashboard needs, from the same pass.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, nil, err
	}
	m := make(map[string]int, len(raw)*2)
	pm := make(map[string]Price, len(raw)*2)
	var skipped []string
	for k, rv := range raw {
		if k == sampleSpecKey {
			continue // the document's own schema documentation, not a model
		}
		var v struct {
			// float64, not int: a few entries spell an integer field as 128000.0,
			// which an int field rejects.
			MaxInputTokens  float64 `json:"max_input_tokens"`
			MaxTokens       float64 `json:"max_tokens"`
			InputCost       float64 `json:"input_cost_per_token"`
			OutputCost      float64 `json:"output_cost_per_token"`
			CacheReadCost   float64 `json:"cache_read_input_token_cost"`
			CacheCreateCost float64 `json:"cache_creation_input_token_cost"`
		}
		if err := json.Unmarshal(rv, &v); err != nil {
			skipped = append(skipped, k)
			continue
		}
		full, tail := normalize(k)
		if w := int(v.MaxInputTokens); w != 0 || v.MaxTokens != 0 {
			if w == 0 {
				w = int(v.MaxTokens)
			}
			m[full] = w
			if _, ok := m[tail]; !ok { // don't clobber a more-specific full key
				m[tail] = w
			}
		}
		p := Price{Input: v.InputCost, Output: v.OutputCost, CacheRead: v.CacheReadCost, CacheWrite: v.CacheCreateCost}
		if p.Zero() {
			continue
		}
		// LiteLLM omits cache rates for models that do not cache; fall back to the
		// provider-standard Anthropic multiples ONLY when a cache tier is missing but
		// input pricing is known, so a cached request is never priced as free.
		if p.CacheRead == 0 {
			p.CacheRead = p.Input * 0.1
		}
		if p.CacheWrite == 0 {
			p.CacheWrite = p.Input * 1.25
		}
		pm[full] = p
		if _, ok := pm[tail]; !ok {
			pm[tail] = p
		}
	}
	// Degrading silently here is what hid the whole-map decode bug for the life of
	// the package: an empty map is indistinguishable from "no model has a window" at
	// every call site, because every lookup fails open. Both outcomes get a log line.
	if len(m) == 0 {
		slog.Warn("modelinfo: the model-window document decoded to nothing; every context window will read as unknown and fraction-based triggers will not fire",
			"url", l.URL, "entries", len(raw), "skipped", len(skipped))
	} else if len(skipped) > 0 {
		slog.Info("modelinfo: skipped malformed model entries", "skipped", len(skipped),
			"kept", len(m), "examples", skipped[:min(3, len(skipped))])
	}
	return m, pm, nil
}

// Price returns the model's per-token rates from the cached LiteLLM map, matched
// the same way Window matches (full id, then bare tail, then a contains scan).
func (l *LiteLLM) Price(ctx context.Context, model string) (Price, bool) {
	l.refreshIfStale(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.priceBy == nil {
		return Price{}, false
	}
	full, tail := normalize(model)
	if p, ok := l.priceBy[full]; ok {
		return p, true
	}
	if p, ok := l.priceBy[tail]; ok {
		return p, true
	}
	// Last resort: a key that CONTAINS the bare id (claude-sonnet-5 vs
	// anthropic.claude-sonnet-5). The SHORTEST such key wins, because the exact
	// matches are already handled above, so the remaining candidates differ from the
	// id only by decoration and the least-decorated one is the canonical entry
	// (`gpt-5` over `gpt-5-nano`). This used to return the
	// first hit of a Go map range, so which price a gateway id resolved to was
	// randomised per process: `gpt-5` could pick up `gpt-5-nano`'s rates on one boot
	// and `gpt-5-pro`'s on the next, and nothing in the dashboard would say so.
	best, found := Price{}, ""
	for k, p := range l.priceBy {
		if strings.Contains(k, tail) && (found == "" || len(k) < len(found)) {
			best, found = p, k
		}
	}
	if found != "" {
		return best, true
	}
	return Price{}, false
}

// sampleSpecKey is the LiteLLM document's self-documenting entry: its fields are
// prose descriptions of the schema, not values. Skipped by name so it never even
// counts as a decode failure.
const sampleSpecKey = "sample_spec"

// Window returns the model's context window from the cached LiteLLM map.
func (l *LiteLLM) Window(ctx context.Context, model string) (int, bool) {
	l.refreshIfStale(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byKey == nil {
		return 0, false
	}
	full, tail := normalize(model)
	if w, ok := l.byKey[full]; ok {
		return w, true
	}
	if w, ok := l.byKey[tail]; ok {
		return w, true
	}
	// last resort: any key that contains the tail (e.g. claude-sonnet-5 vs
	// anthropic.claude-sonnet-5) — pick the max window among matches.
	best, found := 0, false
	for k, w := range l.byKey {
		if strings.Contains(k, tail) && w > best {
			best, found = w, true
		}
	}
	return best, found
}

// Static is a tiny last-resort table keyed by substring. Used only when a dynamic
// source is unavailable. Deliberately small — it is a floor, not a registry.
type Static struct{ table []staticEntry }

type staticEntry struct {
	substr string
	window int
}

// DefaultStatic covers the common families with conservative windows.
func DefaultStatic() Static {
	return Static{table: []staticEntry{
		{"claude-sonnet-5", 1000000}, {"claude-opus-4", 200000}, {"claude", 200000},
		{"gpt-5", 400000}, {"gpt-4o", 128000}, {"gpt-4", 128000}, {"o1", 200000}, {"o3", 200000},
		{"gemini-2", 1000000}, {"gemini", 1000000}, {"llama", 128000}, {"mistral", 32000},
	}}
}

func (s Static) Window(_ context.Context, model string) (int, bool) {
	m := strings.ToLower(model)
	for _, e := range s.table { // first match wins; order most-specific first
		if strings.Contains(m, e.substr) {
			return e.window, true
		}
	}
	return 0, false
}

// Chain tries each resolver in order; the first ok wins.
type Chain []Resolver

func (c Chain) Window(ctx context.Context, model string) (int, bool) {
	for _, r := range c {
		if w, ok := r.Window(ctx, model); ok {
			return w, true
		}
	}
	return 0, false
}

// Price tries each element that can price a model; the first ok wins. Elements
// that only resolve windows are skipped, so a Chain{LiteLLM, Static} prices from
// LiteLLM and reports unknown when it has not loaded.
func (c Chain) Price(ctx context.Context, model string) (Price, bool) {
	for _, r := range c {
		if p, ok := r.(Pricer); ok {
			if pr, found := p.Price(ctx, model); found {
				return pr, true
			}
		}
	}
	return Price{}, false
}
