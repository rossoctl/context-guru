package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

// Golden outputs of the config-document editors (#1304). The files hold the exact text each
// editor writes for one input, taken before the editors moved to package config. The config
// package tests read the same files, so a move or a later change that alters one byte fails in
// both places. Refresh with UPDATE_EDIT_GOLDEN=1 only on purpose.

const goldenDir = "../config/testdata/editgolden"

const goldenDoc = `# account config
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

var goldenDocs = map[string]string{
	"full":     goldenDoc,
	"empty":    "",
	"minimal":  "pipeline: [format]\n",
	"nopipe":   "components:\n  toolfilter:\n    remove: [a, b]\n",
	"llm":      "pipeline: [cacheinject, format, extract_llm]\n",
	"badyaml":  "a: [",
	"always":   "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n    always_defer: [mcp__ops]\n",
	"noremove": "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    defer: off\n",
}

func goldenCheck(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".golden")
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

func goldenText(doc string, err error) string {
	if err != nil {
		return "ERR: " + err.Error() + "\n"
	}
	return doc
}

func TestGoldenEditRemoved(t *testing.T) {
	for dn, doc := range goldenDocs {
		out, err := editRemoved(doc, func(c []string) []string { return withDecl(c, "mcp__new", true) })
		goldenCheck(t, "removed-add-"+dn, goldenText(out, err))
		out, err = editRemoved(doc, func(c []string) []string { return withDecl(c, "mcp__ops", false) })
		goldenCheck(t, "removed-del-"+dn, goldenText(out, err))
	}
}

func TestGoldenEditDefer(t *testing.T) {
	for dn, doc := range goldenDocs {
		out, err := editDefer(doc, func(c deferConf) deferConf {
			c.always = withDecl(c.always, "mcp__pw", true)
			c.keep = withDecl(c.keep, "Read", true)
			c.on = !c.on
			return c
		})
		goldenCheck(t, "defer-flip-"+dn, goldenText(out, err))
		out, err = editDefer(doc, func(c deferConf) deferConf {
			c.always = withDecl(c.always, "mcp__ops", false)
			c.on = true
			return c
		})
		goldenCheck(t, "defer-on-"+dn, goldenText(out, err))
	}
}

func TestGoldenAdaptCompactionDoc(t *testing.T) {
	choice := dash.AdaptCompactionChoice{Candidates: []string{"dedup", "format", "toon"},
		Settings: map[string]map[string]int{"extract": {"min_tokens": 800}, "dedup": {"min_tokens": 100}}}
	for dn, doc := range goldenDocs {
		next, pipe, err := adaptCompactionDoc(doc, choice)
		goldenCheck(t, "adapt-"+dn, goldenText(next, err)+"PIPELINE "+strings.Join(pipe, ",")+"\n")
	}
	// A value outside the searched grid and different from the stored one is refused.
	_, _, err := adaptCompactionDoc(goldenDoc, dash.AdaptCompactionChoice{Settings: map[string]map[string]int{"extract": {"min_tokens": 7}}})
	goldenCheck(t, "adapt-refused", fmt.Sprint(err)+"\n")
	// The stored value is accepted although it is outside the grid.
	next, _, err := adaptCompactionDoc(goldenDoc, dash.AdaptCompactionChoice{Candidates: []string{"extract", "format"},
		Settings: map[string]map[string]int{"extract": {"min_tokens": 400}}})
	goldenCheck(t, "adapt-stored-value", goldenText(next, err))
}
