package tooldefer

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
	_ "modernc.org/sqlite"
)

// ── probe ────────────────────────────────────────────────────────────────

type fakeD struct {
	status int
	resp   func(body []byte) []byte
	err    error
	bodies [][]byte
}

func (f *fakeD) Do(_ context.Context, r dispatch.Request) (dispatch.Response, error) {
	f.bodies = append(f.bodies, append([]byte(nil), r.Body...))
	if f.err != nil {
		return dispatch.Response{}, f.err
	}
	return dispatch.Response{Status: f.status, Body: f.resp(r.Body)}, nil
}
func (f *fakeD) Release(dispatch.Ref) {}
func (f *fakeD) Price(context.Context, string, string, int64) (dispatch.Price, bool) {
	return dispatch.Price{}, false
}

// good answers like the API: a search that loads the probe tool, then a call to it.
func good(body []byte) []byte {
	tool := gjson.GetBytes(body, "tools.1.name").String()
	return []byte(`{"content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"` + tool + `"}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"` + tool + `"}]}},` +
		`{"type":"tool_use","id":"t1","name":"` + tool + `","input":{"code":7}}]}`)
}

func TestProbeVerdicts(t *testing.T) {
	for name, tc := range map[string]struct {
		d    *fakeD
		want Verdict
		err  bool
	}{
		"pass":                      {&fakeD{status: 200, resp: good}, Pass, false},
		"200 that ignores deferral": {&fakeD{status: 200, resp: func([]byte) []byte { return []byte(`{"content":[{"type":"text","text":"7"}]}`) }}, Fail, false},
		"called without a search": {&fakeD{status: 200, resp: func(b []byte) []byte {
			return []byte(`{"content":[{"type":"tool_use","id":"t","name":"` + gjson.GetBytes(b, "tools.1.name").String() + `","input":{}}]}`)
		}}, Fail, false},
		"400 unknown variant": {&fakeD{status: 400, resp: func([]byte) []byte { return []byte(`{"error":{}}`) }}, Fail, false},
		"429 is no verdict":   {&fakeD{status: 429, resp: func([]byte) []byte { return nil }}, Unknown, true},
		"transport error":     {&fakeD{err: errors.New("boom")}, Unknown, true},
	} {
		v, err := RunProbe(context.Background(), tc.d, "r", "", "claude-sonnet-5-5")
		if v != tc.want || (err != nil) != tc.err {
			t.Errorf("%s: v=%v err=%v", name, v, err)
		}
	}
}

// Every probe is unique: the gateway replays a stored answer for a byte-identical request.
func TestProbeCarriesANonce(t *testing.T) {
	d := &fakeD{status: 200, resp: good}
	RunProbe(context.Background(), d, "r", "", "claude-x")
	RunProbe(context.Background(), d, "r", "", "claude-x")
	if string(d.bodies[0]) == string(d.bodies[1]) {
		t.Fatal("two probes sent identical bytes")
	}
	if !gjson.GetBytes(d.bodies[0], "tools.1.defer_loading").Bool() || gjson.GetBytes(d.bodies[0], "tools.2.type").String() != SearchType {
		t.Fatalf("probe shape: %s", d.bodies[0])
	}
}

func TestProberCachesPerKeyAndForgets(t *testing.T) {
	p := NewProber()
	p.Go = func(fn func()) { fn() }
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	calls := 0
	send := func(context.Context) (Verdict, error) { calls++; return Pass, nil }
	k := ProbeKey{"https://gw", "claude-a", "/v1/messages"}
	p.Ensure(k, send)
	p.Ensure(k, send)
	if calls != 1 || p.Verdict(k) != Pass {
		t.Fatalf("calls=%d verdict=%v", calls, p.Verdict(k))
	}
	if p.Verdict(ProbeKey{"https://gw", "claude-b", "/v1/messages"}) != Unknown {
		t.Fatal("a verdict leaked to another model")
	}
	now = now.Add(ProbeTTL + time.Second)
	if p.Verdict(k) != Pass {
		t.Fatal("an expired verdict was dropped before the re-probe finished")
	}
	p.Ensure(k, func(context.Context) (Verdict, error) { calls++; return Fail, nil })
	if calls != 2 || p.Verdict(k) != Fail {
		t.Fatal("no re-probe after 24h")
	}
	p.Forget(k)
	if p.Verdict(k) != Unknown {
		t.Fatal("Forget kept the verdict")
	}
	// A transport failure waits before a retry.
	fails := 0
	k2 := ProbeKey{"x", "y", "z"}
	p.Ensure(k2, func(context.Context) (Verdict, error) { fails++; return Unknown, errors.New("x") })
	p.Ensure(k2, func(context.Context) (Verdict, error) { fails++; return Unknown, errors.New("x") })
	if fails != 1 {
		t.Fatalf("retried at once after a failure: %d", fails)
	}
}

// ── ledger ───────────────────────────────────────────────────────────────

func TestLedgerNetSaving(t *testing.T) {
	p := modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}
	model := "claude-sonnet-5-5"
	f, _ := tokens.BilledDeltaFactor(model)
	// A warm request with no search: gross at the read rate, minus the names list at the same rate.
	m, ok := Price(Request{Model: model, DeferredTokens: 10000, ListTokens: 100, Tier: TierRead}, p)
	if !ok {
		t.Fatal("not priced")
	}
	wantGross := 10000 * f * p.CacheRead
	wantCost := 100 * f * p.CacheRead
	if !near(m.Gross, wantGross) || !near(m.Cost, wantCost) || !near(m.Net, wantGross-wantCost) {
		t.Fatalf("warm: %+v want gross %v cost %v", m, wantGross, wantCost)
	}
	// A cold write prices at the write rate; a 1h write at 2x input.
	m, _ = Price(Request{Model: model, DeferredTokens: 1000, Tier: TierWrite}, p)
	if !near(m.Gross, 1000*f*p.CacheWrite) {
		t.Fatalf("write tier: %+v", m)
	}
	m, _ = Price(Request{Model: model, DeferredTokens: 1000, Tier: TierWrite1h}, p)
	if !near(m.Gross, 1000*f*p.CacheWrite1h()) {
		t.Fatalf("1h tier: %+v", m)
	}
	// A search turn: its output, plus the loaded definition read uncached once.
	m, _ = Price(Request{Model: model, DeferredTokens: 10000, ListTokens: 100, Searches: 1,
		SearchOutput: 40, LoadedTokens: 1500, Tier: TierRead}, p)
	wantCost = 100*f*p.CacheRead + 40*p.Output + 1500*f*p.Input
	if !near(m.Cost, wantCost) {
		t.Fatalf("search turn cost %v, want %v", m.Cost, wantCost)
	}
	// Usage adds up across requests.
	var l Ledger
	l.Add(Request{Model: model, DeferredTokens: 10000, ListTokens: 100, Tier: TierRead}, p)
	l.Add(Request{Model: model, DeferredTokens: 10000, ListTokens: 100, Searches: 1, LoadedTokens: 1500, Tier: TierRead}, p)
	l.Add(Request{Model: "unknown", DeferredTokens: 5}, modelinfo.Price{})
	s := l.Snapshot()
	if s.Requests != 3 || s.Searches != 1 || s.Unpriced != 1 || s.Net >= s.Gross || !near(s.Net, s.Gross-s.Cost) {
		t.Fatalf("ledger: %+v", s)
	}
	if TierOf(5, 5, 0) != TierRead || TierOf(0, 5, 0) != TierWrite || TierOf(0, 5, 5) != TierWrite1h || TierOf(0, 0, 0) != TierFresh {
		t.Fatal("TierOf")
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// ── history ──────────────────────────────────────────────────────────────

func openDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "h.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func waitReady(t *testing.T, m *Mem) {
	for i := 0; i < 500 && !m.Ready(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !m.Ready() {
		t.Fatal("history never loaded")
	}
}

func TestHistoryObserveAndRule(t *testing.T) {
	m := NewMem(nil)
	stop := m.Open(0, nil)
	defer stop()
	tools := catalogue()
	for i, sid := range []string{"a", "b", "c"} {
		used := ""
		if sid == "c" {
			used = `{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"UsedOnce","input":{}}]}`
		}
		var b []byte
		if used != "" {
			b = body(tools, "", used)
		} else {
			b = body(tools, "")
		}
		m.Observe("t", sid, "/w", b, int64(100+i))
	}
	// The current session "d" is left out: only a, b, c count.
	got := strings.Join(Decide(m, Input{Tenant: "t", Session: "d", Project: "/w", Body: body(tools, "")}), ",")
	if strings.Contains(got, "UsedOnce") || !strings.Contains(got, "Workflow") {
		t.Fatalf("deferred = %s", got)
	}
	// Another tenant has no history: nothing.
	if g := Decide(m, Input{Tenant: "u", Session: "d", Body: body(tools, "")}); len(g) != 0 {
		t.Fatalf("history leaked across tenants: %v", g)
	}
	// The current session is excluded: deciding as "c" sees only a and b.
	if g := Decide(m, Input{Tenant: "t", Session: "c", Body: body(tools, "")}); len(g) != 0 {
		t.Fatalf("the current session counted: %v", g)
	}
}

// The project veto: a project that used the tool in its last 10 sessions keeps it loaded.
func TestHistoryProjectVeto(t *testing.T) {
	m := NewMem(nil)
	m.Open(0, nil)
	use := `{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"Workflow","input":{}}]}`
	m.Observe("t", "old", "/proj", body(catalogue(), "", use), 1)
	for i, sid := range []string{"a", "b", "c"} {
		m.Observe("t", sid, "/other", body(catalogue(), ""), int64(10+i))
	}
	in := Input{Tenant: "t", Session: "d", Body: body(catalogue(), "")}
	// "old" is the 4th-newest session, so the user rule sees a, b, c only.
	in.Project = "/proj"
	if g := strings.Join(Decide(m, in), ","); strings.Contains(g, "Workflow") {
		t.Fatalf("project veto ignored: %s", g)
	}
	in.Project = "/other"
	if g := strings.Join(Decide(m, in), ","); !strings.Contains(g, "Workflow") {
		t.Fatalf("veto applied to the wrong project: %s", g)
	}
}

// Claude Code's own deferred built-ins count as declared, and a load through either search counts
// as used.
func TestHistoryRecordsDeferredListAndLoads(t *testing.T) {
	b := []byte(`{"tools":[` + decl("Bash") + `,{"name":"DeferredToolPlaceholder","defer_loading":true}],"messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":"x\n\nThe following deferred tools are now available via ToolSearch.\nCronList\nmcp__a__b\n\n"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"CronList"},{"type":"text","text":"Tool loaded."}]}]}]}`)
	d := DeclaredNames(b)
	if !d["CronList"] || !d["mcp__a__b"] || !d["Bash"] || d["DeferredToolPlaceholder"] {
		t.Fatalf("declared = %v", d)
	}
	if u := recentUses(b); !u["CronList"] {
		t.Fatalf("a load through Claude Code's search was not a use: %v", u)
	}
}

func TestHistoryPersistsAcrossRestart(t *testing.T) {
	db := openDB(t)
	m := NewMem(db)
	stop := m.Open(time.Hour, nil)
	waitReady(t, m)
	for i, sid := range []string{"a", "b", "c"} {
		m.Observe("t", sid, "/w", body(catalogue(), ""), int64(i+1))
	}
	stop() // flushes
	m2 := NewMem(db)
	stop2 := m2.Open(time.Hour, nil)
	defer stop2()
	waitReady(t, m2)
	if got := m2.UserSessions("t", "", 3); len(got) != 3 || !got[0].Declared["Workflow"] {
		t.Fatalf("after restart: %d sessions", len(got))
	}
}

func TestHistorySeedsFromDashboardTables(t *testing.T) {
	db := openDB(t)
	for _, q := range []string{
		`CREATE TABLE tool_declarations (tenant_id TEXT, session_id TEXT, kind TEXT, name TEXT, ts INTEGER)`,
		`CREATE TABLE tool_uses (tenant_id TEXT, session_id TEXT, project TEXT, name TEXT)`,
		`INSERT INTO tool_declarations VALUES ('t','a','tool','Workflow',1),('t','b','tool','Workflow',2),('t','c','tool','Workflow',3),('t','c','skill','x',3)`,
		`INSERT INTO tool_uses VALUES ('t','b','/w/.claude/worktrees/z','Bash')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	m := NewMem(db)
	stop := m.Open(time.Hour, nil)
	defer stop()
	waitReady(t, m)
	got := m.UserSessions("t", "", 3)
	if len(got) != 3 || !got[0].Declared["Workflow"] || got[0].Declared["x"] {
		t.Fatalf("seed: %+v", got)
	}
	if !m.ProjectUsed("t", "/w", "Bash", "", 10) {
		t.Fatal("seeded use lost its (normalized) project")
	}
}

func TestHistoryKeepsNewestSessions(t *testing.T) {
	m := NewMem(nil)
	m.Open(0, nil)
	for i := 0; i < MaxSessions+10; i++ {
		m.Observe("t", "s"+itoa(i), "", body([]string{decl("Bash")}, ""), int64(i))
	}
	if got := m.UserSessions("t", "", MaxSessions+10); len(got) != MaxSessions {
		t.Fatalf("kept %d sessions", len(got))
	}
}

// A verdict survives a restart with the history, and Forget removes the kept copy.
func TestProbeVerdictPersists(t *testing.T) {
	db := openDB(t)
	k := ProbeKey{"https://gw", "claude-a", "/v1/messages"}
	m := NewMem(db)
	m.SaveVerdict(k, Pass, time.Unix(1000, 0))
	p := NewProber()
	stop := m.Open(time.Hour, p)
	waitReady(t, m)
	stop()
	if p.Verdict(k) != Pass {
		t.Fatal("verdict not seeded after a restart")
	}
	p.Persist = m.SaveVerdict
	p.Forget(k)
	p2 := NewProber()
	m.LoadVerdicts(p2)
	if p2.Verdict(k) != Unknown {
		t.Fatal("Forget left the kept verdict")
	}
}

// Observe and Decide run on concurrent requests. UserSessions once returned the live maps, and a
// Decide reading them while an Observe wrote them was a fatal "concurrent map read and map write".
func TestHistoryConcurrentObserveAndDecide(t *testing.T) {
	m := NewMem(nil)
	m.Open(0, nil)
	for i, sid := range []string{"a", "b", "c"} {
		m.Observe("t", sid, "/w", body(catalogue(), ""), int64(i))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(2)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < 300; i++ {
				use := fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"T%d_%d","input":{}}]}`, g, i)
				m.Observe("t", "a", "/w", body(catalogue(), "", use), int64(100+i))
			}
		}(g)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 300; i++ {
				Decide(m, Input{Tenant: "t", Session: "z", Project: "/w", Body: body(catalogue(), "")})
			}
		}()
	}
	close(start)
	wg.Wait()
}

// The deterministic half of the guard above: what UserSessions hands out must not change when the
// history is written afterwards. Live maps would, and a reader of live maps races every writer.
func TestHistoryUserSessionsReturnsCopies(t *testing.T) {
	m := NewMem(nil)
	m.Open(0, nil)
	for i, sid := range []string{"a", "b", "c"} {
		m.Observe("t", sid, "/w", body(catalogue(), ""), int64(i))
	}
	got := m.UserSessions("t", "", 3)
	before := len(got[0].Used) + len(got[1].Used) + len(got[2].Used)
	use := `{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"Fresh","input":{}}]}`
	for _, sid := range []string{"a", "b", "c"} {
		m.Observe("t", sid, "/w", body(catalogue(), "", use), 50)
	}
	if after := len(got[0].Used) + len(got[1].Used) + len(got[2].Used); after != before {
		t.Fatalf("UserSessions returned live maps: %d uses before a later Observe, %d after", before, after)
	}
}

func TestNormProject(t *testing.T) {
	if NormProject("/r/.claude/worktrees/x") != "/r" || NormProject("/r") != "/r" {
		t.Fatal("NormProject")
	}
}
