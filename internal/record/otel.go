// Package record turns one proxied inference turn into one OTel-shaped span, in the
// exact on-disk format the existing claude-record analysis pipeline already consumes.
//
// WHY THIS FORMAT AND NOT OTLP. The consumer is an existing corpus of trace files and the
// scripts built on them, not a collector. Those files are NOT standard OTLP: there is no
// resourceSpans/scopeSpans nesting, `attributes` is a flat JSON object rather than a list
// of {key,value:{stringValue}} entries, and timestamps are ISO-8601 strings rather than
// epoch nanoseconds. Emitting real OTLP here would be more correct in the abstract and
// would silently break every analysis script, so this package matches the corpus and says
// so. A converter to OTLP is a separate, later, additive thing.
//
// WHY THE SPAN NAME IS `chat <model>`. The analysis scripts select turns with
// `name.startswith("chat ")`. A span named plain `chat` is invisible to them -- a trap
// already hit once in this project. The name is built in one place, below, so it cannot
// drift.
package record

import (
	"encoding/json"
	"fmt"
	"time"
)

// CollectorVersion identifies the producer in every file written. It is deliberately
// DISTINCT from claude-record's own version string: a corpus that mixes two producers must
// be able to say which rows came from which, or every coverage difference between them
// becomes unattributable at analysis time.
const CollectorVersion = "cg-record/0.1.0"

// SpanKindClient is the only kind this package emits: a proxied model call is an outbound
// request. Spelled as the corpus spells it.
const SpanKindClient = "SPAN_KIND_CLIENT"

// Status codes as the corpus uses them: 1 = OK, 2 = ERROR. (OTel's own OK is 1 and ERROR
// is 2, so these agree; the corpus just stores the integer.)
const (
	StatusOK    = 1
	StatusError = 2
)

// Status is a span's outcome.
type Status struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Span is one recorded turn, shaped for the existing corpus. Field order in the struct is
// the field order in the JSON, which keeps a hand-diff against a corpus file readable.
type Span struct {
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	StartTime    string         `json:"start_time"`
	EndTime      string         `json:"end_time"`
	Attributes   map[string]any `json:"attributes"`
	ResourceAttr map[string]any `json:"resource_attributes"`
	Status       Status         `json:"status"`
}

// File is one session's worth of spans: the unit claude-record writes and the analysis
// scripts load.
type File struct {
	TraceID          string `json:"trace_id"`
	SpanCount        int    `json:"span_count"`
	CollectedAt      string `json:"collected_at"`
	Spans            []Span `json:"spans"`
	CollectorVersion string `json:"collector_version"`
}

// Turn is everything this package needs to know about one proxied request. It is a plain
// data struct on purpose: the proxy fills it, the recorder writes it, and nothing in
// between needs a live handler. That is what makes the recorder unit-testable without
// standing up a proxy.
type Turn struct {
	SessionID string // becomes trace_id
	SpanID    string
	ParentID  string

	Model     string
	System    string // "anthropic" | "openai"
	Start     time.Time
	End       time.Time
	MaxTokens int
	Effort    string
	OutputTy  string // "json" | "text"

	// Bodies, already redacted by the caller. RequestBody is the PRISTINE client request
	// (pre-pipeline): a corpus of what the agent actually sent is the thing that is
	// reconstructable, and CG is the only place that still has it after compaction.
	RequestBody  string
	ResponseBody string
	OutputSchema string
	// ToolDefs is the tool schema the client advertised on this request. Present on 5,037
	// of the corpus's 5,680 chat spans -- the second-commonest body attribute after the
	// messages -- and it is what makes a turn interpretable: a tool_use block in the
	// response names a tool whose parameters live only here. CG has it for free, in the
	// same pristine request body.
	ToolDefs string

	// Usage. The three components are what claude-record cannot PRESERVE: it reads them from
	// Jaeger tags, sums them into gen_ai.usage.input_tokens, and keeps no column for the
	// split, so no corpus file can answer a cache question after the fact. CG has them at the
	// source. The sum stays identical either way (see InputTokens); the split is the addition,
	// and it is the one mooncake needs.
	FreshInput int
	CacheRead  int
	CacheWrite int
	Output     int
	StopReason string

	// Timing that only a proxy knows.
	UpstreamMS  int64
	FirstByteMS int64
	Rounds      int

	// Incompleteness. THIS IS LOAD-BEARING: two CG response paths (past the 16 MB retain
	// bound, and a continuation where both rounds truncated) yield no body at all, and the
	// stream tee is capped. A turn that quietly carries an empty response body is a corpus
	// defect nobody discovers until analysis; a turn that SAYS it is incomplete is data.
	// Never leave this empty to mean "fine" -- see IncompleteNone.
	Incomplete   string
	ResponseSize int

	Err string
}

// Incompleteness reasons. Exported so the proxy and the analysis side name them
// identically rather than by matching strings by eye.
const (
	IncompleteNone      = ""                 // the whole turn was captured
	IncompleteTeeCap    = "tee_cap_reached"  // response longer than the record cap
	IncompleteRetainCap = "retain_bound"     // CG's 16 MB retain bound; body never materialised
	IncompleteTruncated = "rounds_truncated" // continuation exit, both rounds truncated
	IncompleteNoBody    = "no_body_on_path"  // a terminal path that never buffers a body
	// IncompleteNoUsage: the response's usage block was absent or unparseable, so the
	// cache-split values on this span are zeros that were never measured. Needed because
	// ToSpan emits those keys unconditionally -- deliberately, since a genuine zero is data --
	// which leaves "no cache activity" and "unknown" indistinguishable without this marker.
	IncompleteNoUsage = "no_usage_on_response"
)

// Attribute keys. Every one the existing corpus uses is spelled here exactly as the corpus
// spells it, because a typo is a silently-missing column in someone's analysis. The cg.*
// keys are ADDITIVE -- new information claude-record never had -- and namespaced so
// nothing can mistake them for standard gen_ai semconv.
const (
	AttrOperation    = "gen_ai.operation.name"
	AttrSystem       = "gen_ai.system"
	AttrModel        = "gen_ai.request.model"
	AttrMaxTokens    = "gen_ai.request.max_tokens"
	AttrEffort       = "gen_ai.request.effort"
	AttrOutputSchema = "gen_ai.request.output_schema"
	AttrOutputType   = "gen_ai.output.type"
	AttrInputMsgs    = "gen_ai.input.messages"
	AttrOutputMsgs   = "gen_ai.output.messages"
	AttrToolDefs     = "gen_ai.tool.definitions"
	AttrInputTokens  = "gen_ai.usage.input_tokens"
	AttrOutputTokens = "gen_ai.usage.output_tokens"

	// The cache split, under the GenAI semconv names claude-record also writes since 0.1.5
	// (collector #34). Both are already counted in AttrInputTokens, as the semconv says.
	AttrCacheRead  = "gen_ai.usage.cache_read.input_tokens"
	AttrCacheWrite = "gen_ai.usage.cache_creation.input_tokens"

	// CG-only. Semconv has no name for the uncached remainder.
	AttrFreshInput   = "cg.usage.fresh_input_tokens"
	AttrStopReason   = "cg.response.stop_reason"
	AttrUpstreamMS   = "cg.upstream_ms"
	AttrFirstByteMS  = "cg.first_byte_ms"
	AttrRounds       = "cg.upstream_rounds"
	AttrIncomplete   = "cg.incomplete"
	AttrResponseSize = "cg.response_bytes"
	AttrRecordedBy   = "cg.recorded_by"
)

// timeFormat matches the corpus: ISO-8601 with microseconds and a numeric UTC offset, e.g.
// `2026-09-10T07:18:20.896000+00:00`. Go's RFC3339Nano is NOT the same string (it trims
// trailing zeros and writes `Z`), and the analysis side parses with
// datetime.fromisoformat, which in older Pythons rejects `Z`. So format explicitly.
const timeFormat = "2006-01-02T15:04:05.000000-07:00"

// FormatTime renders t the way the corpus stores timestamps: UTC, microseconds, +00:00.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

// SpanName builds the `chat <model>` name the analysis scripts match on. One place, so the
// `chat ` prefix cannot be lost.
func SpanName(model string) string {
	if model == "" {
		return "chat unknown"
	}
	return "chat " + model
}

// InputTokens is what goes in gen_ai.usage.input_tokens: fresh + cache_read + cache_write.
//
// THIS MATCHES claude-record EXACTLY, and it is worth writing down because an earlier version
// of this comment claimed the opposite. claude-record's converter.py has a total_input_tokens()
// helper that sums all three components for precisely the OTel-semconv reason -- Anthropic
// reports input_tokens as the uncached remainder only, so the full prompt size is the sum. The
// collector already does the right thing here.
//
// So this column IS bit-comparable between the two recorders, which is a stronger parity claim
// than the one previously recorded here. Issue #17 is about a different layer: Claude Code's
// OTel metrics export, not this conversion.
//
// The SPLIT is what mooncake needs, not the total. claude-record before 0.1.5 kept only the
// sum in the OTel file, so the split is not recoverable from those corpus files; since 0.1.5
// both recorders write it under the same semconv names. CG has it at the source, on every turn.
func (t Turn) InputTokens() int { return t.FreshInput + t.CacheRead + t.CacheWrite }

// ToSpan renders the turn. Bodies must already be redacted; see Redact.
func (t Turn) ToSpan() Span {
	at := map[string]any{
		AttrOperation:    "chat",
		AttrModel:        t.Model,
		AttrInputTokens:  t.InputTokens(),
		AttrOutputTokens: t.Output,
		AttrRecordedBy:   CollectorVersion,
	}
	if t.System != "" {
		at[AttrSystem] = t.System
	}
	if t.MaxTokens > 0 {
		at[AttrMaxTokens] = t.MaxTokens
	}
	if t.Effort != "" {
		at[AttrEffort] = t.Effort
	}
	if t.OutputTy != "" {
		at[AttrOutputType] = t.OutputTy
	}
	if t.RequestBody != "" {
		at[AttrInputMsgs] = t.RequestBody
	}
	if t.ResponseBody != "" {
		at[AttrOutputMsgs] = t.ResponseBody
	}
	if t.OutputSchema != "" {
		at[AttrOutputSchema] = t.OutputSchema
	}
	if t.ToolDefs != "" {
		at[AttrToolDefs] = t.ToolDefs
	}
	// The cache split: emitted even when zero, because "this provider reported no cache
	// activity" and "this recorder does not know" are different facts, and the whole point
	// of recording through CG is that we are in the first case, never the second.
	at[AttrFreshInput] = t.FreshInput
	at[AttrCacheRead] = t.CacheRead
	at[AttrCacheWrite] = t.CacheWrite

	if t.StopReason != "" {
		at[AttrStopReason] = t.StopReason
	}
	if t.UpstreamMS > 0 {
		at[AttrUpstreamMS] = t.UpstreamMS
	}
	if t.FirstByteMS > 0 {
		at[AttrFirstByteMS] = t.FirstByteMS
	}
	if t.Rounds > 0 {
		at[AttrRounds] = t.Rounds
	}
	if t.ResponseSize > 0 {
		at[AttrResponseSize] = t.ResponseSize
	}
	// Incompleteness is emitted ONLY when true, so its presence is the signal and no
	// analysis has to compare against a sentinel.
	if t.Incomplete != IncompleteNone {
		at[AttrIncomplete] = t.Incomplete
	}

	st := Status{Code: StatusOK}
	if t.Err != "" {
		st = Status{Code: StatusError, Message: t.Err}
	}

	return Span{
		TraceID:      t.SessionID,
		SpanID:       t.SpanID,
		ParentSpanID: t.ParentID,
		Name:         SpanName(t.Model),
		Kind:         SpanKindClient,
		StartTime:    FormatTime(t.Start),
		EndTime:      FormatTime(t.End),
		Attributes:   at,
		ResourceAttr: map[string]any{"service.name": "claude-code"},
		Status:       st,
	}
}

// NewFile assembles a session file. collectedAt is passed in rather than read from the
// clock so the result is testable.
func NewFile(traceID string, spans []Span, collectedAt time.Time) File {
	return File{
		TraceID:          traceID,
		SpanCount:        len(spans),
		CollectedAt:      FormatTime(collectedAt),
		Spans:            spans,
		CollectorVersion: CollectorVersion,
	}
}

// Marshal renders the file as the corpus stores it: indented, trailing newline.
func (f File) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("record: marshal trace file: %w", err)
	}
	return append(b, '\n'), nil
}
