package dash

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"golang.org/x/sync/errgroup"
)

// API serves the dashboard: the JSON endpoints, the SSE stream, and the embedded
// UI. It holds only read access to the store plus the recorder's counters — it
// never writes a request row.
type API struct {
	rec   *Recorder
	trust []*net.IPNet
	// auth, when set, puts this API in hosted mode: every data route needs a
	// principal and every query is scoped to it. nil = single-tenant, unchanged.
	auth Authenticator
	// whoami describes the caller's session for the UI's mode probe. Supplied by the
	// host in hosted mode; nil means single-tenant.
	whoami func(*http.Request) any
	// toolFilterFn resolves a tenant's declaration-removal configuration for the inventory
	// page's control. A hook rather than a field of our own: the list is the account's
	// compaction configuration, owned, validated and audited by the control plane, and a
	// copy here would be a second answer to "what is this proxy actually sending".
	toolFilterFn func(tenantID string) ToolFilterState
	// tenantCapture reports whether ONE tenant consented to transcript capture. nil =
	// single-tenant: there is no consent layer, so the operator's flag is the whole
	// decision. Supplied by the host in hosted mode.
	tenantCapture func(tenantID string) bool
	// pricer values the pre-instrumentation split figure on read. nil = that figure is omitted.
	pricer modelinfo.Pricer
	// statsCache, facetsCache and componentsCache hold the last rendered body per
	// (principal, query), briefly. Overview alone measured 25s under real production write
	// load (many sequential queries, each one a chance to queue behind the writer), and
	// these endpoints had NO other cache — a dashboard tab left auto-refreshing, or two
	// managers looking at the same window, each cost another full read rather than getting
	// the answer the last one just computed. components was missed by the original fix:
	// its own 5 queries (Components, DecomposeComponentSavedUSD, EstimateComponentSavedUSD)
	// measured ~4.4s cold on a comparable corpus, uncached, on the dashboard's most-read tab.
	statsCache, facetsCache, componentsCache jsonCache
	// toolsCache and toolFilterCache cover the Inventory tab's two aggregate reads. They are here
	// for the same reason as the three above and were found the same way: both read over
	// tool_declarations (2.18M rows) with no cache at all, and both returned 503 on 100% of
	// requests to the default all-time view — /api/tools and /api/toolfilter each appear in the
	// outage's nginx log. /api/prompt is deliberately NOT cached beside them; see routeBounds.
	toolsCache, toolFilterCache jsonCache
	// jsonInflight collapses concurrent COLD reads of the same cache key onto one computation.
	// Keyed by the same principal-scoped cacheKey as the caches, so two tenants never share a
	// computation and a manager never shares one with a tenant.
	jsonInflight singleflight.Group
}

// dashCacheTTL bounds how fresh a cached /api/stats, /api/facets or /api/components body is before
// a request triggers a refresh.
//
// It was 5s, which was SHORTER THAN THE WORK IT CACHED and therefore cached almost nothing. On the
// production corpus Components() alone measures ~6s and the stats set ~5s, so an entry expired at
// or before the moment the next reader arrived: nearly every request was a miss and paid full
// price, and with no collapsing, nineteen readers meant nineteen concurrent multi-second scans
// competing for one connection pool — which is how a query that fits inside the 10s timeout on its
// own produced 503s all day. /metrics had the identical bug against its scrape interval
// (proxy/promexport.go); this is the same mistake in the same codebase, so it gets the same fix.
//
// 30s is chosen against the CLIENT's behaviour, not picked round: the dashboard's own auto-refresh
// defaults to 5 minutes and its SSE feed tells it when anything actually changed
// (dash/ui/app.js), so a rollup up to 30s old is well inside what the page already displays. The
// numbers here are aggregates over hours or a month; none of them turns on a single second.
const dashCacheTTL = 30 * time.Second

// dashCacheStale is how far PAST the TTL a body may still be served while its replacement is being
// computed. Beyond it a reader waits for fresh numbers instead.
//
// The cap is what keeps stale-while-revalidate honest. Without one, a deployment that goes quiet
// serves the last body it ever built, indefinitely, and the page reports it as current — the
// dashboard's freshness line is stamped when the BROWSER fetched, so it cannot see server-side
// staleness. 30s past a 30s TTL bounds the worst case at one minute, which is smaller than the
// client's own refresh interval, so no reader can be shown anything older than they would have
// been shown anyway by not clicking refresh.
const dashCacheStale = 30 * time.Second

// dashHandlerTimeout bounds every dashboard read except the four in unboundedRoutes. It used to
// bound three of them, named by hand at their route entries — see Mount, which now applies it by
// default, and unboundedRoutes for why the default is the safe direction.
//
// It bounds the QUERY as well as the caller's wait, which it did not before and which is the
// difference between an honest error and an outage. http.TimeoutHandler cancels the request
// context when it fires; reads now run under that context (a.db(r) -> WithContext -> readCtx,
// dash/store.go), so a caller who has given up takes their query and its pooled connection with
// them. Previously Overview() and Facets() ran on context.Background(): the caller got a fast 503
// and the query kept going, so a browser retrying a failing load accumulated concurrent scans
// nobody was waiting for until the pool and the memory cap were both exhausted. That is the shape
// this constant now prevents rather than merely reports.
//
// Still matches Prometheus's own scrape_timeout, so a slow tab and a slow scrape fail alike.
const dashHandlerTimeout = 10 * time.Second

// dashHeavyTimeout bounds the three cached aggregate reads — /api/stats, /api/facets and
// /api/components — which the default is simply too short for on a large database.
//
// The default's own comment used to justify 10s as "matches Prometheus's own scrape_timeout, so a
// slow tab and a slow scrape fail the same way". That was the wrong reason for these three: nothing
// scrapes them. Prometheus reads /metrics. Tying a HUMAN's page load to a scraper's patience is how
// /api/components came to fail 100% of the time on its default view — measured on the production
// database, the UNFILTERED all-time Components() aggregate takes 10.2-10.4s, so it crossed a bound
// set for an unrelated reason by a fraction of a second and returned 503 on every single request.
//
// The honest bound is what the CALLER can tolerate. This one is a person who has just clicked a tab
// showing every component over all history, and who is not helped by being told to try again. 45s is
// well inside nginx's 600s proxy_read_timeout, so the answer reaches them rather than dying in the
// hop. It is not a licence for slow queries: the same view is ~2.9s over 24 hours and ~7.5s over 7
// days, the cache and its single-flight mean at most one reader per interval ever waits at all, and
// cg_metrics_render_seconds and the X-Cache header make the real cost visible instead of hidden
// behind a retry.
const dashHeavyTimeout = 45 * time.Second

// dashComputeTimeout bounds the SHARED, detached computation behind serveJSON — the work itself
// rather than any one caller's wait for it.
//
// It has to exist and it has to be larger than the callers' bounds: the computation is deliberately
// not tied to a request (see serveJSON), so nothing else would ever stop it, and a read that hangs
// forever holds a pooled connection forever. Larger than dashHeavyTimeout because the point of
// detaching is that the work survives the reader who triggered it and lands in the cache for the
// next one; a bound below theirs would kill it just as it became useful.
const dashComputeTimeout = 2 * time.Minute

const dashTimeoutMsg = "dashboard query timed out; try again in a moment"

// jsonCache is a short-TTL cache for one expensive, filter-keyed JSON response. Unlike
// promexport's cache (proxy/promexport.go), this one is keyed: /api/stats and /api/facets
// vary by tenant scope and by every filter query parameter, so there is no single body to
// cache — there is one per (principal, query).
type jsonCache struct {
	mu      sync.Mutex
	entries map[string]jsonCacheEntry
}

type jsonCacheEntry struct {
	at   time.Time
	body []byte
}

func (c *jsonCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) >= dashCacheTTL {
		return nil, false
	}
	return e.body, true
}

// load returns the entry whether or not it is still fresh, which get cannot express and
// stale-while-revalidate needs: the difference between "no body" and "a body worth serving while a
// better one is computed" is the difference between a reader waiting six seconds and not waiting.
//
// A body past dashCacheTTL+dashCacheStale is reported as absent, so the staleness cap is enforced
// here rather than trusted to each caller.
func (c *jsonCache) load(key string) (body []byte, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	age := time.Since(e.at)
	if age >= dashCacheTTL+dashCacheStale {
		return nil, false
	}
	return e.body, age < dashCacheTTL
}

func (c *jsonCache) set(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]jsonCacheEntry)
	}
	// Bounded the cheap way: a key per distinct query string could otherwise grow without
	// limit. Once it is bigger than any real deployment's working set, drop it all rather
	// than build an LRU for a cache that exists to survive a few seconds of auto-refresh.
	if len(c.entries) > 256 {
		c.entries = make(map[string]jsonCacheEntry)
	}
	c.entries[key] = jsonCacheEntry{at: time.Now(), body: body}
}

// serveCached writes a cached body if there is one, and labels every response either way.
//
// X-Cache exists because this cache is a masking layer over a real cost, and an unlabelled
// response makes that cost unmeasurable. /api/facets survived at ~1.5s per call precisely
// because the 5s TTL hid it: it is requested on every tab switch (app.js calls loadFacets()
// from every go() but setup/settings), so one miss followed by eleven hits reads as a fast
// endpoint. Any before/after taken through this API without knowing which side of the TTL each
// sample landed on can compare a miss against a hit and report the ratio as an optimisation --
// measured on this corpus, /api/facets is 380ms on a miss and 2.4ms on a hit, a 158x difference
// that no code change produced. The header costs one line per response and makes the harness
// able to tell the two apart.
func serveCached(w http.ResponseWriter, c *jsonCache, key string) bool {
	if body, ok := c.get(key); ok {
		writeCachedJSON(w, body, "hit")
		return true
	}
	w.Header().Set("X-Cache", "miss")
	return false
}

func writeCachedJSON(w http.ResponseWriter, body []byte, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", state)
	_, _ = w.Write(body)
}

// serveJSON answers one expensive, cacheable dashboard read.
//
// Three cases, and only the third is allowed to cost the reader anything:
//
//   - FRESH: serve it. X-Cache: hit.
//   - STALE but inside dashCacheStale: serve it immediately and compute the replacement behind the
//     response. X-Cache: stale. This is the case that was missing, and it is the common one — with
//     a TTL shorter than the query, essentially every request landed here and was treated as cold.
//   - COLD: compute, and collapse every concurrent request for the same key onto ONE computation.
//     Without collapsing, N readers arriving together each ran the whole query; they then contended
//     for the same pool and every one of them got slower, so the busiest moment was the slowest —
//     the shape that turns a 6s query into a 10s timeout.
//
// compute is given its own *DB rather than closing over one, because the two cases need different
// lifetimes: a foreground computation must die with its caller, and a background refresh must NOT
// — r.Context() is already cancelled by the time the refresh runs, so a refresh bound to it would
// be killed instantly and the entry could never become fresh again.
func (a *API) serveJSON(w http.ResponseWriter, r *http.Request, c *jsonCache, key string,
	compute func(db *DB) ([]byte, error)) {
	if body, fresh := c.load(key); body != nil {
		if fresh {
			writeCachedJSON(w, body, "hit")
			return
		}
		go a.refreshJSON(c, key, compute)
		writeCachedJSON(w, body, "stale")
		return
	}
	w.Header().Set("X-Cache", "miss")
	// The shared computation is DETACHED from every caller, and that is deliberate rather than
	// careless. Bound to the leader's r.Context() it inherits the leader's deadline and the leader's
	// abandonment: when the leader timed out, compute failed with a cancelled context and
	// singleflight handed that same error to every waiter — so one reader closing a tab failed the
	// tab for everyone else, and the work already done was thrown away instead of being cached.
	// That is the same "one caller's abandonment costs everyone" shape this whole change set exists
	// to remove, and it would have been reintroduced here by the obvious code.
	//
	// Detaching it means the result is always cached even if nobody is left to receive it, so the
	// next reader gets a hit rather than starting again. Each caller still gets its own deadline
	// from the handler timeout, which is what bounds THEIR wait; this bounds the WORK.
	v, err, _ := a.jsonInflight.Do(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), dashComputeTimeout)
		defer cancel()
		body, err := compute(a.rec.DB().WithContext(ctx))
		if err == nil {
			c.set(key, body)
		}
		return body, err
	})
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(v.([]byte))
}

// refreshJSON recomputes one cache entry off the request path.
//
// Its context is detached and separately bounded: the request that triggered it has already been
// answered, so r.Context() is cancelled and inheriting it would cancel this immediately. The bound
// is generous rather than dashHandlerTimeout because nobody is waiting — the only thing that must
// not happen is a refresh living forever and holding a connection.
func (a *API) refreshJSON(c *jsonCache, key string, compute func(db *DB) ([]byte, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), dashComputeTimeout)
	defer cancel()
	body, err := compute(a.rec.DB().WithContext(ctx))
	if err != nil {
		// Keep the stale entry: it is still servable and still inside its cap. Dropping it would
		// turn a transient failure into a cold miss for the next reader, which is strictly worse.
		slog.Warn("context-guru: dashboard cache refresh failed; serving the previous body until it expires",
			"err", err)
		return
	}
	c.set(key, body)
}

// cacheKey scopes a cache entry to the actual caller, not just the URL: a manager and a
// tenant hitting the same query string must never share a cached body.
//
// since/until are rounded to the cache TTL bucket rather than compared verbatim. The UI's
// relative ranges ("last 24h") re-stamp since/until with Date.now() on every single call —
// so the raw query string is different every time and this cache would otherwise never hit
// for the single most common view on the page.
func cacheKey(p Principal, r *http.Request) string {
	q := r.URL.Query()
	bucket := dashCacheTTL.Milliseconds()
	for _, param := range []string{"since", "until"} {
		if v := q.Get(param); v != "" {
			if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
				q.Set(param, strconv.FormatInt(ms/bucket*bucket, 10))
			}
		}
	}
	return p.TenantID + "\x00" + strconv.FormatBool(p.Manager) + "\x00" + q.Encode()
}

// NewAPI builds the HTTP surface for a recorder. Malformed CIDRs are skipped with
// no error: a typo in a trust list must not stop the proxy, and the failure mode
// (loopback-only) is the safe one.
func NewAPI(rec *Recorder) *API {
	a := &API{rec: rec}
	for _, c := range rec.Opts().TrustedCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c); err == nil {
			a.trust = append(a.trust, n)
		}
	}
	return a
}

// Principal is the authenticated dashboard viewer.
type Principal struct {
	TenantID string
	Manager  bool
}

// Authenticator resolves a request to a principal. Returning ok=false makes the
// request a 401. nil (the default) means SINGLE-TENANT: there is no second party to
// scope against, and the proxy shows its own numbers exactly as it always has.
type Authenticator func(*http.Request) (Principal, bool)

// SetAuth turns on hosted-mode scoping. Once set, every data route requires a
// principal and every query is scoped to it.
func (a *API) SetAuth(fn Authenticator) { a.auth = fn }

// SetWhoami supplies the body for /api/whoami, the UI's mode probe.
func (a *API) SetWhoami(fn func(*http.Request) any) { a.whoami = fn }

// SetTenantCapture supplies the per-tenant half of the content-capture decision, read
// fresh on every request because a tenant can toggle its consent at any time.
//
// Without it the dashboard reported the PROCESS-GLOBAL flag as if it were the answer:
// "captured" to an account that never consented, and "not captured" — phrased as a
// setting the reader should go and turn on — when the operator's service-wide gate was
// the thing that was off. Left nil in single-tenant mode, where there is no second gate.
func (a *API) SetTenantCapture(fn func(tenantID string) bool) { a.tenantCapture = fn }

// SetPricer supplies the model rates, which the DB layer deliberately does not hold: the
// pre-instrumentation split figure is the one number that has to be priced at READ time, because
// the rows it values were written before there was anything to price. nil leaves that figure
// absent rather than zero — an unpriced number must not read as "nothing was saved".
func (a *API) SetPricer(p modelinfo.Pricer) { a.pricer = p }

// Who has to act when there is no transcript to show. This is a SEPARATE axis from the
// transcript state, deliberately: the state answers "why is this panel empty" and the
// answer is the same either way (nothing was captured), while this names the party who
// can change that. Folding it into the state vocabulary would multiply every empty-panel
// state by every cause, and the UI needs both facts anyway.
const (
	// CaptureBlockedByOperator: capture is off service-wide. Nothing the reader can do
	// — telling them to enable their own setting is the bug this field exists to fix.
	CaptureBlockedByOperator = "operator"
	// CaptureBlockedByTenant: the operator allows capture and this account has not
	// opted in. Their own setting, and the one message that should say "turn it on".
	CaptureBlockedByTenant = "tenant"
)

// captureState mirrors proxy.captureContentFor: content is captured only when the
// operator's gate AND that tenant's consent are both on. It returns the EFFECTIVE
// decision for the tenant whose rows are being shown, plus which gate blocked it ("" when
// nothing did, and "" when there is no single tenant in view — a manager looking at the
// whole service is not a party whose consent we can report).
func (a *API) captureState(tenantID string) (captured bool, blockedBy string) {
	if !a.rec.Opts().CaptureContent {
		return false, CaptureBlockedByOperator
	}
	if a.tenantCapture == nil {
		return true, "" // single-tenant: the operator flag is the whole decision
	}
	if tenantID == "" {
		return false, "" // no one tenant in view; say nothing rather than blame someone
	}
	if !a.tenantCapture(tenantID) {
		return false, CaptureBlockedByTenant
	}
	return true, ""
}

// whoamiHandler answers "what kind of deployment is this, and am I signed in" with a
// 200 in every case.
//
// It exists because the UI used to probe by calling /api/me and reading the 401. That
// worked, and it also put a red error in the console of every user on every first load —
// which is indistinguishable, to the person reading it, from something being broken. A
// question with a legitimate negative answer should not be asked with an error.
func (a *API) whoamiHandler(w http.ResponseWriter, r *http.Request) {
	if a.whoami == nil {
		// Single-tenant: no accounts exist, so there is nothing to be signed in to.
		writeJSON(w, map[string]any{"hosted": false, "authenticated": false})
		return
	}
	writeJSON(w, a.whoami(r))
}

// scope authenticates the request and returns the filter to query with. The tenant
// is OVERWRITTEN on the parsed filter rather than merged into it: a filter parsed
// from user input and then narrowed is one forgotten branch away from being a filter
// that was never narrowed, and the failure mode is silent cross-tenant disclosure.
//
// A manager may widen deliberately: ?tenant=<id> for one other tenant, ?tenant=* for
// the service-wide view. Nobody else can, whatever they put in the query string.
func (a *API) scope(r *http.Request) (Filter, Principal, bool) {
	f := filterFrom(r)
	if a.auth == nil {
		f.TenantAll = true // single-tenant: one deployment, one set of numbers
		return f, Principal{Manager: true}, true
	}
	p, ok := a.auth(r)
	if !ok {
		return f, p, false
	}
	f.Tenant, f.TenantAll = p.TenantID, false
	if p.Manager {
		switch q := r.URL.Query().Get("tenant"); q {
		case "me":
			// The way back to own-only, for a manager who wants to see their own
			// traffic. It is a value of the SAME parameter rather than a second knob,
			// so there is still exactly one input that moves the scope.
		case "", "*":
			// A manager's DEFAULT is the whole service. Tenant is EMPTIED as well as
			// widened: captureState(f.Tenant) would otherwise report the manager's own
			// content-capture consent as if it were the viewed session's, and "" is
			// what makes it say "no one tenant in view" instead.
			f.Tenant, f.TenantAll = "", true
		default:
			f.Tenant = q
		}
	}
	return f, p, true
}

// unauthorized is the one place a data route refuses a caller.
func unauthorized(w http.ResponseWriter) {
	httpErr(w, http.StatusUnauthorized, "sign in at /dashboard/ to view your traffic")
}

// scopeClass is the tenant-boundary decision a route has made.
//
// It exists so the decision is DATA rather than something each handler remembers to
// make: TestEveryMountedRouteDeclaresItsScope walks the table below and asserts the
// declared behaviour for every route, so a newly mounted route cannot skip the
// question — which is exactly how three unauthenticated routes shipped.
type scopeClass int

const (
	// scopePublic needs no principal at all: the UI shell and the mode probe.
	scopePublic scopeClass = iota
	// scopeTenant needs a principal and serves only that principal's data.
	scopeTenant
	// scopeManager needs a MANAGER principal in hosted mode: server-wide or
	// process-wide facts that are nobody's tenant data.
	scopeManager
)

// route is one mounted endpoint plus its scoping decision.
type route struct {
	pattern string
	class   scopeClass
	h       http.HandlerFunc
}

// routes is the single mounted route table, read by Mount and by the scoping test.
func (a *API) routes() []route {
	rs := []route{
		{"GET /dashboard", scopePublic, func(w http.ResponseWriter, r *http.Request) {
			// One canonical URL: /dashboard and /dashboard/ must not be two pages.
			http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
		}},
		{"GET /dashboard/", scopePublic, http.StripPrefix("/dashboard/", uiHandler()).ServeHTTP},
		{"GET /api/stats", scopeTenant, a.stats},
		{"GET /api/series", scopeTenant, a.series},
		{"GET /api/requests", scopeTenant, a.requests},
		{"GET /api/requests/{id}", scopeTenant, a.request},
		{"GET /api/sessions", scopeTenant, a.sessions},
		{"GET /api/sessions/{session}/transcript", scopeTenant, a.sessionTranscript},
		{"GET /api/components", scopeTenant, a.components},
		{"GET /api/breakdown", scopeTenant, a.breakdown},
		{"GET /api/facets", scopeTenant, a.facets},
		{"GET /api/config", scopeManager, a.config},
		{"GET /api/benchmarks", scopeManager, a.benchmarks},
		{"GET /api/benchmarks/{id}/tasks", scopeManager, a.benchmarkTasks},
		{"GET /api/capture", scopeTenant, a.capture},
		{"GET /api/whoami", scopePublic, a.whoamiHandler},
		{"GET /api/archive", scopeTenant, a.archive},
		{"GET /api/archive/{session}", scopeTenant, a.archivedSession},
		{"GET /api/events", scopeTenant, a.events},
	}
	// The tool/MCP/skill inventory's route, declared beside its handler in toolapi.go and
	// appended here rather than mounted separately: this table is what the scoping test walks,
	// so a route mounted around it would be a route whose scope nothing checks.
	rs = append(rs, a.toolRoutes()...)
	// The keep-alive tab's reads, declared beside their handlers in keepaliveapi.go and
	// appended here for the same reason: this table is what both scoping tests walk.
	rs = append(rs, a.keepAliveRoutes()...)
	// The manager-controlled keep-alive strategy ledger, declared beside its handler in
	// keepalivestrategy.go and appended here for the same reason.
	rs = append(rs, a.keepAliveStrategyRoutes()...)
	// The KV-cache TTL analysis and strategy simulator, declared beside its handlers in
	// kvcacheapi.go and appended here for the same reason.
	return append(rs, a.kvCacheRoutes()...)
}

// Mount registers every dashboard route on a mux under the given prefix
// (typically "/dashboard" for the UI and "/api" for the data).
func (a *API) Mount(m *http.ServeMux) {
	for _, rt := range a.routes() {
		h := rt.h
		if d := routeBound(rt.pattern); d > 0 {
			h = http.TimeoutHandler(h, d, dashTimeoutMsg).ServeHTTP
		}
		m.HandleFunc(rt.pattern, h)
	}
}

// routeBounds overrides the default handler timeout for particular routes. 0 means NO timeout.
//
// The default is applied to every route by Mount and this map is the only way out, which is the
// whole point: dashHandlerTimeout used to be attached to three routes by hand, so every route added
// after it silently had none. /api/series and /api/capture were two of those, and in the outage this
// fixes they were the endpoints that hung longest — 4,049 and 3,549 gateway timeouts in a single
// day, because nothing in the process ever gave up. A route is now bounded whether or not its author
// thought about it.
//
// Two reasons to appear here, and they are different:
//
//   - 0, UNBOUNDED. Either the response is STREAMED — http.TimeoutHandler buffers the whole body
//     before writing, so wrapping an SSE feed would withhold every event and then discard them all
//     — or the read crosses the NETWORK to cold storage under its own explicit, longer timeout. The
//     static UI is here for neither reason: it touches no database, and buffering an embedded asset
//     only to time it is pure overhead.
//   - LONGER, for the three cached aggregate reads. See dashHeavyTimeout.
var routeBounds = map[string]time.Duration{
	"GET /api/events":                        0, // SSE: a buffered stream is not a stream
	"GET /api/archive/{session}":             0, // rclone fetch, own 60s bound
	"GET /api/sessions/{session}/transcript": 0, // reaches cold storage
	"GET /dashboard/":                        0, // embedded assets, no DB
	"GET /api/stats":                         dashHeavyTimeout,
	"GET /api/facets":                        dashHeavyTimeout,
	"GET /api/components":                    dashHeavyTimeout,
	// The Inventory tab's reads, all three over tool_declarations (2.18M rows). Measured on the
	// production database through the real handlers, unfiltered all-time: every one exceeded the
	// 10s default, so all three returned 503 on 100% of requests to that tab's default view.
	// /api/tools and /api/toolfilter both appear in the outage's nginx log; /api/prompt escaped it
	// only because nobody opened that view while it was being recorded.
	"GET /api/tools":      dashHeavyTimeout,
	"GET /api/toolfilter": dashHeavyTimeout,
	// /api/prompt gets the longer bound but NOT a response cache, and the asymmetry is deliberate.
	// It serves prompt CONTENT — a user's own system prompt and CLAUDE.md text — behind a gate that
	// depends on the caller's ADDRESS (a.trusted, a CIDR check in single-tenant mode), not only on
	// its principal. cacheKey scopes by principal, so a body built for a trusted caller could be
	// handed to an untrusted one on the same account. Caching it correctly means keying on
	// trustedness too; that is a change to a content gate and does not belong in an outage fix, so
	// it waits, and meanwhile it simply gets long enough to finish.
	"GET /api/prompt": dashHeavyTimeout,
	// The KV-cache and keep-alive tabs. Every one of these is an aggregate over the same large
	// tables, and measured on the production database on an IDLE box with no contention they run
	// at 2-8.6s against the 10s default — /api/kvcache/simulate alone at 7.7-8.6s, i.e. 86% of the
	// bound spent before any other reader exists. So they succeed alone and fail under exactly the
	// load a shared dashboard has, which is what the live log showed: 11 successes and 6 timeouts
	// on simulate in one morning, plus timeouts on /api/kvcache, /api/kvcache/rows and
	// /api/keepalive. Intermittent-by-load is the signature of a bound set too close to the work,
	// not of work that is unbounded.
	//
	// Listed as a family rather than individually pruned to the ones seen failing: they read the
	// same rows through the same shapes, so the ones that have not timed out yet are the ones
	// nobody has opened under load. Half of these are already cancellable (kvcacheapi.go has
	// always passed the request context), so a caller who gives up still releases the work.
	//
	// They are NOT cached, unlike the aggregates above, and that is a deliberate stopping point:
	// they complete in seconds, so a longer bound is sufficient to stop the 503s, and caching nine
	// more routes would mean nine more keys to get right for a problem the bound already solves.
	// If they get slower, caching is the next move, not a longer bound.
	"GET /api/kvcache":                          dashHeavyTimeout,
	"GET /api/kvcache/rows":                     dashHeavyTimeout,
	"GET /api/kvcache/simulate":                 dashHeavyTimeout,
	"GET /api/kvcache/pricing":                  dashHeavyTimeout,
	"GET /api/kvcache/suggest":                  dashHeavyTimeout,
	"GET /api/kvcache/suggest/holdout":          dashHeavyTimeout,
	"GET /api/keepalive":                        dashHeavyTimeout,
	"GET /api/keepalive/behaviour":              dashHeavyTimeout,
	"GET /api/keepalive/sessions":               dashHeavyTimeout,
	"GET /api/keepalive/calc":                   dashHeavyTimeout,
	"GET /api/keepalive/live":                   dashHeavyTimeout,
	"GET /api/keepalive/recommend":              dashHeavyTimeout,
	"GET /api/keepalive/strategies/{id}/ledger": dashHeavyTimeout,
}

// routeBound is the timeout Mount applies to one route: its override if it has one, otherwise the
// default. Written as a function so an absent key and an explicit 0 stay distinguishable.
func routeBound(pattern string) time.Duration {
	if d, ok := routeBounds[pattern]; ok {
		return d
	}
	return dashHandlerTimeout
}

// requireManager gates a route serving server-wide or process-wide facts.
//
// In SINGLE-TENANT mode (auth == nil) it is a no-op: there is no second party to
// authenticate, and this is a local development tool whose behaviour must not change.
func (a *API) requireManager(w http.ResponseWriter, r *http.Request, what string) bool {
	if a.auth == nil {
		return true
	}
	p, ok := a.auth(r)
	if !ok {
		unauthorized(w)
		return false
	}
	if !p.Manager {
		httpErr(w, http.StatusForbidden, what+" is visible to a manager only")
		return false
	}
	return true
}

// Transcript states. Three of them are the "why is this panel empty" answers, and
// they are kept apart because they are not the same fact and only one of them is
// something the person reading can act on: capture is a setting they own, permission
// is not theirs to change, and cold storage is a button away.
const (
	TranscriptHot           = "hot"             // content is local; served in this response
	TranscriptCold          = "cold"            // archived — metrics only until a human asks
	TranscriptFetched       = "fetched"         // pulled back from cold storage on this request
	TranscriptNothing       = "nothing_changed" // capture is on and nothing was rewritten
	TranscriptNotCaptured   = "not_captured"    // capture is off, so there is nothing to show
	TranscriptNotPermitted  = "not_permitted"   // someone else's transcript, or an untrusted address
	TranscriptNeverArchived = "never_archived"  // asked for it; it was never uploaded
	TranscriptUnreachable   = "unreachable"     // cold storage is down — the data is safe, try later
	// TranscriptUnknownSession accompanies the 404: no such session for this caller. It
	// carries a state like every other answer so a client has ONE branch on `state`
	// rather than a state machine plus a special case for one status code. It is
	// deliberately the same answer for "never existed" and "belongs to someone else" —
	// a distinguishable 404 confirms other people's session ids.
	TranscriptUnknownSession = "unknown_session"
)

// transcriptPageSize is how many TURNS one page of the compaction-diff view carries.
// It is a render budget, not a taste: blocks-per-turn measured max 24 / avg 7.9 on our
// own corpus, and the client crosses a second of synchronous work somewhere past ~2,000
// blocks, so 50 turns is ~400 blocks typical and ~1,200 worst case. transcriptPageMax is
// what an explicit ?limit= may ask for — a caller may choose to wait, but not to ask for
// 78 MB.
const (
	transcriptPageSize = 50
	transcriptPageMax  = 200
)

// sessionTranscript serves one session's before/after content for the compaction-diff
// view: every message context-guru rewrote in the session, oldest first, with the
// component rows that ran on each request.
//
// It is LAZY about cold storage, which is the whole reason it exists as its own route.
// Without ?fetch=1 it touches the local database only and reports state="cold" when the
// transcripts have moved out — so opening the view costs nothing, and a session list can
// never trigger one rclone round trip per row. The network happens on ?fetch=1 and
// nowhere else, i.e. only when a human pressed the button.
func (a *API) sessionTranscript(w http.ResponseWriter, r *http.Request) {
	f, p, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	session := r.PathValue("session")
	if session == "" {
		httpErr(w, http.StatusBadRequest, "no session named")
		return
	}

	// Content visibility, same rule as the single-request view: single-tenant keeps the
	// CIDR gate; hosted, the owning tenant reads its own transcripts and a manager reads
	// any account's — an explicit owner decision that widens who can read captured
	// content. Nobody else: a non-manager's f.Tenant is their own principal, whatever
	// ?tenant= said.
	visible := a.trusted(r)
	if a.auth != nil {
		visible = p.Manager || (!f.TenantAll && f.Tenant == p.TenantID)
	}

	// PAGED, and the cap is not optional. The worst session in our own corpus holds
	// 1,310 turns / 31,425 rewritten messages / ~78 MB of JSON; serving that in one
	// response killed the renderer in 1.9 s. Keyset, exactly as /api/requests does it.
	limit := transcriptPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, transcriptPageMax)
		}
	}
	tp, err := a.db(r).SessionEventsPage(f, session, visible, r.URL.Query().Get("after"), limit)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	evs := tp.Requests
	// A session id is unique per ACCOUNT, not per service, so under a manager's
	// service-wide scope SessionEvents matches the id in every account that has one —
	// interleaving two people's code into a single diff. Pin the view to the account
	// whose turn came first and treat that as the tenant in view below, so the archive
	// row and the capture-consent answer belong to the same account as the turns.
	if f.TenantAll && len(evs) > 0 {
		owner := evs[0].TenantID
		keep := make([]*Event, 0, len(evs))
		for _, e := range evs {
			if e.TenantID == owner {
				keep = append(keep, e)
			}
		}
		evs, f.Tenant, f.TenantAll = keep, owner, false
	}
	var arch *ArchivedSession
	if meta, mErr := a.db(r).ArchivedSessionByID(session); mErr == nil {
		// Ownership again: the index is keyed by an id a caller could guess, and this
		// row is the thing that says "there is something to fetch".
		if a.auth == nil || f.TenantAll || meta.TenantID == f.Tenant {
			arch = &meta
		}
	}
	if len(evs) == 0 && arch == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{
			"session": session, "state": TranscriptUnknownSession, "requests": []*Event{},
			"error": "no such session",
		})
		return
	}

	// Session-wide, from the DB, NOT from this page: a first page of unstored turns in a
	// partially-stored session must not report "storage is off" for the whole session.
	hot := tp.HasContent
	inCold := arch != nil && (arch.ContentPath != "" || arch.FullPath != "")

	// The EFFECTIVE decision for the tenant whose transcripts these are, not the process
	// flag: on a hosted service those differ for every tenant whose setting differs from
	// the operator's default.
	captured, blockedBy := a.captureState(f.Tenant)

	var fetchErr string
	state := TranscriptNothing
	switch {
	case !visible:
		state = TranscriptNotPermitted
	case hot:
		state = TranscriptHot
	case inCold:
		state = TranscriptCold
		if r.URL.Query().Get("fetch") == "1" {
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			rows, ferr := a.rec.FetchArchived(ctx, session)
			switch {
			case errors.Is(ferr, ErrRemoteMissing):
				state = TranscriptNeverArchived
			case ferr != nil:
				state, fetchErr = TranscriptUnreachable, ferr.Error()
			default:
				evs, state = mergeArchivedContent(evs, rows), TranscriptFetched
			}
		}
	case !captured:
		state = TranscriptNotCaptured
	}

	// A FULL archive left no local metric rows, so the fetched events are the whole
	// session and SessionEventsPage had nothing to cap. Apply the same window in memory
	// rather than handing back every turn through the one path that skipped the LIMIT.
	if len(tp.Requests) == 0 && len(evs) > 0 {
		evs, tp.NextCursor, tp.Total = windowEvents(evs, r.URL.Query().Get("after"), limit)
	}

	writeJSON(w, map[string]any{
		"session":         session,
		"state":           state,
		"requests":        evs,
		"content_visible": visible,
		// The page's own coordinates. `total` is the session's turn count so the panel can
		// say which slice of what it is showing; `next_cursor` is "" on the last page.
		"next_cursor": tp.NextCursor,
		"total":       tp.Total,
		"page_size":   limit,
		// The effective decision, plus WHICH party's gate is off when it is false. The UI
		// used to read the process flag and tell a user to enable a setting they had
		// already enabled, because the operator's gate was the one that was closed.
		"content_captured":   captured,
		"capture_blocked_by": blockedBy,
		"content_cap_bytes":  effectiveContentCap(a.rec.Opts().ContentCap),
		"archive":            arch,
		"remote":             a.rec.RemoteName(),
		"reachable":          a.rec.RemoteReachable(),
		"error":              fetchErr,
	})
}

// mergeArchivedContent folds fetched transcripts onto the local metric rows, matching
// by request id. A content-only archive left the metric rows behind, so the local list
// is authoritative for everything except the text; a full archive took the rows too, in
// which case there is nothing local to merge onto and the fetched events ARE the answer.
func mergeArchivedContent(local, fetched []*Event) []*Event {
	if len(local) == 0 {
		return fetched
	}
	byID := make(map[int64][]ContentRow, len(fetched))
	for _, e := range fetched {
		if len(e.Content) > 0 {
			byID[e.ID] = e.Content
		}
	}
	for _, e := range local {
		if c, ok := byID[e.ID]; ok {
			e.Content = c
		}
	}
	return local
}

// archive lists what has moved to cold storage. Served from the LOCAL index, so a
// user can see their whole history — including the archived part — instantly, and
// even while the remote is unreachable. Only opening one costs a network round trip.
func (a *API) archive(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	rows, err := a.db(r).ArchivedSessions(f, atoiDefault(r.URL.Query().Get("limit"), 100))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"archived": rows,
		// `remote` keeps its name and meaning for the UI — the CONFIGURED destination —
		// and `reachable` is the fact it was missing. "Not configured" is remote == "";
		// "configured but down right now" is remote != "" && !reachable. Reporting the
		// second as the first is what put "cold storage is not configured on this
		// deployment" above a list of archived sessions.
		"remote":    a.rec.RemoteName(),
		"reachable": a.rec.RemoteReachable(),
	})
}

// archivedSession fetches one session back out of cold storage. This is the only
// route that does a network round trip, and only because a human asked for this
// session by name.
//
// Read-only: it does NOT reinsert the rows. Dragging an archived session back into
// the hot tier would re-trigger the eviction that put it there, and turn "let me look
// at last month" into a write amplification loop.
func (a *API) archivedSession(w http.ResponseWriter, r *http.Request) {
	_, p, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	session := r.PathValue("session")
	meta, err := a.db(r).ArchivedSessionByID(session)
	if err != nil {
		httpErr(w, http.StatusNotFound, "no such archived session")
		return
	}
	// Ownership, same rule as a live request: the index is keyed by an id a caller
	// could guess, so it needs the check.
	if a.auth != nil && meta.TenantID != p.TenantID && !p.Manager {
		httpErr(w, http.StatusNotFound, "no such archived session")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	evs, err := a.rec.FetchArchived(ctx, session)
	if err != nil {
		// Distinguish the two, because they mean different things to whoever is
		// looking: one is "this was never archived", the other is "cold storage is
		// down, try later".
		if errors.Is(err, ErrRemoteMissing) {
			httpErr(w, http.StatusNotFound, "this session is not in cold storage")
			return
		}
		httpErr(w, http.StatusServiceUnavailable,
			"cold storage is unreachable right now: "+err.Error())
		return
	}
	// Content needs no second gate here: the ownership check above already admits only
	// the session's own tenant and the manager, which is the same rule the live path
	// applies — so a session cannot become unreadable to a manager by going cold.
	writeJSON(w, map[string]any{"session": meta, "requests": evs})
}

// events streams the live feed, scoped to the caller. The filtering is in the hub's
// fan-out, not in the browser: an unscoped feed has already delivered every tenant's
// session ids and models to every open dashboard.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	if a.auth == nil {
		a.rec.Hub().ServeHTTP(w, r)
		return
	}
	// The scope comes from the ONE resolver, not from a second copy of the widening
	// rule parsed here: `all` is a manager-only service-wide feed, and an `all`
	// computed without the role check ships every tenant's session ids to everyone.
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	a.rec.Hub().ServeScoped(w, r, f.Tenant, f.TenantAll)
}

// db is the store handle every read handler must use, rather than a.rec.DB() directly.
//
// It binds the read to the REQUEST's context, so work the caller has abandoned stops instead of
// running to completion on a pooled connection nobody is waiting for. That mattered here: the
// dashboard's own refresh timer re-issued a failing load every 5s, and because reads were
// uncancellable each retry occupied a connection and its allocations until it finished on its
// own — a pileup that pinned the process at its memory cap and made every subsequent query
// slower, which produced more retries. WithContext already existed for the KV-cache routes
// (dash/store.go) and was simply never wired to the rest.
//
// It is a method taking r, not a field, because the context is per-request; and reads go through
// this ONE accessor so a new handler cannot quietly reintroduce the uncancellable shape. Writes
// are deliberately NOT routed here — a write must complete even if the caller has gone.
func (a *API) db(r *http.Request) *DB { return a.rec.DB().WithContext(r.Context()) }

// trusted reports whether a request may see per-request CONTENT and the effective
// configuration. Loopback always may; otherwise the peer must be in a configured
// trusted CIDR. Aggregates are deliberately NOT gated — a proxy bound to 0.0.0.0
// should still show its own numbers, and the point of this tool is observability.
//
// This is the one place headroom's gate is worth copying, and the one place it is
// not: we gate CONTENT (which can carry a user's source code), never metrics.
func (a *API) trusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range a.trust {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

// writeJSONStatus is writeJSON with a status code, for the routes whose FAILURE is a
// structured answer rather than an error string — an unknown session still carries the
// state field every other transcript answer carries.
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// The dashboard is same-origin only; no CORS header, so a random page cannot
	// read a developer's transcripts out of a locally-bound proxy.
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// filterFrom parses the shared filter query parameters. Unknown values are simply
// not matched — a filter is a view, so a bad value shows an empty list rather than
// a 400 the UI has to special-case.
func filterFrom(r *http.Request) Filter {
	q := r.URL.Query()
	f := Filter{
		Session:    q.Get("session"),
		Model:      q.Get("model"),
		Provider:   q.Get("provider"),
		Agent:      q.Get("agent"),
		Preset:     q.Get("preset"),
		Mode:       q.Get("mode"),
		Component:  q.Get("component"),
		Reason:     q.Get("reason"),
		Accounting: q.Get("accounting"),
		Effort:     q.Get("effort"),
		Thinking:   q.Get("thinking"),
		StopReason: q.Get("stop_reason"),
		Q:          q.Get("q"),
	}
	f.Since = atoi64(q.Get("since"))
	f.Until = atoi64(q.Get("until"))
	return f
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	f, p, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	a.serveJSON(w, r, &a.statsCache, cacheKey(p, r), func(db *DB) ([]byte, error) {
		// The four calls below are independent of each other — none reads another's result, only
		// this handler's own fields afterward do — so they run concurrently rather than one after
		// another. Sequentially, on a production-sized DB, Overview() alone measured ~5-10s (see its
		// own errgroup for why) and DeclCreditFor another ~4-16s (see SelfRemovals' own comment for
		// why it was rewritten), which summed past this handler's 10s caller-facing timeout on every
		// call. Run concurrently, wall time drops to roughly the slowest of the four rather than
		// their sum.
		var (
			o              *Overview
			overviewErr    error
			cachesplitHist *CachesplitHistorical
			tierCosts      *TierCosts
			declCredit     *DeclCredit
		)
		var g errgroup.Group
		g.Go(func() error {
			var err error
			o, err = db.Overview(f)
			overviewErr = err
			return nil // Overview's own error is reported below, not folded into the group's error:
			// a pricing failure in one of the other three must not mask it, and it must not mask them.
		})
		// Best-effort, and omitted rather than zeroed when it cannot be computed: a figure the
		// dashboard cannot value must read as absent, never as "saved nothing".
		if a.pricer != nil {
			g.Go(func() error {
				if h, err := db.CachesplitHistoricalUSD(f, a.pricer); err == nil {
					cachesplitHist = &h
				}
				return nil
			})
			// The bill split by tier, and with it the addressable share and the safety panel's
			// benefit half. Same rule: absent when it cannot be priced, never a zeroed bill.
			g.Go(func() error {
				if t, err := db.TierCosts(f, a.pricer); err == nil {
					tierCosts = t
				}
				return nil
			})
		}
		// The declarations no longer sent, both halves. Needs no pricer for the token counts, so it
		// runs outside the block above; best-effort and non-fatal, because it is an addition to the
		// walk and a deployment where it fails should still get the walk. NEVER silent, though: an
		// error swallowed here returns zeros, and a zero in a savings figure is a claim.
		g.Go(func() error {
			c, err := db.DeclCreditFor(f, a.priceFn(r), a.toolFilterStateForScope(f).Removed)
			if err != nil {
				slog.Warn("dash: declaration-removal credit unavailable", "err", err)
				return nil
			}
			declCredit = c
			return nil
		})
		g.Wait() //nolint:errcheck // every goroutine above always returns nil; see each one's own error handling
		if overviewErr != nil {
			return nil, overviewErr
		}
		if cachesplitHist != nil {
			o.CachesplitHistorical = cachesplitHist
			// Folded into the running total here, not inside Overview() itself, because
			// pricing it needs a.pricer — Overview() returns before this figure exists, the
			// same reason DeclCreditFor's addition below happens out here too. Leaving it out
			// was the actual inconsistency, not a deliberate scoping choice: the page's own
			// "Prefix-cache savings" tile (dash/ui/app.js) already adds CachesplitSavedUSD and
			// this historical figure together and calls the sum ours — the headline total
			// should agree with the tile two inches to its right, not exclude half of it.
			o.TotalSavedUSD += cachesplitHist.USD
		}
		if tierCosts != nil {
			o.SetTiers(tierCosts)
		}
		if declCredit != nil {
			o.SetDeclCredit(declCredit)
		}
		// Rebuilt here, not left as Overview()'s own snapshot: o.Waterfall was materialized
		// before any of the priced additions above existed, so its "declarations no longer
		// sent" step baked in a zero (SetDeclCredit had not run yet) and its own "total_saved"
		// step baked in a total short by both the historical split figure and the decl-filter
		// credit — silently disagreeing with the headline tile two inches above it, which reads
		// o.TotalSavedUSD directly rather than through the waterfall. waterfall() only reads
		// current field values, so calling it again here is exactly as correct as the first
		// call and now sees everything this handler has since added.
		o.Waterfall = o.waterfall()
		return json.Marshal(o)
	})
}

func (a *API) series(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	bucket := atoi64(r.URL.Query().Get("bucket"))
	b, err := a.db(r).Series(f, bucket)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"bucket_ms": bucket, "buckets": b})
}

func (a *API) requests(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	q := r.URL.Query()
	p, err := a.db(r).Requests(f, atoi64(q.Get("before")), atoiDefault(q.Get("limit"), 50))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, p)
}

func (a *API) request(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if id <= 0 {
		httpErr(w, http.StatusBadRequest, "bad id")
		return
	}
	_, p, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	// Content visibility: single-tenant keeps the CIDR gate (loopback or a trusted
	// range). Hosted, the row's own tenant may read its transcript — and so may a
	// manager, an explicit owner decision that widens who can read captured request
	// content: whoever runs the service already holds purge, delete and configuration
	// over the same rows. Nobody else, whatever they put in ?tenant=.
	trusted := a.trusted(r)
	if a.auth != nil {
		trusted = true // hosted: the ownership 404 below is the gate, not the address
	}
	e, err := a.db(r).Request(id, trusted)
	if err != nil {
		httpErr(w, http.StatusNotFound, "no such request")
		return
	}
	if a.auth != nil {
		// A request id is a small sequential integer, so an ownership check is the
		// only thing standing between a curious user and the whole table. It is also
		// the whole content gate: past it the caller is the row's owner or the manager,
		// and both may read the text.
		if e.TenantID != p.TenantID && !p.Manager {
			httpErr(w, http.StatusNotFound, "no such request")
			return
		}
	}
	// If this session's transcripts moved to cold storage, fetch them for this one
	// request. A network round trip inside a handler, deliberately: it happens only
	// when a human opened this specific request, only when its content is genuinely
	// archived, and under a timeout. The alternative — a second endpoint the UI has to
	// discover and call — is more moving parts for the same wait.
	archived := false
	if trusted && len(e.Content) == 0 && e.SessionID != "" {
		if meta, err := a.db(r).ArchivedSessionByID(e.SessionID); err == nil && meta.ContentPath != "" {
			archived = true
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			rows, ferr := a.rec.FetchArchivedContent(ctx, e.SessionID, e.ID)
			cancel()
			if ferr == nil {
				e.Content = rows
			}
			// A failure here is not an error for the request view: the metrics are all
			// present and correct, and the UI says the transcript is in cold storage and
			// currently unreachable rather than showing an empty diff.
		}
	}
	// Capture is decided per tenant, so the ROW's owner is whose consent governed this
	// request's transcript — reporting the process flag here told a tenant their content
	// was captured when their own consent was off, and blamed them when it was not.
	captured, blockedBy := a.captureState(e.TenantID)
	writeJSON(w, map[string]any{
		"request": e,
		// Tell the UI WHY content is missing, so "no content", "not allowed to see
		// content" and "content is in cold storage" are never the same empty panel.
		"content_visible":    trusted,
		"content_captured":   captured,
		"capture_blocked_by": blockedBy,
		"content_cap_bytes":  effectiveContentCap(a.rec.Opts().ContentCap),
		"content_archived":   archived,
	})
}

func (a *API) sessions(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	q := r.URL.Query()
	rows, total, err := a.db(r).Sessions(f,
		atoiDefault(q.Get("limit"), 50), atoiDefault(q.Get("offset"), 0))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"sessions": rows, "total": total})
}

func (a *API) components(w http.ResponseWriter, r *http.Request) {
	f, p, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	a.serveJSON(w, r, &a.componentsCache, cacheKey(p, r), func(db *DB) ([]byte, error) {
		rows, err := db.Components(f)
		if err != nil {
			return nil, err
		}
		// Value the rows whose saved_usd predates the column, into their own field. Without this
		// the most-read tab in the dashboard reports $0.00 for every component over all history
		// that predates the last restart — measured, 6 populated rows out of 100,579.
		if a.pricer != nil {
			// Both read-time valuations, in order: the estimate fills history that predates the
			// saved_usd column, the decomposition splits every priced row into its first-removal
			// and replay halves so the two opposite-signed verdicts can be shown together.
			if err := db.DecomposeComponentSavedUSD(f, a.pricer, rows); err != nil {
				return nil, err
			}
			if err := db.EstimateComponentSavedUSD(f, a.pricer, rows); err != nil {
				// Best effort, like every other read-time valuation: the stored figures are
				// already in `rows` and a failed estimate must not cost the caller the tab.
				slog.Warn("context-guru: component saved_usd estimate failed; pre-column rows read $0.00",
					"err", err)
			}
		}
		return json.Marshal(map[string]any{"components": rows})
	})
}

// breakdown serves spent-vs-saved and usage aggregated by ONE dimension — per model, per
// reasoning effort, per cache_control breakpoint count, per stop reason. One endpoint
// rather than one per dimension: the caller names the dimension and gets the same row
// shape back, so a new chart is a new query string rather than a new route.
//
// Scoped exactly like every other data route: the tenant on the filter is OVERWRITTEN
// from the authenticated principal inside a.scope, so `?tenant=` cannot widen it and no
// principal means no rows.
func (a *API) breakdown(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	dim := r.URL.Query().Get("dim")
	if dim == "" {
		dim = "model"
	}
	rows, err := a.db(r).Breakdown(f, dim)
	if err != nil {
		// A bad dimension is the CALLER's error, not a server fault, and the answer names
		// the dimensions that do exist rather than making the UI guess.
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"dim":        dim,
		"dimensions": BreakdownDims(),
		"groups":     rows,
		"description": "Requests, tokens and spent-vs-saved for each value of one dimension. " +
			"spent_usd is what was billed plus context-guru's own model spend; saved_usd is the " +
			"baseline counterfactual minus that. incomplete_rows counts rows the provider gave " +
			"us no usage for — where it equals requests, the money figures for that bar are " +
			"unknown rather than zero.",
	})
}

func (a *API) facets(w http.ResponseWriter, r *http.Request) {
	flt, p, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	a.serveJSON(w, r, &a.facetsCache, cacheKey(p, r), func(db *DB) ([]byte, error) {
		f, err := db.Facets(flt)
		if err != nil {
			return nil, err
		}
		return json.Marshal(f)
	})
}

// The two /api/config descriptions. This route serves the PROCESS's own resolved
// configuration, and it is labelled as such rather than rebuilt per caller: the control
// plane's /api/me already reports a tenant's effective configuration, and two routes
// answering "what compacts my traffic" is two answers to drift apart.
//
// The live dashboard was reporting preset "codesmart" with extract_llm in the pipeline
// while every request that day ran preset "custom" and extract_llm never ran once —
// because the tenant followed its own configuration. Nothing on the page said whose
// configuration was on screen.
const (
	// "server-wide default" is deliberately NOT said here. In hosted mode there are TWO
	// documents and they are not the same one: this is what --preset/--config resolved for
	// the process, whereas an account that stores nothing of its own runs the hosted tenant
	// default (tenant.DefaultConfigYAML, served by the control plane's /api/options), which
	// is deterministic — no extract_llm — precisely because it runs on a shared box. Calling
	// this the default a tenant tracks would be wrong for every tenant on the deployment.
	serverConfigDescription = "The configuration THIS PROXY PROCESS resolved at startup from " +
		"--preset/--config. On a hosted deployment this is NOT the default your account " +
		"follows and NOT necessarily what compacted your traffic: an account following the " +
		"hosted default runs that (see /api/options), and an account storing its own " +
		"configuration runs that one — its settings page reports whichever is in force. What " +
		"actually ran on a given request is on the request itself: its preset, its mode, and " +
		"the components listed in the order they ran."
	localConfigDescription = "The configuration this proxy resolved at startup. There are no " +
		"accounts on this deployment, so it is also the configuration every request ran " +
		"through — subject to per-request overrides (/compact ?preset= or the pipeline header), " +
		"which is why each request row carries the preset, mode and components that actually " +
		"ran on it."
)

func (a *API) config(w http.ResponseWriter, r *http.Request) {
	// Redact even for a trusted caller: nothing sensitive should be in here at all,
	// and a defence that only applies to untrusted callers is one misconfiguration
	// away from being no defence.
	body := map[string]any{"scope": "server", "config": RedactConfig(a.rec.Opts().Effective)}
	if a.auth != nil {
		// Hosted: this is the SERVER's configuration, not the caller's, so it is a
		// manager view. A tenant's own configuration is served from the control plane.
		if !a.requireManager(w, r, "the server configuration") {
			return
		}
		body["description"] = serverConfigDescription
		writeJSON(w, body)
		return
	}
	if !a.trusted(r) {
		httpErr(w, http.StatusForbidden,
			"effective configuration is visible from loopback or a trusted CIDR only")
		return
	}
	body["description"] = localConfigDescription
	writeJSON(w, body)
}

// benchmarks lists ingested harbor runs. A MANAGER view in hosted mode: the runs are
// the operator's own eval history, not any tenant's traffic — and ?refresh=1 walks the
// filesystem and inserts rows, which unauthenticated is a writer denial of service
// anyone can repeat.
func (a *API) benchmarks(w http.ResponseWriter, r *http.Request) {
	if !a.requireManager(w, r, "benchmark runs") {
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		// The DIRS go back with the counts. Without them a re-scan of a mistyped path and a
		// correct scan of an empty directory are the same response — and the UI reported both
		// as "nothing happened" because it discarded the body entirely.
		dirs := a.rec.Opts().BenchDirs
		runs, tasks := a.db(r).IngestBenchRoots(dirs)
		writeJSON(w, map[string]any{"ingested_runs": runs, "ingested_tasks": tasks, "dirs": dirs})
		return
	}
	runs, err := a.db(r).BenchRuns()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"runs": runs})
}

func (a *API) benchmarkTasks(w http.ResponseWriter, r *http.Request) {
	if !a.requireManager(w, r, "benchmark runs") {
		return
	}
	rows, err := a.db(r).BenchTasks(atoi64(r.PathValue("id")), r.URL.Query().Get("arm"))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"tasks": rows})
}

// capture reports the capture pipeline's own health, drops included. Exposed as a
// first-class endpoint (and rendered in the UI) because a dashboard that hides its
// own coverage gaps cannot be trusted about anything else.
//
// Hosted, the counters are PROCESS-WIDE: captured/written/dropped/queued move with
// every tenant's traffic, so handing them to one tenant is a read on everybody else's
// request volume. There is no per-tenant version of them to scrub down to — the queue
// is one queue — so a non-manager gets the single field that is genuinely about them:
// the deployment's operating mode, because in observe mode nothing was enforced and
// their dashboard has to say so.
// The two /api/capture descriptions. Which one is served depends on what the payload
// actually contains, so the prose can never describe absent data.
const (
	captureDescription = "Captured is what the proxy handed to the capture channel; written is what " +
		"reached the database; dropped is what a full channel discarded rather than " +
		"delay a request. A non-zero drop count means the numbers above under-report — " +
		"raise the queue size or lower the traffic before drawing conclusions."
	captureModeOnlyDescription = "Mode is the deployment's operating mode. In observe mode every request was " +
		"forwarded UNTOUCHED, so any savings shown elsewhere are projections rather than " +
		"achieved. The capture pipeline's own counters are process-wide (they move with " +
		"every tenant's traffic) and are visible to a manager only."
)

func (a *API) capture(w http.ResponseWriter, r *http.Request) {
	s := a.rec.Stats()
	description := captureDescription
	if a.auth != nil {
		p, ok := a.auth(r)
		if !ok {
			unauthorized(w)
			return
		}
		if !p.Manager {
			s = Stats{Mode: s.Mode}
			// The counters are zeroed above, so the paragraph explaining them describes
			// data that is not in this payload — which reads as "captured 0, written 0,
			// dropped 0", i.e. a broken deployment. Say what the caller actually got.
			description = captureModeOnlyDescription
		}
	}
	writeJSON(w, map[string]any{"capture": s, "description": description})
}

// windowEvents applies the transcript route's keyset window to events that came from
// cold storage rather than from SQL — the full-archive case, where there is no local row
// to page over. Same cursor format, same (ts, id) order, so the client cannot tell the
// two sources apart.
func windowEvents(evs []*Event, after string, limit int) ([]*Event, string, int64) {
	total := int64(len(evs))
	if ts, id, ok := parseTranscriptCursor(after); ok {
		i := 0
		for i < len(evs) && !(evs[i].TS > ts || (evs[i].TS == ts && evs[i].ID > id)) {
			i++
		}
		evs = evs[i:]
	}
	next := ""
	if limit > 0 && len(evs) > limit {
		evs = evs[:limit]
		next = strconv.FormatInt(evs[limit-1].TS, 10) + ":" + strconv.FormatInt(evs[limit-1].ID, 10)
	}
	return evs, next, total
}

// effectiveContentCap is the cap that actually binds on captured text, which is not the
// one this package configures. apply clips Change.Before/After to apply.TraceTextCap
// before a row ever reaches capture, and that is 4,000 bytes against a --dashboard-content-cap
// that defaults to 16 KiB — so the dashboard's own cap could never bind, and serving it as
// content_cap_bytes told a reader a number no truncation had ever used. Report the minimum.
func effectiveContentCap(dashCap int) int {
	if dashCap <= 0 {
		return apply.TraceTextCap
	}
	return min(dashCap, apply.TraceTextCap)
}
