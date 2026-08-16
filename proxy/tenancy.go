package proxy

import (
	"container/list"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
)

// Multi-tenancy on the request path.
//
// Single-tenant, the proxy holds one pipeline, one state store and one upstream,
// all fixed at boot. Hosted, every one of those is a property of the authenticated
// caller, and the shared-process versions are not merely inconvenient — sharing a
// state store across tenants means one busy user evicts another's FROZEN compaction
// decisions, and losing a frozen decision re-writes that tenant's whole cached
// suffix at roughly 11.5x the read price. So isolation here is a cost property, not
// only a privacy one.
//
// The seam is deliberately small: Options.Tenants supplies a Tenancy per request,
// and everything downstream (applyMode, observe, serve) reads the pipeline and
// store from the request bundle instead of from the handler. When Options.Tenants
// is nil the handler builds one static Tenancy from its own fields, so
// single-tenant behaviour is byte-identical to before this existed.

// lru is a bounded map, most-recently-used first, that evicts ONE entry when it is
// over its bound. Not goroutine-safe: every user here already holds a mutex over a
// wider critical section, and a second lock inside would only be a second thing to
// get wrong.
//
// One implementation, three users (tenancy cache, rate windows, cached spend),
// because the alternative each of them reached for independently was "replace the
// whole map", which throws away every other tenant's state to bound one entry.
type lru[V any] struct {
	max     int
	ll      *list.List // front = most recently used; values are *lruEntry[V]
	index   map[string]*list.Element
	onEvict func(string, V)
}

type lruEntry[V any] struct {
	k string
	v V
}

func newLRU[V any](max int, onEvict func(string, V)) *lru[V] {
	if max <= 0 {
		max = 1
	}
	return &lru[V]{max: max, ll: list.New(), index: map[string]*list.Element{}, onEvict: onEvict}
}

func (c *lru[V]) get(k string) (V, bool) {
	el, ok := c.index[k]
	if !ok {
		var zero V
		return zero, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*lruEntry[V]).v, true
}

func (c *lru[V]) put(k string, v V) {
	if el, ok := c.index[k]; ok {
		el.Value.(*lruEntry[V]).v = v
		c.ll.MoveToFront(el)
		return
	}
	c.index[k] = c.ll.PushFront(&lruEntry[V]{k: k, v: v})
	for c.ll.Len() > c.max {
		back := c.ll.Back()
		e := back.Value.(*lruEntry[V])
		c.ll.Remove(back)
		delete(c.index, e.k)
		if c.onEvict != nil {
			c.onEvict(e.k, e.v)
		}
	}
}

func (c *lru[V]) remove(k string) {
	if el, ok := c.index[k]; ok {
		c.ll.Remove(el)
		delete(c.index, k)
	}
}

// Tenancy is everything the request path needs to know about the authenticated
// caller. Built once per request (cached per tenant), and NEVER mutated after it is
// published — readers on the request path (spendgate, captureContentFor, newCapture,
// upstreamFor) take no lock, so a refresh publishes a new pointer instead of writing
// through the old one. See tenancy().
type Tenancy struct {
	// ID is the tenant this request belongs to; "" in single-tenant mode, which is
	// also what lands in the dashboard's tenant_id column for pre-tenancy rows.
	ID string
	// Label and Manager are for the dashboard and the /stats gate, not the hot path.
	Label   string
	Manager bool
	// Preset labels captured rows so the dashboard can compare configurations.
	Preset string
	// Pipe is this tenant's compaction pipeline.
	Pipe *components.Pipeline
	// Store holds this tenant's offloaded originals and frozen decisions. Per tenant,
	// for the eviction reason in this file's header comment.
	Store store.Store
	// Shadow is observe mode's disjoint store for this tenant. See Handler.shadow.
	Shadow store.Store
	// Mode is this tenant's operating mode (sync or observe).
	Mode components.Mode
	// CaptureContent is this tenant's consent to storing transcript text.
	CaptureContent bool
	// MonthlyCapUSD is this tenant's spend ceiling against the shared upstream
	// credential. 0 = no cap.
	MonthlyCapUSD float64
	// Upstream names, by dialect, into Options.Upstreams.
	UpAnthropic, UpOpenAI, UpBob string
}

// Upstream is one resolved entry of the server's allow-list.
type Upstream struct {
	Dialect string
	BaseURL string
	KeyEnv  string
	Header  string
}

// setKey returns the credential injector for this upstream. The key is read from
// the environment at CALL time, so rotating a credential does not need a restart,
// and no copy of it is held in a long-lived struct.
func (u Upstream) setKey() func(http.Header) {
	key := os.Getenv(u.KeyEnv)
	if key == "" {
		// LoadUpstreams refuses to boot on an unset key_env, so this is only
		// reachable if the variable was cleared at runtime. Returning nil here would
		// forward the caller's own token upstream; refusing to inject is not an
		// option either, so callers treat a nil injector in hosted mode as fatal for
		// the request. See Handler.upstreamFor.
		return nil
	}
	if h := u.Header; h != "" && !strings.EqualFold(h, "Authorization") {
		return func(hd http.Header) { hd.Set(h, key) }
	}
	if u.Dialect == "anthropic" {
		return func(hd http.Header) { hd.Set("x-api-key", key) }
	}
	return func(hd http.Header) { hd.Set("Authorization", "Bearer "+key) }
}

// tokenHeaders are the request slots a caller may present its context-guru token
// in. Three, because one token has to work for agents that disagree about how to
// send a credential — Claude Code uses Authorization or x-api-key, Bob and other
// Gemini-CLI descendants use x-goog-api-key — and the whole point of the design is
// that a user configures one token once and never thinks about it again.
//
// Every slot listed here is stripped before forwarding (see doUpstream). A slot we
// accept a credential in is a slot that must never reach an upstream.
var tokenHeaders = []string{"Authorization", "x-api-key", "x-goog-api-key"}

// TokenFromRequest extracts a bearer-ish credential from the first slot that has
// one. It does not validate: an unrecognised value is left for the registry to
// reject, so there is exactly one place that decides what a valid token is.
func TokenFromRequest(r *http.Request) string {
	for _, h := range tokenHeaders {
		// Accept "Bearer <t>" and a bare token in any slot; agents differ, and a
		// tenant should not have to know which convention its tool picked. Splitting
		// on fields rather than trimming a prefix so that a lone "Bearer" yields no
		// token at all, instead of the word "Bearer" being sent off to be looked up.
		f := strings.Fields(r.Header.Get(h))
		switch {
		case len(f) == 2 && strings.EqualFold(f[0], "bearer"):
			return f[1]
		case len(f) == 1 && !strings.EqualFold(f[0], "bearer"):
			return f[0]
		}
	}
	return ""
}

// statusError carries the HTTP status a resolution failure should produce.
type statusError struct {
	code int
	msg  string
}

func (e statusError) Error() string   { return e.msg }
func (e statusError) HTTPStatus() int { return e.code }

// StatusError is implemented by resolution errors that name their own HTTP status.
type StatusError interface {
	error
	HTTPStatus() int
}

// Resolution failures. Authentication fails CLOSED: a missing or unknown token is
// never treated as an anonymous or new tenant.
var (
	errNoToken       = statusError{http.StatusUnauthorized, "no context-guru token; see /dashboard/ to register"}
	errBadToken      = statusError{http.StatusUnauthorized, "unknown or revoked context-guru token"}
	errTenantOff     = statusError{http.StatusForbidden, "this context-guru account is disabled"}
	errNoUpstreamFor = statusError{http.StatusBadGateway, "no upstream configured for this route"}
)

// statusOf maps a resolution error to its HTTP status, defaulting to 401 rather
// than 500: every error out of the auth path is an auth failure unless it says
// otherwise, and defaulting to 500 would turn a bad token into a page.
func statusOf(err error) (int, string) {
	var se StatusError
	if errors.As(err, &se) {
		return se.HTTPStatus(), se.Error()
	}
	return http.StatusUnauthorized, "unauthorized"
}

// BuiltConfig is what one configuration document expands to.
type BuiltConfig struct {
	Pipe   *components.Pipeline
	Store  store.Store
	Mode   components.Mode
	Preset string
}

// ConfigBuilder expands a tenant's configuration document into a runnable
// pipeline and state store. Supplied by the host rather than called directly, so
// this package stays decoupled from `config` — the same arrangement Options.
// PipelineFor already uses, and the reason the bifrost adapter can reuse all of
// this without dragging the file loader in.
type ConfigBuilder func(doc []byte, e components.Emitter) (BuiltConfig, error)

// TenantSource turns an authenticated token into a Tenancy, caching the built
// pipeline and state store per tenant.
//
// The cache is not a speed optimisation. A tenant's Store accumulates the frozen
// compaction decisions that make savings COMPOUND across turns; rebuilding it per
// request would throw that away every time and reduce the service to
// single-turn-only compaction. Which also means eviction is not free, so the LRU is
// bounded by tenant count and logs what it drops.
type TenantSource struct {
	reg     *tenant.Registry
	emitter components.Emitter
	builder ConfigBuilder
	max     int

	mu    sync.Mutex
	cache *lru[*cached] // tenant id -> built tenancy
}

type cached struct {
	config string // the config document this tenancy was built from
	t      *Tenancy
}

// DefaultMaxTenancies bounds how many tenants hold live pipelines and stores at
// once. Each is a few hundred KB of state plus its store's entry cap, so this is a
// memory bound; a box serving more than this many CONCURRENTLY active users wants
// the number raised, not the cache removed.
const DefaultMaxTenancies = 256

// NewTenantSource builds a resolver over a control-plane registry. maxTenancies <= 0
// uses DefaultMaxTenancies.
func NewTenantSource(reg *tenant.Registry, e components.Emitter, b ConfigBuilder, maxTenancies int) *TenantSource {
	if maxTenancies <= 0 {
		maxTenancies = DefaultMaxTenancies
	}
	if e == nil {
		e = components.NopEmitter{}
	}
	return &TenantSource{reg: reg, emitter: e, builder: b, max: maxTenancies,
		cache: newLRU[*cached](maxTenancies, func(id string, c *cached) {
			// Worth a log line, not a silent drop: this tenant's next turn re-writes its
			// cached prefix, which shows up as a cost spike with no other explanation.
			slog.Warn("context-guru: evicting an idle tenant's compaction state (cache cold on its next turn)",
				"tenant", id, "max_tenancies", maxTenancies)
		})}
}

// Resolve authenticates a request and returns its tenancy.
func (s *TenantSource) Resolve(r *http.Request) (*Tenancy, error) {
	tok := TokenFromRequest(r)
	if tok == "" {
		return nil, errNoToken
	}
	t, err := s.reg.Resolve(tok)
	switch {
	case errors.Is(err, tenant.ErrDisabled):
		return nil, errTenantOff
	case err != nil:
		return nil, errBadToken
	}
	return s.tenancy(t)
}

// ForTenant returns the tenancy for an already-authenticated tenant, for callers
// that authenticated some other way (a dashboard cookie).
func (s *TenantSource) ForTenant(t *tenant.Tenant) (*Tenancy, error) {
	if t == nil {
		return nil, errNoToken
	}
	if t.Disabled {
		return nil, errTenantOff
	}
	return s.tenancy(t)
}

func (s *TenantSource) tenancy(t *tenant.Tenant) (*Tenancy, error) {
	cfgDoc := s.reg.Config(t)

	s.mu.Lock()
	if c, ok := s.cache.get(t.ID); ok {
		// A configuration change must take effect on the next request, so the cache
		// key includes the document. Rebuilding drops this tenant's frozen decisions,
		// which is the honest cost of changing your own pipeline mid-session.
		if c.config == cfgDoc {
			// Fields that do not affect the pipeline are refreshed without a rebuild —
			// but on a COPY, published by swapping the pointer. Writing through the
			// cached *Tenancy would race every unlocked reader on the request path
			// (spendgate's MonthlyCapUSD, captureContentFor, newCapture, upstreamFor),
			// which is one agent with two turns in flight, not a rare interleaving.
			tn := *c.t
			tn.Label, tn.Manager = t.Label, t.IsManager()
			tn.CaptureContent, tn.MonthlyCapUSD = t.CaptureContent, t.MonthlyCapUSD
			tn.UpAnthropic, tn.UpOpenAI, tn.UpBob = t.UpAnthropic, t.UpOpenAI, t.UpBob
			// Pipe/Store/Shadow are shared with the previous copy on purpose: they are
			// internally synchronised, and per-tenant state must not be duplicated.
			c.t = &tn
			s.mu.Unlock()
			return &tn, nil
		}
		s.cache.remove(t.ID)
		slog.Info("context-guru: tenant configuration changed; rebuilding pipeline",
			"tenant", t.ID)
	}
	s.mu.Unlock()

	// Build outside the lock: constructing a pipeline can be slow (tree-sitter
	// parsers, tokenizer tables) and must not serialise every other tenant's request.
	tn, err := s.build(t, cfgDoc)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Another goroutine may have built the same tenant concurrently; keep the one
	// already published so two requests never hold different stores for one tenant.
	if c, ok := s.cache.get(t.ID); ok {
		if c.config == cfgDoc {
			return c.t, nil
		}
		s.cache.remove(t.ID)
	}
	s.cache.put(t.ID, &cached{config: cfgDoc, t: tn})
	return tn, nil
}

// build constructs a tenancy from a configuration document. It fails OPEN in one
// specific way: a tenant whose configuration no longer builds gets the empty
// pipeline (pass-through) rather than an error, because a bad config row must not
// take someone's agent offline. It is logged loudly, and the settings page
// validates on write so this should be unreachable.
func (s *TenantSource) build(t *tenant.Tenant, cfgDoc string) (*Tenancy, error) {
	tn := &Tenancy{
		ID: t.ID, Label: t.Label, Manager: t.IsManager(),
		CaptureContent: t.CaptureContent, MonthlyCapUSD: t.MonthlyCapUSD,
		UpAnthropic: t.UpAnthropic, UpOpenAI: t.UpOpenAI, UpBob: t.UpBob,
		Mode: components.ModeSync,
	}
	built, err := s.builder([]byte(cfgDoc), s.emitter)
	if err != nil {
		slog.Error("context-guru: tenant configuration failed to build; forwarding uncompacted",
			"tenant", t.ID, "err", err)
		tn.Pipe = components.NewPipeline(nil, s.emitter)
		tn.Store = store.Nop{}
		tn.Shadow = store.Nop{}
		tn.Preset = "invalid"
		return tn, nil
	}
	tn.Pipe, tn.Store, tn.Preset = built.Pipe, built.Store, built.Preset
	if built.Mode != "" {
		tn.Mode = built.Mode
	}
	// Observe mode needs a store as persistent as the live one and completely
	// disjoint from it (see Handler.shadow). Allocated per tenant for the same
	// reason the live store is.
	tn.Shadow = store.NewMemory(store.Options{})
	return tn, nil
}

// upstreamFor resolves the upstream for one route. In single-tenant mode the
// statically configured upstream is used unchanged. In hosted mode the tenant's
// chosen NAME is looked up in the operator's allow-list — a tenant never supplies a
// URL — and a missing credential fails the request rather than forwarding the
// caller's own token to a third party.
func (h *Handler) upstreamFor(tn *Tenancy, pick func(*Tenancy) string, static upstream) (upstream, error) {
	if h.opts.Tenants == nil {
		return static, nil
	}
	name := strings.TrimSpace(pick(tn))
	if name == "" {
		return upstream{}, errNoUpstreamFor
	}
	u, ok := h.opts.Upstreams[name]
	if !ok || u.BaseURL == "" {
		slog.Error("context-guru: tenant selected an upstream that is not in the allow-list",
			"tenant", tn.ID, "upstream", name)
		return upstream{}, errNoUpstreamFor
	}
	set := u.setKey()
	if set == nil {
		slog.Error("context-guru: upstream has no credential in the environment; refusing to forward",
			"upstream", name, "key_env", u.KeyEnv)
		return upstream{}, statusError{http.StatusBadGateway,
			fmt.Sprintf("upstream %q has no credential configured", name)}
	}
	return upstream{base: u.BaseURL, path: static.path, setKey: set}, nil
}

// statsTrusted reports whether a caller may read the process-wide /stats
// aggregate. Loopback is the default because the benchmark harnesses that parse
// /stats run on the same host as the proxy; anything else needs an explicit
// predicate from the host (typically "is a manager").
//
// "Loopback" alone is NOT sufficient, and assuming it was is how this gate came to
// leak. Behind a reverse proxy on the same host — the supported deployment, see
// deploy/service/nginx.conf — EVERY request arrives with RemoteAddr 127.0.0.1,
// because the peer is nginx rather than the caller. A bare loopback test therefore
// stops meaning "the caller is on this host" the moment TLS is terminated in front,
// and hands the whole network a service-wide aggregate over every tenant. Observed
// live: `curl https://<host>/stats` with no credential returned the full rollup.
//
// So a loopback peer is trusted only when it did NOT relay someone else. Forwarded
// headers are the tell, and they are trustworthy in exactly this direction: a remote
// caller cannot make RemoteAddr loopback, so it cannot reach this branch at all, and
// a loopback peer that sets them is the front end. This is the same asymmetry
// registrantIP relies on, used to the opposite end.
//
// Fails closed on purpose: a front end that sets none of these headers loses /stats
// rather than silently exposing it. Local ops (`curl 127.0.0.1:4000/stats`) and the
// Prometheus scrape of 127.0.0.1:4000 send no forwarded headers and keep working.
func (h *Handler) statsTrusted(r *http.Request) bool {
	if h.opts.StatsTrusted != nil {
		return h.opts.StatsTrusted(r)
	}
	ip := net.ParseIP(clientIP(r))
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Forwarded-Proto", "X-Forwarded-Host"} {
		if r.Header.Get(hdr) != "" {
			return false
		}
	}
	return true
}

// tenancyFor resolves the caller's tenancy, or the static single-tenant one.
func (h *Handler) tenancyFor(r *http.Request) (*Tenancy, error) {
	if h.opts.Tenants == nil {
		return h.static, nil
	}
	return h.opts.Tenants.Resolve(r)
}

// failAuth writes a resolution failure. The body names the failure but never
// distinguishes "no such token" from "revoked token" beyond what the caller can
// already determine, and it always points at the one place a user can fix it.
func failAuth(w http.ResponseWriter, err error) {
	code, msg := statusOf(err)
	// Count the refusal. Every auth and upstream-resolution failure funnels through
	// here, so this is the one place that sees them all — and 402/429 are deliberately
	// NOT counted here, because the limit that decided them counted them already (see
	// limits.go, spendgate.go) and only there is it known which limit it was.
	switch code {
	case http.StatusUnauthorized:
		recordRefusal(refuseAuth, "")
	case http.StatusForbidden:
		recordRefusal(refuseForbidden, "")
	case http.StatusBadGateway:
		recordRefusal(refuseNoUpstream, "")
	}
	w.Header().Set("Content-Type", "application/json")
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="context-guru"`)
	}
	w.WriteHeader(code)
	// Hand-rolled rather than encoding/json: msg is one of this file's constants or
	// an upstream name, and a marshal error here would leave a bare status.
	fmt.Fprintf(w, "{\"error\":%q}\n", msg)
}
