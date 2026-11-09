package schema

import (
	"encoding/json"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// The canonical form of a message (#783): the one definition of "the same message" for every
// place that compares a message on one request with a message on a later one.
//
// A client changes an old message between requests in two ways the model cannot see, and the
// canonical form removes both:
//
//   - `cache_control`. It marks where the provider's prompt cache breaks. Claude Code puts it on
//     the newest message of each request, so it moves every turn.
//   - A plain-string `content` against a list of one text block. The provider renders the two the
//     same way, and its cache still hits. Claude Code sends the newest turn as a block (to carry
//     `cache_control`) and the same turn as a string once a later turn follows it.
//
// The representative of the string/block pair is the STRING. A list of exactly one block whose
// only fields are `type: "text"` and its `text` becomes that text. The other direction would
// define the same equality, but it would change the hash of every message that was always a
// string, and stored hashes would then all read as new. With this direction, a message that never
// carried a one-plain-text-block list hashes byte for byte as it did before.
//
// Everything else must match. Each caller keeps its own hash and its own extra rules on top.

// CanonicalMessage returns m in canonical form: every content block's cache_control removed, and
// a list of one plain text block written as its string. It copies rather than mutates: m may be a
// live request's own message, and clearing a field in place would remove the real breakpoint from
// the request itself.
func CanonicalMessage(m schemas.ChatMessage) schemas.ChatMessage {
	if m.Content == nil || len(m.Content.ContentBlocks) == 0 {
		return m
	}
	blocks := m.Content.ContentBlocks
	if len(blocks) == 1 && plainText(blocks[0]) {
		text := *blocks[0].Text
		m.Content = &schemas.ChatMessageContent{ContentStr: &text}
		return m
	}
	marked := false
	for i := range blocks {
		if blocks[i].CacheControl != nil {
			marked = true
			break
		}
	}
	if !marked {
		return m
	}
	content := *m.Content
	content.ContentBlocks = make([]schemas.ChatContentBlock, len(blocks))
	copy(content.ContentBlocks, blocks)
	for i := range content.ContentBlocks {
		content.ContentBlocks[i].CacheControl = nil
	}
	m.Content = &content
	return m
}

// plainText reports whether b is a text block and nothing else once cache_control is set aside:
// the block a provider renders exactly as the same text sent as a string.
func plainText(b schemas.ChatContentBlock) bool {
	return b.Type == schemas.ChatContentBlockTypeText && b.Text != nil &&
		b.Refusal == nil && b.ImageURLStruct == nil && b.InputAudio == nil && b.File == nil &&
		b.Citations == nil && b.CachePoint == nil
}

// CanonicalWireMessage returns the canonical JSON of one wire message (an element of `messages`,
// or of a Responses `input`): cache_control removed at every depth, a one-plain-text-block
// `content` written as its string (on the message, and on a `tool_result` block inside it), keys in
// sorted order, and numbers as their literal text. Decoded as float64, two integers above 2^53
// that differ would read as one, and so would the two messages. ok is false for bytes that are
// not JSON.
func CanonicalWireMessage(raw string) (b []byte, ok bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	v = dropCacheControl(v)
	if m, isObj := v.(map[string]any); isObj {
		collapseTextContent(m)
		if blocks, isArr := m["content"].([]any); isArr {
			for _, x := range blocks {
				if blk, isBlk := x.(map[string]any); isBlk && blk["type"] == "tool_result" {
					collapseTextContent(blk)
				}
			}
		}
	}
	b, err := json.Marshal(v) // encoding/json writes map keys in sorted order
	if err != nil {
		return nil, false
	}
	return b, true
}

func dropCacheControl(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		for k, x := range t {
			t[k] = dropCacheControl(x)
		}
	case []any:
		for i, x := range t {
			t[i] = dropCacheControl(x)
		}
	}
	return v
}

// collapseTextContent writes m's `content` as a string when it is a list of one plain text block.
func collapseTextContent(m map[string]any) {
	blocks, ok := m["content"].([]any)
	if !ok || len(blocks) != 1 {
		return
	}
	blk, ok := blocks[0].(map[string]any)
	if !ok || len(blk) != 2 || blk["type"] != "text" {
		return
	}
	if s, ok := blk["text"].(string); ok {
		m["content"] = s
	}
}
