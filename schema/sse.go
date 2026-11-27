package schema

import (
	"bytes"

	"github.com/tidwall/gjson"
)

// wholeJSONMax is the size of joined data up to which SSEData asks if it is already whole JSON.
const wholeJSONMax = 64 << 10

// SSEData walks a Server-Sent Events stream and calls fn with the data of each event, in order.
// fn returns false to stop. It is the one reader of SSE framing for the readers that only want the
// payloads; a reader that must forward the bytes unchanged keeps its own event splitter.
//
// The rules, from the SSE spec:
//   - A line ends at "\n", and a "\r" before it is dropped, so "\r\n" streams read the same.
//   - A blank line ends an event. The end of the stream ends the last event.
//   - A "data:" line adds to the event's data. Several lines of one event are joined with "\n".
//     One exception, for a stream that leaves out the blank line between events: when the lines so
//     far are already one whole JSON value (and are at most 64 KiB), the next data line starts a new event.
//   - Other fields (event:, id:, retry:) and ":" comment lines are skipped.
//
// Two rules are for this proxy and are not in the spec:
//   - The payload is trimmed of white space. JSON does not care, and a provider may add a space.
//   - An event with no data, and the "[DONE]" end marker of the OpenAI wires, call nothing.
//
// The slice fn gets is only valid inside the call. For an event of one data line it points into
// body, so the common case copies nothing.
func SSEData(body []byte, fn func(data []byte) bool) {
	var (
		first  []byte // the data line of the event, when it has one
		joined []byte // the joined data, when the event has more than one
		many   bool
		have   bool
	)
	flush := func() bool {
		defer func() { first, joined, many, have = nil, joined[:0], false, false }()
		if !have {
			return true
		}
		d := first
		if many {
			d = joined
		}
		d = bytes.TrimSpace(d)
		if len(d) == 0 || string(d) == "[DONE]" {
			return true
		}
		return fn(d)
	}
	for len(body) > 0 {
		var line []byte
		if i := bytes.IndexByte(body, '\n'); i >= 0 {
			line, body = body[:i], body[i+1:]
		} else {
			line, body = body, nil
		}
		if len(bytes.TrimSuffix(line, []byte("\r"))) == 0 {
			if !flush() {
				return
			}
			continue
		}
		v, ok := SSEDataLine(line)
		if !ok {
			continue
		}
		if have {
			cur := first
			if many {
				cur = joined
			}
			// A line that is a whole JSON value on its own is an event, whatever follows it. A
			// stream that leaves out the blank line between two events reads the same as one that
			// does not, as it did before this reader joined lines. A fragment of JSON is joined.
			// The check stops above wholeJSONMax: a long run of fragments is then read by blank-line
			// framing alone, and a stream with no blank lines cannot make the check quadratic.
			if len(cur) <= wholeJSONMax && gjson.ValidBytes(bytes.TrimSpace(cur)) {
				if !flush() {
					return
				}
			}
		}
		switch {
		case !have:
			first, have = v, true
		case !many:
			joined = append(append(append(joined[:0], first...), '\n'), v...)
			many = true
		default:
			joined = append(append(joined, '\n'), v...)
		}
	}
	flush()
}

// SSEEventData is the data of ONE event block (the lines of one event, with or without the
// blank line after them), by the rules of SSEData. It returns nil when the event has no data or
// carries the "[DONE]" marker. The result may point into ev.
func SSEEventData(ev []byte) []byte {
	var out []byte
	SSEData(ev, func(d []byte) bool {
		out = d
		return false
	})
	return out
}

// SSEDataLine is the value of one "data:" line of a stream, without its field name and without a
// trailing "\r". ok is false for any other line. A reader that gets its bytes one line at a time,
// and cannot hold a whole event, uses this. It then reads an event of one data line only: that is
// all Anthropic sends, and an event of several data lines needs SSEData.
func SSEDataLine(line []byte) (value []byte, ok bool) {
	v, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\r")), []byte("data:"))
	return bytes.TrimPrefix(v, []byte(" ")), ok // the spec drops one space after the colon
}
