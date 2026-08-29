package offload

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// THE FALLBACK'S GOAL MUST LEAD WITH THE CURRENT STEP, NOT THE OPENING INSTRUCTION.
//
// The fallback has no transcript by construction, so the goal string is the only thing that can tell
// it the task closed. conversationGoal leads with the FIRST user message, which for a spent-ness
// judgement is actively misleading: it describes what the session set out to do, i.e. exactly what
// may now be finished.
//
// MEASURED (#125): two near-identical transcripts, twelve candidates each — the prefix ask dropped
// 12 of 12, the fallback kept 12 of 12 and cited the original read instruction as the obligation for
// every one.
//
// The original instruction is still included, because criterion (b) is an unfinished USER
// instruction and a standing "…and summarise them at the end" lives in that message. What changes is
// its POSITION and that it is flagged as possibly satisfied. Dropping it would trade a bias toward
// keeping for a bias toward dropping, which is the direction that loses content.
func TestSweepIntentLeadsWithTheCurrentStepNotTheOpeningInstruction(t *testing.T) {
	const opening = "Read every CSV under data/ and tell me the row count in each"
	const latest = "thanks, that is all I needed on the counts"
	const asstLast = "I will now write the summary to report.md"
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg(opening),
		toolResultMsg(strings.Repeat("data/a.csv 1200 rows\n", 400)),
		assistantMsg(asstLast),
		userMsg(latest),
	}}

	got := sweepIntent(req)

	// All three parts must be present: dropping the opening instruction would lose criterion (b).
	for _, want := range []string{opening, latest, asstLast} {
		if !strings.Contains(got, want) {
			t.Fatalf("intent lost a part it needs: %q missing from\n%s", want, got)
		}
	}
	// THE FIX: the current step precedes the opening instruction. This is the assertion that fails if
	// the ordering regresses to conversationGoal's.
	if strings.Index(got, latest) > strings.Index(got, opening) {
		t.Errorf("the opening instruction precedes the current step, which is the ordering that "+
			"made the fallback keep everything:\n%s", got)
	}
	// And the opening instruction must be FLAGGED, or its position alone still leaves the model to
	// guess whether it is outstanding.
	if !strings.Contains(got, "MAY ALREADY BE SATISFIED") {
		t.Errorf("the opening instruction is not marked as possibly satisfied, so the model has no "+
			"way to tell a standing obligation from a finished one:\n%s", got)
	}
	// Each part must be labelled with which criterion it serves; an unlabelled blob is what the model
	// had to infer structure from before.
	for _, label := range []string{"MOST RECENT USER TURN", "THE AGENT'S OWN LAST STATEMENT"} {
		if !strings.Contains(got, label) {
			t.Errorf("part %q is unlabelled:\n%s", label, got)
		}
	}
}

// A transcript whose only user turn is the opening one must not repeat it twice under two labels —
// that would read as two independent obligations pointing at the same text.
func TestSweepIntentDoesNotDuplicateASingleUserTurn(t *testing.T) {
	const only = "summarize the log"
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg(only),
		toolResultMsg(strings.Repeat("line\n", 400)),
	}}
	got := sweepIntent(req)
	if n := strings.Count(got, only); n != 1 {
		t.Errorf("the single user turn appears %d times; it must appear once:\n%s", n, got)
	}
	if strings.Contains(got, "MAY ALREADY BE SATISFIED") {
		t.Errorf("the only user turn is also the current step, so it must not be flagged as "+
			"possibly satisfied:\n%s", got)
	}
}
