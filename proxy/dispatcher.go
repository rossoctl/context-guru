package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
)

// Dispatcher is cmd/context-guru-proxy's dispatch.Dispatcher: every call a core package makes —
// keep-alive pings, cheap-model completions — leaves the proxy here. It holds, per Ref, the
// upstream and the credential, MASKED; the core holds only the Ref.
//
// Lock order: hold and Release may run under the keeper's lock and take d.mu; Do takes only d.mu.
// Nothing here calls back into the core.
type Dispatcher struct {
	// client never follows a redirect: Go strips Authorization on a cross-host redirect but not
	// x-api-key or x-goog-api-key, so a 307 could carry a key to a host nobody chose.
	client *http.Client
	// price answers Price; nil prices nothing. Set by the Handler from its price table.
	price func(ctx context.Context, endpoint, model string, promptTokens int64) (dispatch.Price, bool)
	now   func() time.Time

	mu   sync.Mutex
	next uint64
	held map[dispatch.Ref]*target
}

type targetKind uint8

const (
	kindSession targetKind = iota // a keep-alive hold; released by the keeper
	kindPinned                    // the operator's configured model; lives as long as the proxy
)

type target struct {
	kind targetKind
	up   upstream
	auth []heldHeader
	// key is the keep-alive entry a session target belongs to; for tests and debugging only.
	key keepalive.Key
}

// NewDispatcher returns an empty Dispatcher sending on a copy of c (nil: a default upstream
// transport) that never follows a redirect.
func NewDispatcher(c *http.Client) *Dispatcher {
	var cc http.Client
	if c != nil {
		cc = *c
	} else {
		cc.Transport = upstreamTransport(0)
	}
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Dispatcher{client: &cc, now: time.Now, held: map[dispatch.Ref]*target{}}
}

func (d *Dispatcher) issueLocked(t *target) dispatch.Ref {
	d.next++
	ref := dispatch.Ref("r" + strconv.FormatUint(d.next, 10))
	d.held[ref] = t
	return ref
}

// Pin holds base and auth for the life of the proxy and returns their Ref — the operator's
// configured cheap model, or a cmd/ tool's single target.
func (d *Dispatcher) Pin(base string, auth http.Header) dispatch.Ref {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.issueLocked(&target{kind: kindPinned, up: upstream{base: base}, auth: maskHeaders(auth)})
}

// hold is what proxy's record passes as keepalive.Record.Hold: only the auth slots, only after our
// own token has been scrubbed out of them, and masked. A server-held key (up.setKey) retains
// nothing — Do injects it at send time, exactly as the request path does.
func (d *Dispatcher) hold(key keepalive.Key, up upstream, reqHeaders http.Header) dispatch.Ref {
	var auth []heldHeader
	if up.setKey == nil {
		slots := http.Header{}
		for _, name := range authHeaders {
			for _, v := range reqHeaders.Values(name) {
				slots.Add(name, v)
			}
		}
		scrubToken(slots)
		auth = maskHeaders(slots)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.issueLocked(&target{kind: kindSession, up: up, auth: auth, key: key})
}

// Release zeroizes and forgets what a Ref held. Safe on an unknown Ref.
func (d *Dispatcher) Release(ref dispatch.Ref) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.held[ref]; ok {
		zeroHeaders(t.auth)
		delete(d.held, ref)
	}
}

func (d *Dispatcher) Price(ctx context.Context, endpoint, model string, promptTokens int64) (dispatch.Price, bool) {
	if d.price == nil {
		return dispatch.Price{}, false
	}
	return d.price(ctx, endpoint, model, promptTokens)
}

// heldCount is how many Refs hold something, for tests.
func (d *Dispatcher) heldCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.held)
}

// Do sends one request. It copies the held material under its own lock, so a Release racing an
// in-flight call zeroizes the held copy and never the one this request is using.
//
// Bounded by the caller's ctx: a core call has no client waiting and must never hold a connection
// for the ten minutes the request path's header timeout allows.
func (d *Dispatcher) Do(ctx context.Context, req dispatch.Request) (dispatch.Response, error) {
	d.mu.Lock()
	t, ok := d.held[req.Ref]
	var up upstream
	var auth []heldHeader
	if ok {
		up = t.up
		auth = make([]heldHeader, len(t.auth))
		for i, a := range t.auth {
			auth[i] = a.clone()
		}
	}
	d.mu.Unlock()
	defer zeroHeaders(auth)
	if !ok {
		return dispatch.Response{}, dispatch.ErrNotHeld
	}
	if up.base == "" {
		return dispatch.Response{}, errNoUpstream
	}
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	// The core zeroes req.Body when Do returns, and the transport may still be writing it then, so
	// the request reads a private copy that is wiped when the transport closes it.
	body := newWipeReader(req.Body)
	hreq, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(up.base, "/")+req.Path, body)
	if err != nil {
		body.Close()
		return dispatch.Response{}, err
	}
	hreq.ContentLength = int64(len(req.Body))
	for name, vs := range req.Header {
		hreq.Header[name] = append([]string(nil), vs...)
	}
	// Unmask as late as possible and wipe the buffer at once. The value becomes a Go string in the
	// header map, which cannot be overwritten — so this bounds the plaintext to this request
	// rather than eliminating it.
	for _, a := range auth {
		credMask(a.val)
		hreq.Header.Add(a.name, string(a.val))
		credZero(a.val)
	}
	setUpstreamAuth(hreq.Header, up)
	resp, err := d.client.Do(hreq)
	if err != nil {
		return dispatch.Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, dispatch.MaxResponseBytes))
	if err != nil {
		return dispatch.Response{Status: resp.StatusCode}, err
	}
	return dispatch.Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}, nil
}

// wipeReader is a request body over a private copy of the caller's bytes, zeroed on Close. The
// transport always closes a request body, and only once it has finished reading it.
type wipeReader struct {
	*bytes.Reader
	buf  []byte
	once sync.Once
}

func newWipeReader(b []byte) *wipeReader {
	buf := append([]byte(nil), b...)
	return &wipeReader{Reader: bytes.NewReader(buf), buf: buf}
}

func (w *wipeReader) Close() error {
	w.once.Do(func() { credZero(w.buf) })
	return nil
}
