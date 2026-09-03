package offload

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// gatedStore is a real store whose rewind reserve can be closed part-way through a test, so a
// SECOND turn can meet a saturated reserve while the first turn's checkpoint is still readable.
// That two-turn shape is the whole point: the defect only exists once a checkpoint has been
// emitted, because only then is there cached content for a refusal to flip.
type gatedStore struct {
	*store.Memory
	closed atomic.Bool
	puts   []string
}

func (g *gatedStore) Put(key string, payload []byte) {
	g.puts = append(g.puts, key)
	g.Memory.Put(key, payload)
}

// decisionWrites reports the writes that assert a removal HAPPENED — see decisionPrefixes.
func (g *gatedStore) decisionWrites() []string {
	var out []string
	for _, k := range g.puts {
		for _, p := range decisionPrefixes {
			if strings.HasPrefix(k, p) {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

func (g *gatedStore) PutStash(key string, payload []byte) bool {
	if g.closed.Load() {
		// A refresh of a payload that is PRESENT still succeeds, exactly as the real reserve
		// behaves — otherwise this fake would test a state the store cannot produce.
		if _, ok := g.Memory.Get(key); ok {
			return g.Memory.PutStash(key, payload)
		}
		return false
	}
	return g.Memory.PutStash(key, payload)
}

func (g *gatedStore) StashRoom(size int) bool {
	if g.closed.Load() {
		return false
	}
	return g.Memory.StashRoom(size)
}

// countingModel records how many summaries were asked for.
type countingModel struct {
	out   string
	calls int64
}

func (m *countingModel) Complete(context.Context, string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	return m.out, nil
}

// summarize must not pay for a summary it cannot checkpoint.
//
// The reserve check sat AFTER s.summarize, so a saturated reserve paid the model call — measured
// at ~57k prompt tokens — and threw the result away. And it did so on EVERY turn, because a
// refusal saves no checkpoint and therefore changes nothing about the next turn's inputs: the
// same span arrives, the same call is made, the same refusal follows. The span is all the marker
// key depends on (key = hashKey(spanJSON)), so nothing about the stash needs the summary to
// exist and the question can be asked first.
func TestSummarizeDoesNotPayForACheckpointItCannotStash(t *testing.T) {
	model := &countingModel{out: "SUMMARY: explored the handler, 3 tests fail."}
	s := newSummarizeKeepLast(t, 1)
	s.modelClient = model
	msgs := []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser},
		callMsg("t1"), bulkResult("t1"),
		callMsg("t2"), bulkResult("t2"),
		callMsg("t3"), bulkResult("t3"),
	}
	schema.SetMessageText(&msgs[0], "fix the failing tests")

	// PRECONDITION: with a healthy store this fixture DOES summarize, so a zero call count
	// below means the reserve check skipped it and not that the fixture never fires.
	healthy := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	var rep components.Report
	if _, err := s.Offload(healthy, &rep, ctxFor(store.NewMemory(store.Options{MaxEntries: 400}))); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&model.calls) != 1 {
		t.Fatalf("the fixture made %d model calls against a healthy store, want 1: it does not "+
			"reach the summarize path, so the assertion below would pass vacuously",
			atomic.LoadInt64(&model.calls))
	}

	// The property: a reserve with no room at all, and no call is paid.
	atomic.StoreInt64(&model.calls, 0)
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	rep = components.Report{}
	if _, err := s.Offload(req, &rep, ctxFor(store.NewMemory(store.Options{MaxEntries: 1}))); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&model.calls); n != 0 {
		t.Errorf("summarize paid %d model call(s) although the span could not be stashed. Under "+
			"a saturated reserve it re-pays that call on EVERY turn and refuses again each "+
			"time, because a refusal saves no checkpoint and so nothing about the next turn "+
			"changes", n)
	}
	if len(req.Input) != len(msgs) {
		t.Errorf("the transcript was restructured (%d messages, was %d) although the span it "+
			"replaced could not be stashed", len(req.Input), len(msgs))
	}
}

// A refusal must not flip content the provider has already cached.
//
// This is #188's own new refusal path producing the harm #188 exists to prevent. Once a
// checkpoint has been emitted, earlier turns sent [msg0, summary, tail]. When the tail later
// grows past resummarize_tokens, tryReuse declines and the fresh path runs — and a bare
// `return nil, nil` there sends the transcript FULL. The provider re-writes the whole suffix at
// ~11.5x the read price, for a request that saves nothing.
//
// The covered prefix is byte-unchanged (tryReuse verified its hash before the size test declined
// it), so the old checkpoint is still a faithful summary of it: re-emitting is byte-correct, and
// rolling it forward was merely an improvement that is now unavailable.
func TestSummarizeReplaysItsCheckpointRatherThanFlippingCachedContent(t *testing.T) {
	model := &countingModel{out: "SUMMARY: explored the handler, 3 tests fail."}
	c, err := newSummarize([]byte("keep_last: 1\nmin_tokens: 10\nresummarize_tokens: 200\n" +
		"trigger:\n  min_messages: 2\n  min_request_tokens: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.(*Summarize)
	s.modelClient = model
	st := &gatedStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	ctx := ctxFor(st)

	// --- Turn 1: a checkpoint is made and the compacted shape goes upstream.
	msgs := []bschemas.ChatMessage{{Role: bschemas.ChatMessageRoleUser}}
	schema.SetMessageText(&msgs[0], "fix the failing tests")
	for i := 1; i <= 3; i++ {
		id := "t" + strconv.Itoa(i)
		msgs = append(msgs, callMsg(id), bulkResult(id))
	}
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	var rep components.Report
	if _, err := s.Offload(turn1, &rep, ctx); err != nil {
		t.Fatal(err)
	}
	if len(turn1.Input) >= len(msgs) {
		t.Fatalf("turn 1 did not summarize (%d messages, was %d), so there is no checkpoint and "+
			"no cached shape for a refusal to flip", len(turn1.Input), len(msgs))
	}
	summaryShape := len(turn1.Input)
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatal("turn 1 saved no checkpoint, so the fallback under test has nothing to fall back to")
	}

	// --- Turn 2: the tail has grown past resummarize_tokens, so the checkpoint is STALE but
	// still valid — and the reserve is now closed, so no new checkpoint can be made.
	grown := append([]bschemas.ChatMessage(nil), msgs...)
	for i := 4; i <= 9; i++ {
		id := "t" + strconv.Itoa(i)
		grown = append(grown, callMsg(id), bulkResult(id))
	}
	st.closed.Store(true)
	turn2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), grown...)}
	rep = components.Report{}
	if _, err := s.Offload(turn2, &rep, ctx); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["stash_reserve_exhausted"] == 0 {
		t.Fatalf("turn 2 did not refuse for want of a reserve slot, so the path under test never "+
			"ran (gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	// The property. Sending the full transcript is the flip; re-emitting the checkpoint is not.
	if len(turn2.Input) == len(grown) {
		t.Errorf("the refusal sent the transcript FULL (%d messages): earlier turns sent the "+
			"summarized shape (%d messages), so this flips already-cached content and forces a "+
			"full-suffix cache write at ~11.5x the read price — #188's own refusal path causing "+
			"the harm #188 exists to prevent", len(turn2.Input), summaryShape)
	}
	// And it must be the SAME summary bytes, or the replay is itself a flip.
	if got := schema.MessageText(turn2.Input[1]); got != schema.MessageText(turn1.Input[1]) {
		t.Errorf("the replayed summary differs from the one earlier turns sent:\n got %q\nwant %q",
			got, schema.MessageText(turn1.Input[1]))
	}
	if n := atomic.LoadInt64(&model.calls); n != 1 {
		t.Errorf("the model was called %d time(s) in total, want 1: turn 2 must fall back to the "+
			"existing checkpoint, not pay for a summary it cannot store", n)
	}
	// A checkpoint that is absent or genuinely diverged has nothing to fall back to, and there
	// the full transcript is correct — nothing was ever cached in the summarized shape. Asserted
	// so the fallback cannot quietly become unconditional.
	fresh := ctxFor(&gatedStore{Memory: store.NewMemory(store.Options{MaxEntries: 1})})
	only := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), grown...)}
	rep = components.Report{}
	if _, err := s.Offload(only, &rep, fresh); err != nil {
		t.Fatal(err)
	}
	if len(only.Input) != len(grown) {
		t.Errorf("a session with NO checkpoint was rewritten to %d messages: there is no prior "+
			"shape to preserve, so the full transcript is the correct output and this fallback "+
			"has become unconditional", len(only.Input))
	}
}
