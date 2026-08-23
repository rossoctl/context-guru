package apply_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/internal/skills"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// turn builds request N of a session: the same catalogue and system prompt every time (as
// Claude Code sends them) with one more exchange on the transcript.
func turn(n int) string {
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5","tools":[` +
		`{"name":"Bash","description":"run a command","input_schema":{"type":"object"}},` +
		`{"name":"CronCreate","description":"schedule a job","input_schema":{"type":"object"}},` +
		`{"name":"Workflow","description":"never used","input_schema":{"type":"object"}}],` +
		`"system":[{"type":"text","text":"You are an agent. Prefer Bash for shell work.\nCurrent branch: main\n"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"start"}]}`)
	for i := 0; i < n; i++ {
		b.WriteString(`,{"role":"assistant","content":[{"type":"tool_use","id":"t` + string(rune('0'+i)) +
			`","name":"Bash","input":{"command":"ls"}}]}`)
		b.WriteString(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t` +
			string(rune('0'+i)) + `","content":"a b c"}]}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// TestToolFilterThroughApply is the wiring test: gated on the component name, reading the
// component's own list, and rewriting `tools` without disturbing `messages` — the ordering
// that matters because the writeback takes byte offsets into the body afterwards.
func TestToolFilterThroughApply(t *testing.T) {
	for _, tc := range []struct {
		name, pipeline string
		want           int
	}{
		{"absent without the component", "pipeline: [format]\n", 3},
		{"empty list removes nothing", "pipeline: [format, toolfilter]\n", 3},
		{"removes what it is told", "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [CronCreate, Workflow]\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := pipe(t, tc.pipeline).Build(nil)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(turn(2))
			out, _ := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
				bschemas.Anthropic, body, "s1", false)
			if got := len(gjson.GetBytes(out, "tools").Array()); got != tc.want {
				t.Errorf("tools = %d, want %d (%s)", got, tc.want, gjson.GetBytes(out, "tools").Raw)
			}
			if a, b := gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(out, "messages").Raw; a != b {
				t.Errorf("messages changed:\n%s\n%s", a, b)
			}
		})
	}
}

// TestToolFilterSameToolsEveryTurn is the cache-invalidation test, and it is the one that
// matters most: `tools` renders at position 0 with no breakpoint on it, so a filter that
// acted on some turns of a session and not others would re-anchor the entire prefix on every
// one of them. A session filtered from turn 1 must send byte-identical `tools` on every turn.
func TestToolFilterSameToolsEveryTurn(t *testing.T) {
	p, err := pipe(t, "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [CronCreate, Workflow]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	want := ""
	for n := 0; n < 8; n++ {
		out, _ := apply.Body(context.Background(), p, st, bschemas.Anthropic, []byte(turn(n)), "s1", false)
		got := gjson.GetBytes(out, "tools").Raw
		if n == 0 {
			want = got
			if len(gjson.Parse(got).Array()) != 1 {
				t.Fatalf("turn 0 did not filter: %s", got)
			}
			continue
		}
		if got != want {
			t.Fatalf("turn %d sent different tools:\n got %s\nwant %s", n, got, want)
		}
	}
}

// TestToolFilterBypassed: a bypassed request promises a byte-identical forward.
func TestToolFilterBypassed(t *testing.T) {
	p, err := pipe(t, "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [CronCreate]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(turn(1))
	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "s1", true)
	if changed || string(out) != string(body) {
		t.Error("a bypassed request was filtered")
	}
}

// TestToolFilterRejectsAJunkName: validation at write time, so a bad list is a 400 on the
// settings page rather than a filter that silently matches nothing forever.
func TestToolFilterRejectsAJunkName(t *testing.T) {
	for _, name := range []string{"*", "Cron*", "a b", "Cron;drop"} {
		if _, err := pipe(t, "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    remove: [\""+name+"\"]\n").Build(nil); err == nil {
			t.Errorf("accepted %q as a declaration name", name)
		}
	}
}

// skillTurn is turn(n) with a SKILLS LISTING on it: prose inside a <system-reminder> in a
// role:"system" message, which is where Claude Code really puts it (measured: messages[1], a plain
// string). turn() has no listing, so nothing built on it can exercise the skills half at all.
func skillTurn(n int) string {
	const listing = `<system-reminder>\nAvailable agent types:\n- planner: plans things\n</system-reminder>\n\n` +
		`<system-reminder>\nThe following skills are available for use with the Skill tool:\n\n` +
		`- dataviz: Use for charts and plots.\n` +
		`- deep-research: A research harness.\n  With a continuation line of its own.\n` +
		`- ponytail:ponytail: A plugin skill, colon in the name.\n</system-reminder>`
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5","tools":[` +
		`{"name":"Skill","description":"Invoke a skill.","input_schema":{"type":"object"}},` +
		`{"name":"Bash","description":"run a command","input_schema":{"type":"object"}}],` +
		`"system":[{"type":"text","text":"You are an agent. Prefer Bash for shell work.\nCurrent branch: main\n"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"start"}]},` +
		`{"role":"system","content":"` + listing + `"}`)
	for i := 0; i < n; i++ {
		b.WriteString(`,{"role":"assistant","content":[{"type":"tool_use","id":"t` + string(rune('0'+i)) +
			`","name":"Bash","input":{"command":"ls"}}]}`)
		b.WriteString(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t` +
			string(rune('0'+i)) + `","content":"a b c"}]}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// listedSkills reads the skill names still in the body's listing, through the same parser the
// inventory prices them with — so this test and the page cannot disagree about what "removed"
// means.
func listedSkills(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		c := m.Get("content")
		txt := c.String()
		if c.IsArray() {
			txt = ""
			c.ForEach(func(_, blk gjson.Result) bool { txt += blk.Get("text").String(); return true })
		}
		i := strings.Index(txt, skills.Header)
		if i < 0 {
			return true
		}
		body := txt[i+len(skills.Header):]
		if j := strings.Index(body, skills.ReminderEnd); j >= 0 {
			body = body[:j]
		}
		for _, e := range skills.Parse(body).Entries {
			out = append(out, e.Name)
		}
		return false
	})
	return out
}

// TestSkillFilterThroughApply is the WIRING test for the skills half, and the reason it exists is
// that its absence was invisible: deleting the two lines in apply.Body that call
// filterSkillListing left `go test ./...` entirely green across every package. Seven unit tests
// cover the function and every one of them calls it directly, so none of them notices that nothing
// in production does. A user could tick a skill, have the config written and audited, read "Opted
// out" on the page, and go on being charged for the entry — with CI green.
//
// Deliberately the sibling of TestToolFilterThroughApply: same table, same pipeline strings, same
// apply.Body call. The tools half is covered by that one; this is the half that was not.
func TestSkillFilterThroughApply(t *testing.T) {
	for _, tc := range []struct {
		name, pipeline string
		want           []string
	}{
		{"absent without the component", "pipeline: [format]\n",
			[]string{"dataviz", "deep-research", "ponytail:ponytail"}},
		{"empty list removes nothing", "pipeline: [format, toolfilter]\n",
			[]string{"dataviz", "deep-research", "ponytail:ponytail"}},
		{"a bare name is not a skill entry", "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [dataviz]\n",
			[]string{"dataviz", "deep-research", "ponytail:ponytail"}},
		{"removes the skill it is told", "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [skill__dataviz]\n",
			[]string{"deep-research", "ponytail:ponytail"}},
		{"a plugin skill's colon survives the round trip", "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [skill__ponytail:ponytail]\n",
			[]string{"dataviz", "deep-research"}},
		{"several at once", "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [skill__dataviz, skill__deep-research]\n",
			[]string{"ponytail:ponytail"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := pipe(t, tc.pipeline).Build(nil)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(skillTurn(2))
			out, _ := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
				bschemas.Anthropic, body, "s1", false)
			if got := strings.Join(listedSkills(t, out), ","); got != strings.Join(tc.want, ",") {
				t.Errorf("listing = [%s], want [%s]", got, strings.Join(tc.want, ","))
			}
			// The tools array is not this half's business and must not move.
			if a, b := gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(out, "tools").Raw; a != b {
				t.Errorf("tools changed:\n%s\n%s", a, b)
			}
			// And the entry that went took its continuation line with it, rather than orphaning
			// prose onto whichever entry followed.
			if len(tc.want) < 3 && strings.Contains(string(out), "continuation line of its own") &&
				!containsStr(tc.want, "deep-research") {
				t.Error("a removed entry's continuation line was left in the prompt")
			}
		})
	}
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// TestSkillFilterSameListingEveryTurn is the cache-invalidation sibling of
// TestToolFilterSameToolsEveryTurn, and it matters for the same reason and slightly more: the
// listing sits inside the cached PREFIX, so a filter that acted on some turns of a session and not
// others would re-anchor the whole prompt on every one of them. A session filtered from turn 1
// must send a byte-identical listing on every turn.
func TestSkillFilterSameListingEveryTurn(t *testing.T) {
	p, err := pipe(t, "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [skill__dataviz]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	want := ""
	for n := 0; n < 8; n++ {
		out, _ := apply.Body(context.Background(), p, st, bschemas.Anthropic,
			[]byte(skillTurn(n)), "s1", false)
		got := gjson.GetBytes(out, "messages.1.content").Raw
		if n == 0 {
			want = got
			if s := listedSkills(t, out); len(s) != 2 {
				t.Fatalf("turn 0 did not filter: %v", s)
			}
			continue
		}
		if got != want {
			t.Fatalf("turn %d sent a different listing:\n got %s\nwant %s", n, got, want)
		}
	}
}

// TestFilterRemovalIsNotInsideTheCompactionBaseline is the ORDERING guard behind the word
// "disjoint" in Overview.TotalSavedUSD, and it lives here because the ordering does.
//
// TotalSavedUSD adds the declaration filter's saving to compaction's. For the TOOLS half that is
// disjoint by construction — a tool schema is not in `messages` and tokens_before counts nothing
// else. For the SKILLS half it is not obvious: a skill's listing entry IS in `messages`, exactly
// where tokens_before is measured, so if the removal landed inside Saved() the same tokens would be
// counted once in NetSavedUSD and again in DeclFilterUSD.
//
// It holds only because filterSkillListing runs in Body BEFORE the pipeline takes its baseline, so
// tokens_before is measured on the already-filtered body. That is an ordering property, and the
// assertion is therefore a COMPARISON OF TWO REAL RUNS rather than a hand-built row: with the
// filter on, tokens_before must FALL by the entry's weight while Saved() stays 0. Move the filter
// below the baseline and tokens_before stays put while tokens_after drops — Saved() becomes N and
// the total double-counts.
//
// A dash-side test asserts the downstream arithmetic on such a row; this one is what makes that
// row's shape true in the first place.
func TestFilterRemovalIsNotInsideTheCompactionBaseline(t *testing.T) {
	run := func(pipeline string) apply.Result {
		p, err := pipe(t, pipeline).Build(nil)
		if err != nil {
			t.Fatal(err)
		}
		return apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
			Provider: bschemas.Anthropic, Body: []byte(skillTurn(2)), Session: "s1",
		})
	}
	off := run("pipeline: [format, toolfilter]\n")
	on := run("pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [skill__dataviz]\n")

	if on.Trace.FilteredDeclTokens <= 0 {
		t.Fatalf("the filter reported %d removed tokens; nothing was exercised",
			on.Trace.FilteredDeclTokens)
	}
	// Saved() is tokens_before − tokens_after. It must be ZERO on both runs: the filter's removal
	// happened before the baseline, so the pipeline never saw those tokens to "save".
	for _, c := range []struct {
		name string
		r    apply.Result
	}{{"unfiltered", off}, {"filtered", on}} {
		if got := c.r.Trace.Run.TokensBefore - c.r.Trace.Run.TokensAfter; got != 0 {
			t.Errorf("%s: tokens_before − tokens_after = %d, want 0.\n"+
				"A non-zero saving here is the declaration filter's removal landing inside the "+
				"compaction baseline, where TotalSavedUSD counts it a second time.", c.name, got)
		}
	}
	// And the baseline itself SHRANK by the removal, which is the positive half of the same
	// statement: the tokens left before they were ever counted.
	drop := off.Trace.Run.TokensBefore - on.Trace.Run.TokensBefore
	if drop <= 0 {
		t.Errorf("tokens_before did not fall when the filter was switched on (%d -> %d).\n"+
			"Either the filter did not touch `messages`, or it ran after the baseline was taken.",
			off.Trace.Run.TokensBefore, on.Trace.Run.TokensBefore)
	}
	// The two measurements are of the same bytes by different code, so they should agree closely.
	// Not asserted as equality: FilteredDeclTokens counts the entry's own text, tokens_before
	// counts the whole messages array, and BPE is not additive across a splice.
	if d := drop - on.Trace.FilteredDeclTokens; d > 4 || d < -4 {
		t.Errorf("tokens_before fell by %d but the filter reported %d removed; more than a "+
			"boundary token apart, so these are not describing the same removal",
			drop, on.Trace.FilteredDeclTokens)
	}
}
