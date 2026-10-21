// Tests for the insights surface: scripts/insights.py and the four skills that read it.
//
// Kept in its own file for the reason preset_default_test.go states — reviewing a 300-line file
// beats reviewing an addition to a 7,000-line one — and because these tests police a DIFFERENT
// class of failure from the rest of the package. Everything else here guards what the plugin WRITES
// to a stranger's settings.json. insights.py writes nothing at all; what it can get wrong is say
// something untrue about somebody's money, which no amount of atomic file handling prevents.
//
// So the tests below fall into three groups, and each exists because the defect it catches is
// invisible in a passing report:
//
//  1. DRIFT. insights.py duplicates config.go's preset pipelines, because the plugin ships as a
//     directory of scripts with no Go in it. A duplicate nobody pins is a second, quietly wrong
//     idea of what a preset does — in the one file whose whole job is telling users what their
//     preset is doing.
//  2. HONESTY. A refusal must not become a number, a short window must not produce a monthly
//     projection, and "we cannot measure this" must not render as "nothing is wrong". Each is a
//     silent failure: the report still looks right.
//  3. READ-ONLINESS. This runs against a user's live configuration. A write, or a keep-alive ping
//     fired from a reporting path, would be spending somebody's money from a command that claims
//     to explain their spending.
package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
)

// runInsights runs the script in `dir` with `env` layered over a minimal environment, and returns
// (stdout, exit code). stderr is folded in: a traceback on stderr with a clean stdout is exactly the
// failure this would otherwise miss.
//
// Through sandboxEnv, not os.Environ(), and that is load-bearing rather than tidiness. insights.py
// resolves its port from settings files — `.claude/settings.local.json`, `.claude/settings.json`,
// then `${CLAUDE_CONFIG_DIR:-$HOME/.claude}/settings.json`. Inheriting the developer's HOME let the
// third candidate be their own real install, so a test asserting the no-configuration behaviour
// read a configured port and passed for the wrong reason on the one machine that runs it most. HOME
// and CLAUDE_CONFIG_DIR are therefore pinned under `dir`, and a test that wants a user-scope file
// writes it into `dir/home/.claude` (see insightsProject) instead of hoping there is not one.
func runInsights(t *testing.T, dir string, env map[string]string, args ...string) (string, int) {
	t.Helper()
	requireTool(t, "python3")
	cmd := exec.Command("python3", append([]string{filepath.Join(scriptsDir(t), "insights.py")}, args...)...)
	cmd.Dir = dir
	cmd.Env = sandboxEnv(t,
		"CONTEXT_GURU_STATE="+filepath.Join(dir, "state"),
		"HOME="+filepath.Join(dir, "home"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(dir, "home", ".claude"))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running insights.py: %v\n%s", err, out)
	}
	return string(out), code
}

// facts parses the key=value wire format into a map. Later keys win, matching how a reader scanning
// the output top to bottom would see it.
func facts(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

// findingIDs returns every finding.N.id in the output, in the order printed — which IS the ranking,
// so a test about ordering can assert on this slice directly.
func findingIDs(out string) []string {
	var ids []string
	for i := 1; ; i++ {
		v, ok := facts(out)[fmt.Sprintf("finding.%d.id", i)]
		if !ok {
			return ids
		}
		ids = append(ids, v)
	}
}

// insightsProject writes an options file naming `port` plus whatever else `options` carries, and
// returns the directory to run in. The file is the real shape Claude Code writes:
// pluginConfigs["<plugin>@<marketplace>"].options.
//
// Passing a nil map writes NO file at all — the "nothing is configured here" case, which is not the
// same as an empty options object and must not be spelled as one. `dir/home/.claude` is created
// either way, because that is the sandbox's user scope: it exists and holds nothing unless a test
// puts something there with insightsOptions.
func insightsProject(t *testing.T, options map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{".claude", filepath.Join("home", ".claude")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if options != nil {
		insightsOptions(t, filepath.Join(dir, ".claude", "settings.json"), options)
	}
	return dir
}

// insightsOptions writes one options file at `path`, for tests that need a SECOND one — a
// user-scope file beside a project file. Since per-project ports, two option files is the normal
// state of a machine with a project install and a machine-wide one, so resolution across them is
// behaviour to pin and not an exotic case.
func insightsOptions(t *testing.T, path string, options map[string]any) {
	t.Helper()
	doc := map[string]any{"pluginConfigs": map[string]any{
		"context-guru@context-guru": map[string]any{"options": options}}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// insightsScopeRecord writes install-scope.json into the sandbox state directory with one row for
// `key` — the record that answers "which install routes this directory" once no option file names a
// port. `port` of 0 writes a row with NO port key, the shape written before ports were per-project.
func insightsScopeRecord(t *testing.T, dir, key, scope, file string, port int) {
	t.Helper()
	row := map[string]any{"scope": scope, "file": file, "recorded_at": "2026-01-01T00:00:00Z"}
	if port != 0 {
		row["port"] = port
	}
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(state, "install-scope.json"), map[string]any{
		"version":  1,
		"projects": map[string]any{key: row},
	})
}

// insightsKey is the identity install-scope.json files `dir` under: realpath, since a temp dir on
// macOS is reached through a symlinked /var and a record keyed by the unresolved path would never
// be found by the script.
func insightsKey(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// apiStub serves the dashboard routes insights.py reads, from a path->body map, and counts every
// request it receives. An absent path 404s, which is what a proxy running without --dashboard does.
//
// `queries` records the RAW query string beside the path, and it is separate from `paths` because
// the two answer different questions. `paths` is the read-onliness guard — which routes were
// touched, with what method. `queries` is the parameter guard: `--sessions N` is only a window if
// `sessions=N` actually reaches the server, and a flag that is parsed and then dropped produces a
// report labelled "the last N sessions" computed over all of them. That failure is invisible in
// the output, so the only place to catch it is the wire.
type apiStub struct {
	port    string
	hits    *int32
	paths   *[]string
	queries *map[string]string
}

// query returns the raw query string the stub saw on `path`, and whether it saw the path at all.
func (s apiStub) query(path string) (string, bool) {
	v, ok := (*s.queries)[path]
	return v, ok
}

func newAPIStub(t *testing.T, bodies map[string]string) apiStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var hits int32
	var paths []string
	queries := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		paths = append(paths, r.Method+" "+r.URL.Path)
		queries[r.URL.Path] = r.URL.RawQuery
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok")) //nolint:errcheck
			return
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body)) //nolint:errcheck
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return apiStub{port: fmt.Sprint(ln.Addr().(*net.TCPAddr).Port), hits: &hits, paths: &paths,
		queries: &queries}
}

// ---------------------------------------------------------------------------
// 1. Drift
// ---------------------------------------------------------------------------

// pythonTable dumps one module-level dict from insights.py as JSON, by importing the module rather
// than parsing it. Regex-parsing a python literal from Go is its own source of false greens: a
// reformatted table would read as empty and this whole test would pass by measuring nothing.
func pythonTable(t *testing.T, expr string) []byte {
	t.Helper()
	requireTool(t, "python3")
	script := "import json,sys; sys.path.insert(0, sys.argv[1]); import insights; " +
		"print(json.dumps(" + expr + "))"
	cmd := exec.Command("python3", "-c", script, scriptsDir(t))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("dumping %s from insights.py: %v", expr, err)
	}
	return out
}

// TestPluginPresetPipelinesAgreeWithConfig is the one test in this file whose absence would be a
// real product defect rather than a missing guard.
//
// insights.py holds its own copy of every preset's component pipeline, because a python script
// cannot call config.PresetPipeline. That copy is what decides which components the report lists as
// OFF — and therefore which switched-off component it recommends enabling. If config.go gains a
// component in `house` and this table does not, the report tells a `house` user that a component
// they are already running is off, and offers to turn it on. Nothing else in this repo would catch
// that: the script runs, the JSON parses, the report reads perfectly.
func TestPluginPresetPipelinesAgreeWithConfig(t *testing.T) {
	var table map[string][]string
	if err := json.Unmarshal(pythonTable(t,
		"{k: list(v) for k, v in insights.PRESET_PIPELINES.items()}"), &table); err != nil {
		t.Fatal(err)
	}
	if len(table) == 0 {
		t.Fatal("insights.PRESET_PIPELINES is empty, so this test proved nothing")
	}
	for name, want := range table {
		got, ok := config.PresetPipeline(name)
		if !ok {
			// `off` is legitimately not in config's table: it is the ABSENCE of a pipeline, and the
			// proxy's own --preset off builds nothing. Every other name must resolve, or the plugin
			// is offering a preset the binary cannot build.
			if name == "off" && len(want) == 0 {
				continue
			}
			t.Errorf("insights.py knows preset %q but config.PresetPipeline does not. Either the "+
				"plugin offers a preset the proxy cannot build, or the name was renamed upstream "+
				"and this table kept the old one", name)
			continue
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("preset %q has drifted.\n  config.go:    %v\n  insights.py:  %v\n"+
				"The report decides which components are OFF from the plugin's copy, so a drift "+
				"here makes it recommend enabling something already running, or stay silent about "+
				"something genuinely off.", name, got, want)
		}
	}
}

// TestEveryPresetOfferedIsOneInsightsCanExplain: settings.py's PRESETS is what a user can actually
// select, and insights.py reports `preset_known=false` for anything outside its own table — which
// means the components section degrades to "what ran" and says nothing about what is off. That is a
// correct degradation and a silent one, so the two lists must not be allowed to diverge quietly.
func TestEveryPresetOfferedIsOneInsightsCanExplain(t *testing.T) {
	var pipelines map[string][]string
	if err := json.Unmarshal(pythonTable(t,
		"{k: list(v) for k, v in insights.PRESET_PIPELINES.items()}"), &pipelines); err != nil {
		t.Fatal(err)
	}
	// Read settings.py's table by IMPORTING it, not by parsing it. The first cut brace-matched the
	// literal and picked keys off lines starting with a quote, which collected the continuation
	// lines of its multi-line descriptions as preset names — three phantom "presets" that made this
	// test fail for a reason that had nothing to do with the property it checks.
	var offered map[string]string
	if err := json.Unmarshal(pythonTable(t, "insights.settings.PRESETS"), &offered); err != nil {
		t.Fatal(err)
	}
	if len(offered) == 0 {
		t.Fatal("settings.PRESETS is empty, so this test proved nothing")
	}
	for name := range offered {
		if _, ok := pipelines[name]; !ok {
			t.Errorf("settings.py offers preset %q but insights.py has no pipeline for it, so "+
				"/context-guru:insights-components would report preset_known=false and say nothing "+
				"about what is switched off for anybody who selected it", name)
		}
	}
}

// TestEveryPipelinedComponentIsDescribed: a component named in a pipeline but missing from
// COMPONENTS is dropped from the off-set listing entirely — it is neither reported as running nor as
// off, which is the worst of the three states because it is the invisible one.
func TestEveryPipelinedComponentIsDescribed(t *testing.T) {
	var pipelines map[string][]string
	if err := json.Unmarshal(pythonTable(t,
		"{k: list(v) for k, v in insights.PRESET_PIPELINES.items()}"), &pipelines); err != nil {
		t.Fatal(err)
	}
	var described map[string]map[string]string
	if err := json.Unmarshal(pythonTable(t, "insights.COMPONENTS"), &described); err != nil {
		t.Fatal(err)
	}
	legal := map[string]bool{"measured": true, "related": true, "none": true}
	for name, meta := range described {
		if !legal[meta["counterfactual"]] {
			t.Errorf("COMPONENTS[%q].counterfactual is %q; the skills branch on exactly "+
				"measured/related/none and an unknown tier renders as no tier at all",
				name, meta["counterfactual"])
		}
		if strings.TrimSpace(meta["what"]) == "" {
			t.Errorf("COMPONENTS[%q] has no `what`, so the report would name it with no "+
				"explanation of what enabling it does", name)
		}
	}
	missing := map[string]bool{}
	for _, pipeline := range pipelines {
		for _, comp := range pipeline {
			if _, ok := described[comp]; !ok {
				missing[comp] = true
			}
		}
	}
	if len(missing) > 0 {
		var names []string
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Errorf("components in a preset pipeline with no COMPONENTS entry: %v. Such a component is "+
			"reported neither as running nor as off — it simply vanishes from the report", names)
	}
}

// ---------------------------------------------------------------------------
// 2. Honesty
// ---------------------------------------------------------------------------

// TestProxyDownIsNotReportedAsDashboardOff. These are two different faults with two different
// fixes, and the first cut of this script conflated them: it decided liveness by JSON-parsing
// /healthz, which answers with the plain text "ok", so a perfectly healthy proxy was reported as
// down — and, worse, a genuinely dead proxy was reported as "running but recording nothing", whose
// fix tells the user to restart with a flag when nothing is running to restart.
func TestProxyDownIsNotReportedAsDashboardOff(t *testing.T) {
	// Nothing listening at all. Port 1 is privileged and unbound, so this cannot accidentally hit
	// a real service on the developer's machine.
	dir := insightsProject(t, map[string]any{"port": 1})
	out, code := runInsights(t, dir, nil, "all")
	if code != 0 {
		t.Fatalf("exit %d, want 0; a diagnostic command must not fail because the thing it "+
			"diagnoses is broken:\n%s", code, out)
	}
	ids := findingIDs(out)
	if !contains(ids, "proxy-down") {
		t.Errorf("no proxy-down finding with nothing listening; got %v", ids)
	}
	if contains(ids, "dashboard-off") {
		t.Errorf("reported dashboard-off with nothing listening. Its fix says to restart with the "+
			"dashboard on, which is the wrong instruction for a proxy that is not running: %v", ids)
	}
	if got := facts(out)["proxy_up"]; got != "false" {
		t.Errorf("proxy_up=%q with nothing listening", got)
	}
}

// TestDashboardOffIsDistinguishedFromAHealthyProxy is the other half, and the regression that
// motivated the split: /healthz answers, every /api/ route 404s.
func TestDashboardOffIsDistinguishedFromAHealthyProxy(t *testing.T) {
	stub := newAPIStub(t, nil) // only /healthz answers
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "all")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if got := facts(out)["proxy_up"]; got != "true" {
		t.Errorf("proxy_up=%q while /healthz answers 200 with the plain text \"ok\". Deciding "+
			"liveness by JSON-parsing that body is the bug this test exists for", got)
	}
	ids := findingIDs(out)
	if !contains(ids, "dashboard-off") {
		t.Errorf("no dashboard-off finding when /api/stats 404s but /healthz answers; got %v", ids)
	}
	if contains(ids, "proxy-down") {
		t.Errorf("reported proxy-down while the proxy answered /healthz: %v", ids)
	}
	if !strings.Contains(out, "unavailable=dashboard") {
		t.Error("no `unavailable=dashboard` line. A section that could not be measured must be " +
			"named as unavailable rather than silently omitted, or an unmeasurable setup reads " +
			"as a clean bill of health")
	}
}

// TestARefusalIsForwardedRatherThanTurnedIntoANumber.
//
// /api/keepalive/recommend deliberately carries no point-estimate field: every account measured has
// a 90% interval whose relative half-width is at least 62% and some cross zero, so the field was
// left off the wire specifically so nobody could render one. A reporting layer that supplied the
// midpoint anyway would defeat that at the last step, which is why this is pinned here and not left
// to the script's own good intentions.
func TestARefusalIsForwardedRatherThanTurnedIntoANumber(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":     window(3),
		"/api/keepalive": `{"pings":0,"net_usd":0,"addressable_misses":4,"addressable_usd":9.5}`,
		"/api/keepalive/recommend": `{"refused":"you have 4 cache expiries large enough to act on",
			"n":4,"sessions":2,"service_lo_usd":95,"service_hi_usd":237}`,
	})
	dir := insightsProject(t, map[string]any{"port": stub.port, "cache_strategy": "none"})
	out, _ := runInsights(t, dir, nil, "idle")

	f := facts(out)
	if f["recommend_refused"] == "" {
		t.Fatalf("the refusal was dropped from the facts:\n%s", out)
	}
	for _, forbidden := range []string{"recommend_lo_usd", "recommend_hi_usd",
		"recommend_idle_seconds", "recommend_max_pings"} {
		if _, ok := f[forbidden]; ok {
			t.Errorf("%s was emitted alongside a refusal (%q). A refused recommendation has no "+
				"interval and no interval to invent one from", forbidden, f[forbidden])
		}
	}
	ids := findingIDs(out)
	if !contains(ids, "keepalive-undecidable") {
		t.Errorf("a refusal produced no keepalive-undecidable finding; got %v", ids)
	}
	if contains(ids, "keepalive-off") {
		t.Errorf("a refusal produced a keepalive-off finding, which carries a dollar range: %v", ids)
	}
	// The service-wide figures may be quoted for scale, but only ever as scale. The finding's own
	// text has to say so, because a $95-$237 range beside a user's own account reads as theirs.
	if !strings.Contains(out, "scale, not your figure") {
		t.Error("the refusal finding quotes the service-wide interval without saying it is scale " +
			"rather than this account's figure")
	}
}

// TestNoMonthlyProjectionOnAShortWindow: scaling four hours of traffic to thirty days produces a
// figure with a dollar sign and no meaning. The window figure is real and must still appear.
func TestNoMonthlyProjectionOnAShortWindow(t *testing.T) {
	for _, c := range []struct {
		name      string
		days      float64
		wantMonth bool
	}{
		{"half a day", 0.5, false},
		{"just under the floor", 1.9, false},
		{"three weeks", 21, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub := newAPIStub(t, map[string]string{
				"/api/stats": window(c.days),
				"/api/tools": toolsWithUnusedShare(),
			})
			dir := insightsProject(t, map[string]any{"port": stub.port})
			out, _ := runInsights(t, dir, nil, "capabilities")
			f := facts(out)
			if f["window_projectable"] != fmt.Sprint(c.wantMonth) {
				t.Fatalf("window_projectable=%q for a %.1f-day window\n%s",
					f["window_projectable"], c.days, out)
			}
			var idx string
			for i := 1; i <= 12; i++ {
				if f[fmt.Sprintf("finding.%d.id", i)] == "declarations-unused-share" {
					idx = fmt.Sprint(i)
				}
			}
			if idx == "" {
				t.Fatalf("no declarations-unused-share finding to check\n%s", out)
			}
			if _, ok := f["finding."+idx+".usd_window"]; !ok {
				t.Error("the WINDOW figure is missing; it is measured and must always appear")
			}
			_, gotMonth := f["finding."+idx+".usd_month"]
			if gotMonth != c.wantMonth {
				t.Errorf("usd_month present=%v, want %v on a %.1f-day window",
					gotMonth, c.wantMonth, c.days)
			}
		})
	}
}

// TestUnpricedIsNotReportedAsFree. A model with no rate on the price list means every dollar figure
// for that traffic is UNKNOWN. Reporting it as $0.00 would say the waste cost nothing, which is a
// different claim and a false one — and it is the claim the report makes by default if anyone ever
// swaps an absent field for a zero.
func TestUnpricedIsNotReportedAsFree(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats": window(30),
		"/api/tools": strings.Replace(toolsWithUnusedShare(),
			`"priced": true`, `"priced": false`, -1),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, _ := runInsights(t, dir, nil, "capabilities")
	f := facts(out)
	if f["decl_priced"] != "false" {
		t.Fatalf("decl_priced=%q, want false\n%s", f["decl_priced"], out)
	}
	var idx string
	for i := 1; i <= 12; i++ {
		if f[fmt.Sprintf("finding.%d.id", i)] == "declarations-unused-share" {
			idx = fmt.Sprint(i)
		}
	}
	if idx == "" {
		t.Fatalf("the unused-share finding disappeared when pricing did. The TOKEN measurement is "+
			"unaffected by a missing rate and is the whole point of reporting it anyway:\n%s", out)
	}
	if _, ok := f["finding."+idx+".usd_window"]; ok {
		t.Error("a dollar figure was emitted for unpriced traffic")
	}
	if b := f["finding."+idx+".basis"]; !strings.Contains(b, "unpriced") {
		t.Errorf("basis=%q; it must say the figure is unpriced so a reader does not take the "+
			"absence of dollars for an absence of cost", b)
	}
}

// TestFindingsAreRankedByTheLowEndOfARange. A wide interval must not outrank a smaller certain
// saving on the strength of its optimistic end — that ordering trains a reader to distrust the
// order, and an ordered list nobody trusts is an unordered one.
func TestFindingsAreRankedByTheLowEndOfARange(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats": window(30),
		// A certain $4/window saving from an unused capability...
		"/api/tools":      toolsWithSuggestableServer(),
		"/api/toolfilter": suggestionWorth(4.0),
		// ...against a keep-alive range of $1-$99. Ranked on $1, so the capability wins.
		"/api/keepalive": `{"pings":0,"net_usd":0,"addressable_misses":40,"addressable_usd":50}`,
		"/api/keepalive/recommend": `{"idle_seconds":280,"max_pings":2,"lo_usd":1,"hi_usd":99,
			"n":40,"sessions":9}`,
	})
	dir := insightsProject(t, map[string]any{"port": stub.port, "cache_strategy": "none"})
	out, _ := runInsights(t, dir, nil, "all")
	ids := findingIDs(out)
	first, second := indexOf(ids, "unused-mcp_tool-mcp__lonely__ping"), indexOf(ids, "keepalive-off")
	if first < 0 || second < 0 {
		t.Fatalf("expected both findings, got %v\n%s", ids, out)
	}
	if first > second {
		t.Errorf("the $1-$99 range outranked a certain $4 saving (order %v). Ranking must use the "+
			"LOW end of an interval, not its midpoint or its top", ids)
	}
}

// TestEveryFindingCarriesEvidenceAFixAndABasis. A finding missing any of the three is not a
// finding: without evidence it is an assertion, without a fix it is an observation that belongs in
// the facts, and without a basis a forecast reads as a bill.
func TestEveryFindingCarriesEvidenceAFixAndABasis(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(30),
		"/api/tools":      toolsWithSuggestableServer(),
		"/api/toolfilter": suggestionWorth(4.0),
		"/api/keepalive":  `{"pings":12,"ping_usd":0.5,"saved_usd":0.1,"net_usd":-0.4,"pings_that_wrote":3}`,
		"/api/keepalive/recommend": `{"idle_seconds":280,"max_pings":2,"lo_usd":1,"hi_usd":9,
			"n":40,"sessions":9}`,
		"/api/components": `{"components":[{"component":"extract_llm","runs":50,"acted_tokens":900,
			"net_usd_with_estimate":-1.25,"saved_usd":0.75}]}`,
	})
	dir := insightsProject(t, map[string]any{"port": stub.port, "cache_strategy": "5-min-ping"})
	out, _ := runInsights(t, dir, nil, "all")
	f := facts(out)
	n := 0
	fmt.Sscanf(f["findings"], "%d", &n)
	if n == 0 {
		t.Fatalf("no findings at all from a populated stub:\n%s", out)
	}
	for i := 1; i <= n; i++ {
		id := f[fmt.Sprintf("finding.%d.id", i)]
		for _, field := range []string{"title", "evidence", "fix", "basis", "severity"} {
			if strings.TrimSpace(f[fmt.Sprintf("finding.%d.%s", i, field)]) == "" {
				t.Errorf("finding %q (#%d) has an empty %s", id, i, field)
			}
		}
	}
}

// TestKeyValueOutputIsNeverSplitByAContentNewline. Every value crosses the wire on one line, and a
// field carrying a newline would silently become two facts, the second with no key — so a skill
// reading `finding.3.fix` would get half of it and never know.
func TestKeyValueOutputIsNeverSplitByAContentNewline(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats": window(30),
		"/api/tools": strings.Replace(toolsWithUnusedShare(), `"state": "ok"`,
			"\"state\": \"ok\",\n \"unknown_sessions\": 0", 1),
		"/api/toolfilter": `{"enabled":false,"reason":"a reason\nwith a newline in it",
			"suggestions":[],"min_sessions":5,"min_days":7,"withheld":3}`,
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, _ := runInsights(t, dir, nil, "capabilities")
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "=") {
			t.Errorf("line %d has no key: %q — a value with an embedded newline broke the "+
				"key=value contract", i+1, line)
		}
	}
	if got := facts(out)["toolfilter_reason"]; !strings.Contains(got, "with a newline in it") {
		t.Errorf("toolfilter_reason=%q; the newline should be folded to a space, not truncate "+
			"the value", got)
	}
}

// ---------------------------------------------------------------------------
// 3. Read-onliness
// ---------------------------------------------------------------------------

// TestInsightsWritesNothingAndPingsNothing.
//
// This command runs against a user's live configuration and a live proxy, and it is offered as the
// safe thing to run when you are worried about spending. Two ways it could betray that: editing a
// settings file (this plugin's other scripts all legitimately do, so the habit is in the codebase),
// or POSTing a keep-alive ping — which is money spent, from a command whose whole purpose is
// explaining money spent.
func TestInsightsWritesNothingAndPingsNothing(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(30),
		"/api/tools":      toolsWithSuggestableServer(),
		"/api/toolfilter": suggestionWorth(2.5),
		"/api/keepalive":  `{"pings":3,"net_usd":0.2,"misses_avoided":2}`,
		"/api/components": `{"components":[{"component":"cachesplit","runs":9}]}`,
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})

	before := snapshotTree(t, dir)
	out, code := runInsights(t, dir, nil, "all")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	after := snapshotTree(t, dir)
	for path, want := range before {
		if got := after[path]; got != want {
			t.Errorf("%s changed. insights.py must never write: a report that edits the "+
				"configuration it is reporting on cannot be run safely by somebody who is "+
				"worried about it", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("insights.py created %s", path)
		}
	}
	for _, p := range *stub.paths {
		if !strings.HasPrefix(p, "GET ") {
			t.Errorf("insights.py made a %s request. Every read here is a GET; anything else is "+
				"a write to somebody's account", p)
		}
		if strings.Contains(p, "/v1/messages") || strings.Contains(p, "/anthropic") {
			t.Errorf("insights.py sent %s — that is a keep-alive ping, billed to the user, from a "+
				"command that reports on their billing", p)
		}
	}
}

// TestInsightsResolvesThePortPerOptionAndNotFromTheEnvironment.
//
// Two defects in one test, because they produce the same wrong answer. CLAUDE_PLUGIN_OPTION_PORT
// does not reach a Bash tool call at all, so a script trusting it reads the default on a machine
// configured for something else and reports an empty account as fact. And the fallback is PER
// OPTION: an options file holding only `preset` is a real file with no port in it, so the port
// falls back alone while the preset does not.
func TestInsightsResolvesThePortPerOptionAndNotFromTheEnvironment(t *testing.T) {
	stub := newAPIStub(t, map[string]string{"/api/stats": window(30)})

	t.Run("an unconfigured port refuses rather than falling back to 8787", func(t *testing.T) {
		dir := insightsProject(t, map[string]any{"preset": "house"})
		out, _ := runInsights(t, dir, nil, "env")
		f := facts(out)
		// 8787 is PORT_SCAN_START, the first port the allocator hands out — so on any machine with
		// an install it is somebody's proxy, and a report that reads it prices another project's
		// traffic under this heading. Measured before the fix: `proxy_up=true` and a full set of
		// preset/upstream/pipeline facts, from a sandbox containing no install whatsoever.
		if f["port"] == "8787" {
			t.Errorf("port=8787 with nothing configured and nothing recorded. That is the bottom "+
				"of the allocation scan, not a default this directory is entitled to:\n%s", out)
		}
		if f["port"] != "(none)" {
			t.Errorf("port=%q, want (none): no option file names a port and no record routes "+
				"this directory", f["port"])
		}
		if !strings.Contains(out, "unavailable=port") {
			t.Errorf("the port is not listed as unavailable, so a reader sees a report with no "+
				"port and no statement that the port is the part that could not be resolved:\n%s", out)
		}
		if ids := findingIDs(out); len(ids) != 1 || ids[0] != "no-install" {
			t.Errorf("findings=%v, want exactly [no-install]. Silence here reads as a healthy "+
				"account with nothing to improve", ids)
		}
		if f["proxy_up"] != "" {
			t.Errorf("proxy_up=%q: with no port resolved there is no proxy this report may speak "+
				"about, and whatever answered is not ours", f["proxy_up"])
		}
		if f["preset"] != "house" || f["preset_configured"] != "true" {
			t.Errorf("preset=%q configured=%q; the option that IS present must still be read "+
				"from the file", f["preset"], f["preset_configured"])
		}
	})

	t.Run("the recorded port is used when no option file names one", func(t *testing.T) {
		// The post-#307 shape of a project whose install wrote its port into a settings file that
		// has since been cleaned up (uninstall removes the option and leaves the file), or whose
		// options live in a scope this directory cannot see. install-scope.json is the record of
		// which install routes this directory and is what the rest of the plugin consults.
		dir := insightsProject(t, map[string]any{"preset": "house"})
		insightsScopeRecord(t, dir, insightsKey(t, dir), "project",
			filepath.Join(dir, ".claude", "settings.json"), 8850)
		f := facts(mustRunInsights(t, dir, "env"))
		if f["port"] != "8850" {
			t.Errorf("port=%q, want 8850 from install-scope.json", f["port"])
		}
		if !strings.Contains(f["port_source"], "install-scope.json") {
			t.Errorf("port_source=%q must name the record, so a reader can tell a recorded port "+
				"from a configured one", f["port_source"])
		}
	})

	t.Run("a record predating per-project ports still means 8787", func(t *testing.T) {
		// Rows written before #307 carry no `port`, because there was one proxy per machine and it
		// was on 8787. Those installs really are there, so this is the one surviving use of the
		// default — and the source has to say WHY, not "(default)".
		dir := insightsProject(t, nil)
		insightsScopeRecord(t, dir, insightsKey(t, dir), "project",
			filepath.Join(dir, ".claude", "settings.json"), 0)
		f := facts(mustRunInsights(t, dir, "env"))
		if f["port"] != "8787" {
			t.Errorf("port=%q, want 8787: a row with no port is a pre-per-project install, which "+
				"is on 8787", f["port"])
		}
		if !strings.Contains(f["port_source"], "per-project") {
			t.Errorf("port_source=%q; a bare \"(default)\" leaves the reader unable to tell this "+
				"from a guess", f["port_source"])
		}
	})

	t.Run("a machine-wide install answers for a directory with no record of its own", func(t *testing.T) {
		// Install at user level, then run this from a project that was never installed: the
		// machine-wide install genuinely routes that directory, and its port is the right answer.
		dir := insightsProject(t, nil)
		user := filepath.Join(dir, "home", ".claude", "settings.json")
		insightsScopeRecord(t, dir, filepath.Join(dir, "home", ".claude"), "user", user, 8851)
		f := facts(mustRunInsights(t, dir, "env"))
		if f["port"] != "8851" {
			t.Errorf("port=%q, want 8851 from the machine-wide row: it routes every project "+
				"without one of its own, which is what this directory is", f["port"])
		}
	})

	t.Run("the environment variable is never trusted over the file", func(t *testing.T) {
		dir := insightsProject(t, map[string]any{"port": stub.port})
		out, _ := runInsights(t, dir,
			map[string]string{"CLAUDE_PLUGIN_OPTION_PORT": "65000"}, "env")
		if got := facts(out)["port"]; got != stub.port {
			t.Errorf("port=%q, want %q from the file. CLAUDE_PLUGIN_OPTION_PORT reaches hook "+
				"environments only; honouring it here would read the wrong proxy and report the "+
				"result as measured", got, stub.port)
		}
	})
}

// mustRunInsights is runInsights with the exit code asserted, for the tests whose subject is the
// content of a successful report rather than how it fails.
func mustRunInsights(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, code := runInsights(t, dir, nil, args...)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	return out
}

// TestInsightsResolvesEveryOptionPerFileAndNotJustThePort.
//
// The port was resolved per option; everything else took the first file with a non-empty options
// object, against its own docstring. With one install per machine those agree. With a project
// install and a machine-wide one — the normal state since #307, and precisely what #308's uninstall
// leaves behind — they do not: a project file holding `preset` hid the `upstream` and `idle_exit`
// configured in ~/.claude/settings.json, and the report printed the defaults for them as facts
// under a header naming the project's file.
func TestInsightsResolvesEveryOptionPerFileAndNotJustThePort(t *testing.T) {
	stub := newAPIStub(t, map[string]string{"/api/stats": window(30)})
	dir := insightsProject(t, map[string]any{"preset": "house"})
	insightsOptions(t, filepath.Join(dir, "home", ".claude", "settings.json"), map[string]any{
		"port":     stub.port,
		"upstream": "https://gateway.example.com",
	})
	out := mustRunInsights(t, dir, "env")
	f := facts(out)
	if f["port"] != stub.port {
		t.Errorf("port=%q, want %q from the user-scope file", f["port"], stub.port)
	}
	if f["preset"] != "house" {
		t.Errorf("preset=%q, want house from the project file: the more specific file wins a key "+
			"it names", f["preset"])
	}
	if !strings.Contains(out, "gateway.example.com") {
		t.Errorf("the upstream configured in the user-scope file is absent, so the report states "+
			"a default for an option that IS configured:\n%s", out)
	}
	// Which file each option came from, because "where is this value set" is the next question a
	// surprising value raises, and with two installs one options_file line cannot answer it.
	if got := f["options_file.preset"]; !strings.HasSuffix(got, filepath.Join(".claude", "settings.json")) ||
		strings.Contains(got, filepath.Join("home", ".claude")) {
		t.Errorf("options_file.preset=%q, want the project file", got)
	}
	if got := f["options_file.upstream"]; !strings.Contains(got, filepath.Join("home", ".claude")) {
		t.Errorf("options_file.upstream=%q, want the user-scope file", got)
	}
}

// TestInsightsAndStartProxyAgreeOnTheFingerprintPath: insights.py reads the fingerprint file
// start-proxy.sh writes, by rebuilding its path from the port. Two spellings of one path in two
// languages with no shared constant is the drift the preset table nearly shipped — and the failure
// is silent, because a fingerprint that cannot be found reads exactly like a proxy that never
// wrote one.
func TestInsightsAndStartProxyAgreeOnTheFingerprintPath(t *testing.T) {
	py := readFileString(t, filepath.Join(scriptsDir(t), "insights.py"))
	sh := readFileString(t, filepath.Join(scriptsDir(t), "start-proxy.sh"))
	if !strings.Contains(py, `f"proxy-{port}.fingerprint"`) {
		t.Error("insights.py no longer builds proxy-<port>.fingerprint; if the name moved, move " +
			"this test with it, and check start-proxy.sh moved too")
	}
	if !strings.Contains(sh, `proxy-${PORT}.fingerprint`) {
		t.Error("start-proxy.sh no longer writes proxy-<port>.fingerprint, so insights.py is " +
			"reading a path nothing writes and will report every proxy as having declared nothing")
	}
}

// TestJSONModeIsTheSameDocument: --json exists so a human can take the raw thing, and it must carry
// every finding the flat form does. A JSON mode that quietly dropped the ranking or the unavailable
// list would be a second, disagreeing answer to the same question.
func TestJSONModeIsTheSameDocument(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(30),
		"/api/tools":      toolsWithSuggestableServer(),
		"/api/toolfilter": suggestionWorth(2.5),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	flat, _ := runInsights(t, dir, nil, "capabilities")
	raw, code := runInsights(t, dir, nil, "capabilities", "--json")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, raw)
	}
	var doc struct {
		Facts       map[string]any `json:"facts"`
		Unavailable []string       `json:"unavailable"`
		Findings    []struct {
			ID    string `json:"id"`
			Basis string `json:"basis"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("--json output does not parse: %v\n%s", err, raw)
	}
	var got []string
	for _, f := range doc.Findings {
		got = append(got, f.ID)
		if f.Basis == "" {
			t.Errorf("finding %q has no basis in --json mode", f.ID)
		}
	}
	if strings.Join(got, ",") != strings.Join(findingIDs(flat), ",") {
		t.Errorf("--json findings %v differ from the flat form's %v, in content or in order",
			got, findingIDs(flat))
	}
}

// ---------------------------------------------------------------------------
// The skills
// ---------------------------------------------------------------------------

// The four insights skills are redirects now. Each tells the user where the command moved and runs
// the new one through cg.py, with nothing else it may run and no option read of its own.
func TestInsightSkillsAreRedirects(t *testing.T) {
	moved := map[string]string{"insights-capabilities": "insights tools", "insights-idle": "insights cache", "insights-components": "insights savings",
		"cost-today": "status", "cost-unused": "insights tools", "cost-cold": "insights cache", "cost-compact": "insights cache",
		"cost-trends": "insights savings", "savings": "insights savings", "cache": "insights cache", "waste": "insights tools"}
	for name, to := range moved {
		t.Run(name, func(t *testing.T) {
			body := readFileString(t, filepath.Join("skills", name, "SKILL.md"))
			if !strings.Contains(body, "moved to `/context-guru:"+to+"`") {
				t.Errorf("does not say it moved to /context-guru:%s", to)
			}
			if !regexp.MustCompile(`allowed-tools: Bash\(python3 "\$\{CLAUDE_PLUGIN_ROOT\}/scripts/cg\.py" [a-z]+:\*\)`).MatchString(body) {
				t.Error("must be allowed to run only its own cg.py command")
			}
			if strings.Contains(body, "CLAUDE_PLUGIN_OPTION_") {
				t.Error("must not resolve plugin options; cg.py takes the proxy from the session")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// window renders an /api/stats body whose since/until span `days`. The window is the denominator of
// every projection in the report, so it is the one field a stub must always get right.
func window(days float64) string {
	until := time.Now().UnixMilli()
	since := until - int64(days*86_400_000)
	return fmt.Sprintf(`{"since":%d,"until":%d,"requests":420,"sessions":9,
		"cost_usd":12.5,"net_saved_usd":1.5}`, since, until)
}

// toolsWithUnusedShare is an /api/tools body with a real unused share and a skills listing, priced.
func toolsWithUnusedShare() string {
	return `{
	  "coverage": {"sessions": 9, "captured": 9, "not_captured": 0, "unpriced_sessions": 0,
	               "requests": 420},
	  "totals": {"declared_tokens": 15600, "unused_tokens": 12300, "unused_pct": 78.7,
	             "unused_reads": 129459, "unused_usd": 3.25, "priced": true,
	             "declared_set_tokens": 43769, "requests_per_session_typical": 65.2},
	  "prompt": {"tokens": 5638, "sessions": 9},
	  "tools": [], "servers": [], "aside": [], "models": [],
	  "skills": {"state": "ok", "declared": 92, "invoked": 1, "listing_tokens": 6639,
	             "unused_listing_usd": 0.42, "skills": []}
	}`
}

// toolsWithSuggestableServer carries one never-invoked MCP tool WITH its runnable removal, which is
// what the suggestion's fix line is built from.
func toolsWithSuggestableServer() string {
	return `{
	  "coverage": {"sessions": 9, "captured": 9, "not_captured": 0, "unpriced_sessions": 0},
	  "totals": {"declared_tokens": 15600, "unused_tokens": 4000, "unused_pct": 25.6,
	             "unused_reads": 40000, "unused_usd": 4.0, "priced": true,
	             "declared_set_tokens": 20000, "requests_per_session_typical": 65.2},
	  "prompt": {"tokens": 5638, "sessions": 9},
	  "tools": [{"kind": "mcp_tool", "name": "mcp__lonely__ping", "server": "lonely",
	             "tokens": 1001, "sessions_declared": 9, "sessions_used": 0, "calls": 0,
	             "unused_reads": 40000, "unused_usd": 4.0, "priced": true,
	             "removal": {"kind": "mcp_server", "command": "claude mcp remove lonely",
	                         "effect": "removes the whole server's schemas from the prompt"}}],
	  "servers": [{"server": "lonely", "tools": 1, "tools_used": 0, "tokens": 1001,
	               "sessions_declared": 9, "sessions_used": 0, "calls": 0,
	               "unused_reads": 40000, "unused_usd": 4.0, "priced": true}],
	  "aside": [], "models": [],
	  "skills": {"state": "absent", "declared": 0, "invoked": 0, "listing_tokens": 0, "skills": []}
	}`
}

// suggestionWorth renders an /api/toolfilter body offering that same tool, qualified, worth `usd`.
func suggestionWorth(usd float64) string {
	return fmt.Sprintf(`{
	  "enabled": false, "min_sessions": 5, "min_days": 7, "withheld": 0, "excluded": [],
	  "coverage": {"sessions": 9, "captured": 9, "not_captured": 0},
	  "suggestions": [{"kind": "mcp_tool", "name": "mcp__lonely__ping", "server": "lonely",
	                   "remove_as": "mcp__lonely__ping", "tokens": 1001, "sessions": 9,
	                   "days": 11.5, "unused_reads": 40000, "projected_usd": %.4f,
	                   "priced": true,
	                   "basis": "declared in 9 sessions over 11.5 days, invoked in none"}]
	}`, usd)
}

// snapshotTree records every file's bytes under root, so a test can prove nothing was written
// rather than only that the one file it thought of was not.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func contains(haystack []string, needle string) bool { return indexOf(haystack, needle) >= 0 }

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

// TestInsightsReportsTheInstallEvenWhenThisSessionIsRoutedElsewhere.
//
// The report question, distinguished from the gate — see TestTheGateAndTheReportAreTwoQuestions in
// plugin_test.go for the other half. This file's consumer must NOT use the gate: a cost report is
// about an install, and it is read from a Bash tool call in sessions that started before the
// install, in projects whose environment points at somebody else's endpoint, and after routing was
// removed while the dashboard DB still holds real history. Refusing there would be the same defect
// as the blind 8787, inverted: no numbers at all where honest ones exist.
func TestInsightsReportsTheInstallEvenWhenThisSessionIsRoutedElsewhere(t *testing.T) {
	dir := insightsProject(t, nil)
	insightsScopeRecord(t, dir, insightsKey(t, dir), "project",
		filepath.Join(dir, ".claude", "settings.json"), 8850)
	out, code := runInsights(t, dir, map[string]string{
		"ANTHROPIC_BASE_URL": "https://api.anthropic.com",
	}, "env")
	f := facts(mustZero(t, out, code))
	if f["port"] != "8850" {
		t.Errorf("port=%q, want 8850: the environment routes to the real API, but the install and "+
			"its dashboard DB are still on 8850 and are what this report is about", f["port"])
	}
}

// TestAPortlessMachineWideRowIsNotHandedToAnUnrelatedProject: the blind 8787 surviving inside the
// rule meant to replace it. A row with no `port` is read as a pre-per-project install on 8787 —
// true of THAT install, and the reason the default still exists — but read off the MACHINE-WIDE row
// it answered for any directory on the machine, including one that never installed anything. So the
// legacy reading is allowed only for the row describing the directory being asked about.
func TestAPortlessMachineWideRowIsNotHandedToAnUnrelatedProject(t *testing.T) {
	dir := insightsProject(t, nil)
	insightsScopeRecord(t, dir, "(user)", "user",
		filepath.Join(dir, "home", ".claude", "settings.json"), 0)
	out, code := runInsights(t, dir, nil, "env")
	f := facts(mustZero(t, out, code))
	if f["port"] == "8787" {
		t.Errorf("port=8787 for a project with no install of its own, from a machine-wide row " +
			"that names no port. That is the bottom of the allocation scan and, on any machine " +
			"with an install, somebody's live proxy — priced as this account's spend")
	}
	if f["port"] != "(none)" {
		t.Errorf("port=%q, want (none)", f["port"])
	}
}

// The same install, the same record, and a session pointing at a FOREIGN local proxy — litellm on
// 4000, a colleague's gateway, anything on loopback we did not write. The gate must still say no:
// that traffic does not reach us. The REPORT must not, and it used to, because the "no install of
// ours claims this loopback port" answer sat above the gate/report split and returned for both. The
// three skills read `result=unrouted` as "nothing here is installed at all", so status and uninstall
// reported nothing installed for a project with a record, a proxy and a dashboard DB.
func TestTheReportStillFindsTheInstallWhenTheSessionPointsAtAForeignLocalProxy(t *testing.T) {
	dir := insightsProject(t, nil)
	insightsScopeRecord(t, dir, insightsKey(t, dir), "project",
		filepath.Join(dir, ".claude", "settings.json"), 8850)
	env := map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000/anthropic"}

	out, code := runInsights(t, dir, env, "env")
	f := facts(mustZero(t, out, code))
	if f["port"] != "8850" {
		t.Errorf("port=%q, want 8850: a foreign proxy in the environment is not evidence that this "+
			"project has no install", f["port"])
	}
}

// ---------------------------------------------------------------------------
// 4. Disclosure: the observation floor qualifies advice, it does not hide inventory
// ---------------------------------------------------------------------------

// toolsBelowFloorWithInventory renders an /api/tools body with three never-invoked items — an MCP
// tool, a plugin skill and a standalone skill — over a window far too short to qualify any of them
// for removal. This is the ordinary state of a fresh install, and it is the state in which the
// report used to go quiet: /api/toolfilter withholds every suggestion, so nothing downstream ever
// named an item.
//
// `aside` carries a built-in, because a report that lists inventory must still refuse to offer the
// one group whose removal breaks the agent.
func toolsBelowFloorWithInventory() string {
	return `{
	  "coverage": {"sessions": 2, "captured": 2, "not_captured": 0, "unpriced_sessions": 0,
	               "requests": 62},
	  "totals": {"declared_tokens": 4100, "unused_tokens": 4100, "unused_pct": 100,
	             "unused_reads": 254200, "unused_usd": 0.19, "priced": true,
	             "declared_set_tokens": 9000, "aside_tokens": 5203,
	             "requests_per_session_typical": 31},
	  "prompt": {"tokens": 1994, "sessions": 2},
	  "tools": [
	    {"kind": "mcp_tool", "name": "mcp__atlas__geocode", "server": "atlas",
	     "tokens": 1200, "sessions_declared": 2, "sessions_used": 0, "calls": 0,
	     "unused_reads": 74400, "unused_usd": 0.0563, "priced": true, "builtin": false,
	     "removal": {"kind": "mcp_tool", "command": "claude mcp remove atlas",
	                 "settings_path": "~/.claude/settings.json",
	                 "effect": "the command removes the whole atlas server"}},
	    {"kind": "tool", "name": "DesignSync", "tokens": 2057, "sessions_declared": 2,
	     "sessions_used": 0, "calls": 0, "unused_reads": 127500, "unused_usd": 0.0967,
	     "priced": true, "builtin": false,
	     "removal": {"kind": "client_tool", "command": "claude --disallowedTools \"DesignSync\"",
	                 "settings_path": "~/.claude/settings.json",
	                 "effect": "removes the tool from the prompt entirely"}}
	  ],
	  "servers": [{"server": "atlas", "tools": 1, "tools_used": 0, "tokens": 1200,
	               "sessions_declared": 2, "sessions_used": 0, "calls": 0,
	               "unused_reads": 74400, "unused_usd": 0.0563, "priced": true}],
	  "aside": [{"kind": "tool", "name": "Artifact", "tokens": 5203, "sessions_declared": 2,
	             "sessions_used": 0, "calls": 0, "unused_reads": 322586, "unused_usd": 0.2445,
	             "priced": true, "builtin": true,
	             "removal": {"kind": "builtin", "danger": true,
	                         "command": "claude --disallowedTools \"Artifact\"",
	                         "note": "this is one of Claude Code's OWN tools"}}],
	  "models": [],
	  "skills": {"state": "ok", "declared": 2, "invoked": 0, "listing_tokens": 480,
	             "unused_listing_reads": 29760, "unused_listing_usd": 0.0226,
	             "skills": [
	    {"kind": "skill", "name": "superpowers:brainstorming", "tokens": 332,
	     "sessions_declared": 2, "sessions_used": 0, "calls": 0, "unused_reads": 20584,
	     "unused_usd": 0.0156, "priced": true, "builtin": false,
	     "removal": {"kind": "plugin_skill", "command": "claude plugin disable superpowers",
	                 "note": "skillOverrides does not work on a plugin's skills"}},
	    {"kind": "skill", "name": "dataviz", "tokens": 148, "sessions_declared": 2,
	     "sessions_used": 0, "calls": 0, "unused_reads": 9176, "unused_usd": 0.0070,
	     "priced": true, "builtin": false,
	     "removal": {"kind": "skill", "settings": "{\"skillOverrides\":{\"dataviz\":\"off\"}}",
	                 "settings_path": "~/.claude/settings.json",
	                 "effect": "hides the skill from the listing"}}
	  ]}
	}`
}

// filterWithholdingEverything is /api/toolfilter refusing to suggest anything, which is what the
// server does below its own floor. `withheld` is the count it is sitting on.
func filterWithholdingEverything(withheld int) string {
	return fmt.Sprintf(`{
	  "enabled": false, "min_sessions": 5, "min_days": 7, "withheld": %d, "excluded": [],
	  "coverage": {"sessions": 2, "captured": 2, "not_captured": 0},
	  "suggestions": []
	}`, withheld)
}

// TestInventoryIsListedEvenWhenEveryRemovalIsWithheld is the disclosure rule, and the reason it is
// a test rather than a style preference: below the floor this command used to report
// `suggestions=0` and name nothing, so a user who asked "what am I carrying and never using" got a
// count and no list. The floor's argument — five sessions in one afternoon are five sessions on one
// task — is sound for ADVICE and says nothing about DISCLOSURE. Withholding a recommendation is
// honest; withholding the inventory is just unhelpful.
//
// So: every never-invoked item must appear by name, with its weight and its cost, while the
// per-item REMOVAL advice stays gated. The two must be separable, and this pins that they are.
func TestInventoryIsListedEvenWhenEveryRemovalIsWithheld(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsBelowFloorWithInventory(),
		"/api/toolfilter": filterWithholdingEverything(5),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	// The floor is still in force: nothing is being recommended.
	if f["suggestions"] != "0" {
		t.Fatalf("suggestions=%q, want 0 — this test is about the state where the server offers "+
			"nothing, and if it offers something the premise is gone", f["suggestions"])
	}

	// Every unused item is named, with the two numbers that make it actionable.
	for _, want := range []struct{ name, tokens, usd string }{
		{"mcp__atlas__geocode", "1200", "0.0563"},
		{"DesignSync", "2057", "0.0967"},
		{"superpowers:brainstorming", "332", "0.0156"},
		{"dataviz", "148", "0.007"},
	} {
		if !strings.Contains(out, want.name) {
			t.Errorf("item %q is never named in the output. The floor withholds the "+
				"RECOMMENDATION; it must not withhold the inventory", want.name)
			continue
		}
		if f["item."+want.name+".tokens"] != want.tokens {
			t.Errorf("item.%s.tokens=%q, want %q", want.name,
				f["item."+want.name+".tokens"], want.tokens)
		}
		if f["item."+want.name+".usd"] != want.usd {
			t.Errorf("item.%s.usd=%q, want %q", want.name,
				f["item."+want.name+".usd"], want.usd)
		}
	}

	// Each item says whether it can be removed at all, and at what scope, because "remove this"
	// and "remove the thing that owns this" are different instructions with different blast radii.
	if got := f["item.dataviz.scope"]; got != "item" {
		t.Errorf("item.dataviz.scope=%q, want \"item\": a standalone skill is removable on its own", got)
	}
	if got := f["item.superpowers:brainstorming.scope"]; got != "plugin" {
		t.Errorf("item.superpowers:brainstorming.scope=%q, want \"plugin\": skillOverrides does "+
			"not work on a plugin's skills, so offering a per-skill removal would be a fix that "+
			"silently does nothing", got)
	}
	if got := f["item.mcp__atlas__geocode.scope"]; got != "server" {
		t.Errorf("item.mcp__atlas__geocode.scope=%q, want \"server\"", got)
	}

	// A built-in appears in the inventory but is never offered: the weight is real and removing it
	// breaks the agent, so it is reported as unavailable rather than as a saving.
	if !strings.Contains(out, "Artifact") {
		t.Error("the built-in Artifact is absent from the inventory; its weight is real and a " +
			"report that hides it understates what the prompt costs")
	}
	if got := f["item.Artifact.scope"]; got != "none" {
		t.Errorf("item.Artifact.scope=%q, want \"none\": a built-in must not be offered for "+
			"removal at any scope", got)
	}

	// The gate itself is still reported, so the reader knows why there is no advice yet.
	if f["suggest_withheld"] != "5" {
		t.Errorf("suggest_withheld=%q, want 5", f["suggest_withheld"])
	}
}

// insightsSkillOnDisk writes a SKILL.md with front matter into a fake plugin cache under `home`,
// which is the layout Claude Code itself installs: <home>/.claude/plugins/cache/<marketplace>/
// <plugin>/<version>/skills/<name>/SKILL.md. Tests write here rather than hoping the developer's
// own machine has the plugin, for the reason runInsights documents about HOME.
func insightsSkillOnDisk(t *testing.T, home, marketplace, plugin, version, name, desc string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "plugins", "cache", marketplace, plugin, version,
		"skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEachItemCarriesWhatItDoesWhereThatIsKnowable pins the "what will I lose" half of the report.
//
// A removal instruction without a description asks somebody to delete a thing whose purpose they
// cannot see, which is how a cost report talks a user into breaking their own setup. The
// description is therefore part of the finding, not decoration.
//
// It also pins the limit, which matters more than the feature: a skill built into Claude Code has
// no SKILL.md anywhere on disk, so its description is NOT KNOWABLE from here. That case must be
// reported as absent rather than filled with a plausible sentence — a description this script
// invented would be indistinguishable from one it read, and the whole surface's claim is that
// every line is traceable to a measurement or a file.
func TestEachItemCarriesWhatItDoesWhereThatIsKnowable(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsBelowFloorWithInventory(),
		"/api/toolfilter": filterWithholdingEverything(5),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	home := filepath.Join(dir, "home")

	// The plugin skill the stub declares, with a description only this file can supply.
	insightsSkillOnDisk(t, home, "claude-plugins-official", "superpowers", "6.4.1",
		"brainstorming", "Explore user intent and requirements before implementation.")

	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	if got := f["item.superpowers:brainstorming.desc"]; got !=
		"Explore user intent and requirements before implementation." {
		t.Errorf("item.superpowers:brainstorming.desc=%q, want the description from its SKILL.md: "+
			"a removal offered without saying what the thing does is asking somebody to delete "+
			"something they cannot evaluate", got)
	}

	// `dataviz` has no file in this sandbox, which is the built-in case. Absent, never invented.
	if got, ok := f["item.dataviz.desc"]; ok {
		t.Errorf("item.dataviz.desc=%q, want no key at all: no SKILL.md exists for it here, so a "+
			"description would be fabricated, and a fabricated one is indistinguishable from a "+
			"read one", got)
	}
	if got := f["item.dataviz.desc_unknown"]; got != "true" {
		t.Errorf("item.dataviz.desc_unknown=%q, want true — the gap must be stated rather than "+
			"left as a blank the reader mistakes for 'does nothing'", got)
	}
}

// TestADescriptionIsNeverSplitAcrossLines is the wire-format half of the same feature, and it is
// separate because the failure is silent. Descriptions are prose written by somebody else: they
// contain newlines, and a multi-line front-matter description is normal YAML. Emitted raw into a
// key=value stream, the second line becomes a fact with no key and every reader downstream sees a
// truncated sentence or a corrupt map.
func TestADescriptionIsNeverSplitAcrossLines(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsBelowFloorWithInventory(),
		"/api/toolfilter": filterWithholdingEverything(0),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	home := filepath.Join(dir, "home")
	skillDir := filepath.Join(home, ".claude", "plugins", "cache", "m", "superpowers", "1",
		"skills", "brainstorming")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A YAML folded description over three lines, plus a stray `=` — both are legal in a
	// description and both break a naive key=value writer.
	body := "---\nname: brainstorming\ndescription: >\n  First line of it.\n" +
		"  Second line a=b continues.\n  Third line ends.\nallowed-tools: Bash\n---\n\n# x\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "=") {
			t.Fatalf("output line %q carries no '='; a description's newline has split the "+
				"key=value stream and every reader after it is parsing prose as facts", line)
		}
	}
	got := facts(out)["item.superpowers:brainstorming.desc"]
	for _, want := range []string{"First line of it.", "Second line a=b continues.", "Third line ends."} {
		if !strings.Contains(got, want) {
			t.Errorf("desc=%q is missing %q: a folded description must be joined, not truncated "+
				"at its first newline", got, want)
		}
	}
}

// TestADescriptionIsTrimmedToSomethingAHumanCanScan exists because of what a skill's `description`
// actually is: a trigger written FOR A MODEL, not a summary written for a person. Measured on one
// real machine, the median is 278 characters and the longest is 1,099 — a field whose tail is
// mostly "use this when the user says X, skip it when Y". Dropped whole into a report, it buries
// the three numbers beside it and the reader stops reading the column.
//
// So the inventory carries a scannable first sentence AND keeps the full text under its own key.
// Both, because they answer different questions: `desc` is "what is this" at a glance, `desc_full`
// is "tell me everything before I delete it". Truncating with no way back to the original would
// make the report the only copy, and a lossy one.
func TestADescriptionIsTrimmedToSomethingAHumanCanScan(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsBelowFloorWithInventory(),
		"/api/toolfilter": filterWithholdingEverything(0),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	long := "Charts, graphs and plots in any medium. " +
		"Use this skill whenever you are about to create ANY chart, graph, plot, dashboard or " +
		"data visualization, in ANY output medium, and read it BEFORE writing the first line of " +
		"chart code or choosing chart colors. Triggers on: chart, graph, plot, data viz, " +
		"visualization, dashboard, analytics, categorical colors, sequential palette, stat tile."
	insightsSkillOnDisk(t, filepath.Join(dir, "home"), "m", "superpowers", "1",
		"brainstorming", long)

	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	short := f["item.superpowers:brainstorming.desc"]
	if short == "" {
		t.Fatal("item.superpowers:brainstorming.desc is empty")
	}
	if len(short) > 200 {
		t.Errorf("desc is %d chars (%q): a trigger phrase written for a model is not a summary "+
			"for a person, and at this length it crowds out the numbers it sits beside",
			len(short), short)
	}
	if !strings.HasPrefix(short, "Charts, graphs and plots in any medium.") {
		t.Errorf("desc=%q, want it to lead with the first sentence rather than a mid-field "+
			"fragment", short)
	}
	// The trigger tail is the part a human does not need and must not be what they read first.
	if strings.Contains(short, "Triggers on:") {
		t.Errorf("desc=%q still carries the model-facing trigger list", short)
	}
	// Nothing is lost: the full text stays available for somebody deciding whether to delete it.
	full := f["item.superpowers:brainstorming.desc_full"]
	if !strings.Contains(full, "Triggers on:") {
		t.Errorf("desc_full=%q does not carry the whole description; truncating with no way back "+
			"would make this report a lossy copy of the only source", full)
	}
}

// TestALongSingleSentenceDescriptionIsStillBounded is the case the first trimming test missed, and
// it was found on real data rather than imagined: three skills on one machine have a description
// that is ONE sentence of 326 to 424 characters. A trimmer that cuts on sentence boundaries emits
// such a sentence whole, so the bound held only for descriptions that happened to have an early
// full stop.
//
// The budget has to hold for any shape of input, because its purpose is the column width in a
// report, and prose does not negotiate with a layout.
func TestALongSingleSentenceDescriptionIsStillBounded(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsBelowFloorWithInventory(),
		"/api/toolfilter": filterWithholdingEverything(0),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	// One sentence, no internal full stop, 300+ chars — the real shape of
	// superpowers:diagnosing-superpowers, whose description runs to 424 characters unbroken.
	oneSentence := "Enable or disable the context-guru status line, a terminal status line showing " +
		"the context-window bar, this session's running savings against its own running cost and " +
		"tokens, the proxy and upstream latency split, and a least-used-tool hint, for whichever " +
		"project happens to be routed through the local proxy right now"
	insightsSkillOnDisk(t, filepath.Join(dir, "home"), "m", "superpowers", "1",
		"brainstorming", oneSentence)

	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)
	short := f["item.superpowers:brainstorming.desc"]
	if len(short) > 210 {
		t.Errorf("desc is %d chars: the budget must hold for a single long sentence too, not only "+
			"for text with an early sentence break\n%q", len(short), short)
	}
	if !strings.HasPrefix(short, "Enable or disable the context-guru status line") {
		t.Errorf("desc=%q, want the leading words kept", short)
	}
	// Truncation must be visible, so a reader knows the sentence did not simply end there.
	if !strings.HasSuffix(short, "…") {
		t.Errorf("desc=%q does not mark that it was cut; an invisible truncation reads as the "+
			"author's own full stop", short)
	}
	if full := f["item.superpowers:brainstorming.desc_full"]; !strings.HasSuffix(full,
		"routed through the local proxy right now") {
		t.Errorf("desc_full=%q must keep the whole sentence", full)
	}
}

// ---------------------------------------------------------------------------
// 5. Hierarchy: the owner is the unit of removal, so it is the unit of reporting
// ---------------------------------------------------------------------------

// toolsWithTwoOwners renders an /api/tools body where one plugin contributes BOTH skills and MCP
// tools, which is the shape the inventory has to roll up and the shape no single machine
// necessarily has. One MCP server is also configured on its own, owned by nothing — the ordinary
// case — so the two kinds of owner are exercised side by side.
func toolsWithTwoOwners() string {
	return `{
	  "coverage": {"sessions": 3, "captured": 3, "not_captured": 0, "unpriced_sessions": 0,
	               "requests": 90},
	  "totals": {"declared_tokens": 3000, "unused_tokens": 3000, "unused_pct": 100,
	             "unused_reads": 180000, "unused_usd": 0.30, "priced": true,
	             "declared_set_tokens": 6000, "requests_per_session_typical": 30},
	  "prompt": {"tokens": 1000, "sessions": 3},
	  "tools": [
	    {"kind": "mcp_tool", "name": "mcp__giveinfo__info", "server": "giveinfo",
	     "tokens": 400, "sessions_declared": 3, "sessions_used": 0, "calls": 0,
	     "unused_reads": 24000, "unused_usd": 0.0400, "priced": true,
	     "removal": {"kind": "mcp_tool", "command": "claude mcp remove giveinfo"}},
	    {"kind": "mcp_tool", "name": "mcp__giveinfo__list", "server": "giveinfo",
	     "tokens": 300, "sessions_declared": 3, "sessions_used": 0, "calls": 0,
	     "unused_reads": 18000, "unused_usd": 0.0300, "priced": true,
	     "removal": {"kind": "mcp_tool", "command": "claude mcp remove giveinfo"}},
	    {"kind": "mcp_tool", "name": "mcp__solo__ping", "server": "solo",
	     "tokens": 100, "sessions_declared": 3, "sessions_used": 0, "calls": 0,
	     "unused_reads": 6000, "unused_usd": 0.0100, "priced": true,
	     "removal": {"kind": "mcp_tool", "command": "claude mcp remove solo"}}
	  ],
	  "servers": [
	    {"server": "giveinfo", "tools": 2, "tools_used": 0, "tokens": 700,
	     "sessions_declared": 3, "sessions_used": 0, "calls": 0, "unused_reads": 42000,
	     "unused_usd": 0.0700, "priced": true},
	    {"server": "solo", "tools": 1, "tools_used": 0, "tokens": 100,
	     "sessions_declared": 3, "sessions_used": 0, "calls": 0, "unused_reads": 6000,
	     "unused_usd": 0.0100, "priced": true}
	  ],
	  "aside": [], "models": [],
	  "skills": {"state": "ok", "declared": 3, "invoked": 0, "listing_tokens": 600,
	             "unused_listing_reads": 36000, "unused_listing_usd": 0.0600,
	             "skills": [
	    {"kind": "skill", "name": "giveinfo:blah-one", "tokens": 200, "sessions_declared": 3,
	     "sessions_used": 0, "calls": 0, "unused_reads": 12000, "unused_usd": 0.0200,
	     "priced": true,
	     "removal": {"kind": "plugin_skill", "command": "claude plugin disable giveinfo"}},
	    {"kind": "skill", "name": "giveinfo:blah-two", "tokens": 150, "sessions_declared": 3,
	     "sessions_used": 0, "calls": 0, "unused_reads": 9000, "unused_usd": 0.0150,
	     "priced": true,
	     "removal": {"kind": "plugin_skill", "command": "claude plugin disable giveinfo"}},
	    {"kind": "skill", "name": "lonesome", "tokens": 50, "sessions_declared": 3,
	     "sessions_used": 0, "calls": 0, "unused_reads": 3000, "unused_usd": 0.0050,
	     "priced": true,
	     "removal": {"kind": "skill", "settings": "{\"skillOverrides\":{\"lonesome\":\"off\"}}",
	                 "settings_path": "~/.claude/settings.json"}}
	  ]}
	}`
}

// TestOwnersRollUpTheirChildrenWithASubtotal pins the shape the report is organised around: the
// thing you remove is an OWNER, so the owner is what carries a figure.
//
// Without a rollup the reader has to add rows themselves to answer the only question that decides
// anything — "what does this plugin cost me in total" — and a reader adding dollar figures across
// rows is a reader who will also add the two figures that must never be summed. The subtotal
// belongs to the report, computed once, beside the command that acts on it.
//
// Both kinds of owner are pinned here because they are not the same: a plugin may contribute
// skills AND MCP tools, while an MCP server owns only tools.
func TestOwnersRollUpTheirChildrenWithASubtotal(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsWithTwoOwners(),
		"/api/toolfilter": filterWithholdingEverything(6),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	// The plugin owns two skills (0.0200 + 0.0150) and nothing else. Its MCP-looking tools are
	// named `mcp__giveinfo__*` but are owned by the SERVER, which is a different owner that
	// happens to share a name — exactly the confusion a rollup has to not make.
	if got := f["owner.plugin.giveinfo.skills"]; got != "2" {
		t.Errorf("owner.plugin.giveinfo.skills=%q, want 2", got)
	}
	if got := f["owner.plugin.giveinfo.usd"]; got != "0.035" {
		t.Errorf("owner.plugin.giveinfo.usd=%q, want 0.035 (0.0200+0.0150)", got)
	}
	if got := f["owner.plugin.giveinfo.tokens"]; got != "350" {
		t.Errorf("owner.plugin.giveinfo.tokens=%q, want 350", got)
	}
	if got := f["owner.plugin.giveinfo.command"]; got != "claude plugin disable giveinfo" {
		t.Errorf("owner.plugin.giveinfo.command=%q, want the plugin command: the subtotal is only "+
			"actionable beside the thing that acts on it", got)
	}

	// The MCP server rolls up its own two tools.
	if got := f["owner.server.giveinfo.tools"]; got != "2" {
		t.Errorf("owner.server.giveinfo.tools=%q, want 2", got)
	}
	if got := f["owner.server.giveinfo.usd"]; got != "0.07" {
		t.Errorf("owner.server.giveinfo.usd=%q, want 0.07 (0.0400+0.0300)", got)
	}
	if got := f["owner.server.solo.tools"]; got != "1" {
		t.Errorf("owner.server.solo.tools=%q, want 1", got)
	}

	// A standalone skill has no owner and must not invent one, or the report grows a phantom
	// plugin whose removal command does not exist.
	for key := range f {
		if strings.HasPrefix(key, "owner.plugin.lonesome") {
			t.Errorf("%s exists; `lonesome` is a standalone skill and belongs to no plugin", key)
		}
	}
	if got := f["item.lonesome.scope"]; got != "item" {
		t.Errorf("item.lonesome.scope=%q, want item", got)
	}

	// Every child still points at its owner, so a reader can get from a row to the thing that
	// removes it without re-deriving the relationship from a name.
	if got := f["item.giveinfo:blah-one.plugin"]; got != "giveinfo" {
		t.Errorf("item.giveinfo:blah-one.plugin=%q, want giveinfo", got)
	}
	if got := f["item.mcp__giveinfo__info.server"]; got != "giveinfo" {
		t.Errorf("item.mcp__giveinfo__info.server=%q, want giveinfo", got)
	}
}

// TestOwnerSubtotalsAreNeverSummedIntoAGrandTotal guards the arithmetic the rest of this surface
// is careful about and a rollup makes newly tempting.
//
// An MCP tool is counted once as an item and again inside its server's subtotal; a plugin's skills
// likewise. Those are the SAME tokens viewed at two levels, so adding every `owner.*.usd` together
// double-counts, and adding owner subtotals to the headline `decl_unused_usd` double-counts
// differently. The report therefore publishes one removable total, computed from items, and the
// test pins that it equals the items rather than the owners.
func TestOwnerSubtotalsAreNeverSummedIntoAGrandTotal(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsWithTwoOwners(),
		"/api/toolfilter": filterWithholdingEverything(6),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	// Items: 0.04 + 0.03 + 0.01 + 0.02 + 0.015 + 0.005 = 0.12
	if got := f["inventory_removable_usd"]; got != "0.12" {
		t.Errorf("inventory_removable_usd=%q, want 0.12 — the sum over ITEMS, each counted once", got)
	}
	// Owners: plugin 0.035 + servers 0.07 + 0.01 = 0.115, which omits the standalone skill and is
	// NOT the same number. If a future change ever makes these equal by construction, the two
	// figures have been conflated and one of them is wrong.
	if f["inventory_removable_usd"] == "0.115" {
		t.Error("the removable total equals the sum of owner subtotals, which means items with no " +
			"owner have been dropped from it")
	}
	// And the figure must carry a basis, like every other money in this report.
	if !strings.Contains(out, "inventory_removable_basis=measured over the window") {
		t.Error("inventory_removable_usd has no basis fact; an unlabelled dollar figure in this " +
			"report is the one thing every other number here is careful not to be")
	}
}

// ---------------------------------------------------------------------------
// 6. Self-exclusion: the report does not bill the reader for the reporter
// ---------------------------------------------------------------------------

// toolsIncludingOurOwnPlugin renders an inventory in which context-guru's own skills are among the
// never-invoked rows, which is the ordinary state of any machine with this plugin installed: most
// sessions never run /context-guru:anything.
func toolsIncludingOurOwnPlugin() string {
	return `{
	  "coverage": {"sessions": 3, "captured": 3, "not_captured": 0, "unpriced_sessions": 0,
	               "requests": 90},
	  "totals": {"declared_tokens": 1000, "unused_tokens": 1000, "unused_pct": 100,
	             "unused_reads": 60000, "unused_usd": 0.10, "priced": true,
	             "declared_set_tokens": 2000, "requests_per_session_typical": 30},
	  "prompt": {"tokens": 500, "sessions": 3},
	  "tools": [], "servers": [], "aside": [], "models": [],
	  "skills": {"state": "ok", "declared": 3, "invoked": 0, "listing_tokens": 300,
	             "unused_listing_reads": 18000, "unused_listing_usd": 0.0300,
	             "skills": [
	    {"kind": "skill", "name": "context-guru:insights", "tokens": 143,
	     "sessions_declared": 3, "sessions_used": 0, "calls": 0, "unused_reads": 8580,
	     "unused_usd": 0.0143, "priced": true,
	     "removal": {"kind": "plugin_skill", "command": "claude plugin disable context-guru"}},
	    {"kind": "skill", "name": "context-guru:install", "tokens": 160,
	     "sessions_declared": 3, "sessions_used": 0, "calls": 0, "unused_reads": 9600,
	     "unused_usd": 0.0160, "priced": true,
	     "removal": {"kind": "plugin_skill", "command": "claude plugin disable context-guru"}},
	    {"kind": "skill", "name": "lonesome", "tokens": 50, "sessions_declared": 3,
	     "sessions_used": 0, "calls": 0, "unused_reads": 3000, "unused_usd": 0.0050,
	     "priced": true,
	     "removal": {"kind": "skill", "settings": "{\"skillOverrides\":{\"lonesome\":\"off\"}}"}}
	  ]}
	}`
}

// TestOurOwnPluginIsNotBilledToTheReader pins self-exclusion, which is a correctness rule and not
// tact: a cost report that counts its own weight as a saving it recommends is arguing for its own
// removal in the middle of its advice, and every total it prints is inflated by the cost of
// printing it.
//
// Excluded from the ranked inventory and from the removable total — and NAMED, with its figure, in
// a fact of its own. Silent omission would leave a reader who sees 44 skills in /plugin and 42 in
// this report with no way to find out why, and "the tool hid its own cost" is a worse look than
// the cost.
func TestOurOwnPluginIsNotBilledToTheReader(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsIncludingOurOwnPlugin(),
		"/api/toolfilter": filterWithholdingEverything(3),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	// Not an item anybody is invited to remove.
	for _, gone := range []string{"item.context-guru:insights.usd", "item.context-guru:install.usd",
		"owner.plugin.context-guru.usd"} {
		if got, ok := f[gone]; ok {
			t.Errorf("%s=%q exists; our own plugin must not appear in the inventory it produces", gone, got)
		}
	}

	// The removable total is the OTHER items only: 0.0050, not 0.0353.
	if got := f["inventory_removable_usd"]; got != "0.005" {
		t.Errorf("inventory_removable_usd=%q, want 0.005 — our own weight must not inflate the "+
			"figure this report recommends acting on", got)
	}
	if got := f["inventory_items"]; got != "1" {
		t.Errorf("inventory_items=%q, want 1", got)
	}

	// And it is disclosed rather than hidden: named, counted, priced, with its own command.
	if got := f["self_excluded_items"]; got != "2" {
		t.Errorf("self_excluded_items=%q, want 2", got)
	}
	if got := f["self_excluded_usd"]; got != "0.0303" {
		t.Errorf("self_excluded_usd=%q, want 0.0303 (0.0143+0.0160): the figure is stated so a "+
			"reader can audit the omission rather than discover it", got)
	}
	if got := f["self_excluded_command"]; got != "claude plugin disable context-guru" {
		t.Errorf("self_excluded_command=%q, want the real command — the exclusion is a reporting "+
			"choice, not a claim that this plugin cannot be removed", got)
	}
}

// ---------------------------------------------------------------------------
// 7. The local report: a template filled on this machine, published nowhere
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// The verdict, the window, and the per-session breakdown
// ---------------------------------------------------------------------------

// toolsQualifiedAndNot is an /api/tools body holding one of each case the verdict has to
// distinguish, so a single run exercises every branch:
//
//   - mcp__atlas__geocode — unused, removable, and the server qualified it     -> remove
//   - DesignSync          — unused, removable, below the server's floor        -> consider
//   - Artifact            — unused but one of Claude Code's own (aside)        -> keep
//   - Grep                — invoked, so it earned its place                    -> keep
//
// Every row carries `sessions[]`, the per-session array Phase A added, because the windowed facts
// and the per-item verdict are read off the same body and a stub that only served one of them
// would let a test pass while the other half was never exercised.
//
// The per-session `unused_reads` sum EXACTLY to the row's own `unused_reads` here, matching what
// the server guarantees on real data. A stub whose parts did not add up would make a test about
// per-session attribution unable to catch an implementation that re-derived the figures.
func toolsQualifiedAndNot() string {
	return `{
	  "coverage": {"sessions": 5, "captured": 5, "not_captured": 0, "unpriced_sessions": 0,
	               "requests": 120},
	  "totals": {"declared_tokens": 9000, "unused_tokens": 4000, "unused_pct": 44.4,
	             "unused_reads": 300000, "unused_usd": 0.50, "priced": true,
	             "declared_set_tokens": 12000, "requests_per_session_typical": 40},
	  "prompt": {"tokens": 2000, "sessions": 5},
	  "tools": [
	    {"kind": "mcp_tool", "name": "mcp__atlas__geocode", "server": "atlas",
	     "tokens": 1200, "sessions_declared": 5, "sessions_used": 0, "calls": 0,
	     "unused_reads": 120000, "unused_usd": 0.2000, "priced": true, "builtin": false,
	     "sessions": [
	       {"session": "aaa", "requests": 80, "model": "aws/claude-opus-5",
	        "unused_reads": 96000, "unused_usd": 0.1600, "priced": true},
	       {"session": "bbb", "requests": 20, "model": "aws/claude-opus-5",
	        "unused_reads": 24000, "unused_usd": 0.0400, "priced": true}],
	     "removal": {"kind": "mcp_tool", "command": "claude mcp remove atlas"}},
	    {"kind": "tool", "name": "DesignSync", "tokens": 2057, "sessions_declared": 2,
	     "sessions_used": 0, "calls": 0, "unused_reads": 205700, "unused_usd": 0.1000,
	     "priced": true, "builtin": false,
	     "sessions": [
	       {"session": "aaa", "requests": 80, "model": "aws/claude-opus-5",
	        "unused_reads": 164560, "unused_usd": 0.0700, "priced": true},
	       {"session": "bbb", "requests": 20, "model": "aws/claude-opus-5",
	        "unused_reads": 41140, "unused_usd": 0.0300, "priced": true}],
	     "removal": {"kind": "client_tool", "command": "claude --disallowedTools \"DesignSync\""}},
	    {"kind": "tool", "name": "Grep", "tokens": 500, "sessions_declared": 5,
	     "sessions_used": 4, "calls": 37, "unused_reads": 0, "unused_usd": 0,
	     "priced": true, "builtin": false,
	     "removal": {"kind": "client_tool", "command": "claude --disallowedTools \"Grep\""}}
	  ],
	  "servers": [{"server": "atlas", "tools": 1, "tools_used": 0, "tokens": 1200,
	               "sessions_declared": 5, "sessions_used": 0, "calls": 0,
	               "unused_reads": 120000, "unused_usd": 0.2000, "priced": true}],
	  "aside": [{"kind": "tool", "name": "Artifact", "tokens": 5203, "sessions_declared": 5,
	             "sessions_used": 0, "calls": 0, "unused_reads": 520300, "unused_usd": 0.4000,
	             "priced": true, "builtin": true,
	             "sessions": [
	               {"session": "aaa", "requests": 80, "model": "aws/claude-opus-5",
	                "unused_reads": 416240, "unused_usd": 0.3200, "priced": true},
	               {"session": "bbb", "requests": 20, "model": "aws/claude-opus-5",
	                "unused_reads": 104060, "unused_usd": 0.0800, "priced": true}],
	             "removal": {"kind": "builtin", "danger": true,
	                         "command": "claude --disallowedTools \"Artifact\"",
	                         "note": "this is one of Claude Code's OWN tools"}}],
	  "models": [],
	  "skills": {"state": "absent", "declared": 0, "invoked": 0, "listing_tokens": 0,
	             "skills": []}
	}`
}

// filterQualifyingAtlasOnly is /api/toolfilter past its floor for exactly ONE of the stub's items.
//
// This is the shape that makes the verdict testable at all. The server's suggestion list IS the
// floor applied — it offers an item only when it was declared across min_sessions sessions spanning
// min_days days — so a body qualifying one item and withholding another puts `remove` and
// `consider` side by side in one run, which is the only way a test can prove the script is reading
// the floor rather than labelling everything the same.
func filterQualifyingAtlasOnly() string {
	return `{
	  "enabled": false, "min_sessions": 5, "min_days": 7, "withheld": 1, "excluded": [],
	  "coverage": {"sessions": 5, "captured": 5, "not_captured": 0},
	  "suggestions": [{"kind": "mcp_tool", "name": "mcp__atlas__geocode", "server": "atlas",
	                   "remove_as": "mcp__atlas__geocode", "tokens": 1200, "sessions": 5,
	                   "days": 9.2, "unused_reads": 120000, "projected_usd": 0.2000,
	                   "priced": true,
	                   "basis": "declared in 5 sessions over 9.2 days, invoked in none"}]
	}`
}

// TestEachItemCarriesAVerdictThatMatchesItsEvidence is the whole point of Phase B, and it is one
// test rather than three because the three verdicts are a partition: a bug that promotes
// `consider` to `remove` is the same bug that demotes nothing, and seeing all four rows in one run
// is what makes it visible.
//
// WHY THE VERDICT IS NOT A STYLE CHOICE. Every row in this inventory already carried a dollar
// figure and a removal command, which reads as an instruction — and for two of the four rows here
// it would be the wrong one. `Artifact` is one of Claude Code's own tools and removing it breaks
// the agent. `DesignSync` is unused across two sessions, which is two sessions on one task and no
// evidence at all about what the next task needs. The verdict is the field that keeps the report
// from making a recommendation its evidence cannot carry, so each value is pinned against the
// specific reason it exists.
func TestEachItemCarriesAVerdictThatMatchesItsEvidence(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(9.2),
		"/api/tools":      toolsQualifiedAndNot(),
		"/api/toolfilter": filterQualifyingAtlasOnly(),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	for _, want := range []struct{ name, verdict, why string }{
		// Past the floor and removable: the one row this report is entitled to recommend.
		{"mcp__atlas__geocode", "remove", "5 session"},
		// Unused, removable, and NOT past the floor. The evidence is real and too thin to act on,
		// and `why` must say which of the two it is rather than leaving the reader to infer it.
		{"DesignSync", "consider", "2 session"},
		// A built-in. Not "we have not seen enough yet" — the question is closed at every scope.
		{"Artifact", "keep", "built-in"},
		// Actually invoked. The other `keep`, and a different sentence: this one earned its place,
		// which is the opposite claim from "we may not remove it".
		{"Grep", "keep", "37"},
	} {
		if got := f["item."+want.name+".verdict"]; got != want.verdict {
			t.Errorf("item.%s.verdict=%q, want %q", want.name, got, want.verdict)
		}
		why := f["item."+want.name+".why"]
		if why == "" {
			t.Errorf("item.%s.why is absent. A verdict with no stated evidence is the thing this "+
				"field exists to prevent: the reader cannot check it and the model narrating it "+
				"has to invent a reason", want.name)
			continue
		}
		if !strings.Contains(why, want.why) {
			t.Errorf("item.%s.why=%q, want it to carry %q — the evidence, not a restatement of "+
				"the verdict", want.name, why, want.why)
		}
		// The bound is not cosmetic. `why` is rendered in a table cell beside three numbers and
		// read aloud by a model summarising the report; a paragraph there buries the figures it
		// is supposed to support.
		if len(why) >= 160 {
			t.Errorf("item.%s.why is %d chars, want under 160: %q", want.name, len(why), why)
		}
	}

	// The two `keep`s must not read the same. One says "you may not remove this", the other says
	// "you would not want to", and a reader who cannot tell them apart learns nothing from either.
	if a, b := f["item.Artifact.why"], f["item.Grep.why"]; a == b {
		t.Errorf("item.Artifact.why and item.Grep.why are both %q. `keep` covers two different "+
			"facts — a built-in that cannot be removed, and something that was actually invoked — "+
			"and collapsing them into one sentence loses the only part a reader can act on", a)
	}
}

// TestOnlyTheRemoveVerdictIsEverAutoSelectable is the contract's auto-select rule, pinned from the
// data side because the page cannot be trusted to re-derive it.
//
// The page ticks `remove` rows by default. If `consider` ever came back as `remove`, the page would
// pre-tick a removal justified by two sessions on one task — which is exactly the failure the
// observation floor exists to prevent, arriving through a different door. So the invariant is
// stated here as a property over every item in the output rather than over the four this stub
// happens to hold: no item may be `remove` unless the server itself qualified it.
func TestOnlyTheRemoveVerdictIsEverAutoSelectable(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsBelowFloorWithInventory(),
		"/api/toolfilter": filterWithholdingEverything(5),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	// Nothing qualified, so nothing may be marked `remove`. On the real machine this test was
	// written against, 56 of 56 unused items were below the floor and `suggestions` was 0 — the
	// state a user is in for their first week, and the state in which a pre-ticked removal would
	// be worst.
	var marked []string
	for key, value := range f {
		if strings.HasSuffix(key, ".verdict") && value == "remove" {
			marked = append(marked, key)
		}
	}
	if len(marked) > 0 {
		sort.Strings(marked)
		t.Errorf("%v are marked `remove` while the server qualified nothing (suggestions=%q). "+
			"Auto-select ticks `remove`, so this is a pre-ticked removal resting on %s of "+
			"observation", marked, f["suggestions"], f["decl_sessions_captured"]+" sessions")
	}
	// And the rows are still there with the weaker verdict, because the floor gates the ADVICE and
	// never the disclosure — the rule TestInventoryIsListedEvenWhenEveryRemovalIsWithheld pins
	// for the listing, restated for the verdict that now sits beside it.
	for _, name := range []string{"mcp__atlas__geocode", "DesignSync", "dataviz"} {
		if got := f["item."+name+".verdict"]; got != "consider" {
			t.Errorf("item.%s.verdict=%q, want \"consider\": below the floor the item is still "+
				"listed and still unused, and the verdict says which of those two it is", name, got)
		}
	}
}

// TestSessionsFlagWindowsTheReportAndIsVisibleOnTheWire.
//
// Two failures in one test because they are indistinguishable in the output. A `--sessions N` that
// parses and then never reaches the server produces a report labelled "the last N sessions" and
// computed over all of them — every number plausible, every number wrong, and nothing in the
// output that says so. And `breakdown=session` is what makes the per-session rows exist at all,
// so sending `sessions` without it yields a window whose per-item detail is silently empty.
//
// The window facts are pinned beside the wire because the label is the other half of the claim:
// `window_session_limit` is what the user ASKED for and `window_sessions_analyzed` is what the
// account actually HAS, and a fresh install where those differ (one session, five requested) must
// show both rather than report the request as though it were the sample.
func TestSessionsFlagWindowsTheReportAndIsVisibleOnTheWire(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(9.2),
		"/api/tools":      toolsQualifiedAndNot(),
		"/api/toolfilter": filterQualifyingAtlasOnly(),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities", "--sessions", "3")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}

	raw, ok := stub.query("/api/tools")
	if !ok {
		t.Fatal("/api/tools was never requested")
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	if got := q.Get("sessions"); got != "3" {
		t.Errorf("/api/tools?%s carries sessions=%q, want 3. A --sessions the server never sees "+
			"is a report labelled as a window and computed over all history", raw, got)
	}
	if got := q.Get("breakdown"); got != "session" {
		t.Errorf("/api/tools?%s carries breakdown=%q, want \"session\". The per-session rows are "+
			"the point of windowing; without this parameter the server omits them entirely and "+
			"every per-session fact below is silently absent", raw, got)
	}

	f := facts(out)
	if got := f["window_session_limit"]; got != "3" {
		t.Errorf("window_session_limit=%q, want 3 — what the user asked for", got)
	}
	// What the account actually has, from the server's own coverage, which is the figure every
	// per-session average divides by.
	if got := f["window_sessions_analyzed"]; got != "5" {
		t.Errorf("window_sessions_analyzed=%q, want 5 (the stub's captured count). Reporting the "+
			"REQUESTED count here would claim a sample the account does not have", got)
	}

	// Unwindowed, the limit must be absent rather than zero: "no window" and "a window of zero
	// sessions" are different claims, and one of them is a report about nothing.
	out2, code2 := runInsights(t, dir, nil, "capabilities")
	if code2 != 0 {
		t.Fatalf("exit %d, want 0\n%s", code2, out2)
	}
	if got, ok := facts(out2)["window_session_limit"]; ok {
		t.Errorf("window_session_limit=%q on an unwindowed run, want no key at all", got)
	}
	if raw2, _ := stub.query("/api/tools"); strings.Contains(raw2, "sessions=") {
		t.Errorf("/api/tools?%s carries a sessions parameter on a run that asked for no window", raw2)
	}
}

// TestPerSessionFactsAttributeTheCostToTheSessionsThatPaidIt.
//
// "This skill cost you $0.20" is a figure nobody can check. "In these two sessions it cost $0.16
// and $0.04, and you never called it" is one a user can audit against their own memory of what
// they were doing — which is the entire argument for the per-session array existing, and it only
// pays off if the numbers are carried through rather than re-derived.
//
// So this pins three things that a plausible-looking implementation gets wrong in three different
// ways: the per-item-per-session figures (carried, not averaged), the per-session rollup across
// items (summed over the items that session carried), and `requests` (the multiplier that turns a
// per-request weight into what it actually cost — a 1,200-token tool in an 80-request session cost
// 96,000 billed tokens, and dropping the multiplication understates it by a factor of eighty).
func TestPerSessionFactsAttributeTheCostToTheSessionsThatPaidIt(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(9.2),
		"/api/tools":      toolsQualifiedAndNot(),
		"/api/toolfilter": filterQualifyingAtlasOnly(),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities", "--sessions", "5")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	// Per item per session, exactly as the server attributed it.
	for _, want := range []struct{ key, value string }{
		{"item.mcp__atlas__geocode.session.aaa.requests", "80"},
		{"item.mcp__atlas__geocode.session.aaa.reads", "96000"},
		{"item.mcp__atlas__geocode.session.aaa.usd", "0.16"},
		{"item.mcp__atlas__geocode.session.bbb.reads", "24000"},
		{"item.mcp__atlas__geocode.session.bbb.usd", "0.04"},
		{"item.DesignSync.session.aaa.reads", "164560"},
	} {
		if got := f[want.key]; got != want.value {
			t.Errorf("%s=%q, want %q", want.key, got, want.value)
		}
	}

	// The per-session rollup: what session `aaa` spent carrying everything it never invoked. Summed
	// over the items, each counted once — 96000 (atlas) + 164560 (DesignSync) + 416240 (Artifact).
	// The built-in is INCLUDED here because this figure is what the session cost, not what could be
	// removed from it; excluding it would understate a real bill, and `savings_window_usd` below is
	// the separate figure that holds the removable part.
	if got := f["session.aaa.unused_reads"]; got != "676800" {
		t.Errorf("session.aaa.unused_reads=%q, want 676800 (96000+164560+416240)", got)
	}
	// The session's own request count, and the reason it is pinned separately from the reads is
	// that it is the one per-session figure that must NOT be summed. Every declaration that session
	// carried was re-read by the same 80 requests — verified on real data, where all five sessions
	// report an identical `requests` across all 79 inventory rows — so adding them over the items
	// would multiply the session's length by the size of its inventory and report 240.
	if got := f["session.aaa.requests"]; got != "80" {
		t.Errorf("session.aaa.requests=%q, want 80 — one number per session, never a sum over "+
			"the items reporting it", got)
	}
	if got := f["session.aaa.items"]; got != "3" {
		t.Errorf("session.aaa.items=%q, want 3: how many never-invoked declarations that session "+
			"carried, which is what makes a per-session dollar figure checkable", got)
	}
	if got := f["session.aaa.model"]; got != "aws/claude-opus-5" {
		t.Errorf("session.aaa.model=%q, want aws/claude-opus-5", got)
	}
	if got := f["session.aaa.priced"]; got != "true" {
		t.Errorf("session.aaa.priced=%q, want true", got)
	}
	if got := f["session.bbb.requests"]; got != "20" {
		t.Errorf("session.bbb.requests=%q, want 20", got)
	}

	// The savings summary, and the one figure here that is NOT the session total: it is the
	// REMOVABLE part, so the built-in's $0.40 is excluded while it is counted in session.*.
	// 0.20 (atlas, scope=server) + 0.10 (DesignSync, scope=item) = 0.30.
	if got := f["savings_window_usd"]; got != "0.3" {
		t.Errorf("savings_window_usd=%q, want 0.3 (atlas 0.20 + DesignSync 0.10; the built-in's "+
			"0.40 is NOT removable and must not be in a savings figure)", got)
	}
	// Over the sessions the account actually has, not the five requested.
	if got := f["savings_per_session_avg_usd"]; got != "0.06" {
		t.Errorf("savings_per_session_avg_usd=%q, want 0.06 (0.30 over 5 analyzed sessions)", got)
	}
	if got := f["savings_basis"]; got != "measured over the window" {
		t.Errorf("savings_basis=%q, want \"measured over the window\" — this is what was spent, "+
			"not a forecast, and the basis is the field that keeps the two apart", got)
	}

	// Nothing per-session is ever SYNTHESISED. The server sends the array only when asked, so the
	// absence of the parameter must leave these facts absent rather than reconstructed from the
	// item total — an attribution this script invented would be indistinguishable from one the
	// proxy measured, and the whole surface's claim is that it is not.
	//
	// Asserted against a stub that serves the array REGARDLESS of the query, which is the strictest
	// available form of the check: a stub that withheld it unasked would make the test pass for
	// an implementation that reads the field and one that could not have, equally.
	bare := newAPIStub(t, map[string]string{
		"/api/stats":      window(9.2),
		"/api/tools":      strings.ReplaceAll(toolsQualifiedAndNot(), `"sessions": [`, `"ignored": [`),
		"/api/toolfilter": filterQualifyingAtlasOnly(),
	})
	dir2 := insightsProject(t, map[string]any{"port": bare.port})
	out2, code2 := runInsights(t, dir2, nil, "capabilities", "--sessions", "5")
	if code2 != 0 {
		t.Fatalf("exit %d, want 0\n%s", code2, out2)
	}
	for _, line := range strings.Split(out2, "\n") {
		if strings.HasPrefix(line, "session.") || strings.Contains(line, ".session.") {
			t.Errorf("a body with no per-session array still produced %q. The attribution was "+
				"not read from the server, so it was invented", line)
		}
	}
	// And the rest of the report still stands: a missing breakdown costs the per-session detail
	// and nothing else, because the aggregate figures come from fields that are always present.
	if got := facts(out2)["item.mcp__atlas__geocode.verdict"]; got != "remove" {
		t.Errorf("item.mcp__atlas__geocode.verdict=%q with no per-session array, want \"remove\": "+
			"the verdict rests on the floor and the call counts, not on the breakdown", got)
	}
}

// TestReportWritesALocalFileAndNothingLeavesTheMachine is the rule the whole report mode exists
// under, and it is pinned first because it is the one that matters: the user's traffic goes through
// a gateway holding their credential, and a reporting command that fetched a stylesheet or posted
// a figure anywhere would be exfiltrating their configuration from inside a tool that claims to be
// read-only.
//
// So: the template ships in the plugin, is filled from data already fetched, is written under the
// state directory, and the script prints the path rather than opening anything. Every asset is
// inline. No http(s) reference of any kind survives in the output.
func TestReportWritesALocalFileAndNothingLeavesTheMachine(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsWithTwoOwners(),
		"/api/toolfilter": filterWithholdingEverything(6),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities", "--report")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}

	path := facts(out)["report_path"]
	if path == "" {
		t.Fatalf("no report_path in output; the script must say where it put the file rather than "+
			"leave the user to find it\n%s", out)
	}
	if !strings.HasPrefix(path, filepath.Join(dir, "state")) {
		t.Errorf("report_path=%q is outside the sandbox state dir %q; a reporting command must "+
			"not write wherever it likes", path, filepath.Join(dir, "state"))
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the report the script said it wrote: %v", err)
	}
	html := string(body)

	// Nothing is FETCHED when the page is opened. A remote font or script would turn opening a
	// local cost report into a network event that names this machine to a third party.
	//
	// The check is on markup that causes a fetch, not on the substring "https://": a URL is
	// ordinary DATA here — the report states the user's own upstream and base_url — and banning
	// the substring would fail on a correct report while still missing, say, a background-image.
	// So: the data island is excised first, then the remaining markup is checked for anything
	// that loads.
	markup := html
	if i := strings.Index(markup, `<script type="application/json"`); i != -1 {
		if j := strings.Index(markup[i:], "</script>"); j != -1 {
			markup = markup[:i] + markup[i+j:]
		}
	}
	for _, forbidden := range []string{
		"http://", "https://", "//", "<script src", "<link ", "@import", "url(",
	} {
		if strings.Contains(markup, forbidden) {
			t.Errorf("the report's markup contains %q outside its data block; every asset must be "+
				"inline so that opening the file reaches no network at all", forbidden)
		}
	}
	// And the user's own configuration, which legitimately contains URLs, must still be in the
	// data block rather than quietly stripped.
	if !strings.Contains(html, `type="application/json"`) {
		t.Error("the report has no JSON data island; values interpolated into markup instead " +
			"would mean splicing somebody else's prose into HTML")
	}
	// It has to be a page, not a fragment, because it is opened straight off disk with no host.
	if !strings.Contains(strings.ToLower(html), "<!doctype html>") {
		t.Error("the report has no doctype; opened from the filesystem it renders in quirks mode")
	}
	// And the data must actually be in it.
	for _, want := range []string{"giveinfo", "mcp__giveinfo__info", "lonesome", "0.12"} {
		if !strings.Contains(html, want) {
			t.Errorf("the report does not mention %q, so it is not a rendering of this inventory", want)
		}
	}
}

// TestReportIsStillReadOnly extends the existing read-onliness guarantee to the new mode. --report
// writes exactly one file, under the state directory, and touches no settings file: a command that
// explains somebody's spending must not alter their configuration while doing it.
func TestReportIsStillReadOnly(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsWithTwoOwners(),
		"/api/toolfilter": filterWithholdingEverything(6),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	// Snapshot everything EXCEPT the state dir, which is where the one new file is allowed.
	before := snapshotTree(t, filepath.Join(dir, ".claude"))
	beforeHome := snapshotTree(t, filepath.Join(dir, "home"))

	out, code := runInsights(t, dir, nil, "all", "--report")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if diff := treeDiff(before, snapshotTree(t, filepath.Join(dir, ".claude"))); diff != "" {
		t.Errorf("--report changed the project's settings: %s", diff)
	}
	if diff := treeDiff(beforeHome, snapshotTree(t, filepath.Join(dir, "home"))); diff != "" {
		t.Errorf("--report changed the user's configuration: %s", diff)
	}
}

// TestReportRefusesRatherThanRenderAnEmptyPage covers the state every other part of this surface
// has a rule for: there is no install, or no traffic, so there is nothing to render. A page of
// zeroes and empty tables reads as a clean bill of health, which is the one misreading this whole
// command is built to prevent, so the mode declines and says why.
func TestReportRefusesRatherThanRenderAnEmptyPage(t *testing.T) {
	dir := insightsProject(t, nil) // no port configured anywhere: no install routes this directory
	out, code := runInsights(t, dir, nil, "capabilities", "--report")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)
	if f["report_path"] != "" {
		t.Errorf("report_path=%q: with no install there is no measurement, and a rendered page of "+
			"blanks would read as 'nothing is wrong'", f["report_path"])
	}
	if f["report_refused"] == "" {
		t.Error("no report_refused fact; the refusal and its reason are the answer here, and " +
			"silence would look like a failure to run")
	}
	// The gating finding is still the real output.
	if !strings.Contains(out, "no-install") {
		t.Error("the no-install finding is missing; --report must not suppress the diagnosis that " +
			"explains why there is no report")
	}
}

// treeDiff names the first difference between two snapshots, so a failure says which file changed
// rather than only that something did.
func treeDiff(before, after map[string]string) string {
	for path, body := range after {
		was, existed := before[path]
		if !existed {
			return fmt.Sprintf("created %s", path)
		}
		if was != body {
			return fmt.Sprintf("modified %s", path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			return fmt.Sprintf("deleted %s", path)
		}
	}
	return ""
}

// TestReportDoesNotSpliceTheFactDocumentIntoMarkup pins a defect the sibling report test could not
// see, and the reason it could not is the lesson: its stub facts contain no URL, so a report that
// splices the whole fact document into the page's markup passes a check for "no http:// outside the
// data island" purely because nothing in the fixture looks like a URL.
//
// The real defect: the template is filled with str.replace, which substitutes EVERY occurrence of
// the placeholder. The template's own header comment named the token to document it, so the fact
// document — which carries the user's base_url and their upstream gateway, from running_fingerprint
// — was written into an HTML comment as well as the island. Measured on a real machine: 134,036
// bytes instead of 91,675, with the upstream URL in markup.
//
// This test therefore asserts on a FACT VALUE that is unmistakably configuration, and asserts the
// placeholder appears exactly once in the template. Either alone would have missed it.
func TestReportDoesNotSpliceTheFactDocumentIntoMarkup(t *testing.T) {
	tpl := readFileString(t, filepath.Join(scriptsDir(t), "insights_report.html"))
	if n := strings.Count(tpl, "__CG_DATA__"); n != 1 {
		t.Fatalf("the template names the placeholder %d times; it is filled with a replace-all, so "+
			"every occurrence beyond the data island receives a copy of the whole fact document", n)
	}

	// A fact whose value is a URL, as the real env block always carries.
	const canary = "https://canary.invalid/upstream"
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsBelowFloorWithInventory(),
		"/api/toolfilter": filterWithholdingEverything(1),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port, "upstream": canary})
	out, code := runInsights(t, dir, nil, "capabilities", "--report")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	path := facts(out)["report_path"]
	if path == "" {
		t.Fatalf("no report_path\n%s", out)
	}
	html := readFileString(t, path)
	if !strings.Contains(html, canary) {
		t.Skip("the configured upstream did not reach the report's facts, so this test cannot " +
			"observe where it was written; the placeholder count above still applies")
	}

	// Excise the data island — the one place a URL-valued fact legitimately belongs — and the
	// canary must be gone from everything that remains.
	markup := html
	if i := strings.Index(markup, `<script type="application/json"`); i != -1 {
		if j := strings.Index(markup[i:], "</script>"); j != -1 {
			markup = markup[:i] + markup[i+j:]
		}
	}
	if strings.Contains(markup, canary) {
		t.Errorf("the configured upstream %q appears in the report's MARKUP, outside the data "+
			"island: the fact document is being spliced somewhere beyond the island it belongs in, "+
			"which puts the user's own configuration into the page's prose", canary)
	}
}

// TestNothingIsSelectableThatTheCommandCannotRemove pins the page's central promise, in the one
// place it can be checked without a browser: the template's own selection rule.
//
// A tick box is a promise that the generated command will act on that thing. A plugin's skill and
// an MCP server's tool have no per-item switch — the unit is the plugin or the server — so a box on
// one of those rows can only ever produce a command that takes its siblings too. An earlier version
// offered them anyway and explained the consequence in a warning afterwards, which is the wrong
// order: it let the user express a choice the page could not honour, then argued with it. Reported
// by a user as "bad UI interface that allows to select skills that can't be deleted".
//
// So the rule is `scope === "item"`, and the inverse holds too: what cannot be removed is not
// offered. The owner's own box is the control for a whole unit.
func TestNothingIsSelectableThatTheCommandCannotRemove(t *testing.T) {
	tpl := readFileString(t, filepath.Join(scriptsDir(t), "insights_report.html"))

	// The guard itself. Pinned as a string because the alternative is executing the page, and the
	// one thing that must not drift is which scopes get a box at all.
	if !strings.Contains(tpl, `function selectable(o){ return o.verdict!=="keep" && o.scope==="item" }`) {
		t.Error("the selection rule is no longer `scope===\"item\"`. Any wider rule hands the " +
			"reader a checkbox for something the generated command cannot act on alone, which is " +
			"the defect this test exists to prevent — widen the OWNER's box instead")
	}
	// The owner's box must select the whole unit, or a plugin becomes unremovable from the page
	// once its children lose their own boxes.
	if !strings.Contains(tpl, `o.scope==="plugin"||o.scope==="server"`) {
		t.Error("the owner checkbox no longer collects its unit-scoped children; with per-item " +
			"boxes gone, that box is the ONLY way to select a plugin or a server, and without it " +
			"they cannot be selected at all")
	}
	// A row with no box says why, and where the real control is. A bare disabled control invites
	// the reader to wonder what they did wrong.
	if !strings.Contains(tpl, "select the plugin above instead") ||
		!strings.Contains(tpl, "select the server above instead") {
		t.Error("a row that cannot be picked individually must say where its real control is")
	}
}

// TestTheReportTellsYouHowToRegenerateItWithACommandThatRuns pins a defect reported from real use:
// the page printed `python3 insights.py capabilities --report`, which resolves from nowhere. This
// script lives in a versioned plugin-cache directory nobody ever cd's into, so a bare filename is
// an instruction that cannot be followed from anywhere the reader is plausibly standing.
//
// The fix is one fact — the script's own absolute path — and the test asserts the fact exists AND
// that the path it names is really there. A path fact that pointed at nothing would pass a
// presence check and still leave the reader with a broken command.
func TestTheReportTellsYouHowToRegenerateItWithACommandThatRuns(t *testing.T) {
	stub := newAPIStub(t, map[string]string{
		"/api/stats":      window(0.4),
		"/api/tools":      toolsBelowFloorWithInventory(),
		"/api/toolfilter": filterWithholdingEverything(1),
	})
	dir := insightsProject(t, map[string]any{"port": stub.port})
	out, code := runInsights(t, dir, nil, "capabilities", "--report")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	f := facts(out)

	script := f["report_script"]
	if script == "" {
		t.Fatal("no report_script fact; without it the page can only print a bare filename, and " +
			"a command the reader cannot run is worse than no command — they conclude the whole " +
			"page is decorative")
	}
	if !filepath.IsAbs(script) {
		t.Errorf("report_script=%q is not absolute; a relative path is exactly the bug", script)
	}
	if _, err := os.Stat(script); err != nil {
		t.Errorf("report_script=%q does not exist: %v. A path fact that names nothing passes a "+
			"presence check and still hands the reader a broken command", script, err)
	}
	// The area is carried too, or regenerating a components report silently switches surface.
	if f["report_area"] != "capabilities" {
		t.Errorf("report_area=%q, want capabilities", f["report_area"])
	}

	// The wrapper the user is told to run must exist beside the script, and be executable.
	wrapper := filepath.Join(filepath.Dir(script), "insights-report.sh")
	info, err := os.Stat(wrapper)
	if err != nil {
		t.Fatalf("insights-report.sh is missing beside the script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("insights-report.sh is not executable (mode %v); the user is told to run it", info.Mode())
	}
}

// TestTheItemTableDoesNotSpendAColumnOnMostlyEmptyProse pins a layout decision that was measured
// rather than guessed: on a real inventory of 58 items, 41 had nothing to put in the "what it does"
// column, because a description exists only for a skill with a readable SKILL.md. A column serving
// 17 rows of 58 still takes width from all 58, and the three number columns are what the page is
// for — so the prose moved to the item name's `title`, where it costs no layout.
//
// Pinned because the pull to re-add a description column is strong (it reads as helpful) and the
// cost of doing so is invisible in a screenshot of a wide window.
func TestTheItemTableDoesNotSpendAColumnOnMostlyEmptyProse(t *testing.T) {
	tpl := readFileString(t, filepath.Join(scriptsDir(t), "insights_report.html"))
	// Asserted on the COLUMN LIST, not on the phrase: the phrase also appears in the comment
	// explaining why the column is gone, and in the footnote telling the reader to hover. A test
	// that cannot tell a column from a sentence about columns fails on its own documentation.
	if !strings.Contains(tpl, `cols.push(["item",""],["recommendation","rec"]);`) {
		t.Error(`the item table's column list has changed. It is deliberately item + ` +
			`recommendation and no prose column: measured on a real inventory a description is ` +
			`empty for 41 of 58 rows, and the column takes width from the number columns on ` +
			`every row — put the text on the name's title attribute instead`)
	}
	// The text has to be somewhere, or removing the column just lost it.
	if !strings.Contains(tpl, `nameCell.setAttribute("title"`) {
		t.Error("nothing attaches a description to the item name, so dropping the column dropped " +
			"the information with it")
	}
	// The full text, not the trimmed summary: a tooltip has no layout to protect.
	if !strings.Contains(tpl, "o.desc_full||o.desc") {
		t.Error("the tooltip does not prefer desc_full; the summary exists to fit a table column, " +
			"and a tooltip has no column to fit")
	}
	// And the rows that have one must be visually distinguishable, or nobody hovers.
	if !strings.Contains(tpl, "has-tip") {
		t.Error("no affordance marks which names carry a tooltip; an invisible tooltip is the " +
			"same as no tooltip")
	}
}
