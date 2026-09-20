package offload

import (
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func pinCtx() *components.Ctx {
	return &components.Ctx{Session: "s1", Store: store.NewMemory(store.Options{})}
}

func msg(role bschemas.ChatMessageRole, text string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: role}
	schema.SetMessageText(&m, text)
	return m
}

// A big block whose only per-turn change is a counter — the exact shape observed in
// the Bob traces ("the THIRTY-SECOND time" -> "THIRTY-THIRD").
func scratchpad(n string) string {
	return "<scratchpad>\nThe user has given the same instruction for the " + n +
		" time.\n" + strings.Repeat("stable analysis line that does not change.\n", 200)
}

func turn(t *testing.T, p *Prefixpin, c *components.Ctx, msgs []bschemas.ChatMessage) ([]bschemas.ChatMessage, *components.Report) {
	t.Helper()
	req := &bschemas.BifrostChatRequest{Provider: bschemas.OpenAI, Input: msgs}
	rep := &components.Report{}
	if _, err := p.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	return req.Input, rep
}

func newPin(t *testing.T) *Prefixpin {
	t.Helper()
	comp, err := newPrefixpin(nil)
	if err != nil {
		t.Fatal(err)
	}
	return comp.(*Prefixpin)
}

// The core behaviour: a churning early block gets pinned back to its first
// rendering, so the prefix hashes identically and the provider cache still reads.
func TestPinsChurningEarlyBlock(t *testing.T) {
	p, c := newPin(t), pinCtx()
	first := scratchpad("FIRST")

	// turn 1: baseline recorded, nothing changed
	got, rep := turn(t, p, c, []bschemas.ChatMessage{
		msg(bschemas.ChatMessageRoleUser, first),
		msg(bschemas.ChatMessageRoleUser, "go"),
	})
	if !rep.Skipped || schema.MessageText(got[0]) != first {
		t.Fatal("turn 1 should only baseline, not modify")
	}

	// turn 2: churn observed once -- still below RepeatThreshold, so no pin yet
	second := scratchpad("SECOND")
	got, _ = turn(t, p, c, []bschemas.ChatMessage{
		msg(bschemas.ChatMessageRoleUser, second),
		msg(bschemas.ChatMessageRoleUser, "go"),
	})
	if schema.MessageText(got[0]) != second {
		t.Fatal("pinned on the first edit; a one-off edit must not be pinned")
	}

	// turn 3: churn is now an established pattern -> pin back to the first text
	third := scratchpad("THIRD")
	got, rep = turn(t, p, c, []bschemas.ChatMessage{
		msg(bschemas.ChatMessageRoleUser, third),
		msg(bschemas.ChatMessageRoleUser, "go"),
	})
	if rep.Skipped {
		t.Fatal("did not act on an established churn pattern -- this is the case that pays")
	}
	if schema.MessageText(got[0]) != first {
		t.Fatalf("did not pin to the first rendering; got %.60q", schema.MessageText(got[0]))
	}
}

// An append-only conversation is the common case (100% of claude-code's turns,
// 98% of Bob's). Prefixpin must be completely inert there.
func TestInertWhenPrefixStable(t *testing.T) {
	p, c := newPin(t), pinCtx()
	head := scratchpad("ONLY")
	for turnNo := 0; turnNo < 5; turnNo++ {
		msgs := []bschemas.ChatMessage{msg(bschemas.ChatMessageRoleUser, head)}
		for k := 0; k <= turnNo; k++ {
			msgs = append(msgs, msg(bschemas.ChatMessageRoleAssistant, "step"))
		}
		got, rep := turn(t, p, c, msgs)
		if !rep.Skipped {
			t.Fatalf("turn %d: acted on a stable prefix", turnNo)
		}
		if schema.MessageText(got[0]) != head {
			t.Fatalf("turn %d: mutated a stable message", turnNo)
		}
	}
}

// If a DIFFERENT message occupies the slot (the transcript was restructured rather
// than edited in place), pinning would substitute unrelated content. It must
// re-baseline instead.
func TestDoesNotPinUnrelatedContent(t *testing.T) {
	p, c := newPin(t), pinCtx()
	a := strings.Repeat("alpha content here.\n", 300)
	b := strings.Repeat("completely different beta text.\n", 300)
	for i := 0; i < 4; i++ {
		text := a
		if i > 0 {
			text = b
		}
		got, _ := turn(t, p, c, []bschemas.ChatMessage{
			msg(bschemas.ChatMessageRoleUser, text),
			msg(bschemas.ChatMessageRoleUser, "go"),
		})
		if i > 0 && schema.MessageText(got[0]) != b {
			t.Fatalf("turn %d: substituted unrelated content", i)
		}
	}
}

// The newest message is what the agent is actively reasoning about; pinning it
// would feed the model stale work. It must never be touched.
func TestNeverPinsNewestMessage(t *testing.T) {
	p, c := newPin(t), pinCtx()
	for i, name := range []string{"A", "B", "C", "D"} {
		msgs := []bschemas.ChatMessage{msg(bschemas.ChatMessageRoleUser, scratchpad(name))}
		got, _ := turn(t, p, c, msgs)
		if schema.MessageText(got[len(got)-1]) != scratchpad(name) {
			t.Fatalf("turn %d: modified the newest message", i)
		}
	}
}

// Small blocks are not worth the behavioural risk of showing stale text.
func TestSkipsSmallBlocks(t *testing.T) {
	p, c := newPin(t), pinCtx()
	for i, name := range []string{"A", "B", "C", "D"} {
		got, _ := turn(t, p, c, []bschemas.ChatMessage{
			msg(bschemas.ChatMessageRoleUser, "tiny "+name),
			msg(bschemas.ChatMessageRoleUser, "go"),
		})
		if schema.MessageText(got[0]) != "tiny "+name {
			t.Fatalf("turn %d: pinned a block below MinTokens", i)
		}
	}
}

// Deep messages must be left alone: only structurally-early slots are pinnable.
func TestOnlyPinsEarlyIndices(t *testing.T) {
	p, c := newPin(t), pinCtx()
	deep := 8 // beyond the default MaxPinIndex of 4
	for i, name := range []string{"A", "B", "C", "D"} {
		msgs := make([]bschemas.ChatMessage, 0, deep+2)
		for k := 0; k < deep; k++ {
			msgs = append(msgs, msg(bschemas.ChatMessageRoleUser, "filler message body"))
		}
		msgs = append(msgs, msg(bschemas.ChatMessageRoleUser, scratchpad(name)))
		msgs = append(msgs, msg(bschemas.ChatMessageRoleUser, "go"))
		got, _ := turn(t, p, c, msgs)
		if schema.MessageText(got[deep]) != scratchpad(name) {
			t.Fatalf("turn %d: pinned a message past MaxPinIndex", i)
		}
	}
}

// Lossy by design, so the withheld text MUST be recoverable: the current rendering
// is stashed under the returned key.
func TestStashesOriginalForExpand(t *testing.T) {
	p, c := newPin(t), pinCtx()
	texts := []string{scratchpad("ONE"), scratchpad("TWO"), scratchpad("THREE")}
	var keys []string
	for _, tx := range texts {
		req := &bschemas.BifrostChatRequest{Provider: bschemas.OpenAI, Input: []bschemas.ChatMessage{
			msg(bschemas.ChatMessageRoleUser, tx),
			msg(bschemas.ChatMessageRoleUser, "go"),
		}}
		k, err := p.Offload(req, &components.Report{}, c)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k...)
	}
	if len(keys) == 0 {
		t.Fatal("no rewind keys returned; the withheld text would be unrecoverable")
	}
	got, ok := c.Store.Get(keys[len(keys)-1])
	if !ok || string(got) != texts[len(texts)-1] {
		t.Fatal("stashed original does not match what was withheld")
	}
}

// REGRESSION, from the real trace. The churning block re-rendered its iteration
// counter in ~20 scattered places: 152 changed chars out of 6,024 (98.5% identical),
// but with edits near BOTH ends. A prefix+suffix overlap measure scored that 0.075
// and the guard rejected the exact case this component exists for. similarity must
// score it high.
func TestSimilarityToleratesScatteredEdits(t *testing.T) {
	mk := func(word, num string) string {
		var b strings.Builder
		b.WriteString("<scratchpad>\nInstruction seen for the " + word + " time.\n")
		for i := 0; i < 60; i++ {
			b.WriteString("stable reasoning line " + strconv.Itoa(i) + "\n")
		}
		// counter re-rendered in several forms, spread through the block
		for i := 0; i < 8; i++ {
			b.WriteString("iteration " + num + " of the " + strings.ToLower(word) + " pass\n")
			b.WriteString("more stable content line " + strconv.Itoa(i) + "\n")
		}
		b.WriteString("[DONE] Generate " + strings.ToLower(word) + " state snapshot\n")
		return b.String()
	}
	a, b := mk("THIRTY-SECOND", "32"), mk("THIRTY-THIRD", "33")
	if a == b {
		t.Fatal("fixture is not actually different")
	}
	s := similarity(a, b)
	if s < 0.80 {
		t.Fatalf("scattered-edit rewrite scored %.3f, below the 0.80 gate — the "+
			"component would skip the case it exists to fix", s)
	}
}

// similarity must actually separate the two cases it gates on.
func TestSimilarityDiscriminates(t *testing.T) {
	a := scratchpad("THIRTY-SECOND")
	b := scratchpad("THIRTY-THIRD")
	if s := similarity(a, b); s < 0.9 {
		t.Fatalf("counter-only edit scored %.3f; should be near 1", s)
	}
	c := strings.Repeat("totally unrelated text.\n", 300)
	if s := similarity(a, c); s > 0.2 {
		t.Fatalf("unrelated content scored %.3f; should be near 0", s)
	}
	if similarity("", "x") != 0 || similarity("x", "x") != 1 {
		t.Fatal("degenerate cases wrong")
	}
}

// No store must not panic; it simply cannot track slots.
func TestNoStoreIsSafe(t *testing.T) {
	p := newPin(t)
	req := &bschemas.BifrostChatRequest{Provider: bschemas.OpenAI, Input: []bschemas.ChatMessage{
		msg(bschemas.ChatMessageRoleUser, scratchpad("A")),
		msg(bschemas.ChatMessageRoleUser, "go"),
	}}
	rep := &components.Report{}
	if _, err := p.Offload(req, rep, &components.Ctx{}); err != nil {
		t.Fatal(err)
	}
	if !rep.Skipped {
		t.Fatal("should skip without a store")
	}
}
