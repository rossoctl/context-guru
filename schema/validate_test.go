package schema

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func vsys(text string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleSystem}
	SetMessageText(&m, text)
	return m
}

func vuser(text string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	SetMessageText(&m, text)
	return m
}

func vasst(text string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant}
	SetMessageText(&m, text)
	return m
}

// vcall is an assistant turn carrying tool calls — one id per parallel call, exactly as
// apply.attachToolUse lifts an Anthropic assistant message's tool_use blocks.
func vcall(ids ...string) bschemas.ChatMessage {
	calls := make([]bschemas.ChatAssistantMessageToolCall, 0, len(ids))
	for i := range ids {
		id := ids[i]
		calls = append(calls, bschemas.ChatAssistantMessageToolCall{ID: &id})
	}
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: calls}}
}

// vresult is one synthetic role=tool message, apply.normalize's representation of a single
// tool_result block.
func vresult(id string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &id}}
	SetMessageText(&m, "output for "+id)
	return m
}

// rules returns the rule ids reported, so an assertion names the invariant rather than the
// provider's wording.
func rules(vs []ShapeViolation) string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Rule)
	}
	return strings.Join(out, ",")
}

func TestValidateShapeAcceptsWellFormedTranscripts(t *testing.T) {
	cases := []struct {
		name string
		msgs []bschemas.ChatMessage
	}{
		{"empty", nil},
		{"system prompt then a turn", []bschemas.ChatMessage{
			vsys("you are an agent"), vuser("go"), vasst("done")}},
		{"a serial tool exchange", []bschemas.ChatMessage{
			vsys("s"), vuser("go"), vcall("t1"), vresult("t1"), vuser("next")}},
		// The shape the first version of this check got wrong: one assistant message with two
		// tool_use blocks, answered by ONE user message on the wire, which normalizes to a RUN
		// of two role=tool messages.
		{"a parallel tool exchange", []bschemas.ChatMessage{
			vuser("go"), vcall("pa", "pb"), vresult("pa"), vresult("pb"), vuser("next")}},
		{"four parallel calls", []bschemas.ChatMessage{
			vuser("go"), vcall("a", "b", "c", "d"),
			vresult("a"), vresult("b"), vresult("c"), vresult("d")}},
		// Results may arrive in any order within the run; the provider pairs them by id.
		{"parallel results out of order", []bschemas.ChatMessage{
			vuser("go"), vcall("pa", "pb"), vresult("pb"), vresult("pa")}},
		// REAL, ACCEPTED live traffic: the Claude Agent SDK appends a system-role budget
		// reminder inside `messages` on every turn. apply's captured fixture carries them at
		// indices 1, 4 and 7. Rejecting these would make the validator fire on ordinary
		// traffic — see systemPositionOK.
		{"agent-sdk per-turn system reminders", []bschemas.ChatMessage{
			vuser("task"), vsys("<system-reminder>1000 tokens left</system-reminder>"),
			vasst("working"),
			vuser("more"), vsys("<system-reminder>900 tokens left</system-reminder>"),
			vasst("working"),
			vuser("more"), vsys("<system-reminder>800 tokens left</system-reminder>")}},
		// An idless tool message is this repo's generic "tool output" fixture shape and carries
		// no pairing claim, so it makes no assertion for the validator to break.
		{"tool output with no wire id", []bschemas.ChatMessage{
			vuser("go"), {Role: bschemas.ChatMessageRoleTool}}},
	}
	for _, tc := range cases {
		if got := ValidateShape(tc.msgs); len(got) != 0 {
			t.Errorf("%s: well-formed transcript reported %d violation(s):\n%s",
				tc.name, len(got), FormatShapeViolations(got, tc.msgs))
		}
	}
}

// The shape 2edb9d4 emitted: [msgs[0], summary(SYSTEM), tail...]. With the usual system
// prompt at index 0, the summary lands at index 1 in front of the kept tail and the provider
// rejects the whole request.
func TestValidateShapeRejectsSystemRoleInFrontOfTheKeptTail(t *testing.T) {
	msgs := []bschemas.ChatMessage{
		vsys("you are an agent"),
		vsys("=== History Summary ===\nearlier work"), // the pre-fix summary role
		vuser("keep going"),
	}
	got := ValidateShape(msgs)
	if len(got) != 1 || got[0].Rule != RuleSystemPosition || got[0].Index != 1 {
		t.Fatalf("want one system-position violation at index 1, got [%s]:\n%s",
			rules(got), FormatShapeViolations(got, msgs))
	}
	// The fix, and the only difference: role=user.
	fixed := []bschemas.ChatMessage{msgs[0], vuser("=== History Summary ===\nearlier work"), msgs[2]}
	if got := ValidateShape(fixed); len(got) != 0 {
		t.Errorf("the user-role summary must be accepted, got:\n%s", FormatShapeViolations(got, fixed))
	}
}

// The shape fb5c460 emitted: the span holding a call was replaced by the summary, so the kept
// tail begins with a tool_result whose tool_use is gone.
//
//	400 messages.0.content.2: unexpected `tool_use_id` found in `tool_result` blocks
func TestValidateShapeRejectsOrphanedToolResult(t *testing.T) {
	msgs := []bschemas.ChatMessage{
		vsys("s"),
		vuser("=== History Summary ===\nearlier work"),
		vresult("t1"), // its call was inside the summarized span
		vcall("t2"), vresult("t2"),
	}
	got := ValidateShape(msgs)
	if len(got) != 1 || got[0].Rule != RulePairedToolResult || got[0].Index != 2 {
		t.Fatalf("want one paired-tool-result violation at index 2, got [%s]:\n%s",
			rules(got), FormatShapeViolations(got, msgs))
	}
	if !strings.Contains(got[0].Msg, "t1") {
		t.Errorf("the violation must name the unanswerable id, got %q", got[0].Msg)
	}
}

// The shape e7d1aa8 emitted from the other side: msgs[0] was an assistant tool-call message,
// preserved as the head while its results sat inside the summarized span.
//
//	400 messages.N: `tool_use` ids were found without `tool_result` blocks immediately after
func TestValidateShapeRejectsUnansweredToolUse(t *testing.T) {
	msgs := []bschemas.ChatMessage{
		vcall("t9"), // preserved head; result("t9") was summarized away
		vuser("=== History Summary ===\nearlier work"),
		vuser("keep going"),
	}
	got := ValidateShape(msgs)
	if len(got) != 1 || got[0].Rule != RuleAnsweredToolUse || got[0].Index != 0 {
		t.Fatalf("want one answered-tool-use violation at index 0, got [%s]:\n%s",
			rules(got), FormatShapeViolations(got, msgs))
	}

	// A summary spliced BETWEEN a call and its result breaks the same rule: the run of tool
	// messages must be contiguous with the call, and a repair that merely keeps the result
	// somewhere later in the list does not make the request sendable.
	split := []bschemas.ChatMessage{
		vcall("t1"), vuser("=== History Summary ==="), vresult("t1"),
	}
	if got := ValidateShape(split); len(got) != 1 || got[0].Rule != RuleAnsweredToolUse {
		t.Errorf("a call answered at a DISTANCE must be reported, got [%s]:\n%s",
			rules(got), FormatShapeViolations(got, split))
	}

	// Only the unanswered id of a parallel call is reported — not its answered sibling.
	half := []bschemas.ChatMessage{vuser("go"), vcall("pa", "pb"), vresult("pa"), vuser("next")}
	got = ValidateShape(half)
	if len(got) != 1 || !strings.Contains(got[0].Msg, "pb") {
		t.Fatalf("want exactly one violation naming pb, got [%s]:\n%s",
			rules(got), FormatShapeViolations(got, half))
	}
}

// Both directions must be reported independently. e7d1aa8's finding was that they are one
// mistake seen from either side, and that fixing one alone leaves the other live — so a
// validator that stops at the first is the same trap again.
func TestValidateShapeReportsBothPairingDirections(t *testing.T) {
	msgs := []bschemas.ChatMessage{
		vcall("head"),            // unanswered call
		vuser("=== Summary ==="), //
		vresult("gone"),          // orphaned result
		vsys("mid-array system"), // followed by a user turn, not an assistant one
		vuser("keep going"),      //
	}
	got := ValidateShape(msgs)
	if len(got) != 3 {
		t.Fatalf("want all three violations, got [%s]:\n%s", rules(got), FormatShapeViolations(got, msgs))
	}
	want := map[string]bool{RuleAnsweredToolUse: true, RulePairedToolResult: true, RuleSystemPosition: true}
	for _, v := range got {
		delete(want, v.Rule)
	}
	if len(want) != 0 {
		t.Errorf("missing rules %v; got [%s]", want, rules(got))
	}
}

func TestShapeViolationString(t *testing.T) {
	if s := (ShapeViolation{Index: 3, Rule: "r", Msg: "m"}).String(); s != "messages.3 [r] m" {
		t.Errorf("indexed violation rendered %q", s)
	}
	if s := (ShapeViolation{Index: -1, Rule: "r", Msg: "m"}).String(); s != "[r] m" {
		t.Errorf("list-level violation rendered %q", s)
	}
}
