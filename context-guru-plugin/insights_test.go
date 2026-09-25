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
	"os"
	"os/exec"
	"path/filepath"
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
type apiStub struct {
	port  string
	hits  *int32
	paths *[]string
}

func newAPIStub(t *testing.T, bodies map[string]string) apiStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var hits int32
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		paths = append(paths, r.Method+" "+r.URL.Path)
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
	return apiStub{port: fmt.Sprint(ln.Addr().(*net.TCPAddr).Port), hits: &hits, paths: &paths}
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

// TestEveryInsightSkillRunsTheScriptAndClaimsOnlyWhatItCan.
//
// The skills are where a careful script gets undone: the numbers arrive correct and a sentence of
// prose turns a range into a figure. So each one has to (a) actually run the collector rather than
// reimplement a curl, (b) be allowed to run only that, and (c) carry the two rules that the
// collector cannot enforce from its side — quote both ends of an interval, and never present a
// `related`-tier figure as a saving.
func TestEveryInsightSkillRunsTheScriptAndClaimsOnlyWhatItCan(t *testing.T) {
	names := []string{"insights", "insights-capabilities", "insights-idle", "insights-components"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			body := readFileString(t, filepath.Join("skills", name, "SKILL.md"))
			if !strings.Contains(body, "scripts/insights.py") {
				t.Error("does not run scripts/insights.py. A skill that hand-rolls its own curl " +
					"against /api/ computes its own dollar figures in prose, which is the exact " +
					"thing the collector exists to prevent")
			}
			if !strings.Contains(body, "allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/insights.py)") {
				t.Error("does not restrict allowed-tools to the collector. This surface reads a " +
					"live configuration and must not be able to write one")
			}
			// The one instruction that cannot be enforced from the script: it emits lo/hi and has
			// no midpoint field, and prose is the only place a midpoint can appear.
			if strings.Contains(body, "usd_lo") || strings.Contains(body, "recommend_lo_usd") {
				if !strings.Contains(body, "Never average") && !strings.Contains(body, "never average") {
					t.Error("names the interval fields without forbidding an average of them. " +
						"The endpoint has no point-estimate field precisely so none can be " +
						"rendered; prose is the last place that guarantee can be lost")
				}
			}
			// settings.py's per-option fallback prose is required of any skill that reads the
			// options itself (TestEverySkillStatesThePerOptionFallback). These deliberately do not:
			// the collector resolves them. Assert that, so nobody "helpfully" adds a defaulted
			// port to a skill body and reintroduces the defect in prose.
			if strings.Contains(body, "CLAUDE_PLUGIN_OPTION_") {
				t.Error("mentions CLAUDE_PLUGIN_OPTION_; these skills must not resolve options " +
					"at all — insights.py does it, from disk")
			}
		})
	}
}

// TestTheUmbrellaSkillNamesTheNoInstallRefusal: the script can now answer "there is no port to
// report", and that answer is only useful if the skill reading it knows not to substitute one. The
// original defect was not that the script lacked the information — `port_source=(default)` was
// printed all along — but that no finding was raised and no skill named the field, so a model
// narrated a report about somebody else's proxy in the user's own voice.
func TestTheUmbrellaSkillNamesTheNoInstallRefusal(t *testing.T) {
	body := readFileString(t, filepath.Join("skills", "insights", "SKILL.md"))
	for _, want := range []string{"no-install", "port_source", "unavailable=port"} {
		if !strings.Contains(body, want) {
			t.Errorf("skills/insights/SKILL.md never mentions %q, so the one state where this "+
				"command must refuse outright has no instructions attached to it", want)
		}
	}
}

// TestTheUmbrellaSkillDocumentsEveryBasisTheScriptEmits. `basis` is the field that stops a forecast
// being read as a bill, and the umbrella skill translates each value into what the model may say.
// A basis the script emits and the skill does not document is a value that gets narrated by guess.
func TestTheUmbrellaSkillDocumentsEveryBasisTheScriptEmits(t *testing.T) {
	body := readFileString(t, filepath.Join("skills", "insights", "SKILL.md"))
	script := readFileString(t, filepath.Join(scriptsDir(t), "insights.py"))
	for _, basis := range []string{
		"measured over the window",
		"configuration, not a measurement",
		"no measurement yet",
		"refused: insufficient observation",
		"deliberately withheld: insufficient observation",
		"measured size of the problem, NOT a projected saving",
		"90% interval over a window like this one",
	} {
		if !strings.Contains(script, basis) {
			t.Errorf("this test expects insights.py to emit basis %q and it no longer does; "+
				"re-anchor the list", basis)
			continue
		}
		if !strings.Contains(body, basis) {
			t.Errorf("skills/insights/SKILL.md does not document basis %q, so a model reading it "+
				"has to guess what that qualifier permits it to claim", basis)
		}
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
