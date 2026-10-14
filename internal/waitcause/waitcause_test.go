package waitcause

import (
	"fmt"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func anthropicBody(msgs string) []byte {
	return []byte(`{"model":"claude-sonnet-5","messages":[` + msgs + `]}`)
}

func userText(text string) string {
	return fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q}]}`, text)
}

func userToolResult(toolUseID, text string) string {
	return fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}`, toolUseID, text)
}

// userToolResultWithSibling is the real Anthropic wire shape issue #457/PR #460 fixed:
// one user message mixing a tool_result block with another, non-tool_result block (mid-turn
// user text, a system reminder, a task/teammate notification). apply.NormalizeMessages (via
// apply.normalize) now splits this into TWO normalized messages, in wire order: the extracted
// role=tool message, then a synthetic role=user message for the sibling text. Before #460,
// the sibling was silently dropped.
func userToolResultWithSibling(toolUseID, resultText, siblingText string) string {
	return fmt.Sprintf(
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q},{"type":"text","text":%q}]}`,
		toolUseID, resultText, siblingText)
}

func assistantToolUse(id, name, input string) string {
	if input == "" {
		input = "{}"
	}
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}`, id, name, input)
}

// --- ClassifyWaitingFor ---

func TestClassifyWaitingFor(t *testing.T) {
	cases := []struct {
		name         string
		toolUseNames []string
		stopReason   string
		failed       bool
		usageOK      bool
		openBG       int
		openTM       int
		wantFor      string
		wantTool     string
	}{
		{"own_tool, last of several", []string{"Read", "Bash"}, "tool_use", false, true, 0, 0, WaitingOwnTool, "Bash"},
		{"single tool", []string{"Write"}, "tool_use", false, true, 0, 0, WaitingOwnTool, "Write"},
		{"AskUserQuestion is human, not own_tool", []string{"AskUserQuestion"}, "tool_use", false, true, 0, 0, WaitingHuman, ""},
		{"ExitPlanMode is human", []string{"ExitPlanMode"}, "tool_use", false, true, 0, 0, WaitingHuman, ""},
		{"ask tool alongside another tool still human", []string{"Bash", "AskUserQuestion"}, "tool_use", false, true, 0, 0, WaitingHuman, ""},
		{"end_turn, nothing open -> human", nil, "end_turn", false, true, 0, 0, WaitingHuman, ""},
		{"end_turn, background open -> background", nil, "end_turn", false, true, 1, 0, WaitingBackground, ""},
		{"end_turn, both open -> background wins", nil, "end_turn", false, true, 1, 1, WaitingBackground, ""},
		{"end_turn, teammate open -> teammate", nil, "end_turn", false, true, 0, 1, WaitingTeammate, ""},
		{"failed call -> retry, regardless of tools", []string{"Bash"}, "", true, false, 0, 0, WaitingRetry, ""},
		{"nothing at all -> unknown", nil, "", false, false, 0, 0, WaitingUnknown, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, tool := ClassifyWaitingFor(c.toolUseNames, c.stopReason, c.failed, c.usageOK, c.openBG, c.openTM)
			if got != c.wantFor || tool != c.wantTool {
				t.Fatalf("got (%s, %q), want (%s, %q)", got, tool, c.wantFor, c.wantTool)
			}
		})
	}
}

// --- wake_cause, via Tracker.Observe ---

func TestObserveWakeCauseHuman(t *testing.T) {
	tr := New()
	body := anthropicBody(userText("please fix the bug"))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeHuman)
	}
}

func TestObserveWakeCauseSystem(t *testing.T) {
	tr := New()
	body := anthropicBody(userText("<system-reminder>\nContext low.\n</system-reminder>"))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeSystem {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeSystem)
	}
}

func TestObserveWakeCauseToolResult(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Bash", `{"command":"ls"}`),
		userToolResult("call1", "file1\nfile2"),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeToolResult {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeToolResult)
	}
}

// A bare tool_result answering AskUserQuestion is a HUMAN wake, never tool_result — PR #425
// review finding 1, and issue #424's own exception.
func TestObserveWakeCauseAskUserQuestionException(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "AskUserQuestion", `{"questions":[]}`),
		userToolResult("call1", "the user picked option B"),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeHuman)
	}
}

func TestObserveWakeCauseExitPlanModeException(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "ExitPlanMode", `{"plan":"..."}`),
		userToolResult("call1", "approved"),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeHuman)
	}
}

func TestObserveWakeCauseTeammate(t *testing.T) {
	tr := New()
	body := anthropicBody(userText(`<teammate-message teammate_id="bob">hi</teammate-message>`))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeTeammate {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeTeammate)
	}
}

func TestObserveWakeCauseCrossSession(t *testing.T) {
	tr := New()
	body := anthropicBody(userText(`<cross-session-message from-name="reviewer">done</cross-session-message>`))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeTeammate {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeTeammate)
	}
}

func TestObserveWakeCauseBackground(t *testing.T) {
	tr := New()
	body := anthropicBody(userText(`<task-notification><task-id>bg1</task-id><event>done</event></task-notification>`))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeBackground {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeBackground)
	}
}

// TestObserveWakeCauseAgentMessageIsBackground pins a real defect found live (#453's own
// live test): a backgrounded Agent's hand-back wakes the thread with ONLY an
// `<agent-message from="X">...</agent-message>` tag (plus the harness's own boilerplate
// explaining what it is) — no `<task-notification>`, no plain human text. Before this test
// existed, classifyWake read that as plain, unrecognized text (hasPlainText) and reported
// WakeHuman instead of WakeBackground.
func TestObserveWakeCauseAgentMessageIsBackground(t *testing.T) {
	tr := New()
	body := anthropicBody(userText(`Another Claude session sent a message:
<agent-message from="acbdd7e94f0c14fa7">
status: all good
</agent-message>

That "other Claude session" is an agent working inside this same session.`))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeBackground {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeBackground)
	}
}

func TestObserveWakeCauseRetry(t *testing.T) {
	tr := New()
	body := anthropicBody(userText("please fix the bug"))
	// First request: a normal human wake, status 502 (failed).
	tr.Observe("t", "s", "", bschemas.Anthropic, body)
	tr.RecordOutcome("t", "s", "", 1, 502)
	// Same body resent (same message count), after a non-200 status: retry wins over the
	// content classification that would otherwise say "human".
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeRetry {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeRetry)
	}
}

func TestObserveWakeCauseNoRetryAfterSuccess(t *testing.T) {
	tr := New()
	body := anthropicBody(userText("please fix the bug"))
	tr.Observe("t", "s", "", bschemas.Anthropic, body)
	tr.RecordOutcome("t", "s", "", 1, 200)
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s (a successful previous call is not a retry)", obs.WakeCause, WakeHuman)
	}
}

func TestObserveWakeCauseNoRetryWhenMessageCountChanged(t *testing.T) {
	tr := New()
	body1 := anthropicBody(userText("please fix the bug"))
	tr.Observe("t", "s", "", bschemas.Anthropic, body1)
	tr.RecordOutcome("t", "s", "", 1, 502)
	body2 := anthropicBody(strings.Join([]string{userText("please fix the bug"), userText("actually never mind")}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body2)
	if obs.WakeCause == WakeRetry {
		t.Fatalf("got retry, want a content-based cause: a growing conversation is not a resend")
	}
}

// --- open background tasks ---

func TestOpenBackgroundBash(t *testing.T) {
	tr := New()
	opening := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Bash", `{"command":"sleep 60","run_in_background":true}`),
		userToolResult("call1", "Command running in background with ID: bash1."),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, opening)
	if obs.OpenBackground != 1 {
		t.Fatalf("got %d open background tasks, want 1", obs.OpenBackground)
	}

	closing := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Bash", `{"command":"sleep 60","run_in_background":true}`),
		userToolResult("call1", "Command running in background with ID: bash1."),
		userText(`<task-notification><task-id>bash1</task-id><event>finished</event></task-notification>`),
	}, ","))
	obs = tr.Observe("t", "s", "", bschemas.Anthropic, closing)
	if obs.OpenBackground != 0 {
		t.Fatalf("got %d open background tasks after notification, want 0", obs.OpenBackground)
	}
}

func TestOpenBackgroundMonitorMidWatchIsNotAFinish(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Monitor", `{"description":"watch"}`),
		userToolResult("call1", "Monitor started (task mon1, expires in 15m unless the source ends first; you get one notice at expiry). You will be notified on each event."),
		userText(`<task-notification><task-id>mon1</task-id><summary>Monitor event: a line matched</summary></task-notification>`),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.OpenBackground != 1 {
		t.Fatalf("got %d, want 1: a Monitor's mid-watch event must not close it", obs.OpenBackground)
	}
	if obs.MonitorExpiryMs == 0 {
		t.Fatalf("want a nonzero Monitor expiry")
	}
}

func TestOpenBackgroundMonitorExpiredNoticeCloses(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Monitor", `{"description":"watch"}`),
		userToolResult("call1", "Monitor started (task mon1, expires in 15m unless the source ends first)."),
		userText(`<task-notification><task-id>mon1</task-id><summary>Monitor event: expired</summary>[Monitor expired after 15m]</task-notification>`),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.OpenBackground != 0 {
		t.Fatalf("got %d, want 0: the expired notice closes it", obs.OpenBackground)
	}
}

func TestOpenBackgroundMonitorAbsoluteExpiry(t *testing.T) {
	tr := New()
	tr.now = func() time.Time { return time.Unix(1000, 0) }
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Monitor", `{"description":"watch"}`),
		userToolResult("call1", "Monitor started (task mon1, expires in 1m unless the source ends first; you get one notice at expiry)."),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.OpenBackground != 1 {
		t.Fatalf("got %d open at t=1000, want 1 (not yet expired)", obs.OpenBackground)
	}
	// Advance past the 1-minute expiry: the same body, resent, should now read as closed by
	// expiry alone, with no notification at all.
	tr.now = func() time.Time { return time.Unix(1065, 0) }
	obs = tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.OpenBackground != 0 {
		t.Fatalf("got %d open at t=1065, want 0 (expiry elapsed)", obs.OpenBackground)
	}
}

func TestOpenBackgroundAsyncAgent(t *testing.T) {
	tr := New()
	opening := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Agent", `{"run_in_background":true}`),
		userToolResult("call1", "Async agent launched successfully. Track it with Monitor. agentId: agent1"),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, opening)
	if obs.OpenBackground != 1 {
		t.Fatalf("got %d, want 1", obs.OpenBackground)
	}
	closing := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Agent", `{"run_in_background":true}`),
		userToolResult("call1", "Async agent launched successfully. Track it with Monitor. agentId: agent1"),
		userText(`<agent-message from="agent1">done</agent-message>`),
	}, ","))
	obs = tr.Observe("t", "s", "", bschemas.Anthropic, closing)
	if obs.OpenBackground != 0 {
		t.Fatalf("got %d after agent-message, want 0", obs.OpenBackground)
	}
}

func TestOpenBackgroundSkillAnonymous(t *testing.T) {
	tr := New()
	opening := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Skill", `{"name":"code-review"}`),
		userToolResult("call1", `Skill "code-review" launched (forked execution, running in the background).`),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, opening)
	if obs.OpenBackground != 1 {
		t.Fatalf("got %d, want 1 (anonymous skill open)", obs.OpenBackground)
	}
	closing := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Skill", `{"name":"code-review"}`),
		userToolResult("call1", `Skill "code-review" launched (forked execution, running in the background).`),
		userText(`<task-notification><task-id>whatever-the-real-id-is</task-id><event>done</event></task-notification>`),
	}, ","))
	obs = tr.Observe("t", "s", "", bschemas.Anthropic, closing)
	if obs.OpenBackground != 0 {
		t.Fatalf("got %d after an unmatched notification, want 0 (closes the oldest anonymous open)", obs.OpenBackground)
	}
}

func TestOpenBackgroundTaskStop(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Bash", `{"run_in_background":true}`),
		userToolResult("call1", "Command running in background with ID: bash1."),
		assistantToolUse("call2", "TaskStop", `{"task_id":"bash1"}`),
		userToolResult("call2", "stopped"),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.OpenBackground != 0 {
		t.Fatalf("got %d, want 0: TaskStop closes the task", obs.OpenBackground)
	}
}

// --- open teammate waits ---

func TestOpenTeammateWait(t *testing.T) {
	tr := New()
	opening := anthropicBody(assistantToolUse("call1", "SendMessage", `{"to":"bob","message":"status?"}`))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, opening)
	if obs.OpenTeammate != 1 {
		t.Fatalf("got %d, want 1", obs.OpenTeammate)
	}
	closing := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "SendMessage", `{"to":"bob","message":"status?"}`),
		userText(`<teammate-message teammate_id="bob">done</teammate-message>`),
	}, ","))
	obs = tr.Observe("t", "s", "", bschemas.Anthropic, closing)
	if obs.OpenTeammate != 0 {
		t.Fatalf("got %d after a reply, want 0", obs.OpenTeammate)
	}
}

// TestTeammateReplyClosesAnOpenWaitAndWakesTheThread is the full lifecycle in one test: the
// row that opens a teammate wait (own_tool/SendMessage, then — once the TURN ends with
// nothing else to do — waiting_for=teammate for whatever responds next) and the row whose
// `<teammate-message>` reply closes it (wake_cause=teammate, open_teammate back to 0). Not
// driven live (per policy: no live tests on an individual PR — see the PR body), covered here
// instead.
func TestTeammateReplyClosesAnOpenWaitAndWakesTheThread(t *testing.T) {
	tr := New()

	// The row whose OWN response calls SendMessage: own_tool/SendMessage, nothing open yet
	// (as of ITS OWN body, which has not seen the send happen yet).
	sendBody := anthropicBody(userText("ping bob for status"))
	before := tr.Observe("t", "s", "", bschemas.Anthropic, sendBody)
	if before.OpenTeammate != 0 {
		t.Fatalf("open_teammate before the send = %d, want 0", before.OpenTeammate)
	}
	wf, wt := ClassifyWaitingFor([]string{"SendMessage"}, "tool_use", false, true, before.OpenBackground, before.OpenTeammate)
	if wf != WaitingOwnTool || wt != "SendMessage" {
		t.Fatalf("got waiting_for=%s wait_tool=%s, want own_tool/SendMessage", wf, wt)
	}

	// The NEXT row (the turn that follows the send with no further tool call): open_teammate
	// is now 1, so an end_turn response waits on the teammate.
	open := anthropicBody(strings.Join([]string{
		userText("ping bob for status"),
		assistantToolUse("call1", "SendMessage", `{"to":"bob","message":"status?"}`),
		userToolResult("call1", "Message queued for bob."),
	}, ","))
	mid := tr.Observe("t", "s", "", bschemas.Anthropic, open)
	if mid.OpenTeammate != 1 {
		t.Fatalf("open_teammate while waiting = %d, want 1", mid.OpenTeammate)
	}
	wf, _ = ClassifyWaitingFor(nil, "end_turn", false, true, mid.OpenBackground, mid.OpenTeammate)
	if wf != WaitingTeammate {
		t.Fatalf("got waiting_for=%s, want %s", wf, WaitingTeammate)
	}

	// The row whose body now carries bob's reply: wake_cause=teammate, and the wait is closed.
	closed := anthropicBody(strings.Join([]string{
		userText("ping bob for status"),
		assistantToolUse("call1", "SendMessage", `{"to":"bob","message":"status?"}`),
		userToolResult("call1", "Message queued for bob."),
		userText(`<teammate-message teammate_id="bob">All good here.</teammate-message>`),
	}, ","))
	after := tr.Observe("t", "s", "", bschemas.Anthropic, closed)
	if after.WakeCause != WakeTeammate {
		t.Fatalf("got wake_cause=%s, want %s", after.WakeCause, WakeTeammate)
	}
	if after.OpenTeammate != 0 {
		t.Fatalf("open_teammate after the reply = %d, want 0", after.OpenTeammate)
	}
}

func TestOpenTeammateExpiresAfterCap(t *testing.T) {
	tr := New()
	tr.now = func() time.Time { return time.Unix(0, 0) }
	body := anthropicBody(assistantToolUse("call1", "SendMessage", `{"to":"bob","message":"status?"}`))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.OpenTeammate != 1 {
		t.Fatalf("got %d, want 1 right after sending", obs.OpenTeammate)
	}
	tr.now = func() time.Time { return time.Unix(0, 0).Add(teammateWaitCap + time.Minute) }
	obs = tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.OpenTeammate != 0 {
		t.Fatalf("got %d after the ping cap, want 0: an unanswered send cannot mark every later wait teammate forever", obs.OpenTeammate)
	}
}

// --- a `role: "system"` noise entry after the real wake signal (found live, driving #424's
// own live test: this org's Claude Code build appends a bare {"role":"system","content":
// "<total_tokens>...tokens left</total_tokens>"} entry after EVERY turn, including right
// after the tool_result or human text that is the actual wake signal) ---

func systemNoise() string {
	return `{"role":"system","content":"<total_tokens>15000000 tokens left</total_tokens>"}`
}

func TestObserveWakeCauseToolResultBehindTrailingSystemNoise(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Bash", `{"command":"ls"}`),
		userToolResult("call1", "file1\nfile2"),
		systemNoise(),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeToolResult {
		t.Fatalf("got %q, want %s (trailing system noise must not hide the real tool_result)", obs.WakeCause, WakeToolResult)
	}
}

func TestObserveWakeCauseHumanBehindTrailingSystemNoise(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		userText("please fix the bug"),
		systemNoise(),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeHuman)
	}
}

func TestObserveWakeCauseAskHumanExceptionBehindTrailingSystemNoise(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "AskUserQuestion", `{"questions":[]}`),
		userToolResult("call1", "option B"),
		systemNoise(),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeHuman)
	}
}

// --- a sibling block beside a tool_result in the SAME wire message (issue #457/PR #460:
// apply.normalize used to drop these; it now splits them into a synthetic role=user message,
// in wire order, which is what internal/waitcause now reads via apply.NormalizeMessages) ---

func TestObserveWakeCauseHumanBesideAToolResultSibling(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Bash", `{"command":"ls"}`),
		userToolResultWithSibling("call1", "file1\nfile2", "while that ran, also check the second file"),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s (mid-turn user text beside a tool_result is still a human wake)", obs.WakeCause, WakeHuman)
	}
}

func TestObserveWakeCauseBackgroundBesideAToolResultSibling(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Bash", `{"command":"ls"}`),
		userToolResultWithSibling("call1", "file1\nfile2",
			`<task-notification><task-id>bg1</task-id><event>done</event></task-notification>`),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeBackground {
		t.Fatalf("got %q, want %s (a task-notification sibling still wins over the tool_result)", obs.WakeCause, WakeBackground)
	}
}

// TestObserveWakeCauseSystemNotHumanBesideAToolResultSibling is the case the user explicitly
// asked be checked: a sibling that is ONLY a `<system-reminder>` block (no other text) must
// not read as human just because its role is "user" (apply.normalize's synthetic sibling
// message is always role=user, never role=system — see TestNormalizeKeepsSiblingTextBlocksInOrder
// in apply/normalizer_siblings_test.go). isNoiseEntry's own role check does not catch this (the
// sibling's role genuinely is "user"); hasSystemMarker's CONTENT check is what has to, and
// already does, independent of role.
func TestObserveWakeCauseSystemNotHumanBesideAToolResultSibling(t *testing.T) {
	tr := New()
	body := anthropicBody(strings.Join([]string{
		assistantToolUse("call1", "Bash", `{"command":"ls"}`),
		userToolResultWithSibling("call1", "file1\nfile2", "<system-reminder>\nContext low.\n</system-reminder>"),
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, body)
	if obs.WakeCause != WakeSystem {
		t.Fatalf("got %q, want %s (a system-reminder-only sibling must not read as human)", obs.WakeCause, WakeSystem)
	}
}

// --- fail-open ---

func TestObserveFailsOpenOnMalformedBody(t *testing.T) {
	tr := New()
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, []byte(`{not json`))
	if obs.WakeCause != "" || obs.OpenBackground != 0 || obs.OpenTeammate != 0 {
		t.Fatalf("want the zero Observation on malformed input, got %+v", obs)
	}
}

func TestObserveNilTrackerFailsOpen(t *testing.T) {
	var tr *Tracker
	obs := tr.Observe("t", "s", "", bschemas.Anthropic, anthropicBody(userText("hi")))
	if obs.WakeCause != "" {
		t.Fatalf("want the zero Observation from a nil tracker, got %+v", obs)
	}
}

func TestObserveNoMessageList(t *testing.T) {
	tr := New()
	// A Responses request continuing server-held history: no usable message list.
	obs := tr.Observe("t", "s", "", bschemas.OpenAI, []byte(`{"previous_response_id":"resp_1","input":"go on"}`))
	if obs.WakeCause != "" || obs.MsgCount != 0 {
		t.Fatalf("want the zero Observation, got %+v", obs)
	}
}

// --- OpenAI chat dialect ---

func openAIChatBody(msgs string) []byte {
	return []byte(`{"model":"gpt-5","messages":[` + msgs + `]}`)
}

func TestObserveOpenAIChatToolResult(t *testing.T) {
	tr := New()
	body := openAIChatBody(strings.Join([]string{
		`{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":"Bash","arguments":"{}"}}]}`,
		`{"role":"tool","tool_call_id":"call1","content":"ok"}`,
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.OpenAI, body)
	if obs.WakeCause != WakeToolResult {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeToolResult)
	}
}

func TestObserveOpenAIChatHuman(t *testing.T) {
	tr := New()
	body := openAIChatBody(`{"role":"user","content":"please fix the bug"}`)
	obs := tr.Observe("t", "s", "", bschemas.OpenAI, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeHuman)
	}
}

func TestOpenAIChatWaitingForOwnTool(t *testing.T) {
	toolNames := []string{"lookup"}
	gotFor, gotTool := ClassifyWaitingFor(toolNames, "tool_calls", false, true, 0, 0)
	if gotFor != WaitingOwnTool || gotTool != "lookup" {
		t.Fatalf("got (%s,%s), want (%s, lookup)", gotFor, gotTool, WaitingOwnTool)
	}
}

// --- OpenAI Responses dialect ---

func responsesBody(items string) []byte {
	return []byte(`{"model":"gpt-5","input":[` + items + `]}`)
}

func TestObserveResponsesToolResult(t *testing.T) {
	tr := New()
	body := responsesBody(strings.Join([]string{
		`{"type":"function_call","call_id":"call1","name":"Bash","arguments":"{}"}`,
		`{"type":"function_call_output","call_id":"call1","output":"ok"}`,
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.OpenAI, body)
	if obs.WakeCause != WakeToolResult {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeToolResult)
	}
}

func TestObserveResponsesHuman(t *testing.T) {
	tr := New()
	body := responsesBody(`{"type":"message","role":"user","content":[{"type":"input_text","text":"please fix the bug"}]}`)
	obs := tr.Observe("t", "s", "", bschemas.OpenAI, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeHuman)
	}
}

func TestObserveResponsesAskHumanException(t *testing.T) {
	tr := New()
	body := responsesBody(strings.Join([]string{
		`{"type":"function_call","call_id":"call1","name":"AskUserQuestion","arguments":"{}"}`,
		`{"type":"function_call_output","call_id":"call1","output":"option B"}`,
	}, ","))
	obs := tr.Observe("t", "s", "", bschemas.OpenAI, body)
	if obs.WakeCause != WakeHuman {
		t.Fatalf("got %q, want %s", obs.WakeCause, WakeHuman)
	}
}

// --- latency, measured on a large real-shaped body ---

func BenchmarkObserveLargeBody(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(`{"model":"claude-sonnet-5","messages":[`)
	// ~2,000 turn pairs of a few hundred bytes each approximates the ~600 KB real body issue
	// #424 asks this be measured against.
	for i := 0; i < 2000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"role":"user","content":[{"type":"text","text":"turn %d, some filler text to approximate a real transcript line of a few hundred bytes each, repeated many times over a long session."}]}`, i)
		sb.WriteString(",")
		fmt.Fprintf(&sb, `{"role":"assistant","content":[{"type":"tool_use","id":"call%d","name":"Bash","input":{"command":"echo hi"}}]}`, i)
		sb.WriteString(",")
		fmt.Fprintf(&sb, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"call%d","content":"ok"}]}`, i)
	}
	sb.WriteString(`]}`)
	body := []byte(sb.String())
	b.Logf("body size: %d bytes", len(body))
	tr := New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Observe("t", "s", "", bschemas.Anthropic, body)
	}
}
