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

// TestTheRemovalSwitchIsNotRoleGated pins the assumption POST /api/toolfilter now rests on.
//
// That route accepts a plain account because this page OFFERS a plain account the switch: the
// tab is mounted unconditionally, and the only thing that disables a checkbox is a provider-side
// row or a proxy that reports no control. The route's permission was written to match. If a
// role gate is ever added here, the two ends disagree in the direction that is invisible on
// screen — every account still sees the analysis, managers still see a working switch, and a
// user's switch silently does nothing but alert — which is the exact defect the route gate was.
//
// A static check because there is no DOM here, and a grep for the role helpers is enough: they
// are the only way this view could learn a caller's role, since it is handed a report and a
// control document and neither carries one.
func TestTheRemovalSwitchIsNotRoleGated(t *testing.T) {
	src := readUI(t, "ui/tools.js")
	for _, gate := range []string{"isManager(", "wideScope(", "'manager'", `"manager"`} {
		if strings.Contains(src, gate) {
			t.Errorf("the Inventory view consults %s. Its switch is offered to every account "+
				"and POST /api/toolfilter accepts every account to match; gating the control on "+
				"a role here makes a user's switch a no-op that only alerts. Hide nothing, or "+
				"change the route's permission with it.", gate)
		}
	}
	// And every switch's disabled condition must stay about the ROW and the PROXY, not the
	// reader: `fixed` is a provider-side tool, `!tools.control` is a proxy offering no control.
	//
	// Asserted as a PROPERTY over however many conditions exist, not as a count of them. The
	// first version of this check required exactly 2, which is what main happens to have —
	// and the branch that moves built-in tools into their own section correctly leaves that
	// table with no switch at all, so it has 1. A count assertion fails on that legitimate
	// change, and fails with a message pointing at the role gate, which is the opposite of
	// what happened. Verified against that branch's tools.js, not just this one's.
	conds := 0
	for _, line := range strings.Split(src, "\n") {
		i := strings.Index(line, "disabled:")
		if i < 0 {
			continue
		}
		cond := line[i:]
		conds++
		if !strings.Contains(cond, "fixed") || !strings.Contains(cond, "!tools.control") {
			t.Errorf("a switch's disabled condition is not about the row and the proxy: %s\n"+
				"every one must gate on `fixed` (a provider-side tool) and `!tools.control` (a "+
				"proxy offering no control), and on nothing about the reader", strings.TrimSpace(cond))
		}
		for _, gate := range []string{"isManager", "wideScope", "manager", "role"} {
			if strings.Contains(cond, gate) {
				t.Errorf("a switch's disabled condition consults %q: %s — a user's switch would "+
					"become a no-op that only alerts, which is the defect the route gate was",
					gate, strings.TrimSpace(cond))
			}
		}
	}
	// Zero would mean the view stopped drawing a switch at all, which passes every assertion
	// above by having nothing to check.
	if conds == 0 {
		t.Error("the Inventory view declares no switch disabled condition at all; either the " +
			"switch is gone or it moved somewhere this check cannot see")
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

// TestNoFunctionIsDefinedTwiceInTheDashboardSource is the root-cause check for the defect the
// owner reported as "the system prompt parts are not visible".
//
// tools.js defined `unusedRow` TWICE, 70 lines apart. In JavaScript the second wins, and the
// second was the OLDER one: it wrapped the whole row in a <label> (so a copy button inside it
// toggled the checkbox) and, decisively, it had no promptTextReveal call. The fixed version — the
// one with the reveal, the id/for pairing and the per-item command — was unreachable code for as
// long as both existed. Nothing failed, nothing warned, and the feature was simply absent from
// every grouped row on the page while its code sat right there in the file being read by
// reviewers.
//
// A static check earns its keep here because the failure is invisible three ways at once: the
// page renders, the tests pass, and the dead copy looks like the live one.
func TestNoFunctionIsDefinedTwiceInTheDashboardSource(t *testing.T) {
	// KNOWN EXCEPTION — ui/app.js:kv. A pre-existing duplicate of exactly this shape, found by
	// this check and deliberately not fixed here.
	//
	// app.js defines kv() at ~2929 and ~6644. The later wins; the two bodies differ only in how
	// they pass their text (`text: k` vs a child node, `v` vs `String(v)`), so there is no visible
	// defect today. It is recorded rather than repaired because it sits ~3,700 lines from anything
	// this change touches, and a silent edit to a shared helper is how an unrelated regression gets
	// attributed to the wrong commit.
	//
	// TO CLEAR IT: keep one definition, delete the other, drop this entry. Everything a tracking
	// issue would need is in this comment on purpose — the change that found it could not file one
	// (an outbound write it had no authorisation for), and a finding whose only record is a review
	// thread is a finding that evaporates.
	known := map[string]bool{"ui/app.js:kv": true}
	def := regexp.MustCompile(`(?m)^function ([A-Za-z_$][\w$]*)\s*\(`)
	// Every classic script on the page, not just the two this check originally scanned.
	// kvcache.js and campaigns.js were outside it while sharing ONE global scope with
	// app.js — so a name either of them redefined would win or lose by load order with
	// nothing to catch it, which is the exact failure mode the comment above describes.
	for _, name := range []string{"ui/tools.js", "ui/app.js", "ui/kvcache.js", "ui/campaigns.js"} {
		seen := map[string]int{}
		for _, m := range def.FindAllStringSubmatch(readUI(t, name), -1) {
			seen[m[1]]++
		}
		if len(seen) == 0 {
			t.Fatalf("%s: found no top-level function definitions; this check needs rewriting", name)
		}
		for fn, n := range seen {
			if n > 1 && !known[name+":"+fn] {
				t.Errorf("%s defines %s() %d times.\n"+
					"The later definition silently wins and the earlier one becomes unreachable "+
					"code that still reads as live. This is how the prompt-text reveal came to be "+
					"absent from every grouped row on the Inventory page while the function that "+
					"rendered it sat in the file.", name, fn, n)
			}
		}
	}
}

// TestEveryRemovableRowCarriesItsOwnCommand: the owner asked for the exact command "for each MCP
// tool and each skill", and the group-level command is not that.
//
// A group of one already showed it. A group of MANY showed the SERVER-level `claude mcp remove
// <server>` once at the top — the right answer to "drop this whole server" and no answer at all
// to "drop just this one tool", which is the question a reader with a nineteen-tool server has.
// And the sortable tables, which are where a reader goes for a tool they DO use occasionally,
// showed no command anywhere.
func TestEveryRemovableRowCarriesItsOwnCommand(t *testing.T) {
	src := readUI(t, "ui/tools.js")
	if !strings.Contains(src, "function removalDetails(") {
		t.Fatal("removalDetails is gone; this check needs rewriting against its replacement")
	}
	// The per-item row, the tools table and the skills table each render one.
	for fn, why := range map[string]string{
		"function unusedRow(":   "a member of a multi-item group has no per-item command",
		"function toolTable(":   "the tools table has no removal command column",
		"function skillsPanel(": "the skills table has no removal command column",
	} {
		i := strings.Index(src, fn)
		if i < 0 {
			t.Errorf("%s is gone; this check needs rewriting", fn)
			continue
		}
		end := strings.Index(src[i:], "\n}\n")
		if end < 0 {
			end = len(src) - i
		}
		if !strings.Contains(src[i:i+end], "removalDetails(") {
			t.Errorf("%s: %s", fn, why)
		}
	}
	// And a skill must be switchable from the skills table, not only from the never-invoked list:
	// a skill used once and no longer wanted is exactly the case that list cannot reach.
	i := strings.Index(src, "function skillsPanel(")
	end := strings.Index(src[i:], "\n}\n")
	if !strings.Contains(src[i:i+end], "excludeToggle(") {
		t.Error("the skills table has no opt-out switch, so turning a skill off still needs a " +
			"config file edit")
	}
}

// TestTheAsideGroupComesFromTheServer: the built-in/provider split decides what is in the totals,
// so the client must not re-derive it.
//
// A client-side `filter((t) => t.builtin)` over rep.tools is a second copy of a rule the server
// already applied when it kept those rows out of every total. Two copies drift, and the drift is
// invisible: a row in the wrong group looks perfectly plausible in both places, and the headline
// and the table would simply disagree about which one it was in.
func TestTheAsideGroupComesFromTheServer(t *testing.T) {
	src := readUI(t, "ui/tools.js")
	i := strings.Index(src, "function renderBuiltins(")
	if i < 0 {
		t.Fatal("renderBuiltins is gone; this check needs rewriting against its replacement")
	}
	end := strings.Index(src[i:], "\n}\n")
	if end < 0 {
		end = len(src) - i
	}
	body := src[i : i+end]
	if !strings.Contains(body, "rep.aside") {
		t.Error("the built-ins section does not read rep.aside; it is re-deriving the split the " +
			"server already made when it kept those rows out of the totals")
	}
	if regexp.MustCompile(`rep\.tools[^\n]*\bfilter\(`).MatchString(body) {
		t.Errorf("the built-ins section filters rep.tools:\n%s\n\n"+
			"rep.tools no longer holds them. A filter here would silently render an empty "+
			"section while the whole group went unlisted.", body)
	}
}

// TestTheHeadlineSaysWhatItLeftOut. The four headline tiles now cover MCP tools and skills only,
// which is what makes them actionable — and it also makes them several times smaller than the
// composition bar further down the same page. A reader who meets both and is told nothing has to
// work out for themselves which one is lying. Neither is; they answer different questions, and the
// page has to say so where the smaller number is.
func TestTheHeadlineSaysWhatItLeftOut(t *testing.T) {
	src := readUI(t, "ui/tools.js")
	if !strings.Contains(src, "aside_tokens") {
		t.Error("nothing on the page reads totals.aside_tokens, so the headline never says how " +
			"much weight it excluded")
	}
	if !strings.Contains(src, "inv-aside-note") {
		t.Error("the headline has no disclosure of the excluded group")
	}
}

// TestTheGaugeTellsTheTwoZeroesApart. With the built-ins out of these figures, "declared 0" is now
// the NORMAL state of a plain Claude Code session that carries no MCP tools and no skills — and it
// means something completely different from "declared plenty and used all of it".
//
// Both landed on one line reading "Nothing in this scope was declared and left unused", which over
// an empty controllable set reads as a clean bill while fifteen thousand tokens of the agent's own
// tools sit in the section at the end of the page. A static check because the branch is chosen from
// report data the Go tests do not render.
func TestTheGaugeTellsTheTwoZeroesApart(t *testing.T) {
	src := readUI(t, "ui/tools.js")
	i := strings.Index(src, "function gauge(")
	if i < 0 {
		t.Fatal("gauge is gone; this check needs rewriting against its replacement")
	}
	end := strings.Index(src[i:], "\n}\n")
	if end < 0 {
		end = len(src) - i
	}
	body := src[i : i+end]
	// The two states are distinguished at all...
	if !strings.Contains(body, "!t.declared_tokens") {
		t.Error("the gauge does not branch on declared_tokens being zero, so 'you declared " +
			"nothing controllable' and 'you used everything you declared' print the same line")
	}
	// ...and the empty-set branch does NOT congratulate the reader.
	if !strings.Contains(body, "not \\\"no waste\\\"") && !strings.Contains(body, `not "no waste"`) {
		t.Error("the declared-nothing branch does not say it is not a clean bill; a reader with " +
			"15k tokens of built-ins would read it as one")
	}
	// ...and it points at where the weight actually is.
	if !strings.Contains(body, "aside_tokens") {
		t.Error("the declared-nothing branch does not name the weight it excluded")
	}
}
