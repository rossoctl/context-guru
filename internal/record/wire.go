package record

// Wire recording: one JSONL line per proxied HTTP call, in the format Inference-Perf replays
// directly (kubernetes-sigs/inference-perf#820, inference_perf/datagen/replay/
// wire_trace_converter.py).
//
// WHY WIRE. It is the raw record: the request body as the client sent it, and the response as the
// upstream sent it -- an SSE event stream for a streamed call. Nothing is translated away
// (thinking blocks, unknown fields, provider-specific shapes all survive), so OTLP or any other
// format can be made from it later, and the eval team's replay reads it as is.
//
// THE FORMAT, as the loader reads it. One file is one session. Per line:
//
//	path        the API path; it selects the dialect ("/v1/messages" = Anthropic)
//	request     the request body, as a JSON string
//	response    the response body, as a string: the SSE text for a stream, else the JSON
//	status      the HTTP status
//	start_unix  start time, seconds since the epoch (fractional)
//	total_ms    duration in milliseconds
//	model       the request's model
//	headers     request headers; the loader takes the session id from "session_id" here
//
// The cg_* keys are ours: facts only the proxy knows (time to first byte, upstream rounds, and
// why a body is incomplete). The loader reads only the keys above, so it ignores them.
//
// REDACTION. Both bodies are redacted before the line is built: the request with ContentJSON
// (string by string, so it stays valid JSON), the response with RedactSSE for a stream (across
// event boundaries, so a secret split over two events is caught) and ContentJSON otherwise.
// Headers are an allow-list: only the session id is written, never a credential.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/tidwall/gjson"
)

// WireCall is one proxied call, as the proxy hands it over. Bodies are RAW; Wire redacts them.
type WireCall struct {
	SessionID   string
	Path        string
	Request     []byte
	Response    []byte
	ResponseSSE bool
	Status      int
	Start, End  time.Time
	FirstByteMS int64
	Rounds      int
	Incomplete  string
}

// wireLine is one JSONL line. Field order is the order in the file.
type wireLine struct {
	Path       string            `json:"path"`
	Request    string            `json:"request"`
	Response   string            `json:"response"`
	Status     int               `json:"status"`
	StartUnix  float64           `json:"start_unix"`
	TotalMS    float64           `json:"total_ms"`
	Model      string            `json:"model,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	FirstByte  int64             `json:"cg_first_byte_ms,omitempty"`
	Rounds     int               `json:"cg_upstream_rounds,omitempty"`
	Incomplete string            `json:"cg_incomplete,omitempty"`
	Recorder   string            `json:"cg_recorded_by"`
}

// wireSessionHeader is the request header the Inference-Perf loader reads a session id from,
// first in its preference order.
const wireSessionHeader = "session_id"

// WireRecordedBy identifies the producer on every wire line.
const WireRecordedBy = "cg-record/0.3.0"

// Wire renders one call as a redacted JSONL line, newline included.
//
// bodyCap > 0 caps each body after redaction. A capped body is cut, so it is no longer valid
// JSON or a whole stream; the line says so in cg_incomplete, and the loader skips a stream with
// no message_stop rather than replay half a turn.
func Wire(c WireCall, bodyCap int) ([]byte, error) {
	req := ContentJSON(string(c.Request))
	var resp string
	if c.ResponseSSE {
		resp = RedactSSE(string(c.Response))
	} else {
		resp = ContentJSON(string(c.Response))
	}
	inc := c.Incomplete
	var hit bool
	if req, hit = CapContent(req, bodyCap); hit {
		inc = joinReasons(inc, IncompleteBodyCap)
	}
	if resp, hit = CapContent(resp, bodyCap); hit {
		inc = joinReasons(inc, IncompleteBodyCap)
	}
	l := wireLine{
		Path:       c.Path,
		Request:    req,
		Response:   resp,
		Status:     c.Status,
		StartUnix:  float64(c.Start.UnixNano()) / 1e9,
		TotalMS:    float64(c.End.Sub(c.Start).Microseconds()) / 1e3,
		Model:      gjson.Get(req, "model").String(),
		FirstByte:  c.FirstByteMS,
		Rounds:     c.Rounds,
		Incomplete: inc,
		Recorder:   WireRecordedBy,
	}
	if c.SessionID != "" {
		l.Headers = map[string]string{wireSessionHeader: c.SessionID}
	}
	if l.TotalMS < 0 {
		l.TotalMS = 0
	}
	b, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("record: encode wire line: %w", err)
	}
	return append(b, '\n'), nil
}

// IncompleteBodyCap: a body was longer than --record-body-cap and was cut after redaction.
const IncompleteBodyCap = "body_cap_reached"

func joinReasons(existing, reason string) string {
	t := Turn{Incomplete: existing}
	t.markIncomplete(reason)
	return t.Incomplete
}
