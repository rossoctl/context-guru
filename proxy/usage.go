package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Usage is one response's provider-billed token tiers. ok=false means the
// provider told us nothing, in which case a caller must report the request as
// partially accounted rather than pricing it as free.
type Usage struct {
	FreshInput int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	// CacheWrite1h is the part of CacheWrite the provider billed at the ONE-HOUR write
	// tier, from `usage.cache_creation.ephemeral_1h_input_tokens`. Zero on every response
	// that wrote only 5-minute entries, which on this deployment is all of them.
	//
	// It is here for two reasons, and both are about not being lied to. A 1h write costs
	// 2.0x base input where a 5m write costs 1.25x, so pricing a 1h write as a 5m one
	// understates that request by 0.75x of its whole written prefix. And it is the ONLY
	// signal that a requested `ttl: "1h"` was HONOURED. A provider that does not support the
	// tier for that model downgrades it silently: a normal 200 with a normal-looking
	// `cache_creation_input_tokens` and nothing to distinguish it from a granted 1h entry.
	// Measured live on this gateway: aws/claude-haiku-4-5 returned 36,251 of 36,574 written
	// tokens at the 1h tier, aws/claude-sonnet-5 returned 0 of 48,212. Absent-or-zero here,
	// with a 1h breakpoint on the wire, means the request was downgraded.
	CacheWrite1h int64
	// StopReason is the provider's terminal reason for this response, normalized across
	// dialects (see responseStopReason). It rides on Usage because it comes off the same
	// buffered/sniffed response bytes and reaches the dashboard by the same path — a
	// second out-of-band channel for one string would be a second thing to keep in sync.
	//
	// It is filled even when the token tiers are absent, because the two facts are
	// independent: an OpenAI response without `stream_options` reports no usage at all and
	// still reports finish_reason, and "we could not price this, and it stopped because it
	// hit max_tokens" is exactly the pair worth having.
	StopReason string
}

// parseUsage extracts the four billed token tiers from a buffered response body,
// in whichever dialect it is written. This is the number that actually matters on
// this workload — the request is ~99.95% cached and a cache write bills ~11.5x a
// read, so content-token savings alone cannot express the economics.
//
// Dialects handled:
//
//	Anthropic  usage.{input_tokens, output_tokens,
//	           cache_read_input_tokens, cache_creation_input_tokens}
//	OpenAI     usage.{prompt_tokens, completion_tokens,
//	           prompt_tokens_details.cached_tokens}
//
// Anthropic's `input_tokens` already EXCLUDES the cached tiers, so it is the fresh
// figure directly. OpenAI's `prompt_tokens` INCLUDES its cached_tokens, so fresh is
// the difference — getting this backwards double-counts the whole transcript on
// every turn, which is exactly the kind of error a "savings" number hides.
func parseUsage(body []byte) (Usage, bool) {
	u := gjson.GetBytes(body, "usage")
	if !u.Exists() {
		return Usage{}, false
	}
	var out Usage
	switch {
	case u.Get("input_tokens").Exists(): // Anthropic
		out.FreshInput = u.Get("input_tokens").Int()
		out.Output = u.Get("output_tokens").Int()
		out.CacheRead = u.Get("cache_read_input_tokens").Int()
		out.CacheWrite = u.Get("cache_creation_input_tokens").Int()
		// The per-TTL split, when the provider reports one. `cache_creation_input_tokens`
		// is the total across both tiers, so this is a SUBSET of CacheWrite and never an
		// addition to it.
		out.CacheWrite1h = u.Get("cache_creation.ephemeral_1h_input_tokens").Int()
	case u.Get("prompt_tokens").Exists(): // OpenAI
		prompt := u.Get("prompt_tokens").Int()
		out.Output = u.Get("completion_tokens").Int()
		out.CacheRead = u.Get("prompt_tokens_details.cached_tokens").Int()
		out.FreshInput = prompt - out.CacheRead
		if out.FreshInput < 0 {
			out.FreshInput = 0
		}
	case u.Get("output_tokens").Exists():
		// An OUTPUT-ONLY block. Anthropic's streaming message_delta carries exactly
		// this, and it holds the FINAL completion count — treating it as "no usage"
		// under-reports every streamed response's output tokens.
		out.Output = u.Get("output_tokens").Int()
	default:
		return Usage{}, false
	}
	if out.FreshInput|out.CacheRead|out.CacheWrite|out.Output == 0 {
		return Usage{}, false
	}
	return out, true
}

// parseSSEUsage pulls usage out of a streamed response. Both dialects report it in
// terminal events (Anthropic: message_start carries the input tiers and
// message_delta the output; OpenAI: a final chunk with `usage`), so the tiers are
// merged across events, taking the maximum of each — a later event repeating a
// value must not double it.
func parseSSEUsage(raw []byte) (Usage, bool) {
	var out Usage
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		// Anthropic nests the first usage under message.usage; OpenAI puts it at the top.
		for _, path := range []string{"usage", "message.usage"} {
			u := gjson.Get(payload, path)
			if !u.Exists() {
				continue
			}
			one, ok := parseUsage([]byte(`{"usage":` + u.Raw + `}`))
			if !ok {
				continue
			}
			found = true
			out.FreshInput = max64(out.FreshInput, one.FreshInput)
			out.CacheRead = max64(out.CacheRead, one.CacheRead)
			out.CacheWrite = max64(out.CacheWrite, one.CacheWrite)
			out.CacheWrite1h = max64(out.CacheWrite1h, one.CacheWrite1h)
			out.Output = max64(out.Output, one.Output)
		}
	}
	return out, found
}

// responseUsage picks the right parser for a response's content type. The returned
// Usage carries the stop reason whatever `ok` says — see Usage.StopReason.
func responseUsage(contentType string, body []byte) (Usage, bool) {
	if len(body) == 0 {
		return Usage{}, false
	}
	sse := strings.Contains(contentType, "event-stream")
	var u Usage
	var ok bool
	if sse {
		u, ok = parseSSEUsage(body)
	} else {
		u, ok = parseUsage(body)
	}
	u.StopReason = responseStopReason(sse, body)
	return u, ok
}

// stopReasonPaths are where a terminal reason lives, per dialect and per transport:
//
//	Anthropic  stop_reason              (non-streaming)
//	           delta.stop_reason        (message_delta, the streamed terminal event)
//	OpenAI     choices.0.finish_reason  (both; the streamed final chunk carries it too)
//
// The values are the providers' own vocabulary and are recorded verbatim rather than
// mapped onto one another: `end_turn` and `stop` mean the same thing, but flattening them
// would quietly assert that `pause_turn`, `refusal` and `content_filter` have equivalents
// on the other side, and they do not.
var stopReasonPaths = [...]string{"stop_reason", "delta.stop_reason", "choices.0.finish_reason"}

// responseStopReason reads the terminal reason off a response body. For SSE the events
// are scanned newest-first: the terminal reason is in the LAST event that carries one, and
// an earlier chunk's `finish_reason: null` must not win.
//
// Whatever this returns is still client-influenced text by the time it reaches the
// database (an upstream is configurable, so its response is not trusted input), so it is
// re-checked by dash.metaEnum before the insert.
func responseStopReason(sse bool, body []byte) string {
	if !sse {
		return firstStopReason(string(body))
	}
	lines := strings.Split(string(body), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if s := firstStopReason(payload); s != "" {
			return s
		}
	}
	return ""
}

// firstStopReason returns the first non-empty stop reason among the known paths.
func firstStopReason(payload string) string {
	for _, p := range stopReasonPaths {
		if r := gjson.Get(payload, p); r.Type == gjson.String && r.Str != "" {
			return r.Str
		}
	}
	return ""
}

// sniffMax bounds each half of the sniffer's window. Usage blocks are a few
// hundred bytes; 64 KiB each way is generous and hard-caps the memory an
// adversarially long response can make us hold per in-flight request.
const sniffMax = 64 << 10

// sniffer keeps a bounded head+tail window of a streamed response so its usage
// block can be read after the stream completes, without buffering the response.
// A disabled sniffer allocates nothing and does no work.
type sniffer struct {
	on    bool
	head  []byte
	tail  []byte
	total int // bytes written, so bytes() knows whether the head alone is the whole body
}

func newSniffer(on bool) *sniffer { return &sniffer{on: on} }

func (s *sniffer) write(p []byte) {
	if !s.on {
		return
	}
	s.total += len(p)
	if len(s.head) < sniffMax {
		n := min(len(p), sniffMax-len(s.head))
		s.head = append(s.head, p[:n]...)
	}
	s.tail = append(s.tail, p...)
	if len(s.tail) > sniffMax {
		// Keep the last sniffMax bytes, re-slicing into a fresh buffer so the old one
		// can be collected rather than growing forever behind the slice header.
		keep := append(make([]byte, 0, sniffMax), s.tail[len(s.tail)-sniffMax:]...)
		s.tail = keep
	}
}

// bytes returns the retained window: head, then tail, with a newline between so a
// truncated SSE line in the middle cannot glue two events into one bogus line.
func (s *sniffer) bytes() []byte {
	if !s.on {
		return nil
	}
	if s.total <= len(s.head) {
		return s.head // the whole response fit in the head window; head IS the body
	}
	out := make([]byte, 0, len(s.head)+len(s.tail)+1)
	out = append(out, s.head...)
	out = append(out, '\n')
	return append(out, s.tail...)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
