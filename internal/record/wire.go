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
//	headers     the request headers the client sent, credentials removed
//
// Nothing else: the line holds what went over the wire, and no keys of ours. Claude Code sends
// its session id in its own header, which the loader does not look for; it then names the trace
// after the file, and the recorder writes one file per session.
//
// REDACTION. Both bodies are redacted before the line is built: the request with ContentJSON
// (string by string, so it stays valid JSON), the response with RedactSSE for a stream (across
// event boundaries, so a secret split over two events is caught) and ContentJSON otherwise.
// Headers: any header that carries a credential is dropped (see dropHeader), and every other
// value goes through Content, so a credential in an unexpected header is still caught.
//
// A body longer than the cap is cut after redaction. A cut request is no longer valid JSON and a
// cut stream has no message_stop, and the loader skips both, so a partial call is never replayed
// as a whole one.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// WireCall is one proxied call, as the proxy hands it over. Bodies are RAW; Wire redacts them.
type WireCall struct {
	Path        string
	Headers     http.Header // as the client sent them; Wire removes credentials
	Request     []byte
	Response    []byte
	ResponseSSE bool
	Status      int
	Start, End  time.Time
}

// wireLine is one JSONL line. Field order is the order in the file.
type wireLine struct {
	Path      string            `json:"path"`
	Request   string            `json:"request"`
	Response  string            `json:"response"`
	Status    int               `json:"status"`
	StartUnix float64           `json:"start_unix"`
	TotalMS   float64           `json:"total_ms"`
	Model     string            `json:"model,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// dropHeader reports whether a request header must never be written. Credentials by name, the
// proxy's own control headers, and cookies.
func dropHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "x-goog-api-key":
		return true
	}
	if strings.HasPrefix(n, "x-context-guru-") {
		return true // carries the CG token
	}
	for _, w := range []string{"key", "token", "secret", "auth", "password", "credential"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// wireHeaders keeps the headers that carry no credential, values redacted, multi-valued headers
// joined with ", " as HTTP allows.
func wireHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	out := map[string]string{}
	for _, k := range names {
		if dropHeader(k) {
			continue
		}
		out[strings.ToLower(k)] = Content(strings.Join(h[k], ", "))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Wire renders one call as a redacted JSONL line, newline included.
//
// bodyCap > 0 caps each body after redaction (see the file comment for what a cut body means).
func Wire(c WireCall, bodyCap int) ([]byte, error) {
	req := ContentJSON(string(c.Request))
	var resp string
	if c.ResponseSSE {
		resp = RedactSSE(string(c.Response))
	} else {
		resp = ContentJSON(string(c.Response))
	}
	req, _ = CapContent(req, bodyCap)
	resp, _ = CapContent(resp, bodyCap)
	l := wireLine{
		Path:      c.Path,
		Request:   req,
		Response:  resp,
		Status:    c.Status,
		StartUnix: float64(c.Start.UnixNano()) / 1e9,
		TotalMS:   float64(c.End.Sub(c.Start).Microseconds()) / 1e3,
		Model:     gjson.Get(req, "model").String(),
		Headers:   wireHeaders(c.Headers),
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
