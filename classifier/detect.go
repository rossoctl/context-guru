// Package classifier finds Claude Code's auto-mode classifier requests and answers them with a
// model from the routing table instead of the agent's own.
//
// Two halves, both free of transport and credentials:
//
//   - detect.go: is this request a classifier request? Metadata only — header values and the
//     request's SHAPE (counts and flags). Never the text of a message or a system prompt.
//   - redirect.go: convert the request for the routed model's dialect, send it through
//     dispatch.Dispatcher with the row's Ref, and convert the answer back to an Anthropic
//     Messages response. It retries a short, bounded time, then returns an error so the host
//     forwards the ORIGINAL request unchanged.
//
// The classifier is a safety check. A cheaper model can allow what the original would block, so
// the feature is off unless the operator adds a `classifier` row to the routing table (#613).
package classifier

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// Feature is the routing-table feature name this package reads.
const Feature = "classifier"

// RequestClassHeader is the header #423 lists for Claude Code's request class. Its value for a
// classifier request is NOT yet confirmed on the wire, so HeaderMatch is loose on purpose and
// the shape below stays as the fallback.
const RequestClassHeader = "x-claude-code-request-class"

// Shape is the metadata detection reads. Counts and flags only.
type Shape struct {
	Model            string
	Tools            int
	Messages         int
	SystemBlocks     int
	MaxTokens        int
	Stream           bool
	HasStopSequences bool
}

// The classifier shape measured on production traffic (#614): Sonnet, no tools, 2 messages,
// 3 system blocks, max_tokens 64, no stream, stop sequences set. The Opus max_tokens 2112 shape
// is an open question and is NOT matched.
const (
	wantMessages     = 2
	wantSystemBlocks = 3
	wantMaxTokens    = 64
)

// IsClassifier reports whether every part of the shape matches. One part off is a near-miss and
// does not match: a misrouted main turn would go to a weaker model.
func (s Shape) IsClassifier() bool {
	return strings.Contains(strings.ToLower(s.Model), "sonnet") &&
		s.Tools == 0 &&
		s.Messages == wantMessages &&
		s.SystemBlocks == wantSystemBlocks &&
		s.MaxTokens == wantMaxTokens &&
		!s.Stream &&
		s.HasStopSequences
}

// ShapeOf reads the shape off a request body. It reads counts and flags and keeps no text.
func ShapeOf(body []byte) Shape {
	var s Shape
	s.Model = gjson.GetBytes(body, "model").String()
	s.Tools = int(gjson.GetBytes(body, "tools.#").Int())
	s.Messages = int(gjson.GetBytes(body, "messages.#").Int())
	if sys := gjson.GetBytes(body, "system"); sys.IsArray() {
		s.SystemBlocks = int(sys.Get("#").Int())
	} else if sys.Type == gjson.String && sys.Str != "" {
		s.SystemBlocks = 1
	}
	s.MaxTokens = int(gjson.GetBytes(body, "max_tokens").Int())
	s.Stream = gjson.GetBytes(body, "stream").Bool()
	s.HasStopSequences = gjson.GetBytes(body, "stop_sequences.#").Int() > 0
	return s
}

// HeaderMatch reports whether the request-class header names a classifier. The value is not
// confirmed on the wire, so any value containing "classifier" matches. "auxiliary" alone does
// not: that class covers other small requests, and routing those is not this feature's job.
func HeaderMatch(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get(RequestClassHeader)), "classifier")
}

// Detect is the one entry point. A header match or the shape qualifies. Either way the request
// must not stream, because the redirect answers with one JSON body and cannot produce SSE.
func Detect(h http.Header, body []byte) bool {
	if gjson.GetBytes(body, "stream").Bool() {
		return false
	}
	if HeaderMatch(h) {
		return true
	}
	// Every non-classifier Anthropic request lands here when a row is active, so leave on the
	// cheapest field first: one scan decides almost all of them.
	if gjson.GetBytes(body, "max_tokens").Int() != wantMaxTokens {
		return false
	}
	return ShapeOf(body).IsClassifier()
}
