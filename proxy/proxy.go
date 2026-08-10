// Package proxy is the context-guru HTTP proxy: it runs the component pipeline
// on inbound chat requests, then forwards them to the configured upstream
// provider. It is the eval-containers gateway (exposes /openai + /anthropic on
// one port) and the standalone LLM-proxy integration.
//
// It reuses bifrost's ChatMessage type but not its transport: the transport
// can't inject an in-process Go plugin, so we drive the request path directly.
// Message rewriting is byte-lossless (headroom invariant I1) — only the
// `messages` array is re-serialized; every other field of the original body is
// preserved verbatim via sjson.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Options configures upstreams and credential injection. Each upstream is a base
// URL the matching route forwards to (the incoming path is appended). When a key
// is set it replaces the client's auth on forward — this is the eval-containers
// gateway model, where the agent holds only a placeholder key and the real
// provider key lives in the gateway env. Leave keys empty to pass the incoming
// auth through unchanged (local/dev use).
type Options struct {
	OpenAIUpstream    string // e.g. https://api.openai.com
	AnthropicUpstream string
	// BobUpstream, when set, enables the Bob (BobShell) gateway: Bob's
	// OpenAI-dialect model calls (POST /inference/v1/chat/completions) are reduced
	// and forwarded here, and every other path Bob calls (control-plane:
	// /admin/v1/profile, /inference/v1/model/info, …) is proxied through verbatim
	// so the CLI boots and authenticates. Point Bob's CUSTOM_BASE_URL at this proxy.
	BobUpstream  string // e.g. https://api.us-east.bob.ibm.com
	OpenAIKey    string // injected as Authorization: Bearer <key>
	AnthropicKey string // injected as x-api-key: <key>
	// ForceModel, when set, overwrites the request's "model" field. eval-containers
	// uses this to pin every call to EVAL_MODEL regardless of what the agent asked for.
	ForceModel string
	Client     *http.Client
	// CheapModel is the static "config"-source LLM client for NeedsModel
	// components (nil = none). The "incoming"-source client is built per request
	// from the route's upstream + the gateway's real key.
	CheapModel components.Model
	// Windows resolves a model's context window (max input tokens) so fraction-based
	// Trigger thresholds scale with the model. nil => window unknown (0), fractions
	// ignored, absolutes apply. Built in main from internal/modelinfo.
	Windows interface {
		Window(ctx context.Context, model string) (int, bool)
	}
	// InjectExpand controls advertising the context_guru_expand tool on outgoing
	// requests so Offload markers are actually recoverable (expand.InjectAuto |
	// InjectAlways | InjectNever). Empty defaults to auto. auto injects only when the
	// request already declares tools and the store persists — safe for any agent.
	InjectExpand string
	// CacheMode controls cache-aware compaction ("auto"|"on"|"off"; empty=auto).
	// auto/on keep offloaders from mutating already-cached content on prompt-caching
	// backends; off restores legacy compact-everything (for confirmed non-caching backends).
	CacheMode string
	// PipelineFor builds a pipeline for a per-request override on /compact
	// (?preset=… or x-context-guru-pipeline: a,b,c). nil = overrides ignored, the
	// handler always uses the configured pipeline. Supplied by main (which holds
	// the config + emitter) so proxy stays decoupled from the config package.
	PipelineFor func(preset string, names []string) (*components.Pipeline, error)
	// Mode is the operating mode (#31): components.ModeSync (default, and
	// byte-identical to pre-mode behavior), ModeAsync, or ModeObserve. Empty = sync.
	// Explicit by design — never inferred from the rest of the configuration.
	Mode components.Mode
	// Async tunes async mode. Ignored in the other two.
	Async AsyncOptions
}

// AsyncOptions tunes async mode: one option per real decision.
type AsyncOptions struct {
	// CacheUncompactedTail lets the not-yet-compacted tail be prompt-cached. Default
	// false — the safe choice, because a breakpoint written over a tail a pending
	// compaction then replaces converts a 0.1x cache read into a 1.25x cache write,
	// 11.5x the cost, making async strictly WORSE than sync. Set true only for a
	// backend confirmed not to cache, where the protection buys nothing.
	CacheUncompactedTail bool `yaml:"cache_uncompacted_tail"`
	// StripCallerBreakpoints lets the tail protection remove a cache breakpoint the
	// AGENT placed inside the span a pending compaction will replace. Without it the
	// protection cannot cover an agent that sets its own breakpoints — claude-code does,
	// so on that workload async declines to defer at all rather than pretend to protect
	// (counted as async_tail_unprotected_turns). Default false: removing a directive the
	// agent deliberately placed is a behavior change in someone else's request, so it is
	// opt-in. Turn it on to actually get async's benefit with claude-code.
	StripCallerBreakpoints bool `yaml:"strip_caller_breakpoints"`
	// MaxQueue bounds the off-path job queue; a full queue DROPS (counted) rather than
	// blocking the request path. 0 = modes.DefaultMaxQueue.
	MaxQueue int `yaml:"max_queue"`
	// Workers is the number of drain goroutines. 0 = modes.DefaultWorkers (1), which
	// keeps one compaction LLM call in flight per process.
	Workers int `yaml:"workers"`
}

// upstream binds a provider to its base URL, the canonical provider path to POST
// to (decoupled from the gateway's incoming /openai|/anthropic namespace), and a
// credential injector.
type upstream struct {
	base   string
	path   string
	setKey func(http.Header)
}

// Handler serves the proxy + management routes.
type Handler struct {
	pipe   *components.Pipeline
	store  store.Store
	agg    *metrics.Aggregator
	opts   Options
	client *http.Client
	// tracker owns the per-session cached-prefix boundary and compaction generation.
	// Always present (every mode benefits from the race-free boundary; only async uses
	// the generation).
	tracker *modes.Tracker
	// pool runs off-path work. nil in sync mode — there is none.
	pool *modes.Pool
	// observeSeq numbers observations so each turn of a session enqueues one job (an
	// observe run never commits, so its generation never advances and cannot serve as
	// the dedup key on its own).
	observeSeq atomic.Uint64
	// shadow is observe mode's own state store, separate from the live one. Observe must
	// not write into the live store — a real request would then replay a decision that
	// was never enforced — but it also cannot simply discard its writes: offloaders
	// FREEZE a decision and replay it on every later turn, which is where most of the
	// sustained saving comes from. Throwing that away each turn makes observe see only
	// the current tail and UNDER-project by ~3x against what sync achieves.
	//
	// So observe gets a store of its own: as persistent as the live one, and completely
	// disjoint from it.
	shadow store.Store
}

// New builds the proxy handler. agg may be nil (no /stats rollups).
func New(pipe *components.Pipeline, st store.Store, agg *metrics.Aggregator, opts Options) *Handler {
	c := opts.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	h := &Handler{pipe: pipe, store: st, agg: agg, opts: opts, client: c, tracker: modes.NewTracker(0)}
	if h.mode() != components.ModeSync {
		h.pool = modes.NewPool(opts.Async.MaxQueue, opts.Async.Workers)
	}
	if h.mode() == components.ModeObserve {
		h.shadow = store.NewMemory(store.Options{})
	}
	if agg != nil {
		agg.SetMode(h.mode())
		if h.pool != nil {
			agg.SetAsyncStats(func() any { return h.pool.Stats() })
		}
	}
	return h
}

// Close shuts down the off-path worker pool and waits for its goroutines to exit, so a
// host that builds and discards handlers (tests, a reload) leaks none. Safe on a
// sync-mode handler and safe to call twice.
func (h *Handler) Close() {
	h.pool.Stop()
}

// Mux wires the routes: chat proxying + health/stats/expand management.
func (h *Handler) Mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /openai/v1/chat/completions", h.chat(bschemas.OpenAI, upstream{
		base:   h.opts.OpenAIUpstream,
		path:   "/v1/chat/completions",
		setKey: bearerKey(h.opts.OpenAIKey),
	}))
	m.HandleFunc("POST /anthropic/v1/messages", h.chat(bschemas.Anthropic, upstream{
		base:   h.opts.AnthropicUpstream,
		path:   "/v1/messages",
		setKey: headerKey("x-api-key", h.opts.AnthropicKey),
	}))
	m.HandleFunc("POST /compact", h.compact)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	m.HandleFunc("GET /stats", h.stats)
	m.HandleFunc("GET /expand", h.expand)
	// Bob (BobShell) gateway. Bob is OpenAI-compatible but calls Bob-specific
	// paths: its model call is POST /inference/v1/chat/completions (reduced like
	// any OpenAI chat), and its control-plane calls (/admin/v1/profile,
	// /inference/v1/model/info, …) must pass through verbatim so the CLI boots and
	// authenticates. The "/" catch-all is less specific than every route above, so
	// it only receives what nothing else matched. Enabled only when BobUpstream is
	// set, so default proxy behavior (unknown path => 404) is unchanged.
	if h.opts.BobUpstream != "" {
		m.HandleFunc("POST /inference/v1/chat/completions", h.chat(bschemas.OpenAI, upstream{
			base: h.opts.BobUpstream,
			path: "/inference/v1/chat/completions",
			// setKey nil: pass Bob's own auth (BOBSHELL key) straight through.
		}))
		m.HandleFunc("/", h.passthrough(h.opts.BobUpstream))
	}
	return m
}

// passthrough transparently forwards a request to the Bob upstream unchanged —
// for Bob's control-plane calls that must not be rewritten. Bob's own auth
// header passes straight through (no key injection); the response is streamed
// back as-is. Only the model route is reduced; everything else Bob calls lands
// here and is proxied verbatim.
func (h *Handler) passthrough(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		body, _ := io.ReadAll(r.Body)
		target := base + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		copyHeaders(req.Header, r.Header)
		resp, err := h.client.Do(req)
		if err != nil {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			return
		}
		h.stream(w, resp)
	}
}

// compact runs the pipeline over the request body's messages and returns the
// rewritten body — without forwarding upstream. This is the "compact a context,
// hand it back" endpoint: a caller (e.g. the llm-d-router request-inline-
// compaction step) POSTs an inference request body and gets a smaller body of
// the same shape back. Fail-open: any parse/serialize trouble returns the
// original body with 200, so the caller's passthrough contract always holds.
//
// Provider defaults to OpenAI; ?provider=anthropic switches dialects. Config
// overrides (when Options.PipelineFor is set): ?preset=<name> or header
// x-context-guru-pipeline: comp1,comp2. Session/bypass honor the usual headers.
func (h *Handler) compact(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	provider := bschemas.OpenAI
	if strings.EqualFold(r.URL.Query().Get("provider"), "anthropic") {
		provider = bschemas.Anthropic
	}

	pipe := h.pipe
	if h.opts.PipelineFor != nil {
		preset := r.URL.Query().Get("preset")
		var names []string
		if hp := r.Header.Get("x-context-guru-pipeline"); hp != "" {
			names = splitComma(hp)
		}
		if preset != "" || len(names) != 0 {
			// ponytail: rebuild per override request; add an LRU cache if override QPS ever matters.
			if p, err := h.opts.PipelineFor(preset, names); err == nil {
				pipe = p
			} // build error => fall back to the configured pipeline (fail open)
		}
	}

	// No upstream here, so there is no "incoming" model; only the static
	// "config"-source client (and any endpoint pinned in a component's model: block).
	models := components.ModelSpec{Static: h.opts.CheapModel}
	// Honor the same cache-aware behavior as the live chat path so /compact (used by
	// offline replay/eval) reflects production. ?cache=on|off|auto overrides for A/B.
	cacheMode := h.opts.CacheMode
	if q := r.URL.Query().Get("cache"); q != "" {
		cacheMode = q
	}
	out, _ := apply.BodyFull(
		r.Context(), pipe, h.store, provider, body,
		r.Header.Get("x-context-guru-session"),
		strings.EqualFold(r.Header.Get("x-context-guru-bypass"), "true"),
		models, 0, cacheMode,
	)
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// splitComma splits a comma-separated header value into trimmed, non-empty names.
func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func bearerKey(key string) func(http.Header) {
	if key == "" {
		return nil
	}
	return func(h http.Header) { h.Set("Authorization", "Bearer "+key) }
}

func headerKey(name, key string) func(http.Header) {
	if key == "" {
		return nil
	}
	return func(h http.Header) { h.Set(name, key) }
}

// incomingModel builds an LLM client that reuses the proxied request's own model
// and the route's upstream + credential, so a NeedsModel component can call the
// same backend the request targets. Prefers the gateway's injected key (gateway
// mode); falls back to the client's own auth header (pass-through). Returns nil
// when no upstream/model/key is resolvable, and the component degrades.
func (h *Handler) incomingModel(provider bschemas.ModelProvider, up upstream, body []byte, r *http.Request) components.Model {
	if up.base == "" {
		return nil
	}
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		return nil
	}
	switch provider {
	case bschemas.Anthropic:
		key := h.opts.AnthropicKey
		if key == "" {
			key = r.Header.Get("x-api-key")
		}
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if key == "" {
			return nil
		}
		return cheapmodel.Anthropic{BaseURL: up.base, Model: model, APIKey: key, Client: h.client}
	case bschemas.OpenAI:
		key := h.opts.OpenAIKey
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if key == "" {
			return nil
		}
		return cheapmodel.OpenAI{BaseURL: up.base, Model: model, APIKey: key, Client: h.client}
	}
	return nil
}

// capturePath, when set (CONTEXT_GURU_CAPTURE), names a JSONL file the proxy
// appends each inbound request to (for offline replay-based component evaluation).
var capturePath = os.Getenv("CONTEXT_GURU_CAPTURE")

// captureRequest appends {provider, model, body} for one request. Best-effort;
// any error is ignored (never affects request handling).
func captureRequest(provider string, body []byte) {
	if capturePath == "" {
		return
	}
	f, err := os.OpenFile(capturePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec, _ := json.Marshal(map[string]any{
		"provider": provider,
		"model":    gjson.GetBytes(body, "model").String(),
		"body":     json.RawMessage(body),
	})
	f.Write(append(rec, '\n'))
}

func (h *Handler) chat(provider bschemas.ModelProvider, up upstream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		// Pin the model if configured (eval-containers EVAL_MODEL).
		if h.opts.ForceModel != "" {
			if out, err := sjson.SetBytes(body, "model", h.opts.ForceModel); err == nil {
				body = out
			}
		}
		// Optional request capture (CONTEXT_GURU_CAPTURE): append the pristine inbound
		// body (post force-model, pre-pipeline) as one JSONL record per request, so an
		// external evaluator can REPLAY the exact traffic through any pipeline via
		// /compact — identical requests, no agent nondeterminism, no repeated LLM spend.
		captureRequest(string(provider), body)
		// Rewrite the messages via the shared apply path; fail open. Supply the
		// LLM clients NeedsModel components may call: the per-request "incoming"
		// model (the route's upstream + the gateway's real key + the request's
		// model) and the static "config" cheap model.
		models := components.ModelSpec{
			Incoming: h.incomingModel(provider, up, body, r),
			Static:   h.opts.CheapModel,
		}
		// Resolve the model's context window (dynamic, cached) so fraction-based
		// triggers scale with the model; 0 when unknown (absolutes apply).
		window := 0
		if h.opts.Windows != nil {
			if w, ok := h.opts.Windows.Window(r.Context(), gjson.GetBytes(body, "model").String()); ok {
				window = w
			}
		}
		bypassed := strings.EqualFold(r.Header.Get("x-context-guru-bypass"), "true")
		// Fail open around the whole pre-forward rewrite (pipeline + expand injection): a
		// panic anywhere here must forward the PRISTINE inbound body, never 500 the client.
		// apply.BodyFull has its own recover; this backstops expand.Inject and anything else.
		orig := body
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("context-guru: recovered from panic before forward; sending original", "panic", rec)
					body = orig
				}
			}()
			var added time.Duration
			body, added = h.applyMode(&httpReqInfo{
				ctx:      r.Context(),
				provider: provider,
				body:     body,
				session:  r.Header.Get("x-context-guru-session"),
				bypassed: bypassed,
				models:   models,
				window:   window,
			})
			if h.agg != nil && !bypassed {
				h.agg.RecordAddedLatency(float64(added.Microseconds()) / 1000.0)
			}
			// Advertise the expand tool so the model can recover any offloaded content
			// (closes the reversibility loop h.serve drives). Sticky/idempotent + appended
			// last to keep the provider prefix cache warm; gated by InjectExpand + store.
			//
			// Skipped in observe mode: nothing was offloaded, so there is nothing to
			// recover, and injecting a tool declaration would MODIFY the request — which
			// is precisely the one thing observe mode promises never to do.
			if h.mode() != components.ModeObserve {
				im := h.opts.InjectExpand
				if im == "" {
					im = expand.InjectAuto
				}
				body, _ = expand.Inject(string(provider), im, body, h.store.Persists())
			}
		}()
		h.serve(w, r, provider, up, body, bypassed)
	}
}

// maxExpandRounds caps the expand continuation loop (headroom's default).
const maxExpandRounds = 3

// maxRequestBytes caps an inbound request body so a single huge POST can't exhaust
// proxy memory (the body is buffered and token-counted several times). Generous
// enough for very long agent transcripts; requests above it get 413.
const maxRequestBytes = 32 << 20 // 32 MiB

// The expand tool is advertised on the outgoing request by expand.Inject (called
// in chat, gated by Options.InjectExpand + store.Persists), appended last and
// idempotently so the provider prefix cache stays byte-stable across turns. That
// closes the loop: the marker text tells the model to call context_guru_expand, and
// the tool is now actually declared, so the continuation loop below can fire.

var errNoUpstream = errors.New("no upstream configured")

// serve forwards the request and runs the expand continuation loop: if the model
// calls the expand tool (and ONLY that tool), resolve the originals from the store,
// append the tool-result turn, and re-invoke upstream — up to a few rounds.
//
// Streaming (SSE) works too: when the outgoing request carries expandable markers
// (`<<cg:…>>`, i.e. offload has happened, so the model MIGHT call expand), the SSE
// response is buffered and reconstructed so the loop can inspect it. If it is not a
// lone expand call, the buffered SSE bytes are replayed to the client verbatim (a
// one-time latency cost, no correctness change). Requests without markers — early in
// a session — stream straight through with zero added latency and no possible expand.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, provider bschemas.ModelProvider, up upstream, body []byte, bypassed bool) {
	injectOn := h.opts.InjectExpand != expand.InjectNever
	// For SSE we must buffer to inspect (a latency cost), so only do it when the request
	// actually carries expandable markers (offload happened → the model might expand).
	// Tolerate a client that HTML-escapes "<" in JSON (as <) — a false positive
	// only costs one buffered response; a false negative would miss a real expand.
	bodyStr := string(body)
	hasMarkers := expand.HasPlaceholder(bodyStr) || strings.Contains(bodyStr, "\\u003ccg:")
	for round := 0; ; round++ {
		upStart := time.Now()
		resp, err := h.doUpstream(r, up, body)
		if err != nil {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			return
		}
		if h.agg != nil {
			h.agg.RecordUpstreamLatency(float64(time.Since(upStart).Microseconds())/1000.0, bypassed)
		}
		isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
		// Inspect for a lone expand call when injection is on and we haven't hit the round
		// cap. JSON responses are already buffered (inspect freely); SSE responses are only
		// buffered+inspected when markers are present (else stream through, no added latency).
		checkExpand := injectOn && round < maxExpandRounds && (!isSSE || hasMarkers)
		if !checkExpand {
			h.stream(w, resp)
			return
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// Reconstruct the message the loop reasons over. For SSE, aggregate the events;
		// if that fails, replay the raw stream unchanged (fail-open).
		msg := respBody
		if isSSE {
			agg, ok := expand.AggregateSSE(string(provider), respBody)
			if !ok {
				writeRaw(w, resp, respBody)
				return
			}
			msg = agg
		}

		calls, otherTools := expand.ResponseCalls(string(provider), msg)
		if len(calls) == 0 || otherTools {
			writeRaw(w, resp, respBody) // normal answer (or other tools) — replay verbatim
			return
		}
		// Build a tool_result for EVERY expand call — the provider requires one per
		// tool_call_id or the continuation is malformed. Expired/unknown ids get an
		// explicit placeholder rather than being omitted.
		resolved := map[string]string{}
		got := 0
		for _, c := range calls {
			if orig, ok := expand.Resolve(h.store, c.HashID); ok {
				resolved[c.CallID] = orig
				got++
				// The agent needed this content back — don't re-compact it on later turns
				// (that would loop it straight back into another expand). Keep it verbatim.
				offload.MarkKeptVerbatim(h.store, orig)
				if h.agg != nil {
					h.agg.RecordExpand(schema.TextTokens(orig)) // bounce: offload had to come back
				}
			} else {
				resolved[c.CallID] = "[expand: original for id " + c.HashID + " is no longer available]"
			}
		}
		next, ok := expand.Continuation(string(provider), body, msg, resolved)
		if got == 0 || !ok {
			writeRaw(w, resp, respBody) // nothing recovered; return the model's own call
			return
		}
		body = next // loop: re-invoke with the originals in hand
	}
}

// writeRaw replays a fully-buffered upstream response (SSE or JSON) to the client
// with its original headers/status — byte-for-byte, flushing for SSE.
func writeRaw(w http.ResponseWriter, resp *http.Response, body []byte) {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) doUpstream(r *http.Request, up upstream, body []byte) (*http.Response, error) {
	if up.base == "" {
		return nil, errNoUpstream
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, up.base+up.path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	if up.setKey != nil {
		// Gateway mode: drop the client's placeholder auth, inject the real key.
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
		up.setKey(req.Header)
	}
	return h.client.Do(req)
}

// stream copies an upstream response through with flushing (SSE-friendly).
func (h *Handler) stream(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flush, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flush != nil {
				flush.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
}

func (h *Handler) stats(w http.ResponseWriter, _ *http.Request) {
	if h.agg == nil {
		w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	snap := h.agg.Snapshot()
	// Fill the CG components' own LLM cost (cheap-model usage) — kept out of the
	// metrics package (layering) and merged here at serve time.
	snap.LLMCalls, snap.LLMInputTokens, snap.LLMOutputTokens = cheapmodel.Usage()
	json.NewEncoder(w).Encode(snap)
}

// expand resolves a stashed original by id — the HTTP side of reversibility (the
// model-callable tool loop is a separate concern, added with response handling).
func (h *Handler) expand(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	// Scope retrieval to the caller's session: a stash is keyed by a global content
	// hash, so without this check any client reaching the proxy could fetch another
	// session's offloaded original by supplying its id (cross-session/tenant disclosure).
	// The session comes from the same x-context-guru-session header used on chat requests
	// (or ?session=). The model-driven expand loop needs no such check — it only ever sees
	// markers minted from its own request.
	session := r.Header.Get("x-context-guru-session")
	if session == "" {
		session = r.URL.Query().Get("session")
	}
	if !offload.OwnsKey(h.store, session, id) {
		http.Error(w, "expired, unknown, or not owned by this session", http.StatusNotFound)
		return
	}
	orig, ok := expand.Resolve(h.store, id)
	if !ok {
		http.Error(w, "expired or unknown id", http.StatusNotFound)
		return
	}
	w.Write([]byte(orig))
}

// copyHeaders copies headers except hop-by-hop ones; the caller's provider auth
// (Authorization / x-api-key) passes straight through to the upstream.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length", "Host":
			continue
		}
		if strings.HasPrefix(strings.ToLower(k), "x-context-guru-") {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}
