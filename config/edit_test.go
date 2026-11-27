package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden files were written by the proxy editors before they moved here (#1304). The
// proxy test (proxy/configedit_golden_test.go) reads the same files through the host.

const editDoc = `# account config
pipeline: [format, dedup, cacheinject, extract]  # the order matters
components:
  # keep the big ones
  extract:
    min_tokens: 400
  toolfilter:
    defer: auto
    keep: [WebFetch]   # never defer
    remove: [mcp__ops]
cache:
  keepalive: true
`

var editDocs = map[string]string{
	"full":     editDoc,
	"empty":    "",
	"minimal":  "pipeline: [format]\n",
	"nopipe":   "components:\n  toolfilter:\n    remove: [a, b]\n",
	"llm":      "pipeline: [cacheinject, format, extract_llm]\n",
	"badyaml":  "a: [",
	"always":   "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n    always_defer: [mcp__ops]\n",
	"noremove": "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    defer: off\n",
}

func editGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "editgolden", name+".golden")
	if os.Getenv("UPDATE_EDIT_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run with UPDATE_EDIT_GOLDEN=1)", name, err)
	}
	if string(want) != got {
		t.Errorf("%s changed.\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}

func TestAdaptAccessorsReturnCopies(t *testing.T) {
	c := AdaptCandidates()
	c[0] = "changed"
	if AdaptCandidates()[0] == "changed" || !IsAdaptCandidate("format") {
		t.Error("a change to the returned candidates reached the next call")
	}
	g := AdaptKnobGrid()
	g["extract"]["min_tokens"][0] = -1
	g["extract"]["new"] = []int{1}
	delete(g, "dedup")
	n := AdaptKnobGrid()
	if n["extract"]["min_tokens"][0] != 200 || n["extract"]["new"] != nil || n["dedup"] == nil {
		t.Errorf("a change to the returned grid reached the next call: %v", n)
	}
	if AdaptKnobAllowed("extract", "min_tokens", -1) || !AdaptKnobAllowed("extract", "min_tokens", 200) {
		t.Error("the grid check read the changed copy")
	}
}

func editText(doc string, err error) string {
	if err != nil {
		return "ERR: " + err.Error() + "\n"
	}
	return doc
}

func TestEditToolfilterRemovedGolden(t *testing.T) {
	for dn, doc := range editDocs {
		out, err := EditToolfilterRemoved(doc, func(c []string) []string { return WithName(c, "mcp__new", true) })
		editGolden(t, "removed-add-"+dn, editText(out, err))
		out, err = EditToolfilterRemoved(doc, func(c []string) []string { return WithName(c, "mcp__ops", false) })
		editGolden(t, "removed-del-"+dn, editText(out, err))
	}
}

func TestEditToolfilterDeferGolden(t *testing.T) {
	for dn, doc := range editDocs {
		// The host passes the current deferral state. The proxy's docDefers answers true only
		// for "always": the one document with toolfilter in its pipeline and `defer: auto`.
		on := dn == "always"
		out, err := EditToolfilterDefer(doc, on, func(c ToolfilterDefer) ToolfilterDefer {
			c.Always = WithName(c.Always, "mcp__pw", true)
			c.Keep = WithName(c.Keep, "Read", true)
			c.On = !c.On
			return c
		})
		editGolden(t, "defer-flip-"+dn, editText(out, err))
		out, err = EditToolfilterDefer(doc, on, func(c ToolfilterDefer) ToolfilterDefer {
			c.Always = WithName(c.Always, "mcp__ops", false)
			c.On = true
			return c
		})
		editGolden(t, "defer-on-"+dn, editText(out, err))
	}
}

func TestEditAdaptCompactionGolden(t *testing.T) {
	cands := []string{"dedup", "format", "toon"}
	settings := map[string]map[string]int{"extract": {"min_tokens": 800}, "dedup": {"min_tokens": 100}}
	for dn, doc := range editDocs {
		next, pipe, err := EditAdaptCompaction(doc, cands, settings)
		editGolden(t, "adapt-"+dn, editText(next, err)+"PIPELINE "+strings.Join(pipe, ",")+"\n")
	}
	_, _, err := EditAdaptCompaction(editDoc, nil, map[string]map[string]int{"extract": {"min_tokens": 7}})
	editGolden(t, "adapt-refused", err.Error()+"\n")
	var na ErrNotAdaptable
	if !errors.As(err, &na) || na.Component != "extract" || na.Key != "min_tokens" {
		t.Errorf("refusal = %#v", err)
	}
	next, _, err := EditAdaptCompaction(editDoc, []string{"extract", "format"},
		map[string]map[string]int{"extract": {"min_tokens": 400}})
	editGolden(t, "adapt-stored-value", editText(next, err))
}

func TestEditorsRefuseAnUnloadableDocument(t *testing.T) {
	_, err := EditToolfilterRemoved("a: [", func(c []string) []string { return c })
	if !errors.Is(err, ErrUnloadable) {
		t.Errorf("removed: %v", err)
	}
	_, err = EditToolfilterDefer("a: [", false, func(c ToolfilterDefer) ToolfilterDefer { return c })
	if !errors.Is(err, ErrUnloadable) {
		t.Errorf("defer: %v", err)
	}
	if _, ok, reason := ToolfilterRemoved("a: ["); ok || reason != ErrUnloadable.Error() {
		t.Errorf("read: ok=%v %q", ok, reason)
	}
}

func TestNameListReadsBothShapes(t *testing.T) {
	got := NameList([]any{"b", "", 3, "a"})
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("[]any: %v", got)
	}
	if got := NameList([]string{"z", "y"}); strings.Join(got, ",") != "y,z" {
		t.Errorf("[]string: %v", got)
	}
	if NameList("x") != nil {
		t.Error("a scalar is not a list")
	}
}

func TestWithComponentKeepsPosition(t *testing.T) {
	if got := strings.Join(WithComponent([]string{"format", " toolfilter ", "searchfold"}, "toolfilter", true), ","); got != "format, toolfilter ,searchfold" {
		t.Errorf("present: %q", got)
	}
	if got := strings.Join(WithComponent([]string{"format"}, "toolfilter", true), ","); got != "format,toolfilter" {
		t.Errorf("absent: %q", got)
	}
	if got := strings.Join(WithComponent([]string{"format", "toolfilter"}, "toolfilter", false), ","); got != "format" {
		t.Errorf("drop: %q", got)
	}
}

func TestDeclConfigName(t *testing.T) {
	for _, c := range []struct{ kind, name, server, want, err string }{
		{"mcp_server", "", "ops", "mcp__ops", ""},
		{"mcp_server", "", "", "", "needs a server name"},
		{KindSkill, "pdf", "", "skill__pdf", ""},
		{KindSkill, "", "", "", "no skill name"},
		{KindSkill, "a b!", "", "", "not a skill name"},
		{KindServerTool, "x", "", "", "provider-side"},
		{KindSkillListing, "", "", "", "indivisible"},
		{KindTool, "Read", "", "Read", ""},
		{"", "Read", "", "Read", ""},
		{KindMCPTool, "", "", "", "no declaration name"},
		{"weird", "x", "", "", "unknown declaration kind weird"},
	} {
		got, err := DeclConfigName(c.kind, c.name, c.server)
		if got != c.want || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%+v -> %q, %v", c, got, err)
		}
	}
}

func TestAdaptMergeKeepsFixedComponentsAfterTheirCandidate(t *testing.T) {
	cur := []string{"format", "dedup", "cacheinject", "extract", "extract_llm"}
	got := strings.Join(AdaptMerge(cur, []string{"extract", "format"}), ",")
	if got != "extract,extract_llm,format,cacheinject" {
		t.Errorf("merge = %s", got)
	}
	if got := strings.Join(AdaptMerge([]string{"cacheinject"}, []string{"format", "extract"}), ","); got != "cacheinject,format,extract" {
		t.Errorf("head = %s", got)
	}
}

func TestRemovalNote(t *testing.T) {
	doc := "components:\n  toolfilter:\n    remove: [mcp__ops]\n"
	if got := RemovalNote([]string{"mcp__ops", "mcp__srv"}, doc, true); got !=
		"stopped filtering mcp__srv; still filtered because your configuration lists them: mcp__ops" {
		t.Errorf("restore = %q", got)
	}
	if got := RemovalNote([]string{"mcp__ops"}, doc, false); got != "filtering mcp__ops" {
		t.Errorf("add = %q", got)
	}
	if got := RemovalNote([]string{"mcp__ops"}, doc, true); got != "stopped filtering nothing; still filtered because your configuration lists them: mcp__ops" {
		t.Errorf("none = %q", got)
	}
}
