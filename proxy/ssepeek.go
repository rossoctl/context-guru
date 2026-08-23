package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The expand continuation loop has to see a whole response to know whether the model called
// ONLY the expand tool. For a streaming response that first meant buffering the entire
// stream, and because the tool is advertised for any tools-bearing client, every response in
// a session was buffered. Measured in production over ~37h:
//
//	sse_streamed 2,779 · sse_buffered 1,393        -> 33.4% of responses
//
// against a whole-pipeline cg_added_ms_avg of 154 ms. (The buffered set is also
// later-in-session and longer-generating, so the raw ttfb gap between the two buckets is
// mostly generation time, not buffering — what buffering definitely costs is holding a whole
// response in memory and forbidding the client any early byte. Measured end to end on this
// box, 23 of 30 real streaming responses were buffered before the peek and 0 of 30 after, on
// byte-identical request bodies.)
//
// The upstream DOES stream, so that cost is real. An older comment here claimed the opposite
// ("that gateway does not stream either, ttfb/wall 1.000 on 6 of 6 turns"); it was wrong, and
// two independent re-measurements say so. With no proxy in the path, 22 of 22 responses came
// back as text/event-stream with ttfb/wall 0.47-0.78 (p50 0.58-0.68). Timestamping every
// event of 39 more turns on two models (aws/claude-sonnet-5 n=36, aws/claude-opus-4-7 n=3):
// the first event lands at a MEDIAN of 0.49 of wall, range 0.23-0.81, and the deltas stream
// across the rest. Individual
// turns do read 1.000 when generation finishes fast enough to arrive in one burst, which is
// the likeliest thing the original 6 turns caught.
//
// The first fix decided from the FRONT of the stream: buffer up to the first
// content_block_start, and stream the rest unless the response OPENED with a call to the
// expand tool. That removed the buffering — and removed the interception with it, because a
// tool_use is almost never the first block. A turn opens with thinking or with text and calls
// its tools after. Production, over 1,687 streamed responses:
//
//	sse_expand_after_stream 22 · sse_buffered 1
//
// so the loop missed ~22 calls for each one it caught, and each miss handed a client a
// tool_use for a tool only this proxy implements — `No such tool available:
// context_guru_expand`. (An earlier comment blamed extended thinking and claimed it was on
// for 100% of captured traffic. It is on for about half of it, and it is not the cause: a
// leading `text` block does exactly the same thing.)
//
// Peeking FURTHER — decide at the first tool_use block instead of the first block — was
// measured and rejected. Both arms are derivable from one response's event timeline, so the
// comparison is paired by construction with no proxy restart inside the arm: forwarding from
// the first event against forwarding from the first tool_use block. Over 36 sonnet turns,
// deciding at the first tool_use withholds 98.3% +/- 2.1% of the streaming span on
// tool-calling turns, 99.0% +/- 1.6% with thinking on, and 100% on a turn with no tool_use at
// all, where the decision point becomes message_stop; +3.2 s to +4.5 s of client wait per
// response. Note that peek-further >= this is an identity of the construction, so the sign is
// not evidence; the magnitudes and the prose-only case are. It would pay that on nearly every
// response to intercept the ~1.3% that call expand. Rows in
// docs/results/expand-splice-2026-08.
//
// So decide per BLOCK instead of per response, which is the upgrade path this file used to
// name: forward events as they arrive, stop at the content_block_start that calls the expand
// tool, run the continuation, and splice round 2's blocks into the same stream with their
// indices remapped. The client keeps its stream — the reasoning and prose ahead of the tool
// call are most of a turn's tokens and are already on the wire — and it never receives the
// one block that is ours to answer.
//
// The honest limits:
//
//   - The model can batch expand alongside a tool only the CLIENT implements. The proxy
//     cannot answer half a batch, so the withheld events are handed back as they arrived and
//     the client does see the raw call. Counted (sse_expand_after_stream), never silent.
//   - Same for a stream that will not reconstruct, a Continuation that will not build, and
//     the round cap. (`got == 0` is NOT one of them: a call that resolves nothing continues
//     with a placeholder tool_result, so that turn completes.)
//   - A block the model generated AFTER the expand call is withheld with it and, on a
//     successful continuation, never reaches the client — while the continuation DID send it
//     back upstream, so the model believes it said something the client never saw. The
//     realistic shape is thinking interleaved after a tool call.
//   - Non-Anthropic event streams are not inspected at all: AggregateSSE only reconstructs
//     the Anthropic dialect, so there is nothing the loop could read even if we held the
//     bytes.
//   - The forwarded bytes are retained until the round ends, because Continuation needs the
//     whole assistant turn — bounded by sseRetainMaxBytes. Measured on a 2.25 MB round, the
//     largest a 128K-output model typically produces: the RETAINED bytes are what the old
//     whole-response buffering held, and it now applies to every tools-bearing streaming
//     response rather than only marker-bearing ones (33.4%), since injection stopped
//     requiring markers. Allocation is the separate number, and the two must not be
//     conflated: reading events one at a time churned 13.3x the stream against that path's
//     3.2x until the event buffer was reused, which brought it to 4.8x.
//
// Every one of those paths ends at the same place on the NEXT request, so they are closed
// there instead: expand.RepairToolResults replaces the client's `No such tool available` with
// the content the model asked for, so the error never reaches the model even when the block
// could not be withheld.

// sseSplicer joins the rounds of an expand continuation into ONE client event stream.
//
// Round 1's events are forwarded as they arrive and forwarding stops at the expand call.
// The continuation's blocks are then written into the same open response, renumbered to
// follow the blocks already sent and with their message_start dropped, because a client sees
// one message per turn. If the loop cannot answer after all, handBack writes the withheld
// events through the same transform — which, for a round whose blocks were not renumbered,
// is byte-for-byte the stream as it arrived: exactly the old pass-through.
type sseSplicer struct {
	w        http.ResponseWriter
	flush    http.Flusher
	headers  bool      // response headers and status are on the wire
	msgStart bool      // the client has its message_start
	ended    bool      // a message_stop has gone out: the client's turn is closed
	blocks   int       // content blocks the client holds
	base     int       // index offset applied to the round being forwarded
	first    time.Time // when the client got its first byte
}

func newSSESplicer(w http.ResponseWriter) *sseSplicer {
	f, _ := w.(http.Flusher)
	return &sseSplicer{w: w, flush: f}
}

// round starts an upstream round. Its blocks are numbered from the end of what the client
// already holds, and only the first round's headers and status are sent — by the time a
// continuation answers, the response is long open.
func (sp *sseSplicer) round(resp *http.Response) {
	sp.base = sp.blocks
	if sp.headers {
		return
	}
	copyHeaders(sp.w.Header(), resp.Header)
	sp.w.WriteHeader(resp.StatusCode)
	sp.headers = true
}

// sseRetainMaxBytes bounds what one round may retain. pass keeps the round's bytes because
// Continuation needs the whole assistant turn, and the peek this replaced had its own bound
// (an unreadable or oversized preamble bailed out at 64 KB) — removing it left one
// pathological stream, a runaway generation or an upstream that never sends message_stop,
// retained in full with nothing to stop it. This is that bound, restored at the only place it
// can now live.
//
// The number is the first power of two above the largest round a model can legitimately
// produce, so it never fires on real traffic and only ever catches pathology. Re-derive it if
// it ever looks tight, because the input that moves is not ours: this proxy never caps
// max_tokens, it only reads it, so the ceiling is whatever the UPSTREAM allows and it changes
// when the upstream does. Measured against the gateway this deployment fronts, which accepts
// max_tokens up to 128,000 (200,000 is refused with "the maximum allowed number of output
// tokens"), and an event stream runs ~35 bytes per output
// token — ~4 bytes of text plus ~123 bytes of framing per delta of ~4 tokens — so a
// full-length response is ~4.5 MB, which is 3.4x under the bound. A stream that emitted ONE
// token per delta would pay that framing per TOKEN instead, ~127 bytes, and reach ~16.3 MB:
// a margin of about 3%, not headroom.
//
// That margin is deliberately not treated as a cliff, because overshoot is graceful by
// construction. Past the bound the round is forwarded whole, the call is counted, and the
// repair answers it next request — which is precisely the behaviour of every release before
// the splice existed. A bound that occasionally gives up on the largest imaginable stream
// costs one interception; no bound at all costs the process.
//
// Past it the round is forwarded whole and nothing is intercepted, so the feature degrades
// rather than breaking: the client gets the model's own expand call, it is counted, and
// expand.RepairToolResults answers it on the next request.
const sseRetainMaxBytes = 16 << 20

// pass forwards this round's events until the one that calls the expand tool, and returns
// the round's whole event stream — forwarded and withheld together — because the
// continuation loop reconstructs the assistant turn from all of it. withheld is the
// deciding event and everything after it: the caller either answers it or hands it back.
//
// Past sseRetainMaxBytes it returns whole=nil: the turn can no longer be rebuilt, so nothing
// can be intercepted, and everything is forwarded as it arrives. found still reports whether
// the response called expand, because the client then receives that call and the caller has
// to count it.
func (sp *sseSplicer) pass(body io.Reader, expandTool string) (whole, withheld []byte, found bool) {
	br := bufio.NewReader(body)
	// One event buffer for the round, not one per event: a 2.25 MB round is ~19,000 events,
	// and allocating a buffer for each cost 13.3x the stream in churn against the 3.2x of the
	// whole-response buffering this replaced. Reusing it is 4.8x. Every consumer of ev is
	// done with it before the next read overwrites it.
	var buf, ev bytes.Buffer
	cut := -1     // where withholding began; -1 = forwarding
	over := false // past the bound: the turn cannot be rebuilt
	for {
		e, err := readSSEEvent(br, &ev)
		if len(e) > 0 {
			sent := false
			if startsExpandCall(e, expandTool) {
				found = true
				if !over && cut < 0 {
					cut = buf.Len()
				}
			}
			if !over {
				buf.Write(e)
				if buf.Len() > sseRetainMaxBytes {
					// Nothing is dropped: whatever was already withheld goes to the client
					// now, and the rest of the stream follows it event by event. The flush
					// includes THIS event, so it must not also be forwarded below.
					if cut >= 0 {
						sp.handBack(buf.Bytes()[cut:])
						cut, sent = -1, true
					}
					over, buf = true, bytes.Buffer{}
				}
			}
			if cut < 0 && !sent {
				sp.forward(e)
			}
		}
		if err != nil {
			if over {
				return nil, nil, found
			}
			whole = buf.Bytes()
			if cut < 0 {
				return whole, nil, found
			}
			return whole, whole[cut:], found
		}
	}
}

// handBack sends events the splice withheld — the fail-open path, and the one that hands a
// client the model's own expand call when nothing else can be done with it.
func (sp *sseSplicer) handBack(withheld []byte) {
	br := bufio.NewReader(bytes.NewReader(withheld))
	var ev bytes.Buffer
	for {
		e, err := readSSEEvent(br, &ev)
		if len(e) > 0 {
			sp.forward(e)
		}
		if err != nil {
			return
		}
	}
}

// forward sends one complete event, transformed as the splice requires.
func (sp *sseSplicer) forward(ev []byte) {
	ev, ok := sp.rewrite(ev)
	if !ok {
		return
	}
	if sp.first.IsZero() {
		sp.first = time.Now()
	}
	sp.w.Write(ev)
	if sp.flush != nil {
		sp.flush.Flush()
	}
}

// rewrite drops a second message_start and shifts block indices past the blocks the client
// already has. With no offset to apply it returns the event untouched — byte-for-byte,
// which is what keeps a response that never calls expand an exact pass-through.
func (sp *sseSplicer) rewrite(ev []byte) ([]byte, bool) {
	// With no offset to apply, the only events that can change what the client holds are a
	// message_start (de-duplicated) and a content_block_start (counted) — one per block, so
	// counting starts alone is exact. Every delta of a long turn skips the parse below, which
	// is not a micro-optimisation: a 2.25 MB round is ~18,750 events and building each
	// event's payload string cost 1.6x the stream, twice over.
	if sp.base == 0 && !bytes.Contains(ev, []byte("_start")) && !bytes.Contains(ev, []byte("message_stop")) {
		return ev, true
	}
	payload := sseEventPayload(ev)
	if payload == "" {
		return ev, true
	}
	p := gjson.Parse(payload)
	switch p.Get("type").String() {
	case "message_start":
		if sp.msgStart {
			return nil, false
		}
		sp.msgStart = true
	case "message_stop":
		sp.ended = true
	case "content_block_start", "content_block_delta", "content_block_stop":
		idx := int(p.Get("index").Int()) + sp.base
		if idx >= sp.blocks {
			sp.blocks = idx + 1
		}
		if sp.base == 0 {
			break
		}
		np, err := sjson.Set(payload, "index", idx)
		if err != nil {
			break // fail open: an event we cannot renumber goes as it came
		}
		ev = bytes.Replace(ev, []byte(payload), []byte(np), 1)
	}
	return ev, true
}

// readSSEEvent reads one complete Server-Sent Event into ev: every line up to and including
// the blank line that terminates it, verbatim, framing included. On a read error it returns
// whatever it holds — a partial final event still has to be forwarded, or the stream loses
// its tail. ev is the caller's, reset here and valid until the next call.
func readSSEEvent(br *bufio.Reader, ev *bytes.Buffer) ([]byte, error) {
	ev.Reset()
	for {
		line, err := br.ReadBytes('\n')
		ev.Write(line)
		if err != nil {
			return ev.Bytes(), err
		}
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			return ev.Bytes(), nil
		}
	}
}

// sseEventPayload returns the JSON carried by an event's data: line, or "" if it has none.
func sseEventPayload(ev []byte) string {
	for _, line := range bytes.Split(ev, []byte("\n")) {
		i := bytes.IndexByte(line, ':')
		if i < 0 || !bytes.Equal(bytes.TrimSpace(line[:i]), []byte("data")) {
			continue
		}
		p := string(bytes.TrimSpace(line[i+1:]))
		if p == "[DONE]" {
			return ""
		}
		return p
	}
	return ""
}

// startsExpandCall reports whether this event OPENS a call to the expand tool — the one
// block a client must never receive, because only this proxy implements the tool.
func startsExpandCall(ev []byte, expandTool string) bool {
	if !bytes.Contains(ev, []byte("content_block_start")) {
		return false // cheap reject: the parse below is the expensive half
	}
	p := gjson.Parse(sseEventPayload(ev))
	if p.Get("type").String() != "content_block_start" {
		return false
	}
	cb := p.Get("content_block")
	return cb.Get("type").String() == "tool_use" && cb.Get("name").String() == expandTool
}

// terminate closes the client's turn if nothing has. A continuation round is not obliged to
// carry a message_stop — it can come back empty, truncated, or in another dialect — and the
// round whose terminator the splice withheld is the only other place one exists. Without
// this the client is left with a half-open message, which is worse than the leaked tool_use
// it replaced: on main those rounds cannot arise at all.
func (sp *sseSplicer) terminate(withheld []byte) {
	if sp.ended || !sp.msgStart {
		return
	}
	// Only the CLOSING events, never the content: the withheld bytes begin with the expand
	// call, which is the one block the client must not receive. So this closes the message
	// with the model's own message_delta and message_stop and invents nothing.
	br := bufio.NewReader(bytes.NewReader(withheld))
	var ev bytes.Buffer
	for {
		e, err := readSSEEvent(br, &ev)
		if len(e) > 0 {
			switch gjson.Parse(sseEventPayload(e)).Get("type").String() {
			case "message_delta", "message_stop":
				sp.forward(e)
			}
		}
		if err != nil {
			if !sp.ended {
				// The withheld round had no closer either — it was truncated too, so there
				// are no closing events anywhere to forward. The turn DID fail, and an
				// `error` event is how this protocol says so: the client gets a diagnosable
				// end instead of a socket that stops mid-message. Not a synthetic
				// message_stop — no stop_reason in the enum means "truncated", and end_turn
				// would tell the client a broken turn finished normally.
				sp.forward([]byte("event: error\ndata: {\"type\":\"error\",\"error\":" +
					"{\"type\":\"api_error\",\"message\":\"upstream ended the turn " +
					"without a terminator\"}}\n\n"))
				sp.ended = true
			}
			return
		}
	}
}
