package dash

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The URL contract is tested in JS, against the real resolver, because a second
// implementation of it in Go would prove that the two agreed and nothing about what a
// pasted link does. See navhash.test.mjs for the table and the reasoning; this is the
// wrapper that makes `go test ./dash/` run it.
//
// node is not a build dependency of this project and never will be — the dashboard ships as
// files in a Go binary with no bundler. So this skips when node is absent, LOUDLY, naming
// what then goes unverified. TestTheNavHashContractIsPinned below is the part that holds
// without node.
func TestNavHashCompatibility(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH, so navhash.test.mjs did not run: the URL " +
			"compatibility contract (17 bare views, 14 filter dimensions, legacy range=<ms>, " +
			"the two hashes dash/kvcache.go writes, and #/group/view) is UNVERIFIED in this " +
			"run. Run `node --test dash/navhash.test.mjs`.")
	}
	out, err := exec.Command(node, "--test", "navhash.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test navhash.test.mjs failed: %v\n%s", err, out)
	}
	// A pass with zero tests run is not a pass.
	if !strings.Contains(string(out), "# fail 0") || strings.Contains(string(out), "# pass 0") {
		t.Fatalf("unexpected node --test summary:\n%s", out)
	}
}

// What the JS table covers, asserted statically so it holds in a run with no node: the
// pieces of the URL contract that are single strings in the source, and would each break a
// documented or server-authored link if they went missing.
func TestTheNavHashContractIsPinned(t *testing.T) {
	app := readUI(t, "ui/app.js")
	for _, want := range []struct{ needle, why string }{
		{"function legacyFrom(", "legacy range=<ms> bookmarks (docs/dashboard.md) map onto a relative window here"},
		{"p.get('range')", "range=<ms> is no longer read, so every pre-from/to link widens to all time"},
		{"function resolveNav(", "the one place a hash path becomes a view"},
		{"function navPath(", "the one place a view becomes a hash path"},
		{`replace(/^#\/?/, '')`, "the leading slash of the canonical #/group/view form is no longer optional"},
		{"'#/' + navPath(", "urlFor no longer writes the canonical two-level hash"},
	} {
		if !strings.Contains(app, want.needle) {
			t.Errorf("app.js no longer contains %q: %s", want.needle, want.why)
		}
	}
	// The 14 filter dimensions, by name. Dropping one silently narrows nothing and widens
	// every link that set it.
	for _, dim := range []string{"q", "model", "provider", "agent", "preset", "mode",
		"component", "reason", "accounting", "effort", "thinking", "stop_reason", "session",
		"tenant"} {
		if !strings.Contains(app, "['"+dim+"',") {
			t.Errorf("filter dimension %q is not in DIMS; links carrying it break", dim)
		}
	}
	// The nav's five groups, and the fact that mountTab is the only thing that knows the
	// nav's DOM shape.
	for _, g := range []string{"overview", "savings", "behaviour", "traffic", "admin"} {
		if !strings.Contains(app, "['"+g+"', [") {
			t.Errorf("nav group %q is gone from GROUPS", g)
		}
	}
	for _, f := range []string{"ui/tools.js", "ui/kvcache.js", "ui/campaigns.js"} {
		src := readUI(t, f)
		if !strings.Contains(src, "mountTab({") {
			t.Errorf("%s does not mount its tab through mountTab()", f)
		}
		if strings.Contains(src, "$('.tabs')") {
			t.Errorf("%s reaches into the nav itself; mountTab() is the one place that knows "+
				"its DOM shape", f)
		}
	}
}

// The two hashes the SERVER writes (dash/kvcache.go:510-511) are one-level, legacy-shaped,
// and unreachable by grepping the front end — the UI is forbidden from building them
// (TestTheDetailTableLinksAreServerBuilt). So the resolver's one-segment branch is not a shim
// to be tidied away later; it is what makes every row of the KV-cache table clickable.
func TestTheServerAuthoredHashesNameViewsTheNavStillHas(t *testing.T) {
	html := readUI(t, "ui/index.html")
	for _, view := range []string{"requests", "sessions"} {
		if !strings.Contains(html, `data-view="`+view+`"`) {
			t.Errorf("dash/kvcache.go writes #%s?..., and there is no longer a %q tab for the "+
				"resolver to find", view, view)
		}
	}
	src, err := os.ReadFile("kvcache.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{`"#requests?req=%d"`, `"#sessions?diff=" + url.PathEscape`} {
		if !strings.Contains(string(src), shape) {
			t.Errorf("kvcache.go no longer writes %s; if it moved, navhash.test.mjs "+
				"section 7 must move with it", shape)
		}
	}
}
