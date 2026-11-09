package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func limF(v float64) *float64 { return &v }
func limI(v int64) *int64     { return &v }

// The limit values survive the write and the read, and a missing value stays missing (NULL),
// not 0. A row with no values stores none.
func TestLimitsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	full := &Event{TS: 1000, SessionID: "s", Model: "m", Limits: &Limits{
		FiveHour: LimitWindow{Utilization: limF(0.54), ResetS: limI(1794249600), Status: "allowed"},
		SevenDay: LimitWindow{Utilization: limF(0), Status: "allowed"},
	}}
	none := &Event{TS: 2000, SessionID: "s", Model: "m"}
	empty := &Event{TS: 3000, SessionID: "s", Model: "m", Limits: &Limits{}}
	if err := db.insertBatch([]*Event{full, none, empty}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Request(full.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	l := got.Limits
	if l == nil || *l.FiveHour.Utilization != 0.54 || *l.FiveHour.ResetS != 1794249600 ||
		l.FiveHour.Status != "allowed" || l.SevenDay.Utilization == nil || *l.SevenDay.Utilization != 0 ||
		l.SevenDay.ResetS != nil || l.SevenDay.Status != "allowed" {
		t.Fatalf("round trip = %+v", l)
	}
	for _, e := range []*Event{none, empty} {
		got, err := db.Request(e.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Limits != nil {
			t.Errorf("row %d without values read back %+v", e.ID, got.Limits)
		}
	}
	var seen int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE limit_seen = 1`).Scan(&seen); err != nil || seen != 1 {
		t.Errorf("limit_seen rows = %d (%v), want 1", seen, err)
	}
}

// GET /api/me/limits: {} with no values, the newest values when there are some, and only the
// caller's own account, a manager's too.
func TestMeLimitsEndpoint(t *testing.T) {
	db := openTestDB(t)
	get := func(tenant string, manager bool) map[string]any {
		t.Helper()
		rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
		t.Cleanup(func() { rec.Close() })
		api := NewAPI(rec)
		api.SetAuth(asTenant(tenant, manager))
		mux := http.NewServeMux()
		api.Mount(mux)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/me/limits?tenant=*", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /api/me/limits = %d %s", rr.Code, rr.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := get("a", false); len(got) != 0 {
		t.Fatalf("no rows: %v, want {}", got)
	}
	win := func(u float64, r int64) LimitWindow {
		return LimitWindow{Utilization: limF(u), ResetS: limI(r), Status: "allowed"}
	}
	if err := db.insertBatch([]*Event{
		{TS: 1000, TenantID: "a", SessionID: "s", Model: "m",
			Limits: &Limits{FiveHour: win(0.10, 100), SevenDay: win(0.01, 200)}},
		{TS: 2000, TenantID: "a", SessionID: "s", Model: "m",
			Limits: &Limits{FiveHour: win(0.54, 1794249600)}},
		{TS: 3000, TenantID: "a", SessionID: "s", Model: "m"}, // an answer without the headers
		{TS: 4000, TenantID: "b", SessionID: "s", Model: "m",
			Limits: &Limits{FiveHour: win(0.99, 999)}},
		{TS: 5000, TenantID: "c", SessionID: "s", Model: "m"}, // an API key: never any headers
	}); err != nil {
		t.Fatal(err)
	}
	got := get("a", false)
	five, _ := got["five_hour"].(map[string]any)
	if got["subscription"] != true || got["at_ms"] != float64(2000) || five["utilization"] != 0.54 ||
		five["reset_s"] != float64(1794249600) || five["status"] != "allowed" {
		t.Errorf("tenant a = %v", got)
	}
	if _, ok := got["seven_day"]; ok {
		t.Errorf("the newest row had no 7d values, but the answer has them: %v", got)
	}
	if got := get("c", false); len(got) != 0 {
		t.Errorf("tenant c (no headers) = %v, want {}", got)
	}
	// A manager reads their own account, never the service-wide newest (tenant b's 0.99).
	if got := get("c", true); len(got) != 0 {
		t.Errorf("manager c = %v, want {}", got)
	}

	// Single-tenant: one account, every row.
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	mux := http.NewServeMux()
	NewAPI(rec).Mount(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/me/limits", nil))
	var out map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out["at_ms"] != float64(4000) {
		t.Errorf("single-tenant = %v, want the newest row (4000)", out)
	}
}

// The read uses the partial indexes, so it never scans the whole requests table.
func TestLatestLimitsUsesThePartialIndex(t *testing.T) {
	db := openTestDB(t)
	for _, q := range []struct{ sql, index string }{
		{`EXPLAIN QUERY PLAN SELECT ts FROM requests WHERE limit_seen = 1 AND tenant_id = 'a'
			ORDER BY ts DESC, id DESC LIMIT 1`, "idx_requests_limits_tenant"},
		{`EXPLAIN QUERY PLAN SELECT ts FROM requests WHERE limit_seen = 1
			ORDER BY ts DESC, id DESC LIMIT 1`, "idx_requests_limits_ts"},
	} {
		rows, err := db.sql.Query(q.sql)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			rows.Scan(&id, &parent, &unused, &detail)
			plan += detail + "\n"
		}
		rows.Close()
		if !strings.Contains(plan, q.index) {
			t.Errorf("plan does not use %s:\n%s", q.index, plan)
		}
	}
}
