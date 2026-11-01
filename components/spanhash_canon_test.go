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
