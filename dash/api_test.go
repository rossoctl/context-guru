package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
)

// newTestAPI wires a recorder + API with a few requests already persisted.
func newTestAPI(t *testing.T, opts Options) (*API, *Recorder) {
	t.Helper()
	if opts.DBPath == "" {
		opts.DBPath = filepath.Join(t.TempDir(), "d.db")
	}
	opts.BatchSize, opts.FlushInterval = 1, time.Millisecond
	rec, err := NewRecorder(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	return NewAPI(rec), rec
}

// seed writes events straight through the store so the test does not race the
// writer goroutine.
func seed(t *testing.T, rec *Recorder, evs ...*Event) {
	t.Helper()
	if err := rec.DB().insertBatch(evs); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, a *API, path, remote string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	m := http.NewServeMux()
	a.Mount(m)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if remote != "" {
		req.RemoteAddr = remote
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	var body map[string]any
	if strings.Contains(w.Header().Get("Content-Type"), "json") {
		_ = json.Unmarshal(w.Body.Bytes(), &body)
	}
	return w, body
}

func TestAPIRoutesServeJSON(t *testing.T) {
	a, rec := newTestAPI(t, Options{CaptureContent: true})
	e := mkEvent(time.Now().UnixMilli(), "sess-1", "aws/claude-sonnet-5", 1000, 800)
	e.Content = []ContentRow{{Path: "messages.2", BeforeTokens: 200, AfterTokens: 0,
		Before: "tool output\nline two\n", After: "tool output\n<<cg:abc>>"}}
	seed(t, rec, e)

	for _, path := range []string{
		"/api/stats", "/api/series?bucket=60000", "/api/requests", "/api/sessions",
		"/api/components", "/api/facets", "/api/benchmarks", "/api/capture",
	} {
		w, body := get(t, a, path, "127.0.0.1:1234")
		if w.Code != http.StatusOK {
			t.Errorf("%s -> %d: %s", path, w.Code, w.Body.String())
			continue
		}
		if body == nil {
			t.Errorf("%s returned no JSON object", path)
		}
	}
}

// TestCaptureReportsModeAndObserveQueue covers what the observe banner reads. The
// issue required "You are currently in observe mode…" to be unmistakable, and the mode
// was previously hardcoded to "active" in main.go with dash.Options.Mode set and never
// read — so the banner could not have fired however the proxy was configured.
func TestCaptureReportsModeAndObserveQueue(t *testing.T) {
	// Default: active, and no queue at all (a sync deployment must show no phantom one).
	a, _ := newTestAPI(t, Options{})
	_, body := get(t, a, "/api/capture", "127.0.0.1:1")
	c, _ := body["capture"].(map[string]any)
	if c["mode"] != ModeActive {
		t.Errorf("mode = %v; want %q", c["mode"], ModeActive)
	}
	if _, ok := c["observe_queue"]; ok {
		t.Error("observe_queue is present with no pool running; the UI would render an empty queue")
	}

	// Observe, with the host publishing its pool counters.
	ao, reco := newTestAPI(t, Options{Mode: ModeObserve})
	reco.SetObserveQueue(func() QueueStats {
		return QueueStats{Queued: 7, Pending: 2, Processed: 40, Dropped: 3, Errors: 1}
	})
	_, body = get(t, ao, "/api/capture", "127.0.0.1:1")
	c, _ = body["capture"].(map[string]any)
	if c["mode"] != ModeObserve {
		t.Errorf("mode = %v; want %q", c["mode"], ModeObserve)
	}
	q, ok := c["observe_queue"].(map[string]any)
	if !ok {
		t.Fatalf("observe_queue missing: %v", c)
	}
	// dropped is the counter that changes a reader's conclusion, so assert it explicitly.
	if q["dropped"] != float64(3) || q["processed"] != float64(40) {
		t.Errorf("observe_queue = %v; want processed=40 dropped=3", q)
	}
}

func TestAPIRequestDetailGating(t *testing.T) {
	a, rec := newTestAPI(t, Options{CaptureContent: true, TrustedCIDRs: []string{"10.1.0.0/16"}})
	e := mkEvent(time.Now().UnixMilli(), "sess-1", "m", 1000, 800)
	e.Content = []ContentRow{{Path: "messages.2", Before: "a customer's private source", After: "x"}}
	seed(t, rec, e)
	path := "/api/requests/1"

	// Loopback always sees content.
	_, body := get(t, a, path, "127.0.0.1:5000")
	if body["content_visible"] != true {
		t.Error("loopback should see per-request content")
	}
	req := body["request"].(map[string]any)
	if _, ok := req["content"]; !ok {
		t.Error("loopback response carried no content")
	}

	// A trusted CIDR sees content.
	_, body = get(t, a, path, "10.1.2.3:5000")
	if body["content_visible"] != true {
		t.Error("a trusted CIDR should see per-request content")
	}

	// Anything else gets the row but NOT the content, and is told why.
	w, body := get(t, a, path, "203.0.113.9:5000")
	if w.Code != http.StatusOK {
		t.Fatalf("untrusted peer should still see the metrics row, got %d", w.Code)
	}
	if body["content_visible"] != false {
		t.Error("an untrusted peer must not see content")
	}
	req = body["request"].(map[string]any)
	if c, ok := req["content"]; ok {
		t.Errorf("content leaked to an untrusted peer: %v", c)
	}
	// The aggregate row IS visible — a proxy bound to 0.0.0.0 should still show its
	// own numbers; only content and config are gated.
	if req["tokens_before"] == nil {
		t.Error("metrics were withheld from an untrusted peer; only content should be")
	}
}

func TestAPIConfigIsGatedAndRedacted(t *testing.T) {
	a, _ := newTestAPI(t, Options{
		TrustedCIDRs: []string{"10.1.0.0/16"},
		Effective: map[string]any{
			"preset":            "codesmart",
			"anthropic_api_key": fakeKey("CONFIGLEAK"),
			"unknown_field":     "unclassified",
		},
	})

	w, _ := get(t, a, "/api/config", "203.0.113.9:5000")
	if w.Code != http.StatusForbidden {
		t.Errorf("untrusted peer got %d for /api/config; want 403", w.Code)
	}

	w, body := get(t, a, "/api/config", "127.0.0.1:5000")
	if w.Code != http.StatusOK {
		t.Fatalf("loopback got %d for /api/config", w.Code)
	}
	// The configuration now sits under "config", beside the "scope"/"description" that
	// say WHOSE configuration it is — see TestConfigSaysWhoseConfigurationItIs. Gating
	// and redaction, which is what this test is about, are unchanged.
	cfg, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("no config object in the payload: %v", body)
	}
	if cfg["preset"] != "codesmart" {
		t.Errorf("preset = %v; want codesmart", cfg["preset"])
	}
	// Redacted even for a trusted caller — a defence that only applies to untrusted
	// callers is one misconfiguration from being no defence.
	raw := w.Body.String()
	if strings.Contains(raw, "CONFIGLEAK") {
		t.Errorf("a credential reached a trusted caller: %s", raw)
	}
	if cfg["unknown_field"] != Redacted {
		t.Errorf("unknown_field = %v; want redacted", cfg["unknown_field"])
	}
}

func TestAPIFilterAndKeysetPaginationOverHTTP(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	var evs []*Event
	for i := 0; i < 12; i++ {
		e := mkEvent(now-int64(i)*1000, "sess-a", "model-a", 100, 90)
		if i%2 == 0 {
			e.SessionID, e.Model = "sess-b", "model-b"
		}
		evs = append(evs, e)
	}
	seed(t, rec, evs...)

	_, body := get(t, a, "/api/requests?session=sess-b", "127.0.0.1:1")
	if int(body["total"].(float64)) != 6 {
		t.Errorf("session filter total = %v; want 6", body["total"])
	}

	// Page through with the returned cursor.
	_, body = get(t, a, "/api/requests?limit=5", "127.0.0.1:1")
	cursor := int64(body["next_cursor"].(float64))
	if cursor == 0 {
		t.Fatal("no next_cursor with 12 rows and limit 5")
	}
	first := body["requests"].([]any)
	if len(first) != 5 {
		t.Fatalf("page 1 had %d rows; want 5", len(first))
	}
	_, body2 := get(t, a, "/api/requests?limit=5&before="+strconv.FormatInt(cursor, 10), "127.0.0.1:1")
	second := body2["requests"].([]any)
	if len(second) != 5 {
		t.Fatalf("page 2 had %d rows; want 5", len(second))
	}
	firstIDs := map[float64]bool{}
	for _, r := range first {
		firstIDs[r.(map[string]any)["id"].(float64)] = true
	}
	for _, r := range second {
		if firstIDs[r.(map[string]any)["id"].(float64)] {
			t.Error("page 2 repeated a row from page 1")
		}
	}
}

func TestAPIServesEmbeddedUIWithNoNetworkFetches(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	m := http.NewServeMux()
	a.Mount(m)

	// /dashboard must land on the canonical /dashboard/.
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Errorf("/dashboard -> %d; want a redirect to /dashboard/", w.Code)
	}

	for _, path := range []string{"/dashboard/", "/dashboard/style.css", "/dashboard/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s -> %d", path, w.Code)
		}
		if w.Body.Len() == 0 {
			t.Errorf("%s served an empty body", path)
		}
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s missing a self-only CSP: %q", path, csp)
		}
	}

	// The offline guarantee: no asset may reference an external origin. context-guru
	// ships into air-gapped contexts, so a CDN tag is a shipping bug, not a nit.
	for _, path := range []string{"/dashboard/", "/dashboard/style.css", "/dashboard/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		body := w.Body.String()
		for _, bad := range []string{"https://", "http://", "//cdn.", "unpkg.com", "jsdelivr", "googleapis"} {
			if strings.Contains(body, bad) {
				// Allow the SVG namespace, which is a spec identifier and never fetched.
				if bad == "http://" && strings.Count(body, bad) == strings.Count(body, "http://www.w3.org/2000/svg") {
					continue
				}
				t.Errorf("%s references an external origin (%q); the dashboard must work offline", path, bad)
			}
		}
	}
}

func TestUIHasTestIDsForEveryStatTile(t *testing.T) {
	// The visual layer is regression-tested by asserting the testids the Playwright
	// checks and the docs screenshots depend on. If a tile is renamed, this fails
	// before the screenshots silently go stale.
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(js) + string(html)

	// Stat tiles get their testid as "tile-"+key from the tile() helper, so assert on
	// the call site: tile('<key>', … . Renaming a tile then fails here, before the
	// Playwright checks and the docs screenshots silently go stale.
	for _, key := range []string{
		"requests", "tokens-before", "tokens-after", "saved-gross", "saved-unique",
		"saved-adjusted", "overcount", "cost-baseline", "cost-actual", "cost-cg",
		"saved-usd", "total-saved-usd", "cachesplit-saved",
		"split-requests", "split-tail-moved", "split-credited", "split-hit-rate",
		"split-historical",
		"cache-read", "cache-write", "fresh-input", "output",
		"cg-latency", "upstream-latency", "expands", "reverts", "passthroughs",
	} {
		if !strings.Contains(source, "tile('"+key+"'") {
			t.Errorf("stat tile %q is not rendered (expected a tile('%s', …) call); "+
				"data-testid=tile-%s is asserted by the UI checks and the docs screenshots", key, key, key)
		}
	}
	// Every other panel carries its testid literally.
	for _, id := range []string{
		"denominators", "waterfall", "safety-cost", "cache-miss", "uncompressed-reasons",
		"accounting", "chart-cost", "chart-tokens", "chart-cache", "chart-latency",
		"chart-volume", "chart-components", "components-table", "sessions-table",
		"requests-table", "requests-page", "requests-next", "requests-prev",
		"bench-list", "bench-run", "bench-scatter", "bench-tasks",
		"config-body", "capture-body", "capture-warning", "observe-banner",
		"drawer-body", "diff-block",
		"detail-summary", "detail-components", "live-table", "live-indicator",
		"theme-toggle", "tab-overview", "tab-components", "tab-sessions", "tab-requests",
		"tab-benchmarks", "tab-config", "filter-q", "filter-range", "filter-model",
		"filter-provider", "filter-agent", "filter-preset", "filter-mode",
		"filter-component", "filter-reason", "filter-accounting", "filter-clear",
		"request-row", "diff-mode-git", "diff-mode-side", "diff-mode-orig", "diff-mode-raw",
		"drawer-close",
		// The session compaction-diff view: the entry point, its summary, its
		// per-component roll-up, the all-blocks mode switch, and the two states that
		// only exist on the cold-storage path.
		"session-diff", "session-diff-summary", "session-diff-components",
		"session-diff-modes", "open-session-diff", "fetch-transcript",
		"state-cold", "state-fetched", "retry-transcript", "open-archive",
		// The gate's three registration modes: the closed explanation that replaces the
		// form, and the invite-code field that only appears when a code is checked.
		"gate-closed", "gate-code", "gate-register", "gate-signin",
	} {
		if !strings.Contains(source, `"`+id+`"`) && !strings.Contains(source, "'"+id+"'") {
			t.Errorf("data-testid %q is not produced by the UI; a check or screenshot depends on it", id)
		}
	}
}

// Which tabs a plain hosted account may see. applyAccount hides [data-manager] unless the
// signed-in account is a manager, and [data-local-ok] exempts a single-tenant proxy, whose
// operator is the only user of their own box. Components is manager-only on a shared
// deployment: it is the view a tenant would use to tune a pipeline that is no longer theirs
// to tune.
func TestManagerOnlyTabsCarryTheGatingAttributes(t *testing.T) {
	html, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"components": {"data-manager", "data-local-ok"},
		"benchmarks": {"data-manager", "data-local-ok"},
		"config":     {"data-manager", "data-local-ok"},
		"tenants":    {"data-manager"},
	}
	for name, attrs := range want {
		var line string
		for _, l := range strings.Split(string(html), "\n") {
			if strings.Contains(l, `data-testid="tab-`+name+`"`) {
				line = l
			}
		}
		if line == "" {
			t.Errorf("no tab-%s in the markup", name)
			continue
		}
		for _, a := range attrs {
			if !strings.Contains(line, a) {
				t.Errorf("tab-%s is missing %s; a non-manager would see it: %s",
					name, a, strings.TrimSpace(line))
			}
		}
		if len(attrs) == 1 && strings.Contains(line, "data-local-ok") {
			t.Errorf("tab-%s must not be data-local-ok", name)
		}
	}
}

// TestBenchIngestCommitsNothingForARunWithNoTasks pins the counter/table agreement.
// IngestBenchDir used to INSERT the bench_runs row before it knew whether any rows-*
// file parsed, then return tasks=0 — so IngestBenchRoots did not count the run but the
// row was already committed. A real jobs root produced "runs=17" in the log and 42 rows
// from the API, 25 of them with no arms, padding the Benchmarks tab with empty shells
// and making the PR's own "42 runs ingested" claim wrong.
func TestBenchIngestCommitsNothingForARunWithNoTasks(t *testing.T) {
	dir := t.TempDir()
	write := func(sub, name, body string) {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A summary with no rows files at all: the shape of an abandoned or in-progress run.
	write("smoke-hd", "summary.json", `{"model":"m","dataset":"d"}`)
	// A summary whose only rows file is truncated, so nothing parses.
	write("dbg1", "summary.json", `{"model":"m","dataset":"d"}`)
	write("dbg1", "rows-off.json", `[{"task":"t1",`)
	// And one good run, so this proves selectivity rather than a blanket refusal.
	write("real", "summary.json", `{"model":"m","dataset":"d"}`)
	write("real", "rows-off.json", `[{"task":"t1","reward":1.0,"steps":3,"agent_cost":0.1}]`)

	db := openTestDB(t)
	runs, tasks := db.IngestBenchRoots([]string{dir})
	if runs != 1 || tasks != 1 {
		t.Errorf("ingested %d runs / %d tasks; want 1/1 (the two contentless dirs must not count)", runs, tasks)
	}

	got, err := db.BenchRuns()
	if err != nil {
		t.Fatal(err)
	}
	// The assertion that actually failed before: the API's row count must EQUAL the
	// counter the log reports.
	if len(got) != runs {
		names := make([]string, len(got))
		for i, r := range got {
			names[i] = r.Name
		}
		t.Errorf("BenchRuns returned %d rows but the ingest counted %d runs: %v", len(got), runs, names)
	}
	for _, r := range got {
		if len(r.Arms) == 0 {
			t.Errorf("run %q was committed with no arms; the UI would render an empty row", r.Name)
		}
	}
}

func TestBenchIngestFromRealHarnessArtifacts(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "study-run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	// Exactly the shape deploy/harbor/*.py writes.
	summary := `{"model":"aws/claude-sonnet-5","dataset":"swe-bench-verified",
	  "price":[2e-06,1e-05,2e-07,2.5e-06],"tasks":2,
	  "configs":[{"config":"codesmart","solved":1,"solve_rate":0.5}]}`
	rowsOff := `[{"task":"t1","reward":1.0,"steps":20,"prompt_tokens":1000,"completion_tokens":50,
	   "cache_read":900,"cache_write":90,"fresh_input":10,"agent_cost":0.5,"norm_cost":0.4,
	   "wall_s":100.0,"exception":false},
	  {"task":"t2","reward":0.0,"steps":30,"prompt_tokens":2000,"completion_tokens":60,
	   "cache_read":1800,"cache_write":180,"fresh_input":20,"agent_cost":1.0,"norm_cost":0.8,
	   "wall_s":200.0,"exception":true}]`
	rowsCG := `[{"task":"t1","reward":1.0,"steps":18,"prompt_tokens":800,"completion_tokens":45,
	   "cache_read":740,"cache_write":50,"fresh_input":10,"agent_cost":0.4,"norm_cost":0.32,
	   "wall_s":95.0,"exception":false},
	  {"task":"t2","reward":1.0,"steps":25,"prompt_tokens":1600,"completion_tokens":55,
	   "cache_read":1500,"cache_write":90,"fresh_input":10,"agent_cost":0.8,"norm_cost":0.64,
	   "wall_s":180.0,"exception":false}]`
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(run, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("summary.json", summary)
	write("rows-off.json", rowsOff)
	write("rows-codesmart.json", rowsCG)
	// A directory that is not a run must be skipped silently, since a jobs root is
	// full of in-progress and unrelated directories.
	if err := os.MkdirAll(filepath.Join(dir, "not-a-run"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A truncated rows file must not abort the whole ingest.
	write("rows-broken.json", `[{"task":"t1",`)

	db := openTestDB(t)
	runs, tasks := db.IngestBenchRoots([]string{dir})
	if runs != 1 || tasks != 4 {
		t.Fatalf("ingested %d runs / %d tasks; want 1/4", runs, tasks)
	}

	got, err := db.BenchRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("BenchRuns = %d", len(got))
	}
	r := got[0]
	if r.Model != "aws/claude-sonnet-5" || r.Dataset != "swe-bench-verified" {
		t.Errorf("run metadata: %+v", r)
	}
	if len(r.Arms) != 2 {
		t.Fatalf("arms = %d; want 2 (off, codesmart)", len(r.Arms))
	}
	byArm := map[string]BenchArm{}
	for _, a := range r.Arms {
		byArm[a.Arm] = a
	}
	off, cg := byArm["off"], byArm["codesmart"]
	if off.Tasks != 2 || off.Solved != 1 || off.Exceptions != 1 {
		t.Errorf("off arm = %+v", off)
	}
	// Solve rate is over SCORED trials — an exception is not a failed solve.
	if off.Scored != 1 || off.SolveRate != 1 {
		t.Errorf("off solve rate = %v over %d scored; an exception must not count as unsolved",
			off.SolveRate, off.Scored)
	}
	if cg.Solved != 2 || cg.TotalCostUSD < 1.19 || cg.TotalCostUSD > 1.21 {
		t.Errorf("codesmart arm = %+v", cg)
	}
	if cg.CacheHitRate <= 0 || cg.CacheHitRate >= 1 {
		t.Errorf("cache hit rate = %v; want a fraction", cg.CacheHitRate)
	}

	// Per-task drill-down.
	tasksOut, err := db.BenchTasks(r.ID, "codesmart")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasksOut) != 2 {
		t.Fatalf("per-task rows = %d; want 2", len(tasksOut))
	}
	all, err := db.BenchTasks(r.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("all-arm rows = %d; want 4", len(all))
	}

	// Re-ingest must REPLACE, not duplicate — a proxy restart pointed at a jobs root
	// would otherwise double every historical run.
	runs2, tasks2 := db.IngestBenchRoots([]string{dir})
	if runs2 != 1 || tasks2 != 4 {
		t.Fatalf("re-ingest = %d runs / %d tasks", runs2, tasks2)
	}
	after, _ := db.BenchRuns()
	if len(after) != 1 {
		t.Errorf("re-ingest duplicated the run: %d runs", len(after))
	}
	allAfter, _ := db.BenchTasks(after[0].ID, "")
	if len(allAfter) != 4 {
		t.Errorf("re-ingest duplicated tasks: %d rows", len(allAfter))
	}
}

func TestAPIBadRequestIDs(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	for _, path := range []string{"/api/requests/0", "/api/requests/abc"} {
		w, _ := get(t, a, path, "127.0.0.1:1")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d; want 400", path, w.Code)
		}
	}
	w, _ := get(t, a, "/api/requests/999999", "127.0.0.1:1")
	if w.Code != http.StatusNotFound {
		t.Errorf("missing request -> %d; want 404", w.Code)
	}
}

func TestTrustedRejectsMalformedRemoteAddr(t *testing.T) {
	a, _ := newTestAPI(t, Options{TrustedCIDRs: []string{"not-a-cidr", "10.0.0.0/8"}})
	// A malformed CIDR is skipped rather than failing startup, but the valid one works.
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "10.0.0.5:1"
	if !a.trusted(req) {
		t.Error("a valid CIDR alongside a malformed one should still be honored")
	}
	req.RemoteAddr = "garbage"
	if a.trusted(req) {
		t.Error("an unparseable remote address must not be trusted")
	}
	req.RemoteAddr = "[::1]:4000"
	if !a.trusted(req) {
		t.Error("IPv6 loopback should be trusted")
	}
}

// TestUIScriptParses guards the failure that actually bit during development: a
// missing closing paren in app.js. Go's compiler cannot see it, every Go test
// passed, the HTML still served 200 — and the whole dashboard rendered blank. A
// syntax check is the cheapest possible guard.
//
// It uses node when available (CI images have it; the repo needs no npm and no
// package.json) and otherwise falls back to a paren/brace balance check over the
// source with strings and comments stripped, which is enough to catch this class
// of typo without a JS engine.
func TestUIScriptParses(t *testing.T) {
	src, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if node, lookErr := exec.LookPath("node"); lookErr == nil {
		f := filepath.Join(t.TempDir(), "app.js")
		if err := os.WriteFile(f, src, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, "--check", f).CombinedOutput()
		if err != nil {
			t.Fatalf("app.js does not parse:\n%s", out)
		}
		return
	}
	t.Log("node not found; falling back to a bracket-balance check")
	if line, ok := unbalancedAt(string(src)); !ok {
		t.Errorf("app.js brackets do not balance (first imbalance around line %d)", line)
	}
}

func TestUnbalancedDetectorCatchesADroppedParen(t *testing.T) {
	// The exact shape of the bug that rendered a blank dashboard during development:
	// a nested el(...) chain whose outer appendChild lost its closing paren.
	bad := "body.appendChild(el('tr', {},\n  el('td', { text: x }),\n  el('td', { text: y });\n"
	if _, ok := unbalancedAt(bad); ok {
		t.Error("the balance check missed a dropped closing paren")
	}
	// Brackets inside strings, comments and template literals must not confuse it.
	good := "const a = '(((';\n// )))\n/* ((( */\nconst b = `${x} ) ( `;\nf(g(h()));\n"
	if _, ok := unbalancedAt(good); !ok {
		t.Error("the balance check false-positived on brackets inside strings/comments")
	}
}

// unbalancedAt scans JS source with strings, template literals, regexes and
// comments skipped, and reports whether ()[]{} balance. Deliberately simple: it
// only has to catch a dropped closing paren, not to be a parser.
func unbalancedAt(s string) (int, bool) {
	var stack []byte
	line := 1
	openLine := map[int]int{}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n':
			line++
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			line++
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				if s[i] == '\n' {
					line++
				}
				i++
			}
			i++
		case c == '\'' || c == '"' || c == '`':
			quote := c
			i++
			for i < len(s) && s[i] != quote {
				if s[i] == '\\' {
					i++
				} else if s[i] == '\n' {
					line++
				}
				i++
			}
		case c == '(' || c == '[' || c == '{':
			stack = append(stack, c)
			openLine[len(stack)] = line
		case c == ')' || c == ']' || c == '}':
			want := map[byte]byte{')': '(', ']': '[', '}': '{'}[c]
			if len(stack) == 0 || stack[len(stack)-1] != want {
				return line, false
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		return openLine[len(stack)], false
	}
	return 0, true
}

// The dashboard must not reference a savings field the API stopped emitting, and must not
// re-grow the two it deliberately dropped.
//
// `cache_saved_usd` was a headline tile reading "Prompt-cache savings", and
// `cache_saved_protected_usd` a second one reading "of which where we split the prefix".
// Neither was a saving of ours: the first is the provider's whole prompt cache, which the
// agent's own breakpoints earn, and the second was co-occurrence dressed as cause. They were
// replaced by `cachesplit_saved_usd`, which is measured, and the provider figure stays in the
// API as a diagnostic only. A revert that puts either back on the page fails here.
func TestUIClaimsOnlyTheCacheSavingWeMeasure(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	if !strings.Contains(src, "o.cachesplit_saved_usd") {
		t.Error("the overview does not render cachesplit_saved_usd, the one cache saving we claim")
	}
	if strings.Contains(src, "cache_saved_protected_usd") {
		t.Error("cache_saved_protected_usd is back on the page; it measured co-occurrence, not cause")
	}
	// The provider figure may appear ONCE in the per-request drawer and ONCE in the A/B
	// comparison (where it is the control variable, not the credit) — never in a tile.
	for _, forbidden := range []string{
		"tile('cache-saved'", "'Prompt-cache savings'", "o.cache_saved_usd",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("the provider's cache saving is presented as ours again: %q", forbidden)
		}
	}
}

// The settings page is fields now, and its field NAMES are a wire contract with
// config.Form's json tags. Nothing else checks that: a renamed tag would leave the form
// silently posting a key the server ignores, which is the failure mode the YAML box already
// had — extract_llm's own loader is non-strict, so an ignored key does nothing at all, and
// this is the component that spends money.
//
// Asserted from the JS side because that is the half with no compiler.
func TestSettingsFormSpeaksTheSameFieldNamesAsTheServer(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	// Every key of config.ExtractLLMForm, by its json tag, read off the STRUCT rather than
	// listed here — a hand-kept list is how the page ended up not showing
	// allow_on_caching_backend and model.source, the two fields that decided whether the
	// component could act at all. Add a field on the server and this test names it.
	ft := reflect.TypeOf(config.ExtractLLMForm{})
	for i := 0; i < ft.NumField(); i++ {
		key := strings.Split(ft.Field(i).Tag.Get("json"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		if !strings.Contains(src, key) {
			t.Errorf("the form does not mention %q, a field the server expects", key)
		}
	}
	// The document itself is the server's now: no YAML editor on the settings page, and no
	// hand-rolled writer behind it.
	for _, gone := range []string{"'#set-yaml'", "writeXllm", "readXllm", "unmanagedXllmLines"} {
		if strings.Contains(src, gone) {
			t.Errorf("%s is back; the browser must not build the configuration document "+
				"(that is what produced \"did not find expected key\" on every save)", gone)
		}
	}
	// And it posts fields, not text.
	if !strings.Contains(src, "body.config = {") {
		t.Error("saveSettings does not post structured fields")
	}
	// With the YAML box gone, a document the server could not fully read must be VISIBLE and
	// unsaveable. Otherwise the fields draw a guess as fact and a save writes the guess back,
	// which is a worse version of the bug this replaced.
	for _, want := range []string{"cfg-unreadable", "parse_error"} {
		if !strings.Contains(src, want) {
			t.Errorf("the settings page does not handle an unreadable stored configuration (%q)", want)
		}
	}
	// The fields are a subset of what a document can say, so the document itself has to be
	// on the page somewhere — read-only, since an editable box here is the writer that
	// corrupted configurations.
	if !strings.Contains(src, "full-config-yaml") {
		t.Error("the settings page does not show the full stored configuration")
	}
	// The roster's configuration column must not be the document's first line: the form
	// writes YAML through a marshaller, so that line is the word `components:` for every
	// account that has saved once.
	if strings.Contains(src, "effective_config_yaml || '').split('\\n')[0]") {
		t.Error("the tenants roster is back to showing line one of the document, which now " +
			"reads \"components:\" for every configured account")
	}
}
