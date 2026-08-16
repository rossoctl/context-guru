package dash

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scopeFixture is a recorder holding two tenants' rows plus one pre-tenancy row.
type scopeFixture struct {
	rec *Recorder
	api *API
	mux *http.ServeMux
	ids map[string]int64 // tenant -> one of its request ids
}

func newScopeFixture(t *testing.T, principal func(*http.Request) (Principal, bool)) *scopeFixture {
	t.Helper()
	rec, err := NewRecorder(Options{DBPath: ":memory:", CaptureContent: true, ContentCap: 4096})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() })

	now := time.Now().UnixMilli()
	f := &scopeFixture{rec: rec, ids: map[string]int64{}}
	for i, tid := range []string{"tenant-a", "tenant-b", ""} {
		rec.Record(&Event{
			TS: now + int64(i), TenantID: tid, SessionID: tid + ":sess", Model: "m-" + tid,
			Provider: "openai", Preset: "p", Mode: ModeActive, Route: "/r", Status: 200,
			TokensBefore: 100, TokensAfter: 90, SavedUnique: 10,
			TokenAccounting: AccountingComplete,
			Content:         []ContentRow{{Path: "0", Before: "SECRET-OF-" + tid, After: "x"}},
		})
	}
	// Drain the writer so the rows exist before any query.
	deadline := time.Now().Add(3 * time.Second)
	for {
		p, err := rec.DB().Requests(Filter{TenantAll: true}, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Requests) == 3 {
			for _, e := range p.Requests {
				f.ids[e.TenantID] = e.ID
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer did not persist 3 rows (got %d)", len(p.Requests))
		}
		time.Sleep(10 * time.Millisecond)
	}

	f.api = NewAPI(rec)
	if principal != nil {
		f.api.SetAuth(principal)
	}
	f.mux = http.NewServeMux()
	f.api.Mount(f.mux)
	return f
}

func (f *scopeFixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "10.9.9.9:1234" // NOT loopback: the CIDR gate must not grant anything
	// The live feed never returns on its own. Cancelling up front still exercises the
	// gate and the backlog replay, which is the part with a tenant boundary in it.
	if strings.HasPrefix(path, "/api/events") {
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		r = r.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func asTenant(id string, manager bool) func(*http.Request) (Principal, bool) {
	return func(*http.Request) (Principal, bool) { return Principal{TenantID: id, Manager: manager}, true }
}

// Every MOUNTED route is checked against the scoping decision it declared, by walking
// the route table Mount itself uses.
//
// The list used to be hand-written here, which is why /api/benchmarks,
// /api/benchmarks/{id}/tasks and /api/capture shipped with no auth and no scoping at
// all: they were never in the list, so nothing noticed. Iterating the real table means
// a new route either declares a class and is checked, or fails here.
func TestAPIScopesEveryRouteToTheCaller(t *testing.T) {
	f := newScopeFixture(t, asTenant("tenant-a", false))
	mgr := newScopeFixture(t, asTenant("tenant-a", true))
	closed := newScopeFixture(t, func(*http.Request) (Principal, bool) { return Principal{}, false })

	for _, rt := range f.api.routes() {
		path := f.probe(rt.pattern)
		code, body := f.get(t, path)
		switch rt.class {
		case scopePublic:
			// No principal required, so the only requirement is that it carries no
			// tenant data at all.
			if strings.Contains(body, "tenant-b") {
				t.Errorf("%s is public but served tenant data:\n%s", path, body)
			}
		case scopeTenant:
			// A principal is required, and the answer contains only their own rows.
			if code == http.StatusUnauthorized || code == http.StatusForbidden {
				t.Errorf("%s refused its own tenant with %d: %s", path, code, body)
			}
			if strings.Contains(body, "tenant-b") {
				t.Errorf("%s leaked tenant-b's data:\n%s", path, body)
			}
			if c, _ := closed.get(t, closed.probe(rt.pattern)); c != http.StatusUnauthorized {
				t.Errorf("%s without a principal = %d, want 401", path, c)
			}
		case scopeManager:
			if code != http.StatusForbidden {
				t.Errorf("%s for a plain tenant = %d, want 403: %s", path, code, body)
			}
			if c, b := mgr.get(t, mgr.probe(rt.pattern)); c != http.StatusOK {
				t.Errorf("%s for a manager = %d, want 200: %s", path, c, b)
			}
			if c, _ := closed.get(t, closed.probe(rt.pattern)); c != http.StatusUnauthorized {
				t.Errorf("%s without a principal = %d, want 401", path, c)
			}
		default:
			t.Errorf("%s declared an unknown scope class %d", rt.pattern, rt.class)
		}
	}
}

// probe turns a mounted pattern into a concrete request path for this fixture: the
// caller's OWN ids, so a scoped route answers rather than 404s.
func (f *scopeFixture) probe(pattern string) string {
	p := strings.TrimPrefix(pattern, "GET ")
	p = strings.ReplaceAll(p, "{id}", itoa(f.ids["tenant-a"]))
	p = strings.ReplaceAll(p, "{session}", "tenant-a:sess")
	return p
}

// A crafted ?tenant= must not widen a non-manager's view. This is the specific
// failure that a merge-instead-of-overwrite filter would produce.
func TestAPIIgnoresCraftedTenantParam(t *testing.T) {
	f := newScopeFixture(t, asTenant("tenant-a", false))
	for _, path := range []string{
		"/api/requests?tenant=tenant-b",
		"/api/requests?tenant=*",
		"/api/sessions?tenant=tenant-b",
		"/api/stats?tenant=*",
	} {
		code, body := f.get(t, path)
		if code != http.StatusOK {
			t.Errorf("%s = %d", path, code)
			continue
		}
		if strings.Contains(body, "tenant-b") {
			t.Errorf("%s widened the view:\n%s", path, body)
		}
	}
}

// A request id is a small sequential integer, so fetching one by id needs an
// ownership check or the whole table is walkable.
func TestAPIRequestByIDIsOwnershipChecked(t *testing.T) {
	f := newScopeFixture(t, asTenant("tenant-a", false))
	victim := f.ids["tenant-b"]
	if victim == 0 {
		t.Fatal("fixture has no tenant-b row")
	}
	code, body := f.get(t, "/api/requests/"+itoa(victim))
	if code != http.StatusNotFound {
		t.Fatalf("fetching another tenant's request by id = %d, want 404\n%s", code, body)
	}
	// Its own row is fetchable, with its content.
	code, body = f.get(t, "/api/requests/"+itoa(f.ids["tenant-a"]))
	if code != http.StatusOK {
		t.Fatalf("own request = %d", code)
	}
	if !strings.Contains(body, "SECRET-OF-tenant-a") {
		t.Errorf("a tenant cannot see its own captured content:\n%s", body)
	}
}

// A manager sees everyone's metrics and nobody's transcripts.
func TestManagerSeesMetricsNotTranscripts(t *testing.T) {
	f := newScopeFixture(t, asTenant("tenant-a", true))
	code, body := f.get(t, "/api/requests?tenant=*")
	if code != http.StatusOK {
		t.Fatalf("manager service-wide view = %d", code)
	}
	if !strings.Contains(body, "tenant-b") {
		t.Errorf("a manager could not see another tenant's rows:\n%s", body)
	}
	// But not their content.
	code, body = f.get(t, "/api/requests/"+itoa(f.ids["tenant-b"]))
	if code != http.StatusOK {
		t.Fatalf("manager reading another tenant's row = %d", code)
	}
	if strings.Contains(body, "SECRET-OF-tenant-b") {
		t.Errorf("a manager read another tenant's transcript:\n%s", body)
	}
	if strings.Contains(body, `"content_visible":true`) {
		t.Error("content_visible was true for another tenant's row")
	}
}

// Without a principal, hosted mode refuses rather than defaulting to everything.
func TestAPIFailsClosedWithoutAPrincipal(t *testing.T) {
	f := newScopeFixture(t, func(*http.Request) (Principal, bool) { return Principal{}, false })
	for _, path := range []string{"/api/requests", "/api/stats", "/api/sessions",
		"/api/components", "/api/facets", "/api/series", "/api/config", "/api/events"} {
		if code, _ := f.get(t, path); code != http.StatusUnauthorized {
			t.Errorf("%s without a principal = %d, want 401", path, code)
		}
	}
}

// The server's effective configuration is a manager view in hosted mode.
func TestEffectiveConfigIsManagerOnly(t *testing.T) {
	f := newScopeFixture(t, asTenant("tenant-a", false))
	if code, _ := f.get(t, "/api/config"); code != http.StatusForbidden {
		t.Errorf("/api/config for a plain tenant = %d, want 403", code)
	}
	f = newScopeFixture(t, asTenant("tenant-a", true))
	if code, _ := f.get(t, "/api/config"); code != http.StatusOK {
		t.Errorf("/api/config for a manager = %d, want 200", code)
	}
}

// Single-tenant mode must be untouched: no principal, everything visible, and the
// CIDR gate still the thing that guards content.
func TestSingleTenantAPIUnchanged(t *testing.T) {
	f := newScopeFixture(t, nil)
	code, body := f.get(t, "/api/requests")
	if code != http.StatusOK {
		t.Fatalf("= %d", code)
	}
	for _, want := range []string{"tenant-a", "tenant-b"} {
		if !strings.Contains(body, want) {
			t.Errorf("single-tenant view is missing %s rows", want)
		}
	}
	// Content stays behind the CIDR gate from a non-loopback address.
	code, body = f.get(t, "/api/requests/"+itoa(f.ids["tenant-a"]))
	if code != http.StatusOK {
		t.Fatalf("= %d", code)
	}
	if strings.Contains(body, "SECRET-OF-") {
		t.Error("content was served to an untrusted address in single-tenant mode")
	}
	// The routes that grew a manager gate in hosted mode must be exactly as open as they
	// always were here: there is nobody to authenticate on a developer's laptop, and the
	// full capture counters are the local operator's own numbers.
	for _, path := range []string{"/api/benchmarks", "/api/benchmarks/1/tasks",
		"/api/benchmarks?refresh=1", "/api/capture"} {
		if code, body := f.get(t, path); code != http.StatusOK {
			t.Errorf("single-tenant %s = %d, want 200: %s", path, code, body)
		}
	}
	if _, body := f.get(t, "/api/capture"); !strings.Contains(body, `"queue_cap"`) {
		t.Errorf("single-tenant /api/capture lost its counters:\n%s", body)
	}
}

// The SSE fan-out and its replay ring must both be scoped. Filtering one and not
// the other leaks on every reconnect, which is harder to notice than leaking always.
func TestSSEFanoutAndReplayAreScoped(t *testing.T) {
	h := NewHub()
	defer h.Close()
	a, _ := h.subscribe("tenant-a", false)
	all, _ := h.subscribe("", true)

	h.Publish(&Event{ID: 1, TenantID: "tenant-a", SessionID: "a"})
	h.Publish(&Event{ID: 2, TenantID: "tenant-b", SessionID: "b"})

	got := drain(a)
	if len(got) != 1 || got[0].TenantID != "tenant-a" {
		t.Fatalf("tenant-a's feed received %d events %+v", len(got), got)
	}
	if n := len(drain(all)); n != 2 {
		t.Errorf("the service-wide feed received %d events, want 2", n)
	}
	// Replay must apply the same rule.
	back := h.backlogSince(0, &client{tenant: "tenant-a"})
	if len(back) != 1 || back[0].TenantID != "tenant-a" {
		t.Fatalf("replay gave tenant-a %d events %+v", len(back), back)
	}
	if n := len(h.backlogSince(0, &client{all: true})); n != 2 {
		t.Errorf("service-wide replay gave %d events, want 2", n)
	}
}

func drain(c *client) []*Event {
	var out []*Event
	for {
		select {
		case e := <-c.ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Every facet dropdown is a list of values, and a list of values is a disclosure. The
// component facet reads a join table rather than a requests column, and it was the one
// that shipped unscoped — so one tenant's dropdown enumerated the components every
// other tenant runs.
func TestFacetsAreTenantScoped(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UnixMilli()
	if err := db.insertBatch([]*Event{
		{TS: now, TenantID: "tenant-a", SessionID: "a", Model: "model-a", Provider: "openai",
			Agent: "agent-a", Preset: "preset-a", Mode: ModeActive,
			Components: []CompRow{{Component: "comp-a"}}},
		{TS: now, TenantID: "tenant-b", SessionID: "b", Model: "model-b", Provider: "anthropic",
			Agent: "agent-b", Preset: "preset-b", Mode: ModeObserve,
			Components: []CompRow{{Component: "comp-b"}}},
	}); err != nil {
		t.Fatal(err)
	}
	facets, err := db.Facets(Filter{Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	for name, vals := range facets {
		for _, v := range vals {
			if strings.HasSuffix(v, "-b") {
				t.Errorf("facet %q leaked another tenant's value %q (all: %v)", name, v, vals)
			}
		}
	}
	// Still useful, not just empty: the caller's own values are there.
	if len(facets["component"]) != 1 || facets["component"][0] != "comp-a" {
		t.Errorf("component facet = %v, want [comp-a]", facets["component"])
	}
	// A manager's service-wide view still sees everything.
	all, err := db.Facets(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all["component"]) != 2 {
		t.Errorf("service-wide component facet = %v, want both", all["component"])
	}
}

// The capture counters are process-wide: captured/written/dropped/queued move with every
// tenant's traffic, so serving them to a tenant is a read on other people's request
// volume. A tenant gets the operating mode and nothing else; a manager gets the lot.
func TestCaptureCountersAreManagerOnly(t *testing.T) {
	f := newScopeFixture(t, asTenant("tenant-a", false))
	code, body := f.get(t, "/api/capture")
	if code != http.StatusOK {
		t.Fatalf("/api/capture for a tenant = %d", code)
	}
	if strings.Contains(body, `"captured":3`) || strings.Contains(body, `"queue_cap":4096`) {
		t.Errorf("a tenant read the process-wide capture counters:\n%s", body)
	}
	if !strings.Contains(body, `"mode":"`+ModeActive+`"`) {
		t.Errorf("a tenant cannot see the operating mode, so the observe banner breaks:\n%s", body)
	}
	mgr := newScopeFixture(t, asTenant("tenant-a", true))
	if _, body := mgr.get(t, "/api/capture"); !strings.Contains(body, `"queue_cap":4096`) {
		t.Errorf("a manager cannot see the capture counters:\n%s", body)
	}
}
