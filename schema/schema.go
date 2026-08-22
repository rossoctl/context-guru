// Package schema wraps bifrost's provider-agnostic chat schema with the helpers
// context-guru components need: token accounting, deep-clone for fail-open
// snapshots, tool-result iteration, and byte-preservation for lossless
// round-trips of provider-specific fields.
//
// Components operate directly on *schemas.BifrostChatRequest (see package
// components); this package is the small toolbox around that type, not a
// competing model. Wire<->schema conversion lives in the host adapters — the
// bifrost proxy gets it for free from bifrost's transport, the AuthBridge
// plugin uses FromOpenAIBytes/FromAnthropicBytes (added in P3).
package schema

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// Provider identifies the wire dialect a request arrived in. It drives
// provider-specific behaviour (e.g. cache_control is Anthropic-family) and how
// the adapter renders bytes back out.
type Provider = schemas.ModelProvider

// MessagesTokens estimates the token cost of a request's messages by counting
// the message CONTENT text — what the model actually reads — not the JSON
// envelope. This is the signal the never-worse gate needs: control metadata
// like cache_control adds envelope bytes but no model-visible tokens, so a
// cache-injection Reformat must not look "worse" for adding it.
func MessagesTokens(req *schemas.BifrostChatRequest) int {
	if req == nil {
		return 0
	}
	n := 0
	for _, m := range req.Input {
		n += tokens.Count(MessageText(m))
	}
	return n
}

// TextTokens counts tokens in a raw string via the shared tokenizer.
func TextTokens(s string) int { return tokens.Count(s) }

// CloneMessages deep-copies a message slice via JSON round-trip. Used to
// snapshot a request before a component runs so the pipeline can restore it on
// error/panic or when a component fails the never-worse gate. JSON round-trip
// is safe because bifrost's content types implement custom (Un)MarshalJSON that
// preserve cache_control/citations/cachePoint.
func CloneMessages(in []schemas.ChatMessage) []schemas.ChatMessage {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return fallbackClone(in, err)
	}
	var out []schemas.ChatMessage
	if err := json.Unmarshal(b, &out); err != nil {
		return fallbackClone(in, err)
	}
	return out
}

// fallbackClone copies without JSON when the round-trip fails. Returning `in` (as this
// used to) handed the caller an ALIAS of the live slice: a component mutating a message
// in place mutated the snapshot too, so restoring it on error/panic/never-worse was a
// no-op and pipeline isolation was silently off for that component. The failure is
// reachable — bifrost's ChatMessageContent.MarshalJSON errors when a message carries
// both string and block content, which a component can produce.
//
// Nil would be louder, but the pipeline's revert paths assign this value straight back
// to req.Input, so nil would wipe the transcript. A real deep copy keeps isolation
// working and stays fail-open; the log line is the loud part.
//
// bifrost's deepCopyChatContentBlock (core@v1.7.0/schemas/utils.go) copies only
// Type/Text/Refusal/ImageURLStruct/InputAudio/File — it silently drops the three
// non-OpenAI block fields ChatContentBlock also carries: CacheControl, Citations and
// CachePoint. Losing cache_control here would DELETE a prompt-cache breakpoint on
// revert, i.e. exactly what cachesplit/cacheinject exist to put on the wire, and a
// missed breakpoint costs a full cache write at ~12.5x the cache-read price. So we
// restore them after the bifrost copy.
func fallbackClone(in []schemas.ChatMessage, err error) []schemas.ChatMessage {
	slog.Warn("context-guru: message clone fell back to a non-JSON deep copy", "err", err)
	out := make([]schemas.ChatMessage, len(in))
	for i := range in {
		out[i] = schemas.DeepCopyChatMessage(in[i])
		if in[i].Content == nil || out[i].Content == nil {
			continue
		}
		src, dst := in[i].Content.ContentBlocks, out[i].Content.ContentBlocks
		for j := range dst {
			if j >= len(src) {
				break
			}
			copyBlockCacheFields(&dst[j], src[j])
		}
	}
	return out
}

// copyBlockCacheFields deep-copies the cache/citation fields bifrost's block copier
// omits. Pointer values are cloned, not shared, so mutating the clone's breakpoint
// cannot reach back into the snapshot (and vice versa).
func copyBlockCacheFields(dst *schemas.ChatContentBlock, src schemas.ChatContentBlock) {
	if src.CacheControl != nil {
		cc := *src.CacheControl
		cc.TTL = clonep(src.CacheControl.TTL)
		cc.Scope = clonep(src.CacheControl.Scope)
		dst.CacheControl = &cc
	}
	if src.Citations != nil {
		c := *src.Citations
		c.Enabled = clonep(src.Citations.Enabled)
		dst.Citations = &c
	}
	if src.CachePoint != nil {
		cp := *src.CachePoint
		cp.TTL = clonep(src.CachePoint.TTL)
		dst.CachePoint = &cp
	}
}

func clonep[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// BlockText returns the text payload of a content block, or "" if the block
// carries no text (image/audio/file/refusal).
func BlockText(b schemas.ChatContentBlock) string {
	if b.Text != nil {
		return *b.Text
	}
	return ""
}

// MessageText returns the concatenated text of a message, handling both the
// string-content and block-content representations.
func MessageText(m schemas.ChatMessage) string {
	if m.Content == nil {
		return ""
	}
	if m.Content.ContentStr != nil {
		return *m.Content.ContentStr
	}
	blocks := m.Content.ContentBlocks
	// The common case, and free: one block means the block's own text, no copy at all.
	if len(blocks) == 1 {
		return BlockText(blocks[0])
	}
	// `s += BlockText(blk)` reallocated and re-copied the accumulation on every block,
	// which is O(n^2) in the message's own size. This is called for every tool message on
	// every turn, ahead of any size gate, by several components.
	n := 0
	for _, blk := range blocks {
		n += len(BlockText(blk))
	}
	var b strings.Builder
	b.Grow(n)
	for _, blk := range blocks {
		b.WriteString(BlockText(blk))
	}
	return b.String()
}

// SetMessageText replaces a message's content with a single text string,
// collapsing any block structure. Components that rewrite a whole message's
// text (e.g. cmdfilter, offload markers) use this.
func SetMessageText(m *schemas.ChatMessage, text string) {
	m.Content = &schemas.ChatMessageContent{ContentStr: &text}
}

// Rewritable reports whether m's content can be safely replaced with a plain
// text string (via SetMessageText) without losing data. It is false when the
// message carries any non-text content block — image/audio/file/refusal, or a
// block type bifrost does not model (e.g. Anthropic tool_result, whose payload
// lives in fields MessageText never reads). Those bytes would vanish on a
// text-only rewrite and were never stashed, so components must skip such
// messages. String content and all-text block content are rewritable.
func Rewritable(m schemas.ChatMessage) bool {
	if m.Content == nil || m.Content.ContentStr != nil {
		return true
	}
	for _, b := range m.Content.ContentBlocks {
		switch b.Type {
		case schemas.ChatContentBlockTypeText, "":
			// text (or an untyped block we treat as text) is safe
		default:
			return false
		}
	}
	return true
}

// SessionHead returns the two strings the derived session key is hashed from: the
// conversation's HEAD system text and its first user message.
//
// "Head" is load-bearing. The obvious reading — concatenate every system-role
// message — is what this replaced, and it is wrong for the Claude Agent SDK: that
// host APPENDS a fresh system-role message on every turn carrying its remaining-
// budget reminder (`<system-reminder><total_tokens>N tokens left</total_tokens>`),
// so the concatenation changed on every single request and each turn of one
// conversation derived a DIFFERENT key. Dedup memory, the extract_llm result cache
// and frozen offload decisions are all scoped to that key, so none of them ever
// accumulated past one turn.
//
// A conversation only ever grows at the TAIL, so its head is the one part that is
// immutable by construction. Taking the first system-role message (plus the first
// user message, already head-only) is therefore stable across turns without asking
// the host for anything. Later system-role messages are deliberately ignored: they
// are per-turn host injections, never conversation identity.
func SessionHead(msgs []schemas.ChatMessage) (sys, firstUser string) {
	for _, m := range msgs {
		switch m.Role {
		case schemas.ChatMessageRoleSystem:
			if sys == "" {
				sys = MessageText(m)
			}
		case schemas.ChatMessageRoleUser:
			if firstUser == "" {
				firstUser = MessageText(m)
			}
		}
	}
	return sys, firstUser
}
