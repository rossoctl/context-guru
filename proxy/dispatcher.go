package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
)

// IncomingRefTTL is how long an incoming-model ref (the caller's own endpoint and credential,
// resolved per request for a NeedsModel component) is kept after its last use. A detached call
// (cache_aware_summarizer's own background summary, a keep-alive substitute) can still be running
// after its triggering request returned, so the ref is not dropped at request end — but a request
// that spawns no such call must not hold the caller's credential forever either.
const IncomingRefTTL = 2 * time.Hour

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
	// incomingByKey indexes the kindIncoming entries in held by their dedupKey, so incoming()
	// resolves in O(1) instead of scanning every held ref (session and pinned refs included) on
	// every proxied request.
	incomingByKey map[string]dispatch.Ref
}

type targetKind uint8

const (
	kindSession  targetKind = iota // a keep-alive hold; released by the keeper
	kindPinned                     // the operator's configured model; lives as long as the proxy
	kindIncoming                   // a per-request cheap-model ref, deduped and TTL-expired
)

type target struct {
	kind targetKind
	up   upstream
	auth []heldHeader
	// key is the keep-alive entry a session target belongs to; for tests and debugging only.
	key keepalive.Key
	// dedupKey and lastUse are kindIncoming-only: dedupKey groups refs by (base, credential) so
	// repeated calls for the same caller share one, and lastUse drives IncomingRefTTL expiry.
	dedupKey string
	lastUse  time.Time
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
	return &Dispatcher{client: &cc, now: time.Now, held: map[dispatch.Ref]*target{},
		incomingByKey: map[string]dispatch.Ref{}}
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

// CheapModelConfig is the operator's own "config"-source cheap model: a dedicated endpoint and
// credential, pinned once at startup rather than resolved per request.
type CheapModelConfig struct {
	Provider  string // anthropic (default) | openai
	BaseURL   string // default: the provider's public API
	APIKey    string
	Auth      string // anthropic only: "" | x-api-key (default) | bearer
	Model     string
	MaxTokens int
}

// pinCheapModel pins cfg's endpoint and credential and returns the components.Model to call it.
func (d *Dispatcher) pinCheapModel(cfg CheapModelConfig) components.Model {
	if cfg.Provider == "openai" {
		base := cfg.BaseURL
		if base == "" {
			base = "https://api.openai.com"
		}
		ref := d.Pin(base, AuthHeader(bschemas.OpenAI, cfg.APIKey, ""))
		return cheapmodel.OpenAI{D: d, Ref: ref, Model: cfg.Model, MaxTokens: cfg.MaxTokens}
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	ref := d.Pin(base, AuthHeader(bschemas.Anthropic, cfg.APIKey, cfg.Auth))
	return cheapmodel.Anthropic{D: d, Ref: ref, Model: cfg.Model, MaxTokens: cfg.MaxTokens}
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

// incoming resolves the ref for a NeedsModel component calling the request's own endpoint and
// credential (incomingModel). Two calls for the same (base, credential) share one ref and one
// lastUse clock, via incomingByKey. An idle one expires after IncomingRefTTL: checked here and in
// Do (both touch the ref directly, so expiring it there is free), and otherwise by sweepIncoming,
// which the keeper's OnTick drives — the backstop for a ref nothing ever asks about again.
func (d *Dispatcher) incoming(up upstream, auth http.Header) dispatch.Ref {
	key := incomingDedupKey(up.base, auth)
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if ref, ok := d.incomingByKey[key]; ok {
		t := d.held[ref]
		if now.Sub(t.lastUse) <= IncomingRefTTL {
			t.lastUse = now
			return ref
		}
		d.dropIncomingLocked(ref, t)
	}
	ref := d.issueLocked(&target{kind: kindIncoming, up: up, auth: maskHeaders(auth),
		dedupKey: key, lastUse: now})
	d.incomingByKey[key] = ref
	return ref
}

// dropIncomingLocked removes one kindIncoming entry from both held and incomingByKey. Called
// under d.mu.
func (d *Dispatcher) dropIncomingLocked(ref dispatch.Ref, t *target) {
	zeroHeaders(t.auth)
	delete(d.held, ref)
	if d.incomingByKey[t.dedupKey] == ref {
		delete(d.incomingByKey, t.dedupKey)
	}
}

// sweepIncoming drops every kindIncoming entry idle past IncomingRefTTL. Driven by the keeper's
// OnTick (proxy/keepalive.go), so a ref nothing asks about again — the common case: a request
// whose component never calls the model, or a caller whose key is never reused — is freed on its
// own instead of surviving for the life of the process.
func (d *Dispatcher) sweepIncoming(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, ref := range d.incomingByKey {
		if t := d.held[ref]; t != nil && now.Sub(t.lastUse) > IncomingRefTTL {
			zeroHeaders(t.auth)
			delete(d.held, ref)
			delete(d.incomingByKey, key)
		}
	}
}

// incomingDedupKey groups by base URL and the exact credential presented, order-independent over
// header values: two calls differ only if the caller or the destination actually differs.
func incomingDedupKey(base string, auth http.Header) string {
	h := sha256.New()
	h.Write([]byte(base))
	names := make([]string, 0, len(auth))
	for name := range auth {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		h.Write([]byte{0})
		h.Write([]byte(name))
		vs := append([]string(nil), auth[name]...)
		sort.Strings(vs)
		for _, v := range vs {
			h.Write([]byte{0})
			h.Write([]byte(v))
		}
	}
	return string(h.Sum(nil))
}

// AuthHeader is the header a provider credential goes in, in the exact shape incomingModel and
// cmd/ tools present it: x-api-key (or bearer, for an Anthropic gateway that wants one) for
// Anthropic, Authorization: Bearer for OpenAI.
func AuthHeader(p bschemas.ModelProvider, key, scheme string) http.Header {
	if p == bschemas.OpenAI || scheme == "bearer" {
		return http.Header{"Authorization": {"Bearer " + key}}
	}
	return http.Header{"X-Api-Key": {key}}
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
	if ok && t.kind == kindIncoming {
		if d.now().Sub(t.lastUse) > IncomingRefTTL {
			d.dropIncomingLocked(req.Ref, t)
			ok = false
		} else {
			// A Do counts as use too, matching IncomingRefTTL's own "without use" wording —
			// otherwise a detached call that keeps calling Do past 2h since the last incoming()
			// could get ErrNotHeld partway through a long-running substitute.
			t.lastUse = d.now()
		}
	}
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
	// wipeReader is not *bytes.Reader either, so http.NewRequest cannot infer GetBody from it —
	// and without GetBody, net/http never retries a write that failed on a reused idle
	// connection. req.Body is still live here (the core zeroes it only after Do returns), so a
	// fresh copy for a retry is safe to make on demand.
	hreq.GetBody = func() (io.ReadCloser, error) { return newWipeReader(req.Body), nil }
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
