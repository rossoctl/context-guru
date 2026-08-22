package dash

// Three invariants of the Inventory view that regress silently and that a person reading the
// page cannot check.
//
// They are asserted against the EMBEDDED assets, the same way
// TestProviderCacheSavingIsNeverOurs asserts that the provider's cache figure is always
// labelled. Static checks over source are a blunt instrument and these three earn it: each one
// is a defect that shipped, that looked completely fine on screen, and that made the page say
// something false about the reader's own configuration.

import (
	"regexp"
	"strings"
	"testing"
)

// TestKindLabelNeverClaimsAKindIsBuiltin guards the defect that made the owner distrust the
// page: KIND_LABEL mapped kind `tool` to the literal string "built-in".
//
// Claude Code's own tools and a third-party agent's own tools are the SAME kind — both `tool`,
// because the stored taxonomy cannot tell them apart (see toolremoval.go) — and the answer
// lives in the separate `builtin` boolean. So a label table keyed on kind can never carry the
// word, and every removable client tool an SDK application declared rendered a pill reading
// "built-in" inside the list of things the page was recommending be removed.
func TestKindLabelNeverClaimsAKindIsBuiltin(t *testing.T) {
	src := readUI(t, "ui/tools.js")
	i := strings.Index(src, "const KIND_LABEL")
	if i < 0 {
		t.Fatal("KIND_LABEL is gone; this check needs rewriting against whatever replaced it")
	}
	j := strings.Index(src[i:], "}")
	if j < 0 {
		t.Fatal("could not find the end of the KIND_LABEL table")
	}
	table := src[i : i+j]
	for _, bad := range []string{"built-in", "builtin", "built in"} {
		if strings.Contains(strings.ToLower(table), bad) {
			t.Errorf("KIND_LABEL contains %q:\n%s\n\n"+
				"`kind` cannot answer 'is this a built-in' — Claude Code's own tools and a "+
				"third-party agent's own tools are both kind `tool`, and the report answers it "+
				"in the `builtin` boolean. A label table keyed on kind that says 'built-in' "+
				"puts that word on every removable client tool in the actionable list.",
				bad, table)
		}
	}
	// And the boolean IS consulted, before the kind.
	if !regexp.MustCompile(`function kindPill\([^)]*\)\s*\{[^}]*t\.builtin`).MatchString(src) {
		t.Error("kindPill does not check t.builtin before falling back to the kind label")
	}
}

// TestEveryInventoryTileHasAnExplanation is the owner's "I cannot tell what the statistics
// mean", as a check.
//
// Every Overview tile had a TILE_INFO entry and not one of the eleven Inventory tiles did, so
// the single tab whose figures are used to decide what to DELETE was the one tab that never
// said what its figures were. A tile added later must not quietly reintroduce that.
func TestEveryInventoryTileHasAnExplanation(t *testing.T) {
	src := readUI(t, "ui/tools.js")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`tile\('([a-z0-9-]+)'`).FindAllStringSubmatch(src, -1) {
		keys[m[1]] = true
	}
	if len(keys) == 0 {
		t.Fatal("found no tile() calls in tools.js; this check needs rewriting")
	}
	// The entries tools.js adds to app.js's registry, plus anything app.js defines itself —
	// a key may legitimately be explained in either file.
	registry := src + readUI(t, "ui/app.js")
	for k := range keys {
		if !strings.Contains(registry, "'"+k+"': {") {
			t.Errorf("tile %q has no TILE_INFO entry.\n"+
				"Every figure on this page needs its plain-English definition, how it was "+
				"derived and its catch — this is the tab whose numbers authorise deleting "+
				"things, and a figure nobody can interpret is a figure that gets acted on "+
				"wrongly or ignored.", k)
		}
	}
}

// TestTheRemovalCommandIsInTheActionableList guards the defect the owner reported directly:
// "I cannot see the commands for each tool/skill for how to remove them".
//
// removalCell() existed and was called from exactly ONE place — the built-ins table, i.e. the
// only group on the page nobody should act on. The command was therefore visible only beside a
// danger warning and absent from every row the page was recommending.
func TestTheRemovalCommandIsInTheActionableList(t *testing.T) {
	src := readUI(t, "ui/tools.js")
	n := strings.Count(src, "removalCell(")
	// One definition, plus at least the built-ins table and the actionable list.
	if n < 3 {
		t.Errorf("removalCell appears %d times (definition + %d call sites).\n"+
			"It must be rendered in the ACTIONABLE list, not only in the built-ins table — "+
			"a page whose only visible removal command sits under 'removing this breaks the "+
			"agent' is a page that answers the reader's question in the one place they must "+
			"not act on.", n, n-1)
	}
	// And in the function that builds the actionable list, specifically.
	i := strings.Index(src, "function removalGroup(")
	if i < 0 {
		t.Fatal("removalGroup is gone; this check needs rewriting against its replacement")
	}
	end := strings.Index(src[i:], "\n}\n")
	if end < 0 {
		end = len(src) - i
	}
	if !strings.Contains(src[i:i+end], "removalCell(") {
		t.Error("removalGroup renders no removal command; the actionable list is back to " +
			"telling a reader what to remove and not how")
	}
}

func readUI(t *testing.T, name string) string {
	t.Helper()
	b, err := uiFS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
