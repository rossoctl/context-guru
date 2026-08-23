package apply

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/skills"
	"github.com/tidwall/gjson"
)

// skillBody is real Claude Code shape: the listing is prose inside a <system-reminder> in a
// role:"system" MESSAGE, not in `system` and not in `tools` — measured on capture-tb.jsonl,
// messages[1], a plain string. A second reminder sits ABOVE it using the identical
// `- name: description` bullet shape, which is why the header is the anchor: an unanchored
// scrape would count "planner" as a skill.
const skillBody = `{"model":"claude-opus-5",` +
	`"tools":[{"name":"Skill","description":"Invoke a skill.","input_schema":{"type":"object"}},` +
	`{"name":"Bash","description":"run a command","input_schema":{"type":"object"}}],` +
	`"system":[{"type":"text","text":"You are an agent. Prefer Bash for shell work.\n"}],` +
	`"messages":[` +
	`{"role":"user","content":"go"},` +
	`{"role":"system","content":"<system-reminder>\nAvailable agent types:\n- planner: plans things\n</system-reminder>\n\n<system-reminder>\n` +
	`The following skills are available for use with the Skill tool:\n\n` +
	`- dataviz: Use for charts and plots.\n` +
	`- deep-research: A research harness.\n  It has a continuation line that belongs to it.\n` +
	`- ponytail:ponytail: A plugin skill, with a colon in its name.\n` +
	`</system-reminder>"},` +
	`{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`

// listingOf returns the text of the message that carries the listing.
func listingOf(t *testing.T, body []byte) string {
	t.Helper()
	_, text := skillListingPath(body)
	return text
}

// TestFilterSkillRemovesOnlyTheNamedEntry is the mechanism: the named skill's whole entry goes,
// including its continuation lines, and every other entry survives byte-for-byte.
func TestFilterSkillRemovesOnlyTheNamedEntry(t *testing.T) {
	out, tok, n := filterSkillListing([]byte(skillBody), []string{skills.RemovePrefix + "deep-research"})
	if n != 1 {
		t.Fatalf("removed %d entries, want 1", n)
	}
	if tok <= 0 {
		t.Errorf("removed %d tokens, want a positive weight", tok)
	}
	got := listingOf(t, out)
	if strings.Contains(got, "deep-research") {
		t.Error("the named skill is still in the listing")
	}
	// The continuation line belongs to the entry that was removed. Left behind it would be
	// orphaned prose attached to whichever entry followed — the exact drift internal/skills
	// exists to prevent.
	if strings.Contains(got, "continuation line") {
		t.Error("the entry's continuation line was orphaned in the prompt")
	}
	for _, keep := range []string{"dataviz: Use for charts", "ponytail:ponytail: A plugin skill"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q was removed too", keep)
		}
	}
	// The header, the terminator and the reminder ABOVE the listing are all untouched.
	for _, keep := range []string{skills.Header, skills.ReminderEnd, "- planner: plans things"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q did not survive", keep)
		}
	}
	// And nothing else in the request moved: same tools, same system prompt, same transcript.
	if a, b := gjson.GetBytes(out, "tools").Raw, gjson.Get(skillBody, "tools").Raw; a != b {
		t.Error("the tools array changed")
	}
	if a, b := gjson.GetBytes(out, "messages.2").Raw, gjson.Get(skillBody, "messages.2").Raw; a != b {
		t.Error("a transcript message changed")
	}
}

// TestFilterSkillHandlesAPluginNameWithAColon: `plugin:skill` is one name, and the entry parser
// has to take the longer candidate rather than stopping at the first colon. Getting this wrong
// removes nothing and reports success.
func TestFilterSkillHandlesAPluginNameWithAColon(t *testing.T) {
	out, _, n := filterSkillListing([]byte(skillBody), []string{skills.RemovePrefix + "ponytail:ponytail"})
	if n != 1 {
		t.Fatalf("removed %d entries, want 1", n)
	}
	if strings.Contains(listingOf(t, out), "A plugin skill") {
		t.Error("the plugin skill's entry survived")
	}
}

// TestFilterSkillIsByteStableWhenNothingMatches is the determinism rule at the byte level. The
// listing sits inside the cached prefix, so a body that is merely EQUIVALENT after a no-op
// re-encode still re-anchors the whole prompt at the cache-creation rate for a saving of zero.
func TestFilterSkillIsByteStableWhenNothingMatches(t *testing.T) {
	for _, remove := range [][]string{
		nil,
		{},
		{"Bash"},    // a tool, not a skill: not this filter's business
		{"dataviz"}, // the right name WITHOUT the prefix
		{skills.RemovePrefix + "not-a-skill-here"}, // prefixed, but nothing declares it
		{skills.RemovePrefix},                      // the prefix alone
	} {
		out, tok, n := filterSkillListing([]byte(skillBody), remove)
		if string(out) != skillBody || tok != 0 || n != 0 {
			t.Errorf("remove=%v changed the body (%d tokens, %d entries)", remove, tok, n)
		}
	}
}

// TestFilterSkillFailsOpenWithoutAListing: a request with no skills listing is returned
// untouched rather than reshaped by a rewrite that had nothing to find.
func TestFilterSkillFailsOpenWithoutAListing(t *testing.T) {
	out, tok, n := filterSkillListing([]byte(bodyFixture), []string{skills.RemovePrefix + "dataviz"})
	if string(out) != bodyFixture || tok != 0 || n != 0 {
		t.Errorf("a body with no listing was changed (%d tokens, %d entries)", tok, n)
	}
}

// TestFilterSkillProseGateKeepsADescribedSkill: the same gate the tools half applies. A skill the
// agent's own hand-written instructions name is kept however loudly the configuration asks for
// it — and the gate must test the prose OUTSIDE the listing, since the listing names every skill
// in it by definition.
func TestFilterSkillProseGateKeepsADescribedSkill(t *testing.T) {
	// dataviz named in a `system` block, which is prose the gate reads.
	described := strings.Replace(skillBody,
		"You are an agent. Prefer Bash for shell work.",
		"You are an agent. Always run dataviz before answering.", 1)
	out, tok, n := filterSkillListing([]byte(described), []string{skills.RemovePrefix + "dataviz"})
	if n != 0 || tok != 0 || string(out) != described {
		t.Fatalf("a prose-described skill was removed: %d entries, %d tokens", n, tok)
	}
	// And the gate is not simply refusing everything: another skill in the same body still goes.
	if _, _, n := filterSkillListing([]byte(described), []string{skills.RemovePrefix + "dataviz",
		skills.RemovePrefix + "deep-research"}); n != 1 {
		t.Errorf("removed %d entries, want 1 (deep-research, while dataviz is pinned by prose)", n)
	}
}

// TestFilterSkillRemovesEveryNamedEntry: several at once, which is what a batch opt-out posts,
// and the one shape where an off-by-one in the line bookkeeping shows up.
func TestFilterSkillRemovesEveryNamedEntry(t *testing.T) {
	out, _, n := filterSkillListing([]byte(skillBody), []string{
		skills.RemovePrefix + "dataviz",
		skills.RemovePrefix + "deep-research",
		skills.RemovePrefix + "ponytail:ponytail",
	})
	if n != 3 {
		t.Fatalf("removed %d entries, want 3", n)
	}
	got := listingOf(t, out)
	// The header survives with nothing under it, which is the honest rendering of "this account
	// carries no skills" and is what the agent's own `skillOverrides: off` produces too.
	if !strings.Contains(got, skills.Header) {
		t.Error("the header was removed with the entries")
	}
	for _, gone := range []string{"dataviz:", "deep-research:", "ponytail:ponytail:"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived", gone)
		}
	}
	// Still valid JSON with the message in place.
	if !gjson.ValidBytes(out) {
		t.Fatal("the rewrite produced invalid JSON")
	}
	if gjson.GetBytes(out, "messages.1.role").String() != "system" {
		t.Error("the listing message lost its role")
	}
}

// TestFilterSkillReadsABlockArrayListing: the same listing arriving as content BLOCKS rather
// than a bare string. Real traffic sends a string, so this is the shape that would rot
// unnoticed — and the path it needs (`messages.N.content.M.text`) is a different sjson write.
func TestFilterSkillReadsABlockArrayListing(t *testing.T) {
	blocks := strings.Replace(skillBody,
		`{"role":"system","content":"<system-reminder>`,
		`{"role":"system","content":[{"type":"text","text":"<system-reminder>`, 1)
	blocks = strings.Replace(blocks, `</system-reminder>"},`, `</system-reminder>"}]},`, 1)
	if !gjson.Valid(blocks) {
		t.Fatal("the fixture edit produced invalid JSON")
	}
	out, tok, n := filterSkillListing([]byte(blocks), []string{skills.RemovePrefix + "dataviz"})
	if n != 1 || tok <= 0 {
		t.Fatalf("removed %d entries / %d tokens from a block-array listing, want 1 and >0", n, tok)
	}
	if strings.Contains(listingOf(t, out), "Use for charts") {
		t.Error("the entry survived in the block-array shape")
	}
}

// TestRemoveSetsKeepsSkillEntriesOutOfTheToolNames is the guard in removeSets, which had none.
//
// A `skill__x` entry must not land in the tool-name set: it is removed from the listing prose by
// filterSkillListing, and leaving it in `names` would mean a tool literally called `skill__x` was
// dropped by a request to remove a SKILL called `x`. No such tool exists today, which is exactly
// why the guard needed a test rather than a comment — nothing would have noticed its removal.
func TestRemoveSetsKeepsSkillEntriesOutOfTheToolNames(t *testing.T) {
	names, servers := removeSets([]string{
		skills.RemovePrefix + "dataviz", "Workflow", "mcp__srv", "mcp__srv__tool",
	})
	if names[skills.RemovePrefix+"dataviz"] {
		t.Error("a skill entry reached the tool-name set; a tool of that name would be dropped by " +
			"a request to remove a skill")
	}
	for _, want := range []string{"Workflow", "mcp__srv__tool"} {
		if !names[want] {
			t.Errorf("%s is missing from the tool names", want)
		}
	}
	if !servers["srv"] {
		t.Error("the whole-server entry did not reach the server set")
	}
	if len(names) != 2 {
		t.Errorf("tool names = %v, want exactly the two tool entries", names)
	}
	// The end-to-end consequence: a tool whose NAME is the skill entry survives a skill removal.
	body := `{"model":"m","tools":[` +
		`{"name":"skill__dataviz","description":"a tool that happens to be called this","input_schema":{"type":"object"}},` +
		`{"name":"Bash","description":"run","input_schema":{"type":"object"}}],` +
		`"system":[{"type":"text","text":"You are an agent."}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	out, tok, n := filterDeclarations([]byte(body), []string{skills.RemovePrefix + "dataviz"})
	if n != 0 || tok != 0 || string(out) != body {
		t.Errorf("a skill removal dropped %d tool declarations (%d tokens)", n, tok)
	}
}
