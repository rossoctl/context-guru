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
// the first event lands at 40-55% of wall and the deltas stream across the rest. Individual
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
// deciding at the first tool_use withholds 98.4% +/- 2.1% of the streaming span on
// tool-calling turns (sem 0.6), 99.1% +/- 1.7% with thinking on, and 100% on a turn with no
// tool_use at all, where the decision point becomes message_stop; +3.2 s to +4.5 s of client
// wait per response (sem 0.19-0.28, so ~12-16 sigma). It would pay that on nearly every
// response to intercept the ~1.3% that call expand.
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
//   - Non-Anthropic event streams are not inspected at all: AggregateSSE only reconstructs
//     the Anthropic dialect, so there is nothing the loop could read even if we held the
//     bytes.
//   - The forwarded bytes are retained until the round ends, because Continuation needs the
//     whole assistant turn. That is the memory the old buffered path used, minus the latency
//     — but it now applies to every tools-bearing streaming response rather than only
//     marker-bearing ones, since injection stopped requiring markers.
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

// pass forwards this round's events until the one that calls the expand tool, and returns
// the round's whole event stream — forwarded and withheld together — because the
// continuation loop reconstructs the assistant turn from all of it. withheld is the
// deciding event and everything after it: the caller either answers it or hands it back.
// An empty expandTool withholds nothing (the round cap: there is no continuation left to
// run, so the events go to the client as they are).
func (sp *sseSplicer) pass(body io.Reader, expandTool string) (whole, withheld []byte, found bool) {
	br := bufio.NewReader(body)
	var buf bytes.Buffer
	cut := -1
	for {
		ev, err := readSSEEvent(br)
		if len(ev) > 0 {
			buf.Write(ev)
			if cut < 0 && startsExpandCall(ev, expandTool) {
				cut = buf.Len() - len(ev)
			}
			if cut < 0 {
				sp.forward(ev)
			}
		}
		if err != nil {
			whole = buf.Bytes()
			if cut < 0 {
				return whole, nil, false
			}
			return whole, whole[cut:], true
		}
	}
}

// handBack sends events the splice withheld — the fail-open path, and the one that hands a
// client the model's own expand call when nothing else can be done with it.
func (sp *sseSplicer) handBack(withheld []byte) {
	br := bufio.NewReader(bytes.NewReader(withheld))
	for {
		ev, err := readSSEEvent(br)
		if len(ev) > 0 {
			sp.forward(ev)
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

// readSSEEvent reads one complete Server-Sent Event: every line up to and including the
// blank line that terminates it, verbatim, framing included. On a read error it returns
// whatever it holds — a partial final event still has to be forwarded, or the stream loses
// its tail.
func readSSEEvent(br *bufio.Reader) ([]byte, error) {
	var ev bytes.Buffer
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
	if expandTool == "" {
		return false
	}
	p := gjson.Parse(sseEventPayload(ev))
	if p.Get("type").String() != "content_block_start" {
		return false
	}
	cb := p.Get("content_block")
	return cb.Get("type").String() == "tool_use" && cb.Get("name").String() == expandTool
}
