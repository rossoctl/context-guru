package keepalive

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DispatchRequest is what a test sees of one ping: the dispatch.Request, with the Ref mapped back
// to the entry's Key and the model read out of the body.
type DispatchRequest struct {
	Key     Key
	Route   string
	Model   string
	Body    []byte
	Headers http.Header
}

// pricer is the old keeper-side pricing shape, kept for tests: the fake's Price delegates to it.
type pricer interface {
	Price(ctx context.Context, endpoint, model string, promptTokens int64) (Price, bool)
}

// fakeDispatcher is the keeper's host in tests. resp answers each ping in Usage terms; Do encodes
// that Usage into an Anthropic response body, so every ping round-trips through PingUsage exactly
// as a real host's response would.
type fakeDispatcher struct {
	mu       sync.Mutex
	resp     func(DispatchRequest) (Usage, int, error)
	prices   pricer
	sent     []DispatchRequest
	held     map[Key]int // Hold count minus Release count, per key
	refKey   map[dispatch.Ref]Key
	released []Key
	events   []string // "hold:<key>" and "release:<key>", in call order
	next     int
}

func newFake(resp func(DispatchRequest) (Usage, int, error)) *fakeDispatcher {
	return &fakeDispatcher{resp: resp, held: map[Key]int{}, refKey: map[dispatch.Ref]Key{}}
}

func (f *fakeDispatcher) Do(_ context.Context, r dispatch.Request) (dispatch.Response, error) {
	f.mu.Lock()
	key, ok := f.refKey[r.Ref]
	if !ok || f.held[key] <= 0 {
		f.mu.Unlock()
		return dispatch.Response{}, dispatch.ErrNotHeld
	}
	dr := DispatchRequest{Key: key, Route: r.Path, Model: gjson.GetBytes(r.Body, "model").String(),
		Body: append([]byte(nil), r.Body...), Headers: r.Header.Clone()}
	f.sent = append(f.sent, dr)
	resp := f.resp
	f.mu.Unlock()
	u, status, err := resp(dr)
	if err != nil {
		return dispatch.Response{}, err
	}
	return dispatch.Response{Status: status, Body: anthropicUsageBody(u)}, nil
}

// anthropicUsageBody is the response a provider would send for u.
func anthropicUsageBody(u Usage) []byte {
	b := []byte(`{}`)
	b, _ = sjson.SetBytes(b, "usage.input_tokens", u.FreshInput)
	b, _ = sjson.SetBytes(b, "usage.cache_read_input_tokens", u.CacheRead)
	b, _ = sjson.SetBytes(b, "usage.cache_creation_input_tokens", u.CacheWrite)
	b, _ = sjson.SetBytes(b, "usage.cache_creation.ephemeral_1h_input_tokens", u.CacheWrite1h)
	b, _ = sjson.SetBytes(b, "usage.output_tokens", u.Output)
	if u.StopReason != "" {
		b, _ = sjson.SetBytes(b, "stop_reason", u.StopReason)
	}
	return b
}

func (f *fakeDispatcher) Release(ref dispatch.Ref) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.refKey[ref]
	delete(f.refKey, ref)
	f.held[k]--
	f.released = append(f.released, k)
	f.events = append(f.events, fmt.Sprintf("release:%v", k))
}

func (f *fakeDispatcher) Price(ctx context.Context, endpoint, model string, prompt int64) (Price, bool) {
	f.mu.Lock()
	p := f.prices
	f.mu.Unlock()
	if p == nil {
		return Price{}, false
	}
	return p.Price(ctx, endpoint, model, prompt)
}

// hold is what a test passes as Record.Hold.
func (f *fakeDispatcher) hold(k Key) func() dispatch.Ref {
	return func() dispatch.Ref {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.next++
		ref := dispatch.Ref(fmt.Sprintf("ref:%d", f.next))
		f.refKey[ref] = k
		f.held[k]++
		f.events = append(f.events, fmt.Sprintf("hold:%v", k))
		return ref
	}
}

// setPrices swaps the price source, under the lock: a fire goroutine may read it.
func (f *fakeDispatcher) setPrices(p pricer) {
	f.mu.Lock()
	f.prices = p
	f.mu.Unlock()
}

// log returns a copy of the hold/release event log, read under the lock.
func (f *fakeDispatcher) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// n is the ordinary-ping count, read under the lock.
func (f *fakeDispatcher) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// holds reads one key's Hold-minus-Release count under the lock.
func (f *fakeDispatcher) holds(k Key) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[k]
}

// setResp swaps what Dispatch returns, under the lock: a fire goroutine may read it.
func (f *fakeDispatcher) setResp(resp func(DispatchRequest) (Usage, int, error)) {
	f.mu.Lock()
	f.resp = resp
	f.mu.Unlock()
}

// okPing is the default response: a pure cache READ of a 48,576-token prefix.
func okPing(DispatchRequest) (Usage, int, error) {
	return Usage{CacheRead: 48576, Output: 1}, http.StatusOK, nil
}

// proxyPricer adapts a model-only modelinfo.Pricer, the shape the proxy's price table has, to
// pricer by ignoring the endpoint and the prompt size.
type proxyPricer struct{ p modelinfo.Pricer }

func (a proxyPricer) Price(ctx context.Context, _, model string, _ int64) (Price, bool) {
	return a.p.Price(ctx, model)
}

// fixedPrice prices every model the same, so a cost guard can be tested without a price file.
type fixedPrice struct{ p modelinfo.Price }

func (f fixedPrice) Price(context.Context, string) (modelinfo.Price, bool) { return f.p, true }
