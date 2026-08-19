package offload

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/dsl"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"gopkg.in/yaml.v3"
)

// agentDocs is every extra document, with the sample COMMAND each of its filters is
// keyed on. The command is part of the fixture because these filters are command-keyed
// (`^\$ …`): a routing check that only feeds the output shape, as the builtins' one does,
// would silently pass a filter that can never fire on real traffic.
var agentDocs = map[string]struct {
	doc  string
	cmds map[string]string
}{
	"safe": {agentSafeFilters, map[string]string{
		"django-runtests": "python tests/runtests.py admin_utils -v 2",
		"pytest-warnings": "python -m pytest tests/test_a.py -q",
		"pip-resolver":    "pip download django==6.0",
	}},
	"lossy": {agentLossyFilters, map[string]string{
		"pytest-signal": "python -m pytest tests/test_a.py -q",
	}},
}

func agentDoc(t *testing.T, doc string) dsl.File {
	t.Helper()
	var f dsl.File
	if err := yaml.Unmarshal([]byte(doc), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// Both documents load — which also RUNS every inline test (dsl.Registry.Load) — and the
// modes compose as documented.
func TestAgentFilterModes(t *testing.T) {
	base := &dsl.Registry{}
	if err := base.Load([]byte(builtinFilters)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode string
		want int
	}{{"", 0}, {"off", 0}, {"safe", 3}, {"lossy", 4}} {
		docs, err := agentFilterDocs(tc.mode)
		if err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
		r := &dsl.Registry{}
		if err := r.Load([]byte(builtinFilters)); err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			if err := r.Load([]byte(d)); err != nil {
				t.Fatalf("mode %q: %v", tc.mode, err)
			}
		}
		if got := r.Len() - base.Len(); got != tc.want {
			t.Errorf("mode %q added %d filters, want %d", tc.mode, got, tc.want)
		}
	}
	// An unknown mode must be an error, not a silent "off": a filter set that fails to
	// load is indistinguishable from one that never matches.
	if _, err := agentFilterDocs("aggressive"); err == nil {
		t.Error("an unknown agent_filters mode must fail loudly")
	}
	if _, err := newCmdfilter([]byte("agent_filters: aggressive\n")); err == nil {
		t.Error("cmdfilter must refuse to construct with an unknown agent_filters mode")
	}
}

// Every extra filter carries a test, declares a family, and its test input ROUTES to
// itself through the real selector (command prefix included) rather than to a builtin.
func TestAgentFiltersHaveTestsFamilyAndRoute(t *testing.T) {
	for mode, set := range agentDocs {
		r := &dsl.Registry{}
		if err := r.Load([]byte(builtinFilters)); err != nil {
			t.Fatal(err)
		}
		for _, d := range mustDocs(t, mode) {
			if err := r.Load([]byte(d)); err != nil {
				t.Fatal(err)
			}
		}
		f := agentDoc(t, set.doc)
		for name, def := range f.Filters {
			if def.Family == "" {
				t.Errorf("%s/%s declares no family; its savings would land in \"other\"", mode, name)
			}
			cmd, ok := set.cmds[name]
			if !ok {
				t.Errorf("%s/%s has no sample command in agentDocs; routing is untested", mode, name)
				continue
			}
			cases := f.Tests[name]
			if len(cases) == 0 {
				t.Errorf("%s/%s ships no tests", mode, name)
				continue
			}
			for _, tc := range cases {
				key := matchKey(schema.ToolCall{Name: "Bash", Args: `{"command":` + jsonQuote(cmd) + `}`}, selectorKey(tc.Input))
				got := r.Match(key)
				if got == nil {
					t.Errorf("%s/%s test %q routes to NO filter", mode, name, tc.Name)
				} else if got.Name != name {
					t.Errorf("%s/%s test %q routes to %q instead", mode, name, tc.Name, got.Name)
				}
			}
		}
	}
}

func mustDocs(t *testing.T, mode string) []string {
	t.Helper()
	d, err := agentFilterDocs(mode)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The safe/lossy split is the whole point of two documents, so it is asserted rather
// than trusted: the SAFE set may only remove ENUMERATED line shapes. keep_lines_matching
// is an allow-list of what to keep, i.e. a catch-all drop of everything else — that is
// what "lossy" means here and it must not appear in the safe set. A catch-all in a strip
// list is the same hazard from the other direction.
func TestSafeSetDropsOnlyEnumeratedShapes(t *testing.T) {
	catchAll := regexp.MustCompile(`^\^?\.[*+]\$?$`)
	for name, def := range agentDoc(t, agentSafeFilters).Filters {
		if len(def.KeepLinesMatching) > 0 {
			t.Errorf("safe/%s uses keep_lines_matching: that is a catch-all drop and belongs in the lossy set", name)
		}
		for _, pat := range def.StripLinesMatching {
			if catchAll.MatchString(pat) {
				t.Errorf("safe/%s strips with catch-all %q; strip lists must stay explicit allow-lists", name, pat)
			}
		}
		if def.OnEmpty != nil {
			t.Errorf("safe/%s collapses via on_empty; none of these filters needs a whole-output collapse", name)
		}
	}
	if n := len(agentDoc(t, agentLossyFilters).Filters); n != 1 {
		t.Fatalf("the lossy set holds %d filters; every one of them needs its own justification", n)
	}
	if len(agentDoc(t, agentLossyFilters).Filters["pytest-signal"].KeepLinesMatching) == 0 {
		t.Error("pytest-signal is in the lossy set because it keeps an allow-list; it no longer does")
	}
}

// Determinism: a filter must produce byte-identical output for identical input, across
// registries built independently (Go map iteration over the YAML is randomized, so a
// priority/name ordering bug shows up as a different filter winning on a later run).
func TestAgentFiltersAreDeterministic(t *testing.T) {
	const in = "Testing against Django installed in '/testbed/django'\n" +
		"Creating test database for alias 'default'...\n" +
		"test_a (m.T) ... ok\ntest_b (m.T) ... FAIL\nRan 2 tests in 0.4s\n"
	var first string
	for i := 0; i < 20; i++ {
		r := &dsl.Registry{}
		if err := r.Load([]byte(builtinFilters)); err != nil {
			t.Fatal(err)
		}
		for _, d := range mustDocs(t, "lossy") {
			if err := r.Load([]byte(d)); err != nil {
				t.Fatal(err)
			}
		}
		c := r.Match(matchKey(schema.ToolCall{Name: "Bash", Args: `{"command":"python tests/runtests.py m -v 2"}`}, selectorKey(in)))
		if c == nil {
			t.Fatal("the fixture must route somewhere")
		}
		out, _ := dsl.Apply(c, in)
		if i == 0 {
			first = c.Name + "\x00" + out
			continue
		}
		if c.Name+"\x00"+out != first {
			t.Fatalf("run %d differs: %q vs %q", i, c.Name+"\x00"+out, first)
		}
	}
}

// ROUND TRIP. These filters are lossy by design (cmdfilter is an Offload), so the
// losslessness that must hold is the RECOVERY: the marker's stash reproduces the original
// BYTE FOR BYTE, and the reduced view keeps every failure name and count. Asserted
// end-to-end through the component, not against dsl.Apply, because the stash and the
// never-worse guard are what make the claim true.
func TestAgentFilterRoundTripsThroughTheStash(t *testing.T) {
	in := "Testing against Django installed in '/testbed/django' with up to 16 processes\n" +
		"Importing application admin_utils\n" +
		"Operations to perform:\n  Apply all migrations: admin, sites\n" +
		"Running migrations:\n  Applying admin.0001_initial... OK\n" +
		"    Creating table django_content_type\n    Creating table auth_permission\n" +
		"    Creating table auth_group\n    Creating table auth_user\n" +
		"Creating test database for alias 'default'...\n" +
		"Cloning test database for alias 'default'...\n" +
		"test_cyclic (admin_utils.tests.NestedObjectsTests) ... ok\n" +
		"test_queries (admin_utils.tests.NestedObjectsTests) ... ok\n" +
		"test_broken (admin_utils.tests.NestedObjectsTests) ... FAIL\n" +
		"System check identified no issues (0 silenced).\n" +
		"Ran 3 tests in 0.460s\nFAILED (failures=1)\n" +
		"Destroying test database for alias 'default'...\n"
	if len(in) < defaultMinSize {
		t.Fatalf("fixture is below the size floor (%d bytes)", len(in))
	}
	f := newFilterComp(t, "agent_filters: safe\n")
	req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic, Input: []schemas.ChatMessage{cmdToolMsg(in)}}
	st := store.NewMemory(store.Options{})
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, MaxCachedIdx: -1}
	keys, err := f.Offload(req, &components.Report{}, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected one stash key, got %v", keys)
	}
	got := schema.MessageText(req.Input[0])
	if schema.TextTokens(got) >= schema.TextTokens(in) {
		t.Fatal("the rewrite must be strictly smaller, marker included")
	}
	for _, must := range []string{"test_broken", "FAIL", "Ran 3 tests", "FAILED (failures=1)"} {
		if !strings.Contains(got, must) {
			t.Errorf("the reduced view dropped %q:\n%s", must, got)
		}
	}
	if strings.Contains(got, "Creating table django_content_type") {
		t.Error("setup chatter survived the filter")
	}
	if !strings.Contains(got, expand.Marker(hashKey(in))) {
		t.Errorf("no recovery marker in:\n%s", got)
	}
	raw, ok := st.Get(hashKey(in))
	if !ok {
		t.Fatal("the original was not stashed")
	}
	if string(raw) != in {
		t.Fatal("the stash does not reproduce the original byte for byte")
	}
}

// Off by default: the measured presets (codesmart/codesafe are the published SWE-bench
// arms) must not change because these filters exist. Pinned behaviorally — a fixture the
// extra set reduces and the default set must not touch.
func TestAgentFiltersAreOffByDefault(t *testing.T) {
	in := "Testing against Django installed in '/testbed/django' with up to 16 processes\n" +
		strings.Repeat("test_x (admin_utils.tests.T) ... ok\n", 20) +
		"Ran 20 tests in 0.460s\nOK\nDestroying test database for alias 'default'...\n"
	if out, rep := runFilter(t, newFilterComp(t, ""), in); out != in || !rep.Skipped {
		t.Fatalf("the default config must not touch this output; gates=%v", rep.Gates)
	}
	if out, _ := runFilter(t, newFilterComp(t, "agent_filters: safe\n"), in); out == in {
		t.Fatal("agent_filters: safe must reduce it")
	}
}
