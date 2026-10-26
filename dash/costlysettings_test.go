package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type costlyRow struct {
	model                    string
	tenant                   string
	tools, sys, maxTok, msgs int
	stream                   bool
	cost                     float64
}

func seedCostly(t *testing.T, rows []costlyRow) *DB {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	for i, r := range rows {
		e := mkEvent(1000+int64(i), "s1", r.model, 100, 100)
		e.TenantID, e.Tools, e.SystemBlocks, e.MaxTokens, e.Messages, e.Stream, e.CostUSD =
			r.tenant, r.tools, r.sys, r.maxTok, r.msgs, r.stream, r.cost
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	return db
}

func costlyByID(d *CostlyDoc, id string) CostlyRow {
	for _, c := range d.Checks {
		if c.ID == id {
			return c
		}
	}
	return CostlyRow{ID: "missing"}
}

func TestCostlySettingsMatchesTheClassifierShapeOnly(t *testing.T) {
	db := seedCostly(t, []costlyRow{
		{"claude-sonnet-5-5", "t1", 0, 3, 64, 2, false, 0.10},   // classifier
		{"aws/Claude-Sonnet-5", "t1", 0, 3, 64, 2, false, 0.20}, // classifier, other casing
		{"claude-sonnet-5-5", "t1", 0, 3, 64, 2, true, 1},       // near miss: streamed
		{"claude-sonnet-5-5", "t1", 1, 3, 64, 2, false, 1},      // near miss: a tool
		{"claude-sonnet-5", "t1", 0, 2, 64, 1, false, 0.04},     // classifier with 2 system blocks: a clean Claude Code 2.1.305 sends this
		{"claude-sonnet-5-5", "t1", 0, 3, 65, 2, false, 1},      // near miss: max_tokens
		{"claude-sonnet-5", "t1", 0, 3, 64, 1, false, 0.06},     // classifier, 3 blocks and 1 dashboard message
		{"claude-opus-5-5", "t1", 0, 3, 64, 2, false, 1},        // near miss: not sonnet
		{"claude-opus-5-5", "t1", 0, 3, 2112, 2, false, 0.50},   // stage 2, in "also"
		{"claude-opus-5-5", "t1", 0, 3, 2112, 3, false, 1},      // near miss for stage 2
		{"claude-sonnet-5-5", "t2", 0, 3, 64, 2, false, 9},      // another tenant
	})
	doc, err := db.CostlySettings(Filter{Tenant: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	c := costlyByID(doc, "disable-experimental-betas")
	if !c.Measured || c.Requests != 4 || c.CostUSD == nil || *c.CostUSD < 0.3999 || *c.CostUSD > 0.4001 {
		t.Fatalf("classifier row = %+v, want 4 requests and $0.40", c)
	}
	if len(c.Also) != 1 || c.Also[0].ID != "classifier-stage-2" || c.Also[0].Requests != 1 || *c.Also[0].CostUSD != 0.5 {
		t.Fatalf("stage 2 line = %+v, want 1 request and $0.50, apart from the main figure", c.Also)
	}
	other, _ := db.CostlySettings(Filter{Tenant: "t2"})
	if o := costlyByID(other, "disable-experimental-betas"); o.Requests != 1 || *o.CostUSD != 9 {
		t.Fatalf("tenant t2 row = %+v, want its own 1 request and $9", o)
	}
}

func TestCostlySettingsUnknownShapeIsNullNotZero(t *testing.T) {
	db := seedCostly(t, []costlyRow{{"claude-sonnet-5-5", "t1", 0, 3, 64, 2, false, 0.10}})
	doc, _ := db.CostlySettings(Filter{Tenant: "t1"})
	r := costlyByID(doc, "session-recap")
	if r.Measured || r.CostUSD != nil {
		t.Fatalf("session-recap = %+v, want measured=false and cost null", r)
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m) //nolint:errcheck
	if v, ok := m["cost_usd"]; !ok || v != nil {
		t.Fatalf("cost_usd in JSON = %v (present %v), want an explicit null", v, ok)
	}
}

func TestCostlySettingsEmptyWindowIsMeasuredZero(t *testing.T) {
	db := seedCostly(t, []costlyRow{{"claude-sonnet-5-5", "t1", 0, 3, 64, 2, false, 0.10}})
	doc, _ := db.CostlySettings(Filter{Tenant: "t1", Since: 5000, Until: 6000})
	c := costlyByID(doc, "disable-experimental-betas")
	if !c.Measured || c.Requests != 0 || c.CostUSD == nil || *c.CostUSD != 0 {
		t.Fatalf("empty window = %+v, want measured with 0 requests and $0", c)
	}
	doc, _ = db.CostlySettings(Filter{Tenant: "t1", Since: 1000, Until: 1001})
	if c := costlyByID(doc, "disable-experimental-betas"); c.Requests != 1 {
		t.Fatalf("window [1000,1001) = %+v, want 1 request", c)
	}
}

func TestCostlySettingsRouteIsOneAccount(t *testing.T) {
	db := seedCostly(t, []costlyRow{
		{"claude-sonnet-5-5", "t1", 0, 3, 64, 2, false, 0.10},
		{"claude-sonnet-5-5", "t2", 0, 3, 64, 2, false, 9},
	})
	api := NewAPI(&Recorder{db: db})
	api.SetAuth(func(r *http.Request) (Principal, bool) {
		switch r.Header.Get("X-Who") {
		case "t1":
			return Principal{TenantID: "t1"}, true
		case "boss":
			return Principal{TenantID: "boss", Manager: true}, true
		}
		return Principal{}, false
	})
	m := http.NewServeMux()
	api.MountTools(m)
	for _, tc := range []struct {
		who, query string
		code, reqs int
	}{{"t1", "", 200, 1}, {"t1", "&tenant=t2", 200, 1}, {"boss", "", 200, 0}, {"boss", "&tenant=t2", 200, 1}, {"", "", 401, 0}} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/adapt/settings?since=1"+tc.query, nil)
		req.Header.Set("X-Who", tc.who)
		m.ServeHTTP(rr, req)
		if rr.Code != tc.code {
			t.Fatalf("%s%s: status %d, want %d", tc.who, tc.query, rr.Code, tc.code)
		}
		if tc.code != 200 {
			continue
		}
		var doc CostlyDoc
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		got := costlyByID(&doc, "disable-experimental-betas").Requests
		want := tc.reqs
		if tc.who == "t1" && tc.query != "" { // t1 may not name another tenant: it keeps its own rows
			want = 1
		}
		if got != want {
			t.Errorf("%s%s: %d requests, want %d", tc.who, tc.query, got, want)
		}
	}
}

// The plugin's list (context-guru-plugin/scripts/costly_settings.json) and CostlyChecks must carry the same shapes.
func TestCostlyChecksMatchThePluginList(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "context-guru-plugin", "scripts", "costly_settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	type shape struct {
		ID            string  `json:"id"`
		ModelContains *string `json:"model_contains"`
		Tools         *int    `json:"tools"`
		SystemBlocks  *int    `json:"system_blocks"`
		MaxTokens     *int    `json:"max_tokens"`
		Stream        *int    `json:"stream"`
		Messages      *int    `json:"messages"`
		Status        *int    `json:"status"`
		SessionHas    *shape  `json:"session_has"`
	}
	var list struct {
		Checks []struct {
			ID    string `json:"id"`
			Price struct {
				Shapes []shape `json:"shapes"`
				Also   []shape `json:"also"`
			} `json:"price"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	var same func(a []shape, b []CostlyShape) bool
	same = func(a []shape, b []CostlyShape) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if (a[i].SessionHas == nil) != (b[i].SessionHas == nil) {
				return false
			}
			if a[i].SessionHas != nil {
				in := *a[i].SessionHas
				in.ID = b[i].SessionHas.ID // the inner shape has no id of its own
				if !same([]shape{in}, []CostlyShape{*b[i].SessionHas}) {
					return false
				}
			}
			if (a[i].Status == nil) != (b[i].Status == nil) || (a[i].Status != nil && *a[i].Status != *b[i].Status) {
				return false
			}
			m := ""
			if a[i].ModelContains != nil {
				m = *a[i].ModelContains
			}
			eq := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
			if a[i].ID != b[i].ID || m != b[i].ModelContains || !eq(a[i].Tools, b[i].Tools) || !eq(a[i].SystemBlocks, b[i].SystemBlocks) ||
				!eq(a[i].MaxTokens, b[i].MaxTokens) || !eq(a[i].Stream, b[i].Stream) || !eq(a[i].Messages, b[i].Messages) {
				return false
			}
		}
		return true
	}
	if len(list.Checks) != len(CostlyChecks) {
		t.Fatalf("plugin list has %d checks, CostlyChecks has %d", len(list.Checks), len(CostlyChecks))
	}
	for i, c := range list.Checks {
		g := CostlyChecks[i]
		if c.ID != g.ID || !same(c.Price.Shapes, g.Shapes) || !same(c.Price.Also, g.Also) {
			t.Errorf("check %q differs between costly_settings.json and CostlyChecks", c.ID)
		}
	}
}

// seedCostlySessions inserts rows with an explicit session, tenant, ts and status.
type costlySessRow struct {
	session, model, tenant string
	ts                     int64
	status                 int
	maxTok, tools          int
	cost                   float64
}

func seedCostlySessions(t *testing.T, rows []costlySessRow) *DB {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	for _, r := range rows {
		e := mkEvent(r.ts, r.session, r.model, 100, 100)
		e.TenantID, e.Status, e.MaxTokens, e.Tools, e.Stream, e.CostUSD = r.tenant, r.status, r.maxTok, r.tools, false, r.cost
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCostlySmallModelCountsClassifiersOnlyInSessionsWithAHaiku400(t *testing.T) {
	const id = "small-model-rejects-server-check"
	db := seedCostlySessions(t, []costlySessRow{
		{"a", "claude-haiku-4-5", "t1", 1000, 400, 32000, 0, 7},    // the 400 itself: never counted
		{"a", "claude-sonnet-5-5", "t1", 1001, 200, 64, 0, 0.10},   // counted
		{"a", "aws/claude-sonnet-5", "t1", 1002, 200, 64, 0, 0.20}, // counted
		{"a", "claude-sonnet-5-5", "t1", 1003, 200, 65, 0, 5},      // near miss: max_tokens
		{"b", "claude-haiku-4-5", "t1", 1004, 200, 32000, 0, 1},    // haiku 200 only
		{"b", "claude-sonnet-5-5", "t1", 1005, 200, 64, 0, 9},      // no 400 in session b: not counted
		{"c", "claude-opus-5-5", "t1", 1006, 400, 64, 0, 9},        // a 400, but not haiku
		{"c", "claude-sonnet-5-5", "t1", 1007, 200, 64, 0, 9},      // not counted
		{"d", "claude-haiku-4-5", "t2", 1008, 400, 32000, 0, 1},    // other tenant's 400
		{"d", "claude-sonnet-5-5", "t1", 1009, 200, 64, 0, 9},      // t1 row, but the 400 is t2's: not counted
		{"e", "claude-haiku-4-5", "t1", 500, 400, 32000, 0, 1},     // 400 before the window
		{"e", "claude-sonnet-5-5", "t1", 1010, 200, 64, 0, 9},      // not counted: the 400 is outside the window
		{"f", "claude-haiku-4-5", "t1", 1011, 400, 32000, 0, 1},    // 400 in the window
		{"f", "claude-sonnet-5-5", "t1", 5000, 200, 64, 0, 9},      // after the window: not counted
		{"g", "claude-haiku-4-5", "t2", 1012, 400, 32000, 0, 1},    // t2 own session
		{"g", "claude-sonnet-5-5", "t2", 1013, 200, 64, 0, 4},      // t2 counts this
	})
	doc, err := db.CostlySettings(Filter{Tenant: "t1", Since: 1000, Until: 2000})
	if err != nil {
		t.Fatal(err)
	}
	c := costlyByID(doc, id)
	if !c.Measured || c.Requests != 2 || c.CostUSD == nil || *c.CostUSD < 0.2999 || *c.CostUSD > 0.3001 {
		t.Fatalf("t1 row = %+v, want 2 requests and $0.30", c)
	}
	doc2, _ := db.CostlySettings(Filter{Tenant: "t2", Since: 1000, Until: 2000})
	if c2 := costlyByID(doc2, id); c2.Requests != 1 || *c2.CostUSD != 4 {
		t.Fatalf("t2 row = %+v, want 1 request and $4", c2)
	}
}
