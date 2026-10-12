package keepalive

import (
	"context"
	"net/http"
	"sync"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// fakeDispatcher replaces today's k.send override. resp is what each Dispatch returns.
type fakeDispatcher struct {
	mu       sync.Mutex
	resp     func(DispatchRequest) (Usage, int, error)
	sent     []DispatchRequest
	held     map[Key]int // Hold count minus Release count, per key
	released []Key
}

func newFake(resp func(DispatchRequest) (Usage, int, error)) *fakeDispatcher {
	return &fakeDispatcher{resp: resp, held: map[Key]int{}}
}

func (f *fakeDispatcher) Dispatch(_ context.Context, r DispatchRequest) (Usage, int, error) {
	f.mu.Lock()
	f.sent = append(f.sent, DispatchRequest{Key: r.Key, Route: r.Route, Model: r.Model,
		Body: append([]byte(nil), r.Body...), Headers: r.Headers.Clone()})
	resp := f.resp
	f.mu.Unlock()
	return resp(r)
}

func (f *fakeDispatcher) Release(k Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[k]--
	f.released = append(f.released, k)
}

// hold is what a test passes as Record.Hold.
func (f *fakeDispatcher) hold(k Key) func() {
	return func() { f.mu.Lock(); f.held[k]++; f.mu.Unlock() }
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
// Pricer by ignoring the endpoint and the prompt size.
type proxyPricer struct{ p modelinfo.Pricer }

func (a proxyPricer) Price(ctx context.Context, _, model string, _ int64) (Price, bool) {
	return a.p.Price(ctx, model)
}

// fixedPrice prices every model the same, so a cost guard can be tested without a price file.
type fixedPrice struct{ p modelinfo.Price }

func (f fixedPrice) Price(context.Context, string) (modelinfo.Price, bool) { return f.p, true }
