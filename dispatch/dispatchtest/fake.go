package dispatchtest

import (
	"context"
	"strconv"
	"sync"

	"github.com/rossoctl/context-guru/dispatch"
)

// Fake is a recording Dispatcher for core tests. Respond answers every Do on a pinned ref; Prices
// answers Price (nil: nothing is priced).
type Fake struct {
	Respond func(dispatch.Request) (dispatch.Response, error)
	Prices  func(endpoint, model string, promptTokens int64) (dispatch.Price, bool)

	mu   sync.Mutex
	next int
	live map[dispatch.Ref]bool
	sent []dispatch.Request
}

func NewFake(respond func(dispatch.Request) (dispatch.Response, error)) *Fake {
	return &Fake{Respond: respond, live: map[dispatch.Ref]bool{}}
}

// Pin issues a ref that Do answers until Release.
func (f *Fake) Pin() dispatch.Ref {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	ref := dispatch.Ref("fake:" + strconv.Itoa(f.next))
	f.live[ref] = true
	return ref
}

func (f *Fake) Do(_ context.Context, r dispatch.Request) (dispatch.Response, error) {
	f.mu.Lock()
	if !f.live[r.Ref] {
		f.mu.Unlock()
		return dispatch.Response{}, dispatch.ErrNotHeld
	}
	c := r
	c.Body = append([]byte(nil), r.Body...)
	c.Header = r.Header.Clone()
	f.sent = append(f.sent, c)
	respond := f.Respond
	f.mu.Unlock()
	return respond(r)
}

func (f *Fake) Release(ref dispatch.Ref) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, ref)
}

func (f *Fake) Price(_ context.Context, endpoint, model string, promptTokens int64) (dispatch.Price, bool) {
	if f.Prices == nil {
		return dispatch.Price{}, false
	}
	return f.Prices(endpoint, model, promptTokens)
}

// Sent returns copies of every request Do accepted, in order.
func (f *Fake) Sent() []dispatch.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dispatch.Request(nil), f.sent...)
}
