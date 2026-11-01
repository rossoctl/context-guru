package schema

import (
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// blockMarks are the two spellings a breakpoint takes on an array ELEMENT: a system block, a
// tool, or a content block. The cap applies across every location together. The count stays
// STRUCTURAL (a field, per gjson): a tool output whose text merely contains "cache_control"
// must not count.
//
// `cachePoint` is the Bedrock Converse spelling. Bedrock places it as its OWN entry in the
// `system` and `tools` arrays, so both spellings must count at every location.
var blockMarks = [...]string{"cache_control", "cachePoint"}

// MayHoldBreakpoint is the cheap prefilter in front of every structural parse. A mark key can
// be a field of raw only if "cache" is a substring of raw, or if raw holds a \u escape (a JSON
// key may spell "cache" with one, and gjson decodes it). A plain substring scan is memchr-fast.
// gjson.Get on a message with 50 KB of tool output parses that text. The filter errs only
// toward doing the parse, never toward missing a real mark.
func MayHoldBreakpoint(raw string) bool {
	return strings.Contains(raw, "cache") || strings.Contains(raw, `\u`)
}

// Breakpoints is where a request's prompt-cache breakpoints sit, split by the location the
// provider hashes them at. `tools` and `system` render AHEAD of `messages`, so the location
// decides how much of the prefix a breakpoint protects.
type Breakpoints struct {
	System   int // system.#.cache_control / .cachePoint
	Tools    int // tools.#.cache_control / .cachePoint
	Messages int // messages.#.cache_control (message level)
	Blocks   int // messages.#.content.#.cache_control / .cachePoint (content-block level)
}

// Total is the figure the provider's cap (MaxBreakpoints) applies to.
func (b Breakpoints) Total() int { return b.System + b.Tools + b.Messages + b.Blocks }

// CountBreakpoints counts every breakpoint the provider will see in this request, by location.
// It takes the raw wire body, so it sees `system` and `tools` and the marks on `tool_result`
// blocks, which a component working on the chat schema cannot see (issue #32).
//
// ONE pass over the top-level object, not one gjson `#.field` query per location. The path
// form re-scanned the whole body seven times and cost about 22% of the rewrite path's CPU on
// real 600 KB Claude Code requests.
func CountBreakpoints(body []byte) Breakpoints {
	var b Breakpoints
	gjson.ParseBytes(body).ForEach(func(key, val gjson.Result) bool {
		switch key.String() {
		case "system":
			b.System += arrayMarks(val)
		case "tools":
			b.Tools += arrayMarks(val)
		case "messages":
			val.ForEach(func(_, m gjson.Result) bool {
				if !MayHoldBreakpoint(m.Raw) {
					return true
				}
				// A MESSAGE carries only the Anthropic spelling (cachePoint is a block).
				if m.Get("cache_control").IsObject() {
					b.Messages++
				}
				b.Blocks += arrayMarks(m.Get("content"))
				return true
			})
		}
		return true
	})
	return b
}

// arrayMarks counts the breakpoint marks across every element of one array.
func arrayMarks(arr gjson.Result) int {
	n := 0
	arr.ForEach(func(_, v gjson.Result) bool {
		n += ElemMarks(v)
		return true
	})
	return n
}

// ElemMarks counts the breakpoint marks on one array element (a system block, a tool, or a
// content block). The host placer uses it to see whether a target block already holds one.
func ElemMarks(v gjson.Result) int {
	if !MayHoldBreakpoint(v.Raw) {
		return 0
	}
	n := 0
	for _, k := range blockMarks {
		if v.Get(k).IsObject() {
			n++
		}
	}
	return n
}

// HasBlockBreakpoint reports whether a chat-schema message already carries a breakpoint on one
// of its content blocks. The chat-schema twin of CountBreakpoints, for components that hold
// bifrost messages and not the wire body.
func HasBlockBreakpoint(m *bschemas.ChatMessage) bool {
	if m == nil || m.Content == nil {
		return false
	}
	for i := range m.Content.ContentBlocks {
		if m.Content.ContentBlocks[i].CacheControl != nil {
			return true
		}
	}
	return false
}
