package record

// Redaction of a raw Anthropic event stream, for wire recording.
//
// THE PROBLEM. A streamed response arrives as many small events, and the model's text is split
// across them at arbitrary points. A secret the model repeats from a tool result can therefore
// be split too: "sk-ant-api03-AAAA" in one event and "BBBB...-CCCC" in the next. Redacting each
// event on its own sees half a key in each, matches nothing, and writes the whole key to disk in
// two pieces. The translated corpus never had this problem, because it redacts the ASSEMBLED
// message; a wire recording keeps the raw events, so it has to be handled here.
//
// THE APPROACH. For each content block, join its deltas into the block's full text, redact that
// text once with Content (credentials and identity, exactly as for every other recorded field),
// and give the redacted text back to the same events. Each event gets the stretch of redacted text
// that lines up with its original piece, so:
//
//   - the stream keeps its events, in order, and stays a valid stream a replay tool can read;
//   - the deltas still join to the redacted message;
//   - a placeholder that would straddle two events goes whole into the first, so no event carries
//     part of a secret.
//
// Every other event (message_start, stops, usage) is redacted on its own with ContentJSON, which
// is correct for them: none of them is split.
//
// A clean stream comes back byte for byte.

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Delta fields that carry a block's streamed text, by delta type.
var deltaField = map[string]string{
	"text_delta":       "text",
	"input_json_delta": "partial_json",
	"thinking_delta":   "thinking",
}

type sseEvent struct {
	lines []string // the event's lines, without the trailing blank line
	data  int      // index of the data: line in lines, or -1
	json  string   // the data payload
}

// RedactSSE returns the stream with credentials and identity removed, across event boundaries.
func RedactSSE(raw string) string {
	events, tail := splitSSE(raw)

	// Pass 1: collect each block's streamed pieces.
	type piece struct{ ev int }
	pieces := map[int64][]piece{} // block index -> events carrying its deltas, in order
	texts := map[int64]*strings.Builder{}
	for i, e := range events {
		if e.data < 0 || gjson.Get(e.json, "type").String() != "content_block_delta" {
			continue
		}
		field := deltaField[gjson.Get(e.json, "delta.type").String()]
		if field == "" {
			continue
		}
		idx := gjson.Get(e.json, "index").Int()
		if texts[idx] == nil {
			texts[idx] = &strings.Builder{}
		}
		texts[idx].WriteString(gjson.Get(e.json, "delta."+field).String())
		pieces[idx] = append(pieces[idx], piece{ev: i})
	}

	changed := false
	// Pass 2: redact each block's whole text and hand it back to its events.
	for idx, b := range texts {
		orig := b.String()
		red := Content(orig)
		if red == orig {
			continue
		}
		changed = true
		var origPieces []string
		for _, p := range pieces[idx] {
			e := events[p.ev]
			field := deltaField[gjson.Get(e.json, "delta.type").String()]
			origPieces = append(origPieces, gjson.Get(e.json, "delta."+field).String())
		}
		for k, s := range splitLike(orig, red, origPieces) {
			e := &events[pieces[idx][k].ev]
			field := deltaField[gjson.Get(e.json, "delta.type").String()]
			if j, err := sjson.Set(e.json, "delta."+field, s); err == nil {
				e.json = j
				e.lines[e.data] = "data: " + j
			}
		}
	}

	// Pass 3: every other event on its own.
	for i := range events {
		e := &events[i]
		if e.data < 0 || gjson.Get(e.json, "type").String() == "content_block_delta" &&
			deltaField[gjson.Get(e.json, "delta.type").String()] != "" {
			continue
		}
		if r := ContentJSON(e.json); r != e.json {
			changed = true
			e.json = r
			e.lines[e.data] = "data: " + r
		}
	}
	if !changed {
		return raw
	}
	var out bytes.Buffer
	for _, e := range events {
		out.WriteString(strings.Join(e.lines, "\n"))
		out.WriteString("\n\n")
	}
	out.WriteString(tail)
	return out.String()
}

// splitSSE splits a stream into events (blank-line separated) and any unterminated tail.
func splitSSE(raw string) ([]sseEvent, string) {
	var events []sseEvent
	rest := strings.ReplaceAll(raw, "\r\n", "\n")
	for {
		i := strings.Index(rest, "\n\n")
		if i < 0 {
			break
		}
		block := rest[:i]
		rest = rest[i+2:]
		e := sseEvent{lines: strings.Split(block, "\n"), data: -1}
		for k, l := range e.lines {
			if strings.HasPrefix(l, "data:") {
				e.data = k
				e.json = strings.TrimSpace(strings.TrimPrefix(l, "data:"))
				break
			}
		}
		events = append(events, e)
	}
	return events, rest
}

// splitLike cuts red (the redacted form of orig) into len(pieces) parts that line up with how
// orig was cut into pieces.
//
// It walks orig and red together. Text the redaction left alone is copied one character at a
// time, and every piece boundary that falls in it stays where it was. A stretch that was replaced
// (from where the two first differ to where they agree again, measured from the end) goes WHOLE
// into the piece where it starts; later pieces that fell inside it become empty. So no piece
// ever holds part of a placeholder, and none holds part of what it replaced.
func splitLike(orig, red string, pieces []string) []string {
	out := make([]string, len(pieces))
	// Piece boundaries as byte offsets into orig.
	bounds := make([]int, len(pieces))
	off := 0
	for i, p := range pieces {
		off += len(p)
		bounds[i] = off
	}
	// Align by common prefix/suffix repeatedly is overkill; a linear diff by runs is enough:
	// find each replaced region with a two-pointer scan over the original text and the result.
	piece := 0
	oi, ri := 0, 0
	emit := func(s string) { out[piece] += s }
	advance := func(to int) { // move piece index to the piece containing orig offset `to`
		for piece < len(bounds)-1 && to >= bounds[piece] {
			piece++
		}
	}
	for oi < len(orig) && ri < len(red) {
		advance(oi)
		if orig[oi] == red[ri] && !startsReplacement(orig[oi:], red[ri:]) {
			emit(red[ri : ri+1])
			oi++
			ri++
			continue
		}
		// A replaced region: the placeholder in red, and the text it replaced in orig.
		pEnd, oEnd := replacedRegion(orig[oi:], red[ri:])
		emit(red[ri : ri+pEnd])
		ri += pEnd
		oi += oEnd
	}
	advance(oi)
	if ri < len(red) {
		emit(red[ri:])
	}
	return out
}

// placeholders are the markers Content writes. A replaced region in red always begins with one.
var placeholders = []string{
	Placeholder, RedactedName, RedactedUsername, RedactedEmail, RedactedTerm, RedactedOwner, RedactedRepo,
}

func startsReplacement(orig, red string) bool {
	for _, p := range placeholders {
		if strings.HasPrefix(red, p) && !strings.HasPrefix(orig, p) {
			return true
		}
	}
	return false
}

// replacedRegion returns how many bytes of red are the placeholder(s) starting here, and how
// many bytes of orig they replaced: up to where the text after the placeholder reappears in orig.
func replacedRegion(orig, red string) (redLen, origLen int) {
	redLen = 0
	for {
		matched := false
		for _, p := range placeholders {
			if strings.HasPrefix(red[redLen:], p) {
				redLen += len(p)
				matched = true
				break
			}
		}
		if !matched {
			break
		}
	}
	if redLen == 0 { // not a placeholder: a byte that differs for another reason; take it alone
		return 1, 1
	}
	after := red[redLen:]
	if after == "" {
		return redLen, len(orig)
	}
	// The original text resumes where the next stretch of red first appears in orig. Use a short
	// anchor so a long tail does not need to match exactly.
	anchor := after
	if len(anchor) > 8 {
		anchor = anchor[:8]
	}
	if k := strings.Index(orig, anchor); k >= 0 {
		return redLen, k
	}
	return redLen, len(orig)
}

// aggregateTextForTest joins block 0's text deltas.
func aggregateTextForTest(stream string) (string, bool) {
	events, _ := splitSSE(stream)
	var b strings.Builder
	saw := false
	for _, e := range events {
		if gjson.Get(e.json, "type").String() == "content_block_delta" {
			b.WriteString(gjson.Get(e.json, "delta.text").String())
			saw = true
		}
	}
	return b.String(), saw
}
