package modelinfo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sample = `{
  "claude-sonnet-5": {"max_input_tokens": 1000000, "max_output_tokens": 128000},
  "anthropic.claude-opus-4": {"max_input_tokens": 200000},
  "gpt-4o": {"max_tokens": 128000},
  "no-window-model": {"input_cost_per_token": 0.001}
}`

// waitReady polls Window (which triggers an async background fetch and returns
// unknown until it lands) until the map is loaded, so the resolution assertions run
// against a populated cache.
func waitReady(t *testing.T, l *LiteLLM, ctx context.Context) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if _, ok := l.Window(ctx, "gpt-4o"); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("litellm map never loaded")
}

func TestLiteLLMResolvesAndNormalizes(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Write([]byte(sample))
	}))
	defer srv.Close()
	l := NewLiteLLM(srv.URL, srv.Client(), time.Hour)
	ctx := context.Background()
	waitReady(t, l, ctx)

	// exact + provider-prefixed (aws/claude-sonnet-5 -> claude-sonnet-5).
	if w, ok := l.Window(ctx, "aws/claude-sonnet-5"); !ok || w != 1000000 {
		t.Fatalf("aws/claude-sonnet-5 => %d,%v want 1000000", w, ok)
	}
	// max_tokens fallback when max_input_tokens absent.
	if w, ok := l.Window(ctx, "gpt-4o"); !ok || w != 128000 {
		t.Fatalf("gpt-4o => %d,%v", w, ok)
	}
	// substring match: opus via dotted litellm key.
	if w, ok := l.Window(ctx, "claude-opus-4"); !ok || w != 200000 {
		t.Fatalf("claude-opus-4 => %d,%v", w, ok)
	}
	// unknown model => not ok (fail open).
	if _, ok := l.Window(ctx, "totally-unknown-xyz"); ok {
		t.Fatal("unknown model must return ok=false")
	}
	// A BRACKETED CONTEXT-LENGTH VARIANT must resolve to its base model, and EXACTLY, because
	// Trigger.FracResolvable refuses to act on a guess. Before this, `aws/claude-opus-5[1m]` missed
	// the map entirely: the chain fell through to DefaultStatic, which answers 200,000 for every
	// Opus with exact=false, so summarize's shipped min_request_frac: 0.9 could never fire on such an
	// id — silently, which looks exactly like a gate that is working — and every non-exact reader
	// (OutputFloor, IsHuge, extract_llm's fraction) saw a window five times too low.
	for _, id := range []string{"claude-sonnet-5[1m]", "aws/claude-sonnet-5[1m]"} {
		w, exact, ok := Exact(l, ctx, id)
		if !ok || w != 1000000 {
			t.Errorf("%s => %d,%v want 1000000: the bracketed suffix is a context-length variant, "+
				"not part of the model key", id, w, ok)
		}
		if !exact {
			t.Errorf("%s resolved inexactly; FracResolvable refuses a guess, so summarize's "+
				"fraction gate would never fire on this id", id)
		}
	}
	// And an id that merely CONTAINS a bracket, or ends with an unterminated one, is not rewritten:
	// this must strip a variant marker, never repair a malformed name.
	if _, ok := l.Window(ctx, "totally-unknown-xyz[1m"); ok {
		t.Error("an unterminated bracket must not be stripped into a different lookup")
	}
	// cache: many lookups, one fetch.
	for i := 0; i < 5; i++ {
		l.Window(ctx, "gpt-4o")
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("expected a single cached fetch, got %d", got)
	}
}

// TestLiteLLMConcurrentSingleFlight: many concurrent cold-cache lookups must trigger
// exactly ONE upstream fetch (no thundering herd), and Window must never block on the
// (slow) upstream — it returns unknown until the background fetch lands.
func TestLiteLLMConcurrentSingleFlight(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		time.Sleep(150 * time.Millisecond) // slow upstream: a blocking impl would stall callers
		w.Write([]byte(sample))
	}))
	defer srv.Close()
	l := NewLiteLLM(srv.URL, srv.Client(), time.Hour)
	ctx := context.Background()

	// 20 concurrent cold lookups must each return PROMPTLY (non-blocking).
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			l.Window(ctx, "gpt-4o")
			if time.Since(start) > 100*time.Millisecond {
				t.Errorf("Window blocked on the upstream fetch (%v) — must be non-blocking", time.Since(start))
			}
		}()
	}
	wg.Wait()
	waitReady(t, l, ctx) // let the single background fetch complete
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("single-flight violated: expected 1 fetch, got %d", got)
	}
}

func TestChainAndStaticFallback(t *testing.T) {
	// LiteLLM pointing at a dead URL => falls through to Static.
	l := NewLiteLLM("http://127.0.0.1:0/nope", &http.Client{Timeout: 200 * time.Millisecond}, time.Hour)
	c := Chain{l, DefaultStatic()}
	if w, ok := c.Window(context.Background(), "aws/claude-sonnet-5"); !ok || w == 0 {
		t.Fatalf("chain should fall back to static: %d,%v", w, ok)
	}
}

// priceSample carries the four billed tiers plus a model with no published cache
// rates, on top of the malformed-entry shapes litellm_decode_test.go pins.
const priceSample = `{
  "sample_spec": {"max_input_tokens": "max input tokens, if the provider specifies it",
                  "input_cost_per_token": "the cost"},
  "aws/claude-sonnet-5": {"max_input_tokens": 1000000, "input_cost_per_token": 2e-06,
                          "output_cost_per_token": 1e-05,
                          "cache_read_input_token_cost": 2e-07,
                          "cache_creation_input_token_cost": 2.5e-06},
  "no-cache-model": {"max_input_tokens": 8192, "input_cost_per_token": 4e-06, "output_cost_per_token": 8e-06}
}`

func TestLiteLLMPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(priceSample))
	}))
	defer srv.Close()
	l := NewLiteLLM(srv.URL, srv.Client(), time.Hour)
	ctx := context.Background()
	for i := 0; i < 400; i++ {
		if _, ok := l.Price(ctx, "aws/claude-sonnet-5"); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Prices must resolve, with the four tiers intact.
	p, ok := l.Price(ctx, "aws/claude-sonnet-5")
	if !ok {
		t.Fatal("Price(aws/claude-sonnet-5) not resolved")
	}
	want := Price{Input: 2e-06, Output: 1e-05, CacheRead: 2e-07, CacheWrite: 2.5e-06}
	if p != want {
		t.Errorf("price = %+v; want %+v", p, want)
	}
	// A model with no published cache rates must not be priced as if a cached
	// request were FREE: the provider-standard multiples fill in.
	np, ok := l.Price(ctx, "no-cache-model")
	if !ok {
		t.Fatal("Price(no-cache-model) not resolved")
	}
	if np.CacheRead == 0 || np.CacheWrite == 0 {
		t.Errorf("missing cache tiers left at zero (%+v): a cached request would price as free", np)
	}
	if np.CacheRead >= np.Input || np.CacheWrite <= np.Input {
		t.Errorf("filled cache tiers are not read<input<write: %+v", np)
	}
	// An entry with no pricing at all must report unknown, not a zero Price.
	if _, ok := l.Price(ctx, "totally-absent-xyz"); ok {
		t.Error("Price() reported ok for an unknown model")
	}
}

func TestPriceCostAndZero(t *testing.T) {
	if !(Price{}).Zero() {
		t.Error("empty Price should be Zero")
	}
	p := Price{Input: 1e-06, Output: 2e-06, CacheRead: 1e-07, CacheWrite: 1.25e-06}
	if p.Zero() {
		t.Error("populated Price should not be Zero")
	}
	// 1000 fresh, 10000 read, 2000 write, 500 out.
	got := p.Cost(1000, 10000, 2000, 500)
	want := 1000*1e-06 + 10000*1e-07 + 2000*1.25e-06 + 500*2e-06
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("Cost = %v; want %v", got, want)
	}
}

func TestChainPriceSkipsNonPricers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(priceSample))
	}))
	defer srv.Close()
	l := NewLiteLLM(srv.URL, srv.Client(), time.Hour)
	ctx := context.Background()
	// Static resolves windows but cannot price; the Chain must fall through to the
	// LiteLLM element rather than reporting unknown.
	c := Chain{DefaultStatic(), l}
	for i := 0; i < 400; i++ {
		if _, ok := c.Price(ctx, "aws/claude-sonnet-5"); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Chain.Price never resolved through a non-Pricer element")
}
