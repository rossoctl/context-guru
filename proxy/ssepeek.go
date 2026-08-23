package proxy

import (
	"bufio"
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// The expand continuation loop has to inspect a whole response to see whether the model
// called ONLY the expand tool. For a streaming response that meant buffering the stream,
// and because the expand tool is advertised from the first offload onward, EVERY response
// in a session was buffered from that point. Measured in production over ~37h:
//
//	sse_streamed 2,779 · sse_buffered 1,393        -> 33.4% of responses
//	sse_ttfb_ms_avg           7,918 ms
//	sse_ttfb_ms_avg_buffered 28,998 ms             -> ~21 SECONDS of extra time to first byte
//
// against a whole-pipeline cg_added_ms_avg of 154 ms.
//
// Read that production gap carefully, because it is confounded and this comment used to
// over-claim from it: the buffered set is later-in-session, larger-prefix, longer-generating
// requests (buffering starts at the first offload), so most of the 21 s is what those
// responses take to generate, not what buffering added. What buffering definitely adds is
// holding an entire response in memory and forbidding the client any early byte — measured
// end to end on this box, 23 of 30 real streaming responses were buffered before this change
// and 0 of 30 after, on byte-identical request bodies.
//
// And the honest negative result: on the IBM gateway this deployment talks to, the client
// sees NO time-to-first-byte improvement, because that gateway does not stream either.
// Measured with no proxy in the path at all — straight client to gateway, `stream: true` —
// ttfb/wall was 1.000 on 6 of 6 turns. Our first byte cannot precede theirs. So this change
// removes context-guru's own contribution to the problem and pays off on an upstream that
// streams; it does not fix that upstream.
//
// So decide from the FRONT of the stream instead. In the Anthropic dialect the first
// content_block_start arrives early and names the block's type and, for a tool_use, the
// tool. Buffer only up to that event: if the response does not OPEN with a call to the
// expand tool, it cannot be intercepted from its first block, so flush the peek and stream
// the remainder.
//
// The honest limit, stated because it is a real behaviour change. A response that opens
// with thinking or text and calls expand LATER is streamed through, so the client receives
// the model's raw expand tool_use instead of the proxy resolving it. That outcome already
// exists on other live paths (`otherTools`; `Continuation` returning !ok; the round cap;
// AggregateSSE failing; and every non-Anthropic SSE response, which is never peeked at all),
// so it is within the design's failure envelope rather than new. `got == 0` is NO LONGER one
// of them: a call that resolves nothing now continues with a placeholder tool_result instead
// of replaying the model's own call — but it is not free, and the peek
// deliberately does not pretend otherwise: streamFrom counts every streamed response that
// turned out to name the expand tool, so the rate is a number on /stats
// (sse_expand_after_stream) rather than an argument here.
//
// The reason it decides on the first block INSTEAD of skipping past thinking: extended
// thinking is enabled on 100% of the captured production traffic, and a thinking block is
// where most of a reasoning turn's tokens are. Buffering through it would keep most of the
// 21 seconds, which is the entire thing being fixed.
//
// The upgrade path that removes the trade-off, when the counter says it is worth it: stop
// forwarding at the expand block instead of at the first block, run the continuation, and
// splice round 2's content_block events into the same stream with their indices remapped.
// That keeps the stream AND the interception; it costs index rewriting, message_start
// suppression, and a defined fallback for a mid-splice failure (forward the buffered
// remainder verbatim — exactly today's behaviour).

// ssePeekMaxBytes bounds the peek. A message_start plus the first content_block_start is a
// few hundred bytes; this is orders of magnitude above that, so hitting it means the stream
// is not the dialect we can read. Then we buffer, which is the old behaviour — fail open.
const ssePeekMaxBytes = 64 << 10

// sseVerdict is what a bounded peek concluded about a streaming response.
type sseVerdict int

const (
	// sseStreamable: the response cannot be a lone expand call from its first block.
	sseStreamable sseVerdict = iota
	// sseMustBuffer: it opens with a call to the expand tool, or the peek could not read
	// the stream at all. Either way, buffer and let the loop inspect the whole thing.
	sseMustBuffer
)

// peekSSE reads events from br until the first content_block_start decides the verdict,
// or until the stream ends or the bound is hit. Every byte it consumes is returned in
// head, so the caller can either write it out (and stream the rest) or concatenate it with
// the remainder (and buffer). It never discards input.
func peekSSE(br *bufio.Reader, expandTool string) (head []byte, v sseVerdict) {
	var buf bytes.Buffer
	for buf.Len() < ssePeekMaxBytes {
		line, err := br.ReadBytes('\n')
		buf.Write(line) // including a final partial line, and including on error
		if d, decided := sseLineVerdict(line, expandTool); decided {
			return buf.Bytes(), d
		}
		if err != nil {
			// EOF or a read error before any content_block_start. Nothing can be
			// intercepted (there is no tool_use block), and a mid-stream error is the
			// caller's to surface — streaming the bytes we hold reproduces it.
			return buf.Bytes(), sseStreamable
		}
	}
	return buf.Bytes(), sseMustBuffer // unreadable/oversized preamble: old behaviour
}

// sseLineVerdict decides on one SSE line, or reports that it decides nothing.
//
// `message_stop` before any content_block_start is a decision: an empty message has no
// tool call in it. An `error` event likewise — the loop has nothing to inspect and the
// client needs to see the error as it arrived.
func sseLineVerdict(line []byte, expandTool string) (sseVerdict, bool) {
	i := bytes.IndexByte(line, ':')
	if i < 0 || !bytes.Equal(bytes.TrimSpace(line[:i]), []byte("data")) {
		return sseStreamable, false
	}
	payload := strings.TrimSpace(string(line[i+1:]))
	if payload == "" || payload == "[DONE]" {
		return sseStreamable, false
	}
	ev := gjson.Parse(payload)
	switch ev.Get("type").String() {
	case "content_block_start":
		cb := ev.Get("content_block")
		if cb.Get("type").String() == "tool_use" && cb.Get("name").String() == expandTool {
			return sseMustBuffer, true // this is the case the loop exists for
		}
		return sseStreamable, true
	case "message_stop", "error":
		return sseStreamable, true
	}
	return sseStreamable, false
}

// sseNamesExpandTool reports whether a chunk of a STREAMED response mentions the expand
// tool, which means the peek let through a response the loop would have intercepted. It is
// an upper bound: the model could write the name in prose. Cheap enough to run per chunk.
func sseNamesExpandTool(p []byte, expandTool string) bool {
	return bytes.Contains(p, []byte(expandTool))
}
