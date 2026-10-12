package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/rossoctl/context-guru/credmask"
	"github.com/rossoctl/context-guru/keepalive"
)

// httpDispatcher is cmd/context-guru-proxy's keepalive.Dispatcher. It holds, per tracked
// thread, the upstream the request went to and — when that upstream is caller-pays — the
// caller's credential, MASKED. See credmask and keepalive.Record.Hold.
type httpDispatcher struct {
	client *http.Client
	mu     sync.Mutex
	held   map[keepalive.Key]heldUpstream
}

type heldUpstream struct {
	up   upstream
	auth []credmask.Header
}

func newHTTPDispatcher(c *http.Client) *httpDispatcher {
	return &httpDispatcher{client: c, held: map[keepalive.Key]heldUpstream{}}
}

// hold is what proxy's record passes as keepalive.Record.Hold. The credential half of the old
// pingHeaders, unchanged: only the auth slots, only after our own token has been scrubbed out of
// them, and masked. A server-held key (up.setKey) retains nothing.
func (d *httpDispatcher) hold(key keepalive.Key, up upstream, r *http.Request) {
	var auth []credmask.Header
	if up.setKey == nil {
		slots := http.Header{}
		for _, name := range authHeaders {
			for _, v := range r.Header.Values(name) {
				slots.Add(name, v)
			}
		}
		scrubToken(slots)
		for name, vs := range slots {
			for _, v := range vs {
				b := []byte(v)
				credmask.Mask(b)
				auth = append(auth, credmask.Header{Name: name, Val: b})
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if prev, ok := d.held[key]; ok {
		for _, a := range prev.auth {
			credmask.Zero(a.Val)
		}
	}
	d.held[key] = heldUpstream{up: up, auth: auth}
}

// Release zeroizes and forgets what hold kept. Called under the keeper's lock: no keeper calls.
func (d *httpDispatcher) Release(key keepalive.Key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.held[key]; ok {
		for _, a := range h.auth {
			credmask.Zero(a.Val)
		}
		delete(d.held, key)
	}
}

// Dispatch is the old sendPing. It copies the held material under its own lock, so a Release
// racing an in-flight ping zeroizes the held copy and never the one this request is using.
//
// Bounded by the keeper's own timeout (ctx): a ping has no client waiting and must never hold a
// concurrency slot for the ten minutes the request path's header timeout allows.
func (d *httpDispatcher) Dispatch(ctx context.Context, req keepalive.DispatchRequest) (keepalive.Usage, int, error) {
	d.mu.Lock()
	h, ok := d.held[req.Key]
	var auth []credmask.Header
	if ok {
		auth = make([]credmask.Header, len(h.auth))
		for i, a := range h.auth {
			auth[i] = a.Clone()
		}
	}
	d.mu.Unlock()
	defer func() {
		for _, a := range auth {
			credmask.Zero(a.Val)
		}
	}()
	if !ok || h.up.base == "" {
		return keepalive.Usage{}, 0, errNoUpstream
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.up.base+h.up.path,
		strings.NewReader(string(req.Body)))
	if err != nil {
		return keepalive.Usage{}, 0, err
	}
	for name, vs := range req.Headers {
		hreq.Header[name] = append([]string(nil), vs...)
	}
	// Unmask the credential as late as possible and wipe the buffer immediately. The value
	// necessarily becomes a Go string once it is in the header map, and a string cannot be
	// overwritten — so this bounds the plaintext to this request rather than eliminating it.
	for _, a := range auth {
		credmask.Mask(a.Val)
		hreq.Header.Add(a.Name, string(a.Val))
		credmask.Zero(a.Val)
	}
	setUpstreamAuth(hreq.Header, h.up)
	resp, err := d.client.Do(hreq)
	if err != nil {
		return keepalive.Usage{}, 0, err
	}
	defer resp.Body.Close()
	// Bounded read: a ping's response is a few hundred bytes of JSON, and an upstream that
	// answers a max_tokens:1 request with megabytes is not one to buffer.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil && !errors.Is(err, io.EOF) {
		return keepalive.Usage{}, resp.StatusCode, err
	}
	u, _ := responseUsage(resp.Header.Get("Content-Type"), raw)
	return keepalive.Usage{FreshInput: u.FreshInput, CacheRead: u.CacheRead,
		CacheWrite: u.CacheWrite, Output: u.Output, CacheWrite1h: u.CacheWrite1h,
		StopReason: u.StopReason}, resp.StatusCode, nil
}
