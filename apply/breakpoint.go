package apply

import (
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/rossoctl/context-guru/schema"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// One breakpoint placer for every request the HOST builds or rebuilds after the client sent it:
// the splice anchor (#723) and the in-band follow-up (#784). Both find one content block, put a
// cache_control on it within the provider's cap, and count what happened. Only the target and the
// mark differ.

// placerStats is one caller's outcome counters.
type placerStats struct {
	written  atomic.Int64 // the mark was put on the target block
	noSlot   atomic.Int64 // an addition was skipped: every breakpoint slot was in use
	noMark   atomic.Int64 // a move was skipped: the client put no breakpoint in messages
	notFound atomic.Int64 // no block to put the mark on
	tooFar   atomic.Int64 // a move was skipped: the target is past the provider's lookback
	// atEnd: the client's last mark already sits at the very end, or the target already holds a
	// mark. shortTail: the client's last mark is followed by more than 0 and at most tailMinBytes
	// bytes. Only the tail step reports them
	atEnd     atomic.Int64
	shortTail atomic.Int64
}

// Cache-anchor counters (#723): anchors written, anchors skipped for want of a free breakpoint slot,
// and anchors whose tool_result was not found in the rebuilt body.
var cacheAnchor placerStats

// In-band counters (#784): breakpoints moved to the end of a follow-up, follow-ups whose client
// request had no message breakpoint to move, follow-ups whose last block cannot take one, and
// follow-ups whose new content is longer than the provider's lookback. noSlot stays 0: a move never
// needs a free slot.
var inBandBreakpoint placerStats

// CacheAnchorStats reports the cache-anchor counters: written, no free slot, block not found.
func CacheAnchorStats() (written, noSlot, notFound int64) {
	return cacheAnchor.written.Load(), cacheAnchor.noSlot.Load(), cacheAnchor.notFound.Load()
}

// InBandBreakpointStats reports the in-band follow-up counters: moved, no client mark, no target,
// past the lookback.
func InBandBreakpointStats() (moved, noMark, notFound, tooFar int64) {
	return inBandBreakpoint.written.Load(), inBandBreakpoint.noMark.Load(), inBandBreakpoint.notFound.Load(),
		inBandBreakpoint.tooFar.Load()
}

// Tail-breakpoint counters (#795): breakpoints added at the end of a client request, requests
// with no free slot, requests whose last mark already sits at the end, requests with only a short
// tail after it, requests whose client put no mark in messages, and requests whose last block
// cannot take a mark.
var tailBreakpoint placerStats

// TailBreakpointStats reports the tail-breakpoint counters: added, no free slot, mark at the end,
// short tail, no client mark, no target.
func TailBreakpointStats() (added, noSlot, atEnd, shortTail, noMark, notFound int64) {
	return tailBreakpoint.written.Load(), tailBreakpoint.noSlot.Load(), tailBreakpoint.atEnd.Load(),
		tailBreakpoint.shortTail.Load(), tailBreakpoint.noMark.Load(), tailBreakpoint.notFound.Load()
}

// lookbackBlocks is how far back from a breakpoint the provider looks for an earlier cache entry,
// in content blocks: the figure cacheinject reads too. A breakpoint moved further than this from where it was
// loses the read of the cached prefix, and the whole request is written again.
const lookbackBlocks = schema.LookbackBlocks

// ephemeralMark is the mark the splice anchor adds: the provider's default 5-minute tier.
const ephemeralMark = `{"type":"ephemeral"}`

// blockPath is the sjson path of content block bi of message mi.
func blockPath(mi, bi int) string {
	return "messages." + strconv.Itoa(mi) + ".content." + strconv.Itoa(bi)
}

// placeBreakpoint puts mark (a raw cache_control object) on the content block at target.
//
// from == "" ADDS a breakpoint, so it needs a free slot under maxWireBreakpoints; without one it
// is skipped and counted. A non-empty from is the path of the cache_control to remove in the same
// step: a MOVE, which keeps the count the client sent, so the cap can never be breached by it.
//
// A target that already holds a mark is left as it is. A target that is not a JSON object (a
// bare string, number or null in a content array) is counted as not found and left as it is:
// sjson would replace it whole with the mark, and its bytes would be lost. Any write error
// returns body unchanged. ok reports whether the mark was placed.
func placeBreakpoint(body []byte, target, mark, from string, st *placerStats) (out []byte, ok bool) {
	tb := gjson.GetBytes(body, target)
	if !tb.IsObject() {
		st.notFound.Add(1)
		return body, false
	}
	if schema.ElemMarks(tb) > 0 {
		st.atEnd.Add(1)
		return body, false // already a breakpoint there; nothing to add
	}
	out = body
	if from == "" {
		if wireBreakpoints(body) >= maxWireBreakpoints {
			st.noSlot.Add(1)
			return body, false
		}
	} else {
		var err error
		if out, err = sjson.DeleteBytes(body, from); err != nil {
			return body, false
		}
	}
	out, err := sjson.SetRawBytes(out, target+".cache_control", []byte(mark))
	if err != nil {
		return body, false
	}
	st.written.Add(1)
	return out, true
}

// anchorCacheBreakpoint writes one {"type":"ephemeral"} cache_control onto the tool_result block
// whose tool_use_id is toolID, in the raw body a count-changing rebuild just produced (#723). A
// splice that keeps its last tool exchange verbatim only gives the next turn a readable cache
// entry once that exchange's tool_result carries a breakpoint: the provider never reads an entry
// that ends at a text turn, and Claude Code's own last breakpoint sits on a trailing text turn.
//
// THE BUDGET. The provider rejects a request with more than maxWireBreakpoints. The count is taken
// on the body AS REBUILT, after every component ran, so it already includes any breakpoint
// cacheinject placed from its own (earlier, raw-body) count; adding ours only when that total is
// below the cap can never push the wire to 5. With no free slot the anchor is skipped and counted:
// the turn then behaves exactly as before #723. Claude Code itself uses 3 (2 system, 1 message).
//
// Returns the body unchanged when there is nothing to do.
func anchorCacheBreakpoint(body []byte, toolID string) []byte {
	if toolID == "" {
		return body
	}
	msgs := gjson.GetBytes(body, "messages").Array()
	for mi := len(msgs) - 1; mi >= 0; mi-- {
		blocks := msgs[mi].Get("content")
		if !blocks.IsArray() {
			continue
		}
		arr := blocks.Array()
		for bi := range arr {
			b := arr[bi]
			if b.Get("type").String() != "tool_result" || b.Get("tool_use_id").String() != toolID {
				continue
			}
			out, _ := placeBreakpoint(body, blockPath(mi, bi), ephemeralMark, "", &cacheAnchor)
			return out
		}
	}
	cacheAnchor.notFound.Add(1)
	return body
}

// MarkInBandFollowUp gives an in-band follow-up request the breakpoint the client would have
// given it (#784). An in-band follow-up is a request the HOST sends upstream inside one client
// request, after it answered a tool call itself: the expand continuation (expand.Continuation),
// and #485's tool-search stub when it lands. It ends with content the client never sent — the assistant's
// tool_use and the host's tool_result — and no breakpoint covers it, so the provider bills it as
// plain input and caches none of it.
//
// WHEN IT PAYS. The mark makes the provider WRITE the new content (1.25x the input price, 2x for a
// 1h TTL) instead of billing it as plain input (1x). That pays only when a later request READS the
// entry, which needs that request to carry the same bytes up to the mark. The caller owns that:
//
//   - #485's search stub, a future caller, will splice the stored exchange back into the client's
//     next request byte-identically, so the next turn reads it (probe: 474 tokens written
//     without, 0 with).
//   - The expand continuation does NOT qualify, so the proxy calls this only when
//     `expand.inband_breakpoint` is set. The client never sees the tool_use or the tool_result;
//     its next request carries an assistant turn without the tool_use, and fixed_restore inserts a
//     user text message, not the tool_result. Both diverge before the mark, so only a later round
//     of the same loop could read the entry. Pinned by TestTheNextClientRequestDivergesBeforeTheMark.
//
// THE LOOKBACK. The follow-up still reads the client's cached prefix only through the provider's
// lookback from the moved mark (lookbackBlocks). A follow-up that adds more blocks than that after
// the client's mark is left unmarked and counted as too far.
//
// It MOVES the last cache_control in `messages`, whoever placed it (the client, or the pipeline
// before it), onto the last block of the last message,
// with the same cache_control value, so the TTL the client asked for ("1h" stays "1h") is kept.
// That is what Claude Code itself does on each new request. A move keeps the client's count, so
// the provider's cap of 4 is never at risk, and breakpoints in `tools` and `system` are not
// touched. A client that put no breakpoint in `messages` asked for no cache strategy there, and
// none is invented: the body is returned unchanged.
//
// Call it on each follow-up as built. On a later round the mark found is the one the previous
// round moved, so it moves on again. Only the Anthropic messages wire carries cache_control; on
// every other wire this is a no-op. Fails open: any unexpected shape or error returns body
// unchanged.
func MarkInBandFollowUp(wire string, body []byte) []byte {
	if wire != schema.WireAnthropic {
		return body
	}
	msgs := gjson.GetBytes(body, "messages").Array()
	if len(msgs) == 0 {
		return body
	}
	from, mark := lastMessageMark(msgs)
	if from == "" {
		inBandBreakpoint.noMark.Add(1)
		return body
	}
	work, target, ok := lastBlockTarget(body, len(msgs)-1, &inBandBreakpoint)
	if !ok {
		return body
	}
	if from == target+".cache_control" {
		return body // already at the end
	}
	if blocksAfter(gjson.GetBytes(work, "messages").Array(), from) > lookbackBlocks {
		inBandBreakpoint.tooFar.Add(1)
		return body
	}
	if out, ok := placeBreakpoint(work, target, mark, from, &inBandBreakpoint); ok {
		return out
	}
	return body // nothing placed: send the follow-up exactly as built
}

// lastBlockTarget returns the path of the last content block of message last, the block a mark
// at the end of the request goes on, and the body to write it into. A string content has no block
// to carry the mark, so it becomes the one text block it means; only this request carries the
// change, the client never sees it, and the provider renders the two the same way (schema/canon.go).
// A last block that is not an object (a bare string, number or null), is a thinking block or is
// an empty text block takes no mark: counted on st as not found, ok false.
func lastBlockTarget(body []byte, last int, st *placerStats) (work []byte, target string, ok bool) {
	work = body
	mp := "messages." + strconv.Itoa(last) + ".content"
	content := gjson.GetBytes(body, mp)
	if content.Type == gjson.String && content.Str != "" {
		blk, err := sjson.Set(`{"type":"text"}`, "text", content.Str)
		if err != nil {
			return body, "", false
		}
		if work, err = sjson.SetRawBytes(body, mp, []byte("["+blk+"]")); err != nil {
			return body, "", false
		}
		content = gjson.GetBytes(work, mp)
	}
	blocks := content.Array()
	if !content.IsArray() || len(blocks) == 0 {
		st.notFound.Add(1)
		return body, "", false
	}
	bi := len(blocks) - 1
	if !blocks[bi].IsObject() {
		st.notFound.Add(1) // a bare string, number or null takes no mark
		return body, "", false
	}
	if t := blocks[bi].Get("type").String(); t == "thinking" || t == "redacted_thinking" {
		st.notFound.Add(1) // the provider takes no cache_control on a thinking block
		return body, "", false
	}
	if blocks[bi].Get("type").String() == "text" && blocks[bi].Get("text").String() == "" {
		st.notFound.Add(1) // the provider rejects cache_control on an empty text block
		return body, "", false
	}
	return work, blockPath(last, bi), true
}

// tailMinBytes is how much request content must follow the client's last breakpoint before
// markTail adds one at the end: 4 KiB of JSON, about 1k tokens. The write premium is 0.25x, so one
// later read of the entry (0.9x saved) pays for it. The floor keeps the step off the requests where
// the client's mark already sits at the end. With the betas setting, Claude Code's later requests
// end in a short `system` message after the mark: 350 bytes measured, under the floor. The first
// request of a session ends in the 8.7 KB environment message: over it.
const tailMinBytes = 4096

// markTail adds a breakpoint on the last block of the last message when the client's last
// breakpoint is followed by more than tailMinBytes of content (#795).
//
// WHY. The provider caches only up to a breakpoint. With CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1,
// Claude Code puts its last mark on the user's text and appends a `role: "system"` message after
// it, about 3k tokens on a session's first request. Nothing caches that message, so the provider
// bills it as plain input, and it bills it again on the next request, which writes it to the cache
// at last. With the mark at the end, the first request writes it and the next one reads it: live,
// $0.0574 to $0.0424 a run (3 runs each way, Sonnet 5.5).
//
// WHAT IT KEEPS. It ADDS, never moves: the client's marks stay where they are, so every read the
// client planned still happens. It copies the client's last mark value, so the TTL stays the
// client's ("1h" stays "1h"), and the mark after it has the same TTL, which the provider's order
// rule (longer TTLs first) allows. It needs a free slot under the provider's cap of 4
// (placeBreakpoint); with none it does nothing. A client that put no mark in `messages` asked for
// no cache strategy there, and none is invented.
//
// THE LOOKBACK. The provider looks back lookbackBlocks blocks from a breakpoint for an earlier
// entry. The new mark is added even when the client's mark is further back than that: the client's
// mark is still on the request and still reads its own entry, so the new one costs only its write.
// The next request reads the new entry when its own mark is within the lookback of this request's
// end, which it is when that turn adds fewer than 20 blocks.
//
// Call it on the final body, after every other edit, so the cap check counts every breakpoint.
// Fails open: any unexpected shape or error returns body unchanged, ok false.
func markTail(body []byte) (out []byte, ok bool) {
	msgs := gjson.GetBytes(body, "messages").Array()
	if len(msgs) == 0 {
		return body, false
	}
	from, mark := lastMessageMark(msgs)
	if from == "" {
		tailBreakpoint.noMark.Add(1)
		return body, false
	}
	switch n := bytesAfter(msgs, from); {
	case n == 0:
		tailBreakpoint.atEnd.Add(1)
		return body, false
	case n <= tailMinBytes:
		tailBreakpoint.shortTail.Add(1)
		return body, false
	}
	work, target, ok := lastBlockTarget(body, len(msgs)-1, &tailBreakpoint)
	if !ok {
		return body, false
	}
	if out, ok = placeBreakpoint(work, target, mark, "", &tailBreakpoint); !ok {
		return body, false // nothing placed: not even the string-to-block rewrite goes out
	}
	return out, true
}

// bytesAfter is the size in JSON bytes of the content after the mark at path from (a
// lastMessageMark path): the later blocks of its message and every later message.
func bytesAfter(msgs []gjson.Result, from string) int {
	parts := strings.Split(from, ".")
	mi, _ := strconv.Atoi(parts[1])
	n := 0
	if len(parts) == 5 { // messages.M.content.B.cache_control
		bi, _ := strconv.Atoi(parts[3])
		for _, b := range msgs[mi].Get("content").Array()[bi+1:] {
			n += len(b.Raw)
		}
	}
	for _, m := range msgs[mi+1:] {
		n += len(m.Raw)
	}
	return n
}

// blocksAfter counts the content blocks after the mark at path from (a lastMessageMark path) up to
// the end of msgs: the distance the provider's lookback has to cover from a mark on the last
// block. A string content counts as one block.
func blocksAfter(msgs []gjson.Result, from string) int {
	parts := strings.Split(from, ".")
	mi, _ := strconv.Atoi(parts[1])
	n := 0
	if len(parts) == 5 { // messages.M.content.B.cache_control
		bi, _ := strconv.Atoi(parts[3])
		n = len(msgs[mi].Get("content").Array()) - 1 - bi
	}
	for _, m := range msgs[mi+1:] {
		if c := m.Get("content"); c.IsArray() {
			n += len(c.Array())
		} else {
			n++
		}
	}
	return n
}

// lastMessageMark finds the last cache_control in msgs, at block level or message level, and
// returns its sjson path and raw value. "" when there is none.
func lastMessageMark(msgs []gjson.Result) (path, raw string) {
	for mi := len(msgs) - 1; mi >= 0; mi-- {
		m := msgs[mi]
		if !schema.MayHoldBreakpoint(m.Raw) {
			continue
		}
		blocks := m.Get("content").Array()
		for bi := len(blocks) - 1; bi >= 0; bi-- {
			if cc := blocks[bi].Get("cache_control"); cc.IsObject() {
				return blockPath(mi, bi) + ".cache_control", cc.Raw
			}
		}
		if cc := m.Get("cache_control"); cc.IsObject() {
			return "messages." + strconv.Itoa(mi) + ".cache_control", cc.Raw
		}
	}
	return "", ""
}
