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
	"github.com/rossoctl/context-guru/internal/logging"
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
	// keep, when set, reports that an entry is EXPENSIVE to drop, and moves it to the
	// back of the queue of candidates rather than pinning it: the bound still holds. Set
	// it when recency alone picks the wrong victim — see TenantSource, where one request
	// from each of max+1 throwaway accounts otherwise evicts every real tenant's frozen
	// compaction state at ~11.5x the read price on their next turn.
	keep func(V) bool
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
		el := c.victim()
		e := el.Value.(*lruEntry[V])
		c.ll.Remove(el)
		delete(c.index, e.k)
		if c.onEvict != nil {
			c.onEvict(e.k, e.v)
		}
	}
}

// victim picks the entry to drop: the least-recently-used one that `keep` does not
// protect, or — when every candidate is protected — the least-recently-used one outright,
// because bounding memory is what the cache is for. onEvict is called either way, so a
// caller that cares which case it was can tell from the value it is handed.
//
// The front is never a victim. It is the entry put() just inserted, and dropping it would
// starve every newcomer for as long as the cache stays full of protected entries — which
// is the opposite failure, and the harder one to see.
//
// ponytail: O(n) walk under the caller's lock, n = max (256 tenancies), and only on an
// insert that overflows. A cache large enough for that to matter wants a second list of
// unprotected keys, not a cleverer scan.
func (c *lru[V]) victim() *list.Element {
	back := c.ll.Back()
	if c.keep == nil {
		return back
	}
	for el := back; el != nil && el != c.ll.Front(); el = el.Prev() {
		if !c.keep(el.Value.(*lruEntry[V]).v) {
			return el
		}
	}
	return back
}

func (c *lru[V]) remove(k string) {
	if el, ok := c.index[k]; ok {
		c.ll.Remove(el)
		delete(c.index, k)
	}
}

// Tenancy is everything the request path needs to know about the authenticated
// caller. Built once per request (cached per tenant), and NEVER mutated after it is
// published — readers on the request path (captureContentFor, newCapture,
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

// setKey returns the credential injector for this upstream, or nil when the
// operator configured no server-held key — which is the DEFAULT and the point: the
// caller's own provider credential is forwarded instead, so nobody's traffic is
// billed to the operator. A server key remains supported as an explicit per-upstream
// fallback (key_env), for single-tenant and local deployments where the agent holds
// only a placeholder.
//
// The key is read from the environment at CALL time, so rotating a credential does
// not need a restart, and no copy of it is held in a long-lived struct.
//
// A nil injector and a nil error mean CALLER-PAYS. "key_env named but unset" is a
// different thing and returns errKeyEnvUnset instead: the two used to be the same nil,
// and the caller could only test "no injector", so a half-finished config edit fell
// through to forwarding whatever the caller sent — which in gateway mode is a
// PLACEHOLDER, non-empty, and therefore not caught by the "no credential" check either.
// The operator then debugs a provider 401 instead of reading their own misconfiguration.
func (u Upstream) setKey() (func(http.Header), error) {
	if u.KeyEnv == "" {
		return nil, nil // the default: the caller's own credential is forwarded
	}
	key := os.Getenv(u.KeyEnv)
	if key == "" {
		return nil, errKeyEnvUnset
	}
	if h := u.Header; h != "" && !strings.EqualFold(h, "Authorization") {
		return func(hd http.Header) { hd.Set(h, key) }, nil
	}
	if u.Dialect == "anthropic" {
		return func(hd http.Header) { hd.Set("x-api-key", key) }, nil
	}
	return func(hd http.Header) { hd.Set("Authorization", "Bearer "+key) }, nil
}

// errKeyEnvUnset says the operator asked for a server-held key and the environment does
// not have one. Deliberately NOT a fall-through to caller-pays: an upstream configured
// for gateway mode has callers holding placeholders, and forwarding a placeholder
// publishes it to a third party and returns someone else's 401.
var errKeyEnvUnset = errors.New("upstream key_env names an unset environment variable")

// TokenHeader is where a caller presents its context-guru token. A DEDICATED header,
// because the Authorization / x-api-key slot now carries the caller's OWN provider
// credential, which is what gets forwarded upstream — the whole point of not billing
// every user's traffic to one server-held key.
//
// copyHeaders already strips every x-context-guru-* header before forwarding, so this
// name is safe by construction: it cannot reach an upstream.
const TokenHeader = "x-context-guru-token"

// authHeaders are the slots a caller's provider credential may arrive in. Three,
// because agents disagree: Anthropic-dialect tools use Authorization or x-api-key,
// Gemini-CLI descendants use x-goog-api-key.
//
// These are FORWARDED, not stripped — unless the operator configured a server-held
// key for the upstream, or the slot happens to hold one of our own tokens (see
// scrubToken).
var authHeaders = []string{"Authorization", "x-api-key", "x-goog-api-key"}

// headerCredential pulls the credential out of one auth header: either a bare value,
// or a `<scheme> <value>` pair. Splitting on fields rather than trimming a prefix so
// that a lone "Bearer" yields nothing at all, instead of the word "Bearer" being sent
// off to be looked up.
//
// The scheme is deliberately NOT checked against a list. It used to require "Bearer",
// and BobShell sends `Authorization: Apikey <key>` — so its key was invisible to
// CallerKey, every Bob request was refused 401 "no context-guru token" no matter what
// the tenant had bound, and scrubToken could not have removed one of our own tokens
// from that slot either. Anything that is not our token is forwarded unchanged, scheme
// included, so accepting the pair only affects whether we can RECOGNISE it.
func headerCredential(v string) string {
	f := strings.Fields(v)
	switch {
	case len(f) == 2:
		return f[1]
	case len(f) == 1 && !strings.EqualFold(f[0], "bearer"):
		return f[0]
	}
	return ""
}

// TokenFromRequest extracts the caller's context-guru token. The dedicated header
// first; failing that, an auth slot — but ONLY a value shaped like one of our tokens,
// so a provider key is never mistaken for a token and sent to the registry.
//
// It does not validate beyond the shape: the registry decides what a valid token is.
func TokenFromRequest(r *http.Request) string {
	if t := headerCredential(r.Header.Get(TokenHeader)); t != "" {
		return t
	}
	for _, h := range authHeaders {
		if v := headerCredential(r.Header.Get(h)); tenant.LooksLikeToken(v) {
			return v
		}
	}
	return ""
}

// CallerAuthScheme reports HOW the caller presented the credential CallerKey returns, so a
// component that reuses it presents it the same way. "bearer" when it arrived in
// Authorization; "" — the Anthropic client's x-api-key default — for the api-key slots.
//
// Without this the credential was reused in the wrong slot. A caller authenticating with
// `Authorization: Bearer <token>` (which is what Claude Code does when it is given
// ANTHROPIC_AUTH_TOKEN rather than ANTHROPIC_API_KEY) had its token re-sent as `x-api-key`
// on every extraction call, and a gateway that authenticates with bearer tokens answered
// 401 to all of them. `model.source: incoming` is the DEFAULT, so for that whole class of
// caller extract_llm could never make a single successful call — silently, because the
// failure was reported as "no usable program or reply".
//
// It returns the scheme of the SAME header CallerKey picks, so the two cannot disagree.
func CallerAuthScheme(r *http.Request) string {
	for _, h := range authHeaders {
		if v := headerCredential(r.Header.Get(h)); v != "" && !tenant.LooksLikeToken(v) {
			if h == "Authorization" {
				// Bearer is the only Authorization scheme our client can emit. A caller using
				// some other one (BobShell sends `Apikey <key>`) is still better served by the
				// right HEADER with a different scheme than by the wrong header entirely.
				return "bearer"
			}
			return ""
		}
	}
	return ""
}

// CallerKey returns the caller's OWN provider credential — the first auth slot that
// holds something which is not one of our tokens. "" means the caller presented no
// credential of their own.
func CallerKey(r *http.Request) string {
	for _, h := range authHeaders {
		if v := headerCredential(r.Header.Get(h)); v != "" && !tenant.LooksLikeToken(v) {
			return v
		}
	}
	return ""
}

// scrubToken removes OUR token from any auth slot, so a token presented there (by an
// agent that can set no custom header) never leaves the box. The caller's provider
// credential — anything not shaped like our token — is deliberately left in place:
// forwarding it is the entire change.
// It checks EVERY value of each slot rather than h.Get's first one: copyHeaders forwards
// all of them, so a client that sent Authorization twice — provider key first, our token
// second — forwarded the token upstream. A slot holding any token-shaped value is deleted
// whole; a caller who put both in one slot has already given up the second.
func scrubToken(h http.Header) {
	h.Del(TokenHeader) // belt and braces; copyHeaders drops x-context-guru-* already
	for _, hd := range authHeaders {
		for _, v := range h.Values(hd) {
			if tokenShaped(headerCredential(v)) {
				h.Del(hd)
				break
			}
		}
	}
}

// tokenShaped is the SCRUBBING test, and it is deliberately LOOSER than
// tenant.LooksLikeToken: the prefix alone, with no length requirement.
//
// The asymmetry is the point. LooksLikeToken gates AUTHENTICATION, where strict is
// correct — a token that lost a character in transit must fail auth. Scrubbing is the
// opposite decision: a value that merely LOOKS like one of ours has no business going to
// a provider, and under the strict test a 33-of-34-character token was not token-shaped,
// so it was forwarded as if it were the caller's own credential — publishing all but one
// character of a live token to a third party. Being liberal here costs nothing, because no
// provider credential begins with our prefix.
func tokenShaped(s string) bool { return strings.HasPrefix(s, tenant.TokenPrefix) }

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
	errNoToken = statusError{http.StatusUnauthorized,
		"no context-guru token; send it in " + TokenHeader + " (see /dashboard/ to register)"}
	errBadToken = statusError{http.StatusUnauthorized, "unknown or revoked context-guru token"}
	// errNoSession is the CONTROL PLANE's 401, kept separate from errNoToken because the
	// two are fixed differently. Those routes authenticate with the browser session cookie
	// and never read the agent header at all, so answering "send it in
	// x-context-guru-token" sent people off to add a header that changes nothing — and
	// then to guess that the token IS the cookie, which fails with this same message and
	// reads as the token being wrong. Name the credential the route actually wants.
	errNoSession = statusError{http.StatusUnauthorized,
		"not signed in: this route authenticates with your dashboard session, not the agent " +
			"token; sign in at /dashboard/ (a cg_live_ token is not a cg_dash cookie)"}
	errTenantOff  = statusError{http.StatusForbidden, "this context-guru account is disabled"}
	errUnboundKey = statusError{http.StatusUnauthorized,
		"no " + TokenHeader + " header, and this provider key is not bound to an account; " +
			"see /dashboard/ to register or bind it"}
	errNoUpstreamFor = statusError{http.StatusBadGateway, "no upstream configured for this route"}
	// errNoProviderKey is the one that must never become a fallback. Without a
	// credential of their own the caller does not get someone else's: they get a 401.
	errNoProviderKey = statusError{http.StatusUnauthorized,
		"no provider credential; send your own API key in Authorization or x-api-key"}
)

// tenantOff is the 403 for a disabled account, carrying the manager's reason when one was
// recorded.
//
// Why the reason travels this far: "disabled" used to be undiagnosable from outside. An
// agent got a bare 403, the dashboard refused the sign-in that would have explained it,
// and the person whose work had stopped could not tell a deliberate suspension from a bug
// in the proxy. The manager writes the note; the account's owner is who reads it.
//
// Falls back to the unchanged constant when there is no reason, so accounts disabled
// before this existed answer exactly as they did.
func tenantOff(err error) StatusError {
	var de *tenant.DisabledError
	if errors.As(err, &de) && de.Reason != "" {
		return statusError{http.StatusForbidden,
			"this context-guru account is disabled: " + de.Reason}
	}
	return errTenantOff
}

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
	// returning records that this tenant came BACK — a second request against the same
	// configuration, which is the first moment its frozen decisions have anything to
	// compound onto. It is what the LRU protects (see lru.keep): a self-registered
	// account that sends one request and never returns must not be able to evict a
	// tenant that is mid-session, which is what pure recency allowed with max+1 accounts.
	//
	// ponytail: "came back once" is a cheap proxy for "holds valuable frozen state", and
	// its ceiling is that two requests per throwaway account defeat it — twice the
	// attacker cost, not a wall. The next rung is asking the Store how many frozen
	// entries it holds, which is only worth adding if this shows up in the eviction
	// counters as more than idle churn.
	returning bool
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
	s := &TenantSource{reg: reg, emitter: e, builder: b, max: maxTenancies,
		cache: newLRU[*cached](maxTenancies, func(id string, c *cached) {
			// Worth a log line, not a silent drop: this tenant's next turn re-writes its
			// cached prefix, which shows up as a cost spike with no other explanation.
			//
			// Which of the two evictions it was decides who acts on it, so they are separate
			// lines. Dropping an account that came once and left is the cache working.
			// Dropping a tenant that is mid-session means every entry is mid-session and the
			// cap is now the binding constraint — an operator decision, not churn.
			if c.returning {
				slog.Warn("context-guru: evicted a RETURNING tenant's compaction state — every cached "+
					"tenant is active, so max_tenancies is the binding constraint; raise it",
					"tenant", id, "max_tenancies", maxTenancies)
				return
			}
			slog.Warn("context-guru: evicting an idle tenant's compaction state (cache cold on its next turn)",
				"tenant", id, "max_tenancies", maxTenancies)
		})}
	// Recency alone picks the wrong victim here: registration is open, so max+1 accounts
	// sending one request each would evict every mid-session tenant's frozen decisions.
	s.cache.keep = func(c *cached) bool { return c.returning }
	return s
}

// Resolve authenticates a request and returns its tenancy.
//
// Two ways in, in order of preference. A context-guru token (TokenHeader) is the
// normal one. Failing that, the caller's own provider key is looked up by its sha256
// — the path for an agent that can set no header we do not already occupy (Bob), and
// only for a digest a tenant has explicitly bound. Neither one falls open.
func (s *TenantSource) Resolve(r *http.Request) (*Tenancy, error) {
	if tok := TokenFromRequest(r); tok != "" {
		t, err := s.reg.Resolve(tok)
		switch {
		case errors.Is(err, tenant.ErrDisabled):
			return nil, tenantOff(err)
		case err != nil:
			return nil, errBadToken
		}
		return s.tenancy(t)
	}
	key := CallerKey(r)
	if key == "" {
		return nil, errNoToken
	}
	t, err := s.reg.ResolveAgentKey(key)
	switch {
	case errors.Is(err, tenant.ErrDisabled):
		return nil, tenantOff(err)
	case err != nil:
		return nil, errUnboundKey
	}
	return s.tenancy(t)
}

// Forget drops a tenant's cached pipeline and state store. Called when an account is
// DELETED: their tokens are gone, so nothing can authenticate as them again, but this
// cache is keyed by tenant id and holds a live Store — every offloaded original and frozen
// compaction decision that account accumulated. Without this it sits in memory until 255
// other tenants push it out, and a re-registered id would inherit it.
func (s *TenantSource) Forget(tenantID string) {
	if s == nil || tenantID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache.remove(tenantID)
}

// ForTenant returns the tenancy for an already-authenticated tenant, for callers
// that authenticated some other way (a dashboard cookie).
func (s *TenantSource) ForTenant(t *tenant.Tenant) (*Tenancy, error) {
	if t == nil {
		return nil, errNoToken
	}
	if t.Disabled {
		return nil, tenantOff(&tenant.DisabledError{Reason: t.DisabledReason})
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
			// The tenant came back: this entry now protects real compounding state, not just
			// a build. See cached.returning and lru.keep.
			c.returning = true
			// Fields that do not affect the pipeline are refreshed without a rebuild —
			// but on a COPY, published by swapping the pointer. Writing through the
			// cached *Tenancy would race every unlocked reader on the request path
			// (captureContentFor, newCapture, upstreamFor),
			// which is one agent with two turns in flight, not a rare interleaving.
			tn := *c.t
			tn.Label, tn.Manager = t.Label, t.IsManager()
			tn.CaptureContent = t.CaptureContent
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
		CaptureContent: t.CaptureContent,
		UpAnthropic:    t.UpAnthropic, UpOpenAI: t.UpOpenAI, UpBob: t.UpBob,
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
// URL, so this stays an allow-list and not an SSRF hop.
//
// Credentials: an upstream with no key_env forwards the CALLER's own provider key,
// which is the hosted default. That makes a caller with no key of their own an
// explicit 401 — never a silent fallback onto whatever key the box happens to hold.
// An upstream that NAMES a key_env with nothing behind it is a misconfiguration, not
// caller-pays, and gets a 502 that names it (see setKey).
func (h *Handler) upstreamFor(r *http.Request, tn *Tenancy, pick func(*Tenancy) string, static upstream) (upstream, error) {
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
	set, err := u.setKey()
	if errors.Is(err, errKeyEnvUnset) {
		// The fault is ours, so say so: 502 naming the upstream, and the env var only in
		// the log — the caller has no business learning our variable names.
		slog.Error("context-guru: upstream names an unset key_env; refusing rather than forwarding "+
			"the caller's credential, which in gateway mode is a placeholder",
			"tenant", tn.ID, "upstream", name, "key_env", u.KeyEnv)
		return upstream{}, statusError{http.StatusBadGateway,
			fmt.Sprintf("upstream %q has no credential configured", name)}
	}
	if set == nil && CallerKey(r) == "" {
		return upstream{}, errNoProviderKey
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

// refuse logs a refusal and then writes it. Every deliberate turn-away on the
// request path funnels through failAuth, so wrapping it is how one log line covers
// all of them — and the reason it prints is the same string the caller receives and
// the same event /metrics counts in cg_refused_requests_total, so a report of "I keep
// getting 429s" and the graph agree.
//
// WARN rather than ERROR: a refusal is the service working as configured — a rate
// limit, a concurrency cap, an unknown token — not a fault of ours. It is also the
// class of event that gets reported as "context-guru is broken", which is why it has
// to be findable by tenant. Never money: there is no spend cap to refuse over.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := statusOf(err)
	logging.From(r.Context()).Warn("cg.refused", "status", code, "reason", msg,
		"path", r.URL.Path, "client", clientIP(r))
	failAuth(w, err)
}

// failAuth writes a resolution failure. The body names the failure but never
// distinguishes "no such token" from "revoked token" beyond what the caller can
// already determine, and it always points at the one place a user can fix it.
func failAuth(w http.ResponseWriter, err error) {
	code, msg := statusOf(err)
	// Count the refusal. Every auth and upstream-resolution failure funnels through
	// here, so this is the one place that sees them all — and 429 is deliberately NOT
	// counted here, because the limiter that decided it counted it already (see
	// limits.go) and only there is it known which limit it was.
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
