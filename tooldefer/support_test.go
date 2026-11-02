package tooldefer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
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
		"401 is no verdict":   {&fakeD{status: 401, resp: func([]byte) []byte { return nil }}, Unknown, true},
		"404 is no verdict":   {&fakeD{status: 404, resp: func([]byte) []byte { return nil }}, Unknown, true},
		"408 is no verdict":   {&fakeD{status: 408, resp: func([]byte) []byte { return nil }}, Unknown, true},
		"409 is no verdict":   {&fakeD{status: 409, resp: func([]byte) []byte { return nil }}, Unknown, true},
		"413 is no verdict":   {&fakeD{status: 413, resp: func([]byte) []byte { return nil }}, Unknown, true},
		"pause_turn is no verdict": {&fakeD{status: 200, resp: func([]byte) []byte {
			return []byte(`{"stop_reason":"pause_turn","content":[{"type":"server_tool_use","id":"s","name":"tool_search_tool_bm25","input":{}}]}`)
		}}, Unknown, true},
		"403 is no verdict": {&fakeD{status: 403, resp: func([]byte) []byte { return nil }}, Unknown, true},
		"transport error":   {&fakeD{err: errors.New("boom")}, Unknown, true},
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

// B4a. The seed copies one tenant's own sessions, at that tenant's first deferral request, and no
// other tenant's.
func TestHistorySeedsOneTenantFromDashboardTables(t *testing.T) {
	db := openDB(t)
	for _, q := range []string{
		`CREATE TABLE tool_declarations (tenant_id TEXT, session_id TEXT, kind TEXT, name TEXT, ts INTEGER)`,
		`CREATE TABLE tool_uses (tenant_id TEXT, session_id TEXT, project TEXT, name TEXT)`,
		`INSERT INTO tool_declarations VALUES ('t','a','tool','Workflow',1),('t','b','tool','Workflow',2),('t','c','tool','Workflow',3),('t','c','skill','x',3)`,
		`INSERT INTO tool_declarations VALUES ('other','o1','tool','Workflow',1),('other','o2','tool','Workflow',2)`,
		`INSERT INTO tool_uses VALUES ('t','b','/w/.claude/worktrees/z','Bash')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	m := NewMem(nil)
	m.SeedDB = db
	stop := m.Open(time.Hour, nil)
	defer stop()
	waitReady(t, m)
	if got := m.UserSessions("t", "", 3); len(got) != 0 {
		t.Fatalf("seeded before the tenant asked: %d", len(got))
	}
	m.SeedTenant("t")
	var got []Session
	for i := 0; i < 500 && len(got) < 3; i++ {
		got = m.UserSessions("t", "", 3)
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) != 3 || !got[0].Declared["Workflow"] || got[0].Declared["x"] {
		t.Fatalf("seed: %+v", got)
	}
	time.Sleep(50 * time.Millisecond) // let the seed finish copying, so a leak would show
	if n := len(m.UserSessions("t", "", 10)); n != 3 {
		t.Fatalf("tenant t holds %d sessions, want its own 3", n)
	}
	if !m.ProjectUsed("t", "/w", "Bash", "", 10) {
		t.Fatal("seeded use lost its (normalized) project")
	}
	if n := len(m.UserSessions("other", "", 10)); n != 0 {
		t.Fatalf("another tenant was seeded: %d sessions", n)
	}
}

// B3. Rows older than the retention are deleted, in memory and on disk.
func TestHistoryRetentionPrunes(t *testing.T) {
	db := openDB(t)
	m := NewMem(db)
	now := time.Unix(100000, 0)
	m.now = func() time.Time { return now }
	m.Retention = time.Hour
	stop := m.Open(time.Hour, nil)
	waitReady(t, m)
	m.Observe("t", "old", "/w", body(catalogue(), ""), now.Add(-2*time.Hour).UnixMilli())
	m.Observe("t", "new", "/w", body(catalogue(), ""), now.UnixMilli())
	stop() // flushes
	if got := m.UserSessions("t", "", 10); len(got) != 1 {
		t.Fatalf("%d sessions in memory after the prune", len(got))
	}
	var n int
	db.QueryRow(`SELECT COUNT(DISTINCT session_id) FROM tool_defer_history`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d sessions on disk after the prune", n)
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
	p.Go = func(fn func()) { fn() } // run the background removal inline
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

// A Fail verdict is re-probed once it is older than ProbeTTL: one bad answer must not keep
// deferral off forever (it is also kept across restarts).
func TestStaleFailVerdictIsReprobed(t *testing.T) {
	e := env(t, threeUnused())
	k := ProbeKey{"u", "m", "r"}
	now := time.Unix(5000, 0)
	e.Prober.now = func() time.Time { return now }
	e.Prober.Seed(k, Fail, now)
	o := Apply(e, req(body(catalogue(), "")))
	if o.Changed || o.NeedProbe {
		t.Fatalf("a fresh Fail: %s", show(o))
	}
	now = now.Add(ProbeTTL + time.Minute)
	if !e.Prober.Stale(k) {
		t.Fatal("not stale after ProbeTTL")
	}
	r := req(body(catalogue(), ""))
	r.Session = "s2"
	if o = Apply(e, r); !o.NeedProbe {
		t.Fatalf("a stale Fail asked for no re-probe: %s", show(o))
	}
	// The re-probe passes: the next new thread defers.
	e.Prober.Ensure(k, func(context.Context) (Verdict, error) { return Pass, nil })
	r.Session = "s3"
	if o = Apply(e, r); !o.Changed || o.NeedProbe {
		t.Fatalf("after a passing re-probe: %s", show(o))
	}
}

// Forget runs on the request path: it must not wait on the kept copy.
func TestForgetDoesNotPersistOnTheCaller(t *testing.T) {
	p := NewProber()
	var ran func()
	p.Go = func(fn func()) { ran = fn } // the background runner, held
	block := make(chan struct{})
	called := make(chan struct{}, 1)
	p.Persist = func(ProbeKey, Verdict, time.Time) { called <- struct{}{}; <-block }
	k := ProbeKey{"u", "m", "r"}
	p.Seed(k, Pass, time.Now())
	done := make(chan struct{})
	go func() { p.Forget(k); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Forget waited on Persist")
	}
	if p.Verdict(k) != Unknown {
		t.Fatal("Forget kept the verdict in memory")
	}
	select {
	case <-called:
		t.Fatal("Persist ran on the caller")
	default:
	}
	if ran == nil {
		t.Fatal("the kept copy is never removed")
	}
	close(block)
	go ran()
	<-called
}

// Through the real history: one session that used one tool of a server keeps the whole server
// loaded in the next session.
func TestHistoryMCPServerUseCarriesOver(t *testing.T) {
	m := NewMem(nil)
	m.Open(0, nil)
	tools := append([]string{decl("Bash")}, serverTools("big", 30)...)
	use := `{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"mcp__big__t4","input":{}}]}`
	m.Observe("t", "a", "/w", body(tools, ""), 1)
	m.Observe("t", "b", "/w", body(tools, "", use), 2)
	m.Observe("t", "c", "/w", body(tools, ""), 3)
	if got := Decide(m, Input{Tenant: "t", Session: "d", Body: body(tools, "")}); len(got) != 0 {
		t.Fatalf("server deferred after one of its tools was used: %v", got)
	}
	m.Observe("t", "e", "/w", body(tools, ""), 4)
	m.Observe("t", "g", "/w", body(tools, ""), 5) // now c, e, g: the use in b is out of the window
	if got := Decide(m, Input{Tenant: "t", Session: "f", Body: body(tools, "")}); len(got) != 30 {
		t.Fatalf("after 3 newer unused sessions: %d deferred, want 30", len(got))
	}
}

// H2. The probe leaves room for a preamble, and a Fail holds FailTTL (15 minutes), not a day.
func TestProbeFailIsShortLived(t *testing.T) {
	if n := gjson.GetBytes(ProbeBody("claude-x", "n"), "max_tokens").Int(); n != 1000 {
		t.Fatalf("probe max_tokens = %d", n)
	}
	p := NewProber()
	p.Go = func(fn func()) { fn() }
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	k := ProbeKey{"u", "m", "r"}
	calls := 0
	p.Ensure(k, func(context.Context) (Verdict, error) { calls++; return Fail, nil })
	now = now.Add(FailTTL - time.Minute)
	if p.Stale(k) {
		t.Fatal("a Fail went stale early")
	}
	p.Ensure(k, func(context.Context) (Verdict, error) { calls++; return Pass, nil })
	if calls != 1 {
		t.Fatal("a fresh Fail was probed again")
	}
	now = now.Add(2 * time.Minute)
	if !p.Stale(k) {
		t.Fatal("a Fail older than FailTTL is not stale")
	}
	p.Ensure(k, func(context.Context) (Verdict, error) { calls++; return Pass, nil })
	if calls != 2 || p.Verdict(k) != Pass {
		t.Fatalf("calls=%d verdict=%v", calls, p.Verdict(k))
	}
	// A Pass still holds the full ProbeTTL.
	now = now.Add(FailTTL + time.Hour)
	if p.Stale(k) {
		t.Fatal("a Pass went stale after FailTTL")
	}
}

// B2. A probe's verdict is written to the database AFTER p.mu is released: Verdict and Stale run on
// the request path and must not wait on a slow write.
func TestEnsurePersistsOutsideTheLock(t *testing.T) {
	p := NewProber()
	block := make(chan struct{})
	entered := make(chan struct{})
	p.Persist = func(ProbeKey, Verdict, time.Time) { close(entered); <-block }
	k := ProbeKey{"u", "m", "r"}
	p.Ensure(k, func(context.Context) (Verdict, error) { return Pass, nil }) // runs on its own goroutine
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Persist never ran")
	}
	done := make(chan Verdict, 1)
	go func() { done <- p.Verdict(k); p.Stale(k) }()
	select {
	case v := <-done:
		if v != Pass {
			t.Fatalf("verdict %v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Verdict waited on Persist")
	}
	close(block)
}

// H4. At most maxInFlight probes run at once, and the prober keeps at most maxKeys keys.
func TestProberCaps(t *testing.T) {
	p := NewProber()
	var held []func()
	p.Go = func(fn func()) { held = append(held, fn) }
	for i := 0; i < maxInFlight+3; i++ {
		p.Ensure(ProbeKey{"u", "m" + itoa(i), "r"}, func(context.Context) (Verdict, error) { return Pass, nil })
	}
	if len(held) != maxInFlight {
		t.Fatalf("%d probes started, want %d", len(held), maxInFlight)
	}
	for _, fn := range held {
		fn()
	}
	p.Go = func(fn func()) { fn() }
	for i := 0; i < maxKeys+20; i++ {
		p.Ensure(ProbeKey{"u2", "m" + itoa(i), "r"}, func(context.Context) (Verdict, error) { return Pass, nil })
		p.Seed(ProbeKey{"u3", "m" + itoa(i), "r"}, Pass, time.Now())
	}
	p.mu.Lock()
	n := len(p.m)
	p.mu.Unlock()
	if n > maxKeys {
		t.Fatalf("%d keys, cap %d", n, maxKeys)
	}
}

// A probe in flight when Forget runs does not write its answer back.
func TestForgetVoidsAProbeInFlight(t *testing.T) {
	p := NewProber()
	var held []func()
	p.Go = func(fn func()) { held = append(held, fn) }
	k := ProbeKey{"u", "m", "r"}
	p.Ensure(k, func(context.Context) (Verdict, error) { return Pass, nil })
	p.Forget(k)
	held[0]() // the probe finishes now, after the Forget
	if p.Verdict(k) != Unknown {
		t.Fatal("a probe that finished after Forget wrote its verdict back")
	}
}

func TestNormProject(t *testing.T) {
	if NormProject("/r/.claude/worktrees/x") != "/r" || NormProject("/r") != "/r" {
		t.Fatal("NormProject")
	}
}
