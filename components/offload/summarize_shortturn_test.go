package offload

import (
	"context"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// A transcript SHORTER THAN keep_last must not panic.
//
// summarizeSpan computes end = len(msgs) - keepLast and then walks forward past tool messages,
// indexing msgs[end]. A negative end still satisfies `end < len(msgs)`, so the walk read msgs[-1]
// and panicked. With the default keep_last: 3 that is any request with fewer than three messages —
// the first turn or two of every session, not an edge case.
//
// It was invisible because pipeline.runOne recovers per component: the panic surfaced only as
// verdict=reverted while summarize silently did nothing on short turns. The tests written with the
// tool-atomicity fix all use fixtures deliberately long enough to summarize, so none of them
// exercised "too short to act on yet". Found in review against live sessions.
func TestSummarizeSpanDoesNotPanicOnShortTranscripts(t *testing.T) {
	// The reduced case from the review: two messages, default keep_last.
	msgs := []bschemas.ChatMessage{userMsg("hi"), assistantMsg("hello")}
	for _, keepLast := range []int{1, 2, 3, 5, 20} {
		headCount, start, end := summarizeSpan(msgs, 1, keepLast)
		if end < start {
			t.Errorf("keepLast=%d: end=%d below start=%d; Offload's `end <= start` guard "+
				"expects a clamped boundary", keepLast, end, start)
		}
		if end > len(msgs) || start > len(msgs) || headCount < 0 {
			t.Errorf("keepLast=%d: boundary out of range (head=%d start=%d end=%d len=%d)",
				keepLast, headCount, start, end, len(msgs))
		}
		// The whole point: for a transcript this short there is nothing to summarize, so the
		// span must be empty rather than merely in-range.
		if keepLast >= len(msgs) && end != start {
			t.Errorf("keepLast=%d >= len(msgs)=%d: span must be empty, got start=%d end=%d",
				keepLast, len(msgs), start, end)
		}
	}
	// An empty transcript must be handled too — the head probe indexes msgs[0].
	if _, start, end := summarizeSpan(nil, 1, 3); end != start {
		t.Errorf("nil transcript: span must be empty, got start=%d end=%d", start, end)
	}
}

// End to end through Offload, since that is where the panic was actually observed: it must
// decline a short turn rather than panic, and leave the request untouched.
func TestSummarizeDeclinesShortTurnWithoutPanicking(t *testing.T) {
	comp, err := newSummarize([]byte("keep_last: 3\nstart_from_message: 0\nmin_tokens: 1\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s := comp.(*Summarize)
	model := &silentModel{}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("hi"), assistantMsg("hello"),
	}}
	rep := components.Report{}
	ctx := &components.Ctx{
		Session: "s", Ctx: context.Background(),
		Store: store.NewMemory(store.Options{}), CtxWindow: 1_000_000,
		Model: components.ModelSpec{Static: model, Incoming: model},
	}
	// No recover() here on purpose: a panic must fail this test, not be absorbed the way
	// pipeline.runOne absorbs it in production.
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("offload: %v", err)
	}
	if !rep.Skipped {
		t.Errorf("a 2-message request must be skipped, got rep=%+v", rep)
	}
	if len(req.Input) != 2 {
		t.Errorf("a declined turn must leave the request untouched, got %d messages",
			len(req.Input))
	}
}
