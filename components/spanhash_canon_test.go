package components

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func str(s string) *string { return &s }

func textMsg(role schemas.ChatMessageRole, s string) schemas.ChatMessage {
	return schemas.ChatMessage{Role: role, Content: &schemas.ChatMessageContent{ContentStr: str(s)}}
}

func blocksMsg(role schemas.ChatMessageRole, marked bool, texts ...string) schemas.ChatMessage {
	m := schemas.ChatMessage{Role: role, Content: &schemas.ChatMessageContent{}}
	for _, t := range texts {
		b := schemas.ChatContentBlock{Type: schemas.ChatContentBlockTypeText, Text: str(t)}
		if marked {
			b.CacheControl = &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral}
		}
		m.Content.ContentBlocks = append(m.Content.ContentBlocks, b)
	}
	return m
}

// Stored span hashes (#783): a summary checkpoint and a held sweep answer store a SpanHash. A span
// with no content of exactly one plain text block must hash byte for byte as before, so nothing
// stored before the upgrade reads as new.
func TestSpanHashIsUnchangedWithoutAOneTextBlockContent(t *testing.T) {
	span := []schemas.ChatMessage{
		textMsg(schemas.ChatMessageRoleSystem, "sys"),
		textMsg(schemas.ChatMessageRoleUser, "hi"),
		blocksMsg(schemas.ChatMessageRoleAssistant, true, "a", "b"),
		{Role: schemas.ChatMessageRoleUser},
	}
	if SpanHash(span) != LegacySpanHash(span) {
		t.Fatal("SpanHash changed for a span with no one-text-block content")
	}
}

// A hash stored by the old rule over a one-text-block message still matches, on the same block
// and on the string a later request sends for it. It never matches a different span.
func TestSpanHashMatchesAcceptsAHashStoredByTheOldRule(t *testing.T) {
	block := []schemas.ChatMessage{blocksMsg(schemas.ChatMessageRoleUser, true, "go")}
	stored := LegacySpanHash(block)
	if stored == SpanHash(block) {
		t.Fatal("precondition: the old and the new hash of a one-text-block span must differ")
	}
	if !SpanHashMatches(block, stored) {
		t.Error("a checkpoint stored by the old rule read as a different span")
	}
	if !SpanHashMatches([]schemas.ChatMessage{textMsg(schemas.ChatMessageRoleUser, "go")}, SpanHash(block)) {
		t.Error("the string form of a one-text-block message read as a different span")
	}
	for _, other := range [][]schemas.ChatMessage{
		{textMsg(schemas.ChatMessageRoleUser, "stop")},
		{textMsg(schemas.ChatMessageRoleAssistant, "go")},
		{blocksMsg(schemas.ChatMessageRoleUser, false, "g", "o")},
	} {
		if SpanHashMatches(other, stored) || SpanHashMatches(other, SpanHash(block)) {
			t.Errorf("a different span matched: %+v", other[0])
		}
	}
}

// SpanHash copies, never mutates: the span is a live request's own messages.
func TestSpanHashDoesNotTouchTheMessage(t *testing.T) {
	m := blocksMsg(schemas.ChatMessageRoleUser, true, "go")
	_ = SpanHash([]schemas.ChatMessage{m})
	if m.Content.ContentStr != nil || len(m.Content.ContentBlocks) != 1 || m.Content.ContentBlocks[0].CacheControl == nil {
		t.Fatal("SpanHash changed the message it hashed")
	}
}

// Round 1, finding 1: a checkpoint stored by the old rule while its span held the then-newest
// message (keep_last_turns: 0). That message was a block with cache_control; on every later
// request it is a string. The stored hash must still match, in both directions, and never for a
// different span.
func TestSpanHashMatchesAcceptsAnOldHashAcrossTheBlockToStringChange(t *testing.T) {
	head := []schemas.ChatMessage{
		textMsg(schemas.ChatMessageRoleSystem, "sys"),
		textMsg(schemas.ChatMessageRoleUser, "hi"),
		textMsg(schemas.ChatMessageRoleAssistant, "ok"),
	}
	asBlock := append(append([]schemas.ChatMessage(nil), head...), blocksMsg(schemas.ChatMessageRoleUser, true, "go"))
	asString := append(append([]schemas.ChatMessage(nil), head...), textMsg(schemas.ChatMessageRoleUser, "go"))
	if !SpanHashMatches(asString, LegacySpanHash(asBlock)) {
		t.Error("an old checkpoint over the block form read as new once the newest message became a string")
	}
	if !SpanHashMatches(asBlock, LegacySpanHash(asString)) {
		t.Error("an old checkpoint over the string form read as new once the message became a block")
	}
	// Every message in block form, read back as strings.
	allBlocks := []schemas.ChatMessage{
		blocksMsg(schemas.ChatMessageRoleUser, false, "hi"),
		blocksMsg(schemas.ChatMessageRoleAssistant, false, "ok"),
	}
	allStrings := []schemas.ChatMessage{
		textMsg(schemas.ChatMessageRoleUser, "hi"),
		textMsg(schemas.ChatMessageRoleAssistant, "ok"),
	}
	if !SpanHashMatches(allStrings, LegacySpanHash(allBlocks)) {
		t.Error("an old checkpoint over all-block content read as new once every message became a string")
	}
	// Negative: the fallback never accepts a span the canonical rule rejects.
	for name, other := range map[string][]schemas.ChatMessage{
		"other text":  append(append([]schemas.ChatMessage(nil), head...), textMsg(schemas.ChatMessageRoleUser, "stop")),
		"other role":  append(append([]schemas.ChatMessage(nil), head...), textMsg(schemas.ChatMessageRoleAssistant, "go")),
		"two blocks":  append(append([]schemas.ChatMessage(nil), head...), blocksMsg(schemas.ChatMessageRoleUser, true, "g", "o")),
		"one shorter": head,
	} {
		if SpanHash(other) == SpanHash(asBlock) {
			t.Fatalf("%s: precondition: the canonical rule must reject it", name)
		}
		if SpanHashMatches(other, LegacySpanHash(asBlock)) {
			t.Errorf("%s: the fallback accepted a span the canonical rule rejects", name)
		}
	}
}
