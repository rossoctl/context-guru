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

	"slices"

	"github.com/rossoctl/context-guru/components"
	// The registry is populated by each component's init(), so the descriptors this test
	// walks only exist if every component is linked in.
	_ "github.com/rossoctl/context-guru/components/all"
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
		// The Grafana-style time range: the popover itself, its absolute pair and Apply.
		// (the per-range buttons get their testid from QUICK_RANGES at runtime, so they are
		// asserted in TestUINeverShowsABareCost against the table instead of by literal)
		"filter-from", "filter-to", "filter-range-apply",
		// The manager's scope control, and the two things the money pass added to the page:
		// the not-netted prefix-change diagnostic and the unfinished-amortization pill.
		"filter-tenant", "prefix-change-cost", "in-flight",
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

// TestSettingsFormSpeaksTheSameFieldNamesAsTheServer is the UI half of the descriptor
// contract, and it is deliberately INVERTED from the test it replaces.
//
// The old one asserted that app.js MENTIONED every field name the server expected, because
// the page hand-wrote one control per knob and the failure mode was omission: it reached 18
// keys of 97 and one component of fourteen, and the two fields that decided whether
// extract_llm could act at all (allow_on_caching_backend, model.source) were simply absent.
//
// A page that renders from components.Field cannot omit a field — the loop does not have an
// opinion — so mentioning a name is no longer evidence of anything. What can still go wrong
// is the opposite: somebody hand-writes a control again, and their copy of a default, an
// enum list or a threshold drifts from the declaration. That already happened once, and it
// was not cosmetic: the browser offered four strategies where the engine parses five, so a
// stored `strategy: deterministic` was not recognised and got rewritten to `code`, silently
// turning an LLM-free configuration into one that makes model calls.
//
// So this test walks every declared field and asserts the page does NOT name it, plus the
// typed structure a name-only grep could not check: one branch per declared type, the enum
// options taken from the descriptor, secrets write-only, and Min carried through as the
// number input's floor. The per-field RENDERING is checked by driving the real page under
// jsdom; what a Go test can pin is that nothing here is hand-copied.
func TestSettingsFormSpeaksTheSameFieldNamesAsTheServer(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	all := components.AllFields()
	if len(all) < 10 {
		t.Fatalf("only %d components registered; the blank import of components/all is missing "+
			"and this test would pass by having nothing to check", len(all))
	}

	// The page reads the descriptors, every part of them. Each of these is a fact about a
	// field that used to be retyped in JavaScript.
	for _, want := range []string{
		"opts.component_fields", "renderComponentFields(",
		"fd.key", "fd.type", "fd.default", "fd.options", "fd.min", "fd.secret", "fd.hint",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the settings form does not read %s; it is not descriptor-driven", want)
		}
	}
	// And the hand-kept copies are gone. XLLM_DEFAULTS duplicated the server's default table,
	// which is the drift this refactor removes; renderXllmForm was the sixteen literal control
	// calls, and one component of fourteen.
	for _, gone := range []string{"XLLM_DEFAULTS", "renderXllmForm", "xllmState"} {
		if strings.Contains(src, gone) {
			t.Errorf("%s is back: the page is hand-listing fields again, which is a second "+
				"source of truth for every default and enum it names", gone)
		}
	}

	// One branch per DECLARED type, derived from the declarations rather than listed here: a
	// component that gains the first field of some type must not fall through to a text box
	// that posts a string where the server demands a number.
	types := map[string]string{} // type -> an example field, for the message
	for name, decls := range all {
		for _, fd := range decls {
			if _, seen := types[fd.Type]; !seen {
				types[fd.Type] = name + "." + fd.Key
			}
		}
	}
	if len(types) < 5 {
		t.Errorf("only %d field types in the registry (%v); this test is not covering much", len(types), types)
	}
	for typ, example := range types {
		if !strings.Contains(src, "case '"+typ+"'") {
			t.Errorf("no control for field type %q (e.g. %s): it would fall through to the "+
				"string branch and post the wrong JSON type", typ, example)
		}
	}

	// No field NAME is written in the page. The allowlist is short and each entry is a
	// deliberate exception with a reason — add a control by hand and this names your key.
	allowed := map[string]string{
		// The one coupling the page is allowed to know: extract_llm's constructor refuses
		// both passes off ("nothing to do"), so the form must not be able to post that.
		"per_output":         "the extract_llm both-switches-off coupling",
		"cold_cache.enabled": "the extract_llm both-switches-off coupling",
		// Whether `source: config` resolves to anything on this deployment. It does not on
		// the hosted service, and a page that offers the choice silently is how an account
		// ran extract_llm 251 times and made zero model calls.
		"model.source": "the no-configured-compaction-model warning",
		// Not a component key here: the request drawer's own max_tokens fact band, which
		// predates the form and is a different thing from collapse.max_tokens.
		"max_tokens": "the request drawer's sampling-parameter band",
	}
	for name, decls := range all {
		for _, fd := range decls {
			if _, ok := allowed[fd.Key]; ok {
				continue
			}
			for _, quoted := range []string{"'" + fd.Key + "'", `"` + fd.Key + `"`} {
				if strings.Contains(src, quoted) {
					t.Errorf("app.js names the field %s.%s literally (%s); controls come from "+
						"the descriptor, and a hand-written one is a copy of a default, a min "+
						"or an enum list that can drift from the declaration",
						name, fd.Key, quoted)
				}
			}
		}
	}

	// Enum options come from the descriptor, in its order, and the page adds exactly one
	// option of its own: the empty "use the component's default" choice. R8 was a hand-typed
	// list missing a value the engine accepts.
	if !strings.Contains(src, "(fd.options || []).map(") {
		t.Error("the enum control does not render fd.options; a retyped list is how a stored " +
			"`strategy: deterministic` came to be rewritten to `code`")
	}
	for name, decls := range all {
		for _, fd := range decls {
			if fd.Type != components.FieldEnum {
				continue
			}
			// A descriptor whose default is not one of its own options would render an
			// unselectable "— default (x) —", i.e. a control with no way back to unset.
			if len(fd.Options) == 0 {
				t.Errorf("%s.%s is an enum with no options: the control would offer only the "+
					"default choice", name, fd.Key)
			}
			if def, ok := fd.Default.(string); ok && def != "" && !slices.Contains(fd.Options, def) {
				t.Errorf("%s.%s defaults to %q, which is not one of its options %v", name, fd.Key, def, fd.Options)
			}
		}
	}

	// Secrets are WRITE-ONLY, in both directions: never rendered from the server's payload
	// (a credential in the DOM is a credential in every screenshot), and an empty box means
	// "leave the stored one alone" rather than "delete it".
	if !strings.Contains(src, "fd.secret ? 'password' : 'text'") {
		t.Error("a secret field is not rendered as a password input")
	}
	if !strings.Contains(src, "fd.secret || !stated(fd.key) ? '' :") {
		t.Error("the text control does not blank a secret before rendering: a stored " +
			"credential would be echoed into the DOM")
	}
	secrets := 0
	for name, decls := range all {
		for _, fd := range decls {
			if !fd.Secret {
				continue
			}
			secrets++
			if fd.Type != components.FieldString {
				t.Errorf("%s.%s is secret but typed %q; only the string control blanks a secret",
					name, fd.Key, fd.Type)
			}
		}
	}
	if secrets == 0 {
		t.Error("no secret fields declared; the write-only path above is untested")
	}

	// Min is semantics, not validation trivia, and the number input has to carry the right
	// one: 0 on a CAP means unlimited and is a legitimate choice, while 0 on a size threshold
	// is a removed brake the server answers 400 for. Both readings must be in the page,
	// because a control that posts a value earning an unactionable 400 is the failure.
	if !strings.Contains(src, "min: String(min)") || !strings.Contains(src, "fd.min || 0") {
		t.Error("the number control does not carry the declared min")
	}
	if !strings.Contains(src, "0 is allowed here and means unlimited") {
		t.Error("the number control does not distinguish min 0 (a cap, where 0 is a choice)")
	}
	if !strings.Contains(src, "it removes the brake") {
		t.Error("the number control does not refuse a value below a min of 1 (a threshold, " +
			"where 0 is not a setting)")
	}
	caps, floors := 0, 0
	for _, decls := range all {
		for _, fd := range decls {
			if fd.Type != components.FieldInt && fd.Type != components.FieldFloat {
				continue
			}
			if fd.Min == 0 {
				caps++
			} else {
				floors++
			}
		}
	}
	if caps == 0 || floors == 0 {
		t.Errorf("the min 0 / min 1 distinction is not exercised by the registry "+
			"(%d with min 0, %d with a floor)", caps, floors)
	}

	// The document itself is the server's: no YAML editor on the settings page, and no
	// hand-rolled writer behind it.
	for _, gone := range []string{"'#set-yaml'", "writeXllm", "readXllm", "unmanagedXllmLines"} {
		if strings.Contains(src, gone) {
			t.Errorf("%s is back; the browser must not build the configuration document "+
				"(that is what produced \"did not find expected key\" on every save)", gone)
		}
	}
	// And it posts fields, in the shape the server reads: components keyed by name, each a
	// map of DOTTED keys. The old flat `extract_llm:` payload was silently ignored by
	// ApplyForm — no data loss, but every component control on the page was inert on save,
	// which is worse than a missing one.
	if !strings.Contains(src, "body.config = {") || !strings.Contains(src, "components: cfgState") {
		t.Error("saveSettings does not post the descriptor-driven shape " +
			"({pipeline, mode, components: {name: {dotted: value}}})")
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

// TestUINeverShowsABareCost is the UI contract for the cost-accounting pass, and it reflects
// over the Go structs on purpose: a hand-kept list of field names is exactly how the view
// ended up improvising dollars client-side from a hardcoded rate table. Rename a field on the
// server and this test names it; drop it from the view and this test names it.
//
// The complaint it exists to prevent is specific. Users saw what extract_llm COST — a bare
// figure, with no saving and no net anywhere near it — and concluded the product was
// worthless. Every one of these fields is half of a pair that has to appear together.
func TestUINeverShowsABareCost(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js) + string(html)

	// hasTag asserts the struct really carries this json tag. Checking only the JS side
	// would pass forever after a server-side rename; checking only the Go side would pass
	// with the field rendered nowhere.
	hasTag := func(v any, name string) bool {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			if strings.Split(rt.Field(i).Tag.Get("json"), ",")[0] == name {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		v     any
		field string
		why   string
	}{
		{ComponentRow{}, "saved_usd", "the components table would show a bare LLM cost again"},
		{ComponentRow{}, "net_usd", "verdict() would have to improvise a net client-side"},
		{SessionRow{}, "in_flight", "a young session's incomplete amortization would read as a verdict"},
		{SessionRow{}, "tenant_id", "a manager's service-wide list would be unattributable"},
		{Overview{}, "prefix_change_cost_usd", "the largest figure on the corpus would be invisible"},
		// The value pass. Every one of these is the missing half of a pair that already
		// shipped: a component dollar column populated on 6 rows of 100,579, a saving
		// divided by a bill that is mostly output tokens, a freeze shown as a cost with no
		// benefit, a replay shown only as a discount, and an exposure larger than every
		// saving on the page rendered as a folded footnote.
		{ComponentRow{}, "saved_usd_estimated", "the components tab would show $0.00 for all history predating the column"},
		{ComponentRow{}, "net_usd_with_estimate", "the verdict would judge a component's whole spend against six rows of its saving"},
		{ComponentRow{}, "acted_structural", "a component whose job is cache placement would read 0% act rate forever"},
		{Overview{}, "replay_projected_tokens", "the headroom the cache freeze forgoes would have no number"},
		{Overview{}, "prefix_change_cost_all_usd", "the whole exposure of the failure mode would stay below the fold"},
		{Overview{}, "split_credited_moved", "credit paid on a forgotten session would be indistinguishable from a moved snapshot"},
		{SafetyCost{}, "frozen_write_risk_usd", "the freeze would keep being shown as a cost with no counterpart"},
		{TierCosts{}, "addressable_usd", "savings would keep being divided by output tokens nothing can touch"},
	} {
		rt := reflect.TypeOf(c.v)
		if !hasTag(c.v, c.field) {
			t.Errorf("%s no longer has json field %q; the dashboard reads it", rt.Name(), c.field)
		}
		if !strings.Contains(src, c.field) {
			t.Errorf("the dashboard does not render %s.%s — %s", rt.Name(), c.field, c.why)
		}
	}

	// The client-side rate table is gone and must stay gone: 3.75/0.30 per MTok is
	// sonnet-class, this deployment bills opus (4.75/0.38), and the figures it produced were
	// ~27% wrong with no way for a reader to tell. It also valued only what the CALLS removed
	// and so discarded replay, which is ~93% of an extractor's realized value.
	for _, gone := range []string{
		"componentNetUSD", "AGENT_CACHE_WRITE_PER_MTOK", "AGENT_CACHE_READ_PER_MTOK",
		"AGENT_FRESH_PER_MTOK", "savedTokenUSD",
	} {
		if strings.Contains(src, gone) {
			t.Errorf("%s is back on the page; component dollars must come from the server, "+
				"which prices them at the model the request actually paid", gone)
		}
	}
	// verdict() must read the server's net rather than any local arithmetic.
	if !strings.Contains(src, "c.net_usd") {
		t.Error("verdict() does not read c.net_usd")
	}
	// The Saved $ / Net $ columns are static markup, so their absence is a markup fact.
	for _, th := range []string{`<th class="num">Saved $</th>`, `<th class="num">Net $</th>`} {
		if !strings.Contains(string(html), th) {
			t.Errorf("the components table has no %s column header", th)
		}
	}

	// The time range is from/to now, not one duration. A <select id="f-range"> back in the
	// markup means the absolute window and the once-per-refresh clock stamp went with it.
	if strings.Contains(string(html), `<select id="f-range"`) {
		t.Error(`the time range is a <select> again; an absolute "from X to Y" window cannot ` +
			"be expressed as one value, and a relative window resolved per-request produced " +
			"a different window for each panel of one repaint")
	}
	if !strings.Contains(src, "state.nowMs = Date.now()") {
		t.Error("refresh() does not stamp state.nowMs; relative windows would resolve " +
			"separately in every fetch of one repaint")
	}
	// Legacy range= bookmarks must still land somewhere sensible.
	// The quick ranges are a table, and their testids are derived from it, so the table is
	// what a check can assert on.
	for _, tok := range []string{"now-5m", "now-1h", "now-6h", "now-24h", "now-7d", "now-30d"} {
		if !strings.Contains(src, "'"+tok+"'") {
			t.Errorf("QUICK_RANGES no longer offers %q", tok)
		}
	}
	if !strings.Contains(src, "'range-' + (tok || 'all')") {
		t.Error("the quick-range buttons no longer carry a data-testid per range")
	}
	if !strings.Contains(src, "legacyFrom") {
		t.Error("the legacy range=<ms> URL parameter is no longer honoured; existing " +
			"bookmarks would silently widen to all time")
	}

	// Sorting: aria-sort on the <th> is the announced state, and the arrow is drawn from it.
	if !strings.Contains(src, "aria-sort") {
		t.Error("no column publishes aria-sort")
	}
	css, err := uiFS.ReadFile("ui/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), `th[aria-sort="ascending"]`) {
		t.Error("the sort arrow is not driven by aria-sort, so the glyph and the announced " +
			"state can drift apart")
	}
	// Only the unpaginated table is sorted client-side. /api/components returns every row;
	// /api/sessions and /api/requests are LIMIT 25 / LIMIT 50, so sorting those here would
	// order one arbitrary page under a header that reads as global.
	if strings.Contains(src, "sortable('[data-testid=sessions-table]") ||
		strings.Contains(src, "sortable('[data-testid=requests-table]") {
		t.Error("a paginated table was made sortable client-side; that sorts one page and " +
			"presents it as a global ordering. Push ?sort=/?dir= into the SQL first.")
	}

	// Manager scope: one select, and tenant is a full filter dimension with a control.
	if !strings.Contains(src, `id="f-tenant"`) {
		t.Error("there is no manager scope control")
	}
	if !strings.Contains(src, "['tenant', 'tenant', '#f-tenant']") {
		t.Error("tenant is still a control-less filter dimension; the select cannot be synced " +
			"from state.filter and the chip/URL layer would disagree with the bar")
	}
	// The control must be manager-gated by the same attribute every other manager-only
	// element uses, and must NOT be data-local-ok: a single-tenant proxy has nothing to scope.
	for _, line := range strings.Split(string(html), "\n") {
		if !strings.Contains(line, `id="f-tenant"`) {
			continue
		}
		if !strings.Contains(line, "data-manager") {
			t.Errorf("the scope select is not manager-gated: %s", strings.TrimSpace(line))
		}
		if strings.Contains(line, "data-local-ok") {
			t.Error("the scope select is data-local-ok; a single-tenant proxy has one account")
		}
	}
}
