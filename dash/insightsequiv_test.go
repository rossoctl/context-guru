// The equivalence test: the Go shaping in this package and the Python shaping in
// context-guru-plugin/scripts/insights.py must reach the same conclusions about the same data.
//
// WHY THIS IS THE MOST IMPORTANT TEST IN THE FEATURE. Two surfaces answer "what should I remove":
// the static report, shaped in Python, and the served view, shaped here. They read the same
// endpoints and apply the same rules, and nothing but this test makes them agree. The failure mode
// is not a crash — it is one surface saying "remove this, it cost you $3" while the other says
// "keep it", and a user with no way to tell which is lying.
//
// The duplication exists for a reason that is not going away: the plugin ships as scripts with no
// Go in it, and a browser can neither read SKILL.md off disk nor hold the honesty rules anywhere a
// test can reach them. So the duplication is accepted and PINNED, which is the same bargain
// TestPluginPresetPipelinesAgreeWithConfig strikes for the preset table.
//
// WHAT IS NOT ASSERTED: identical key names. Python emits a flat key space for a line-oriented
// reader (`item.<name>.scope`, `inventory_removable_usd`); Go emits nested JSON for a page. Those
// are deliberate and different presentations of one document. What must agree is every CONCLUSION:
// which items exist, their scope, their verdict, their cost, which owners roll up, and what is
// excluded.
package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pluginScript locates insights.py from this package's directory. Skips rather than fails when it
// is absent: this package must stay testable in a checkout without the plugin, and a skip that
// names what went unverified is honest where a silent pass is not.
func pluginScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "context-guru-plugin", "scripts", "insights.py"))
	if err != nil {
		t.Skipf("cannot resolve the plugin script: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("insights.py is not in this checkout, so the Go and Python shaping rules are "+
			"UNVERIFIED against each other in this run: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH, so the Go and Python shaping rules are UNVERIFIED " +
			"against each other in this run")
	}
	return p
}

// equivStub serves the two endpoints both implementations read, from one body each, so the only
// difference between their outputs can be the rules they apply.
func equivStub(t *testing.T, tools, filter string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok")) //nolint:errcheck
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A window wide enough that no projection is refused for being short.
		w.Write([]byte(`{"since":1,"until":99999999999999,"requests":420,"sessions":9,
			"cost_usd":12.5,"net_saved_usd":1.5}`)) //nolint:errcheck
	})
	for path, body := range map[string]string{"/api/tools": tools, "/api/toolfilter": filter} {
		b := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(b)) //nolint:errcheck
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
}

// pythonFacts runs insights.py against the stub and parses its key=value output.
func pythonFacts(t *testing.T, port string) map[string]string {
	t.Helper()
	script := pluginScript(t)
	dir := t.TempDir()
	for _, sub := range []string{".claude", filepath.Join("home", ".claude")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opts := map[string]any{"pluginConfigs": map[string]any{
		"context-guru@context-guru": map[string]any{"options": map[string]any{"port": port}}}}
	b, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("python3", script, "capabilities")
	cmd.Dir = dir
	// HOME pinned inside the sandbox: inheriting the developer's own would let insights.py read
	// their real plugin cache for descriptions and their real settings for a port, so the test
	// would pass or fail for reasons that have nothing to do with the code.
	cmd.Env = append(os.Environ(),
		"HOME="+filepath.Join(dir, "home"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(dir, "home", ".claude"),
		"CONTEXT_GURU_STATE="+filepath.Join(dir, "state"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("insights.py failed: %v\n%s", err, out)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			facts[k] = v
		}
	}
	return facts
}

// TestShapingAgreesWithThePluginsOwnRules is the pin.
func TestShapingAgreesWithThePluginsOwnRules(t *testing.T) {
	// One inventory with every shape that matters, and a floor that qualifies exactly one item —
	// so the test covers both verdict branches rather than only the one this machine happens to
	// be in. (On real data nothing qualifies, which would leave `remove` untested.)
	const tools = `{
	  "coverage": {"sessions": 9, "captured": 9, "not_captured": 0, "unpriced_sessions": 0,
	               "requests": 420},
	  "totals": {"declared_tokens": 7000, "unused_tokens": 7000, "unused_pct": 100,
	             "unused_reads": 430000, "unused_usd": 0.5, "priced": true,
	             "declared_set_tokens": 12000, "requests_per_session_typical": 46.7},
	  "prompt": {"tokens": 1994, "sessions": 9},
	  "tools": [
	    {"kind": "tool", "name": "DesignSync", "tokens": 2057, "sessions_declared": 9,
	     "sessions_used": 0, "calls": 0, "unused_reads": 127500, "unused_usd": 0.0967,
	     "priced": true,
	     "removal": {"kind": "client_tool", "command": "claude --disallowedTools \"DesignSync\"",
	                 "settings_path": "~/.claude/settings.json"}},
	    {"kind": "mcp_tool", "name": "mcp__atlas__geocode", "server": "atlas", "tokens": 1200,
	     "sessions_declared": 9, "sessions_used": 0, "calls": 0, "unused_reads": 74400,
	     "unused_usd": 0.0563, "priced": true,
	     "removal": {"kind": "mcp_tool", "command": "claude mcp remove atlas"}},
	    {"kind": "tool", "name": "Bash", "tokens": 900, "sessions_declared": 9,
	     "sessions_used": 9, "calls": 79, "unused_reads": 0, "unused_usd": 0, "priced": true,
	     "removal": {"kind": "builtin", "danger": true}}
	  ],
	  "servers": [{"server": "atlas", "tools": 1, "tools_used": 0, "tokens": 1200,
	               "sessions_declared": 9, "sessions_used": 0, "calls": 0,
	               "unused_reads": 74400, "unused_usd": 0.0563, "priced": true}],
	  "aside": [{"kind": "tool", "name": "Artifact", "tokens": 5203, "sessions_declared": 9,
	             "sessions_used": 0, "calls": 0, "unused_reads": 322586, "unused_usd": 0.2445,
	             "priced": true, "builtin": true,
	             "removal": {"kind": "builtin", "danger": true,
	                         "command": "claude --disallowedTools \"Artifact\""}}],
	  "models": [],
	  "skills": {"state": "ok", "declared": 3, "invoked": 0, "listing_tokens": 540,
	             "unused_listing_reads": 33480, "unused_listing_usd": 0.025,
	             "skills": [
	    {"kind": "skill", "name": "superpowers:brainstorming", "tokens": 332,
	     "sessions_declared": 9, "sessions_used": 0, "calls": 0, "unused_reads": 20584,
	     "unused_usd": 0.0156, "priced": true,
	     "removal": {"kind": "plugin_skill", "command": "claude plugin disable superpowers"}},
	    {"kind": "skill", "name": "dataviz", "tokens": 148, "sessions_declared": 9,
	     "sessions_used": 0, "calls": 0, "unused_reads": 9176, "unused_usd": 0.007,
	     "priced": true,
	     "removal": {"kind": "skill",
	                 "settings": "{\"skillOverrides\":{\"dataviz\":\"off\"}}",
	                 "settings_path": "~/.claude/settings.json"}},
	    {"kind": "skill", "name": "context-guru:insights", "tokens": 143,
	     "sessions_declared": 9, "sessions_used": 0, "calls": 0, "unused_reads": 8580,
	     "unused_usd": 0.0143, "priced": true,
	     "removal": {"kind": "plugin_skill",
	                 "command": "claude plugin disable context-guru"}}
	  ]}
	}`
	// dataviz is past the floor; nothing else is.
	const filter = `{
	  "enabled": false, "min_sessions": 5, "min_days": 7, "withheld": 3, "excluded": [],
	  "coverage": {"sessions": 9, "captured": 9, "not_captured": 0},
	  "suggestions": [{"kind": "skill", "name": "dataviz", "remove_as": "skill__dataviz",
	                   "tokens": 148, "sessions": 9, "days": 11.5, "unused_reads": 9176,
	                   "projected_usd": 0.007, "priced": true,
	                   "basis": "declared in 9 sessions over 11.5 days, invoked in none"}]
	}`

	port := equivStub(t, tools, filter)
	py := pythonFacts(t, port)

	// The Go side, shaped from the same bodies. Built by hand rather than through the HTTP layer
	// because the stub is the plugin's view of a proxy, and this package's own handler would need
	// a real store behind it — the rules are what must agree, not the plumbing.
	var rep ToolReport
	if err := json.Unmarshal([]byte(tools), &rep); err != nil {
		t.Fatalf("the fixture does not parse as a ToolReport, so the two sides are not being "+
			"given the same data: %v", err)
	}
	var fdoc ToolFilterDoc
	if err := json.Unmarshal([]byte(filter), &fdoc); err != nil {
		t.Fatal(err)
	}
	qualified := map[string]bool{}
	for _, s := range fdoc.Suggestions {
		qualified[s.Name] = true
	}
	doc := ShapeInsights(&rep, InsightsOptions{Qualified: qualified})

	goItems := map[string]InsightsItem{}
	for _, it := range doc.Items {
		goItems[it.Name] = it
	}

	// 1. THE SAME ITEMS EXIST. A surface that silently lists fewer is the one a user trusts less
	//    for no stated reason.
	pyNames := map[string]bool{}
	for k := range py {
		if rest, ok := strings.CutPrefix(k, "item."); ok {
			if i := strings.LastIndex(rest, "."); i > 0 {
				pyNames[rest[:i]] = true
			}
		}
	}
	for name := range goItems {
		if !pyNames[name] {
			t.Errorf("%q is in the Go document and not in the Python one", name)
		}
	}
	for name := range pyNames {
		if _, ok := goItems[name]; !ok {
			t.Errorf("%q is in the Python document and not in the Go one", name)
		}
	}

	// 2. THE SAME SCOPE, VERDICT AND COST PER ITEM. These are the three fields a reader acts on.
	//
	// An INVOKED item is compared on its verdict alone, deliberately. Neither surface gives one a
	// scope or a cost: every cost field means "spent carrying this for nothing", which is not a
	// figure that exists for a capability that was used, and removal was never on offer. Asserting
	// on absent fields would be asserting that both agree about nothing.
	for name, it := range goItems {
		if it.Invoked {
			if got := py["item."+name+".verdict"]; got != it.Verdict {
				t.Errorf("%s verdict: python %q, go %q", name, got, it.Verdict)
			}
			if py["item."+name+".usd"] != "" || py["item."+name+".scope"] != "" {
				t.Errorf("%s was invoked, yet the Python side gives it a cost or a scope; both "+
					"surfaces must withhold those for something that earned its weight", name)
			}
			if it.Scope != "" || it.USD != 0 {
				t.Errorf("%s was invoked, yet the Go side gives it scope=%q usd=%v", name,
					it.Scope, it.USD)
			}
			continue
		}
		if got := py["item."+name+".scope"]; got != it.Scope {
			t.Errorf("%s scope: python %q, go %q — the two surfaces would offer different "+
				"removal instructions for the same item", name, got, it.Scope)
		}
		if got := py["item."+name+".verdict"]; got != it.Verdict {
			t.Errorf("%s verdict: python %q, go %q — one surface recommends what the other "+
				"does not", name, got, it.Verdict)
		}
		if it.Priced {
			if !sameMoney(py["item."+name+".usd"], it.USD) {
				t.Errorf("%s usd: python %q, go %v", name, py["item."+name+".usd"], it.USD)
			}
		}
		if got := py["item."+name+".tokens"]; got != strconv.Itoa(it.Tokens) {
			t.Errorf("%s tokens: python %q, go %d", name, got, it.Tokens)
		}
	}

	// 3. BOTH BRANCHES OF THE FLOOR WERE ACTUALLY EXERCISED, or the agreement above is vacuous.
	var sawRemove, sawConsider, sawKeep bool
	for _, it := range goItems {
		switch it.Verdict {
		case VerdictRemove:
			sawRemove = true
		case VerdictConsider:
			sawConsider = true
		case VerdictKeep:
			sawKeep = true
		}
	}
	if !sawRemove || !sawConsider || !sawKeep {
		t.Errorf("the fixture exercised remove=%v consider=%v keep=%v; agreement on verdicts "+
			"means nothing unless every branch appears", sawRemove, sawConsider, sawKeep)
	}

	// 4. THE SAME REMOVABLE TOTAL. The figure a reader would act on, and the one place a
	//    double-count or a dropped exclusion shows up as money.
	if !sameMoney(py["inventory_removable_usd"], doc.RemovableUSD) {
		t.Errorf("removable total: python %q, go %v — the two surfaces put a different price on "+
			"the same decision", py["inventory_removable_usd"], doc.RemovableUSD)
	}
	if got := py["inventory_removable_tokens"]; got != strconv.Itoa(doc.RemovableTokens) {
		t.Errorf("removable tokens: python %q, go %d", got, doc.RemovableTokens)
	}

	// 5. THE SAME SELF-EXCLUSION. Both must hold this feature out of its own report, and by the
	//    same amount.
	if got := py["self_excluded_items"]; got != strconv.Itoa(doc.SelfExcluded.Items) {
		t.Errorf("self-excluded items: python %q, go %d", got, doc.SelfExcluded.Items)
	}
	if !sameMoney(py["self_excluded_usd"], doc.SelfExcluded.USD) {
		t.Errorf("self-excluded usd: python %q, go %v", py["self_excluded_usd"], doc.SelfExcluded.USD)
	}

	// 6. THE SAME OWNERS, with the same subtotals and the same command.
	for _, o := range doc.Owners {
		pre := "owner." + o.Kind + "." + o.Name + "."
		if got := py[pre+"usd"]; !sameMoney(got, o.USD) {
			t.Errorf("owner %s/%s usd: python %q, go %v", o.Kind, o.Name, got, o.USD)
		}
		if got := py[pre+"command"]; got != o.Command {
			t.Errorf("owner %s/%s command: python %q, go %q", o.Kind, o.Name, got, o.Command)
		}
	}
}

// sameMoney compares a Python-formatted dollar figure with a Go float. Both round to four decimal
// places for display, so agreement to that precision is agreement — exact float equality across
// two languages' formatting is a test that fails on arithmetic that is correct.
func sameMoney(pyVal string, goVal float64) bool {
	if pyVal == "" {
		return goVal == 0
	}
	p, err := strconv.ParseFloat(pyVal, 64)
	if err != nil {
		return false
	}
	d := p - goVal
	if d < 0 {
		d = -d
	}
	return d < 5e-5
}

// TestEveryVerdictGoEmitsIsOneThePluginAlsoKnows catches the drift the main equivalence test
// structurally cannot: a verdict Go can produce and Python has never heard of.
//
// That test compares the two over one fixture, so it only sees states the fixture reaches. When Go
// gained `stale` — a capability used once and abandoned, which `calls > 0` previously filed under
// `keep` — the fixture had no use timestamps, so `stale` was unreachable and the comparison stayed
// green while the two surfaces had genuinely diverged. The same would be true of any verdict added
// later.
//
// So this asserts on the VOCABULARY rather than on one run: every verdict constant here must
// appear in insights.py. It is the cheap half of the pin, and it is the half that fails the moment
// a new state is added on one side only.
func TestEveryVerdictGoEmitsIsOneThePluginAlsoKnows(t *testing.T) {
	script := pluginScript(t)
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading insights.py: %v", err)
	}
	py := string(b)
	for _, v := range []string{VerdictRemove, VerdictConsider, VerdictKeep, VerdictStale} {
		if !strings.Contains(py, `"`+v+`"`) && !strings.Contains(py, `'`+v+`'`) {
			t.Errorf("Go can emit verdict %q and insights.py never mentions it. The two surfaces "+
				"answer the same question, so one that cannot produce a state the other can is a "+
				"user seeing different advice depending on which they opened — add it there, or "+
				"stop emitting it here", v)
		}
	}
}
