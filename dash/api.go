package dash

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	// tenantCapture reports whether ONE tenant consented to transcript capture. nil =
	// single-tenant: there is no consent layer, so the operator's flag is the whole
	// decision. Supplied by the host in hosted mode.
	tenantCapture func(tenantID string) bool
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
		case "":
		case "*":
			f.TenantAll = true
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
	return []route{
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
}

// Mount registers every dashboard route on a mux under the given prefix
// (typically "/dashboard" for the UI and "/api" for the data).
func (a *API) Mount(m *http.ServeMux) {
	for _, rt := range a.routes() {
		m.HandleFunc(rt.pattern, rt.h)
	}
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
	// CIDR gate; hosted, only the owning tenant reads its own transcripts. A manager
	// reading another tenant with ?tenant= gets the metrics and none of the text.
	visible := a.trusted(r)
	if a.auth != nil {
		visible = !f.TenantAll && f.Tenant == p.TenantID
	}

	evs, err := a.rec.DB().SessionEvents(f, session, visible)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var arch *ArchivedSession
	if meta, mErr := a.rec.DB().ArchivedSessionByID(session); mErr == nil {
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

	hot := false
	for _, e := range evs {
		if len(e.Content) > 0 {
			hot = true
			break
		}
	}
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

	writeJSON(w, map[string]any{
		"session":         session,
		"state":           state,
		"requests":        evs,
		"content_visible": visible,
		// The effective decision, plus WHICH party's gate is off when it is false. The UI
		// used to read the process flag and tell a user to enable a setting they had
		// already enabled, because the operator's gate was the one that was closed.
		"content_captured":   captured,
		"capture_blocked_by": blockedBy,
		"content_cap_bytes":  a.rec.Opts().ContentCap,
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
	rows, err := a.rec.DB().ArchivedSessions(f, atoiDefault(r.URL.Query().Get("limit"), 100))
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
	meta, err := a.rec.DB().ArchivedSessionByID(session)
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
	// A manager may read another tenant's metrics but not their transcripts — the
	// same rule as the live path, applied to the archive so the archive is not a way
	// around it.
	if a.auth != nil && meta.TenantID != p.TenantID {
		for _, e := range evs {
			e.Content = nil
		}
	}
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
	p, ok := a.auth(r)
	if !ok {
		unauthorized(w)
		return
	}
	all := p.Manager && r.URL.Query().Get("tenant") == "*"
	tenant := p.TenantID
	if p.Manager {
		if q := r.URL.Query().Get("tenant"); q != "" && q != "*" {
			tenant = q
		}
	}
	a.rec.Hub().ServeScoped(w, r, tenant, all)
}

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
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	o, err := a.rec.DB().Overview(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, o)
}

func (a *API) series(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	bucket := atoi64(r.URL.Query().Get("bucket"))
	b, err := a.rec.DB().Series(f, bucket)
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
	p, err := a.rec.DB().Requests(f, atoi64(q.Get("before")), atoiDefault(q.Get("limit"), 50))
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
	// range). Hosted, the row's own tenant is the only party who may read its
	// transcript — not the operator, not a manager. Seeing another user's source code
	// is not an administrative need, and the consent they gave was for their own view.
	trusted := a.trusted(r)
	if a.auth != nil {
		trusted = true // narrowed below once the row's owner is known
	}
	e, err := a.rec.DB().Request(id, trusted)
	if err != nil {
		httpErr(w, http.StatusNotFound, "no such request")
		return
	}
	if a.auth != nil {
		// A request id is a small sequential integer, so an ownership check is the
		// only thing standing between a curious user and the whole table.
		if e.TenantID != p.TenantID && !p.Manager {
			httpErr(w, http.StatusNotFound, "no such request")
			return
		}
		if e.TenantID != p.TenantID {
			e.Content = nil // manager: metrics for everyone, transcripts for no one
			trusted = false
		}
	}
	// If this session's transcripts moved to cold storage, fetch them for this one
	// request. A network round trip inside a handler, deliberately: it happens only
	// when a human opened this specific request, only when its content is genuinely
	// archived, and under a timeout. The alternative — a second endpoint the UI has to
	// discover and call — is more moving parts for the same wait.
	archived := false
	if trusted && len(e.Content) == 0 && e.SessionID != "" {
		if meta, err := a.rec.DB().ArchivedSessionByID(e.SessionID); err == nil && meta.ContentPath != "" {
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
		"content_cap_bytes":  a.rec.Opts().ContentCap,
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
	rows, total, err := a.rec.DB().Sessions(f,
		atoiDefault(q.Get("limit"), 50), atoiDefault(q.Get("offset"), 0))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"sessions": rows, "total": total})
}

func (a *API) components(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	rows, err := a.rec.DB().Components(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"components": rows})
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
	rows, err := a.rec.DB().Breakdown(f, dim)
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
	flt, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	f, err := a.rec.DB().Facets(flt)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, f)
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
		runs, tasks := a.rec.DB().IngestBenchRoots(a.rec.Opts().BenchDirs)
		writeJSON(w, map[string]any{"ingested_runs": runs, "ingested_tasks": tasks})
		return
	}
	runs, err := a.rec.DB().BenchRuns()
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
	rows, err := a.rec.DB().BenchTasks(atoi64(r.PathValue("id")), r.URL.Query().Get("arm"))
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
