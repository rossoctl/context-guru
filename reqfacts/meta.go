package reqfacts

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Meta is the request's own metadata — the knobs the CLIENT chose, plus the stop
// reason the provider answered with. Captured because they are the levers that explain
// a cost: reasoning effort and thinking budget buy output tokens, and where the
// cache_control breakpoints sit decides how much of the prefix was billed as a read
// rather than a write, which is the whole subject of this project.
//
// Real columns rather than a JSON blob, deliberately. Every field here is either
// GROUPED BY in an aggregate (effort, thinking mode, stop reason, tool_choice, the
// breakpoint counts) or a scalar rendered on the request row; a JSON column would put
// json_extract() in the middle of every one of those queries and give the free-text tail
// somewhere to hide from the redactor. There is no long tail to justify one.
//
// Anthropic and OpenAI spell the same knobs differently; capture normalizes them (see
// MetaFromBody) so one column means one thing across dialects.
type Meta struct {
	// ReasoningEffort is Anthropic's `output_config.effort` or OpenAI's
	// `reasoning_effort` — low|medium|high|xhigh|max, "" when unset. NOT a synonym for
	// thinking: on current Anthropic models effort is the depth control and
	// thinking.budget_tokens is the removed predecessor, so both are recorded.
	ReasoningEffort string `json:"reasoning_effort"`
	// ThinkingMode is Anthropic `thinking.type` — adaptive|enabled|disabled, "" absent.
	ThinkingMode string `json:"thinking_mode"`
	// ThinkingBudget is `thinking.budget_tokens` (pre-4.6 models only; 0 = unset).
	ThinkingBudget int `json:"thinking_budget"`
	// Temperature and TopP are POINTERS, and the column is NULLABLE, because "the client
	// did not set it" and "the client set it to 0" are different facts and 0 is a
	// legitimate value for both. A sentinel would make a deterministic request
	// indistinguishable from an unspecified one.
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	// MaxTokens is the client's output cap (`max_tokens`, or OpenAI's
	// `max_completion_tokens`); 0 = unset.
	MaxTokens int  `json:"max_tokens"`
	Stream    bool `json:"stream"`
	// ToolChoice is the normalized forcing mode: auto|any|none|required|tool for the
	// object form, or the bare string OpenAI sends. The forced tool's NAME is
	// deliberately not stored — it is unbounded client text with no aggregate use.
	ToolChoice string `json:"tool_choice"`
	// Tools is how many tools the request declared; SystemBlocks how many blocks the
	// top-level `system` array carried (1 for a bare string).
	Tools        int `json:"tools"`
	SystemBlocks int `json:"system_blocks"`
	// The prompt-cache breakpoints ON ARRIVAL, by location: `tools` and `system` render
	// ahead of `messages`, so location decides how much prefix a breakpoint protects.
	// Zero on a request the pipeline never inspected (observe mode), like every other
	// pipeline-derived field on such a row.
	CacheBPSystem   int `json:"cache_bp_system"`
	CacheBPTools    int `json:"cache_bp_tools"`
	CacheBPMessages int `json:"cache_bp_messages"`
	CacheBPBlocks   int `json:"cache_bp_blocks"`
	CacheBPRoot     int `json:"cache_bp_root"` // the top-level cache_control (automatic caching)
	// StopReason is the provider's own terminal reason, normalized across dialects:
	// Anthropic `stop_reason` (end_turn|max_tokens|tool_use|stop_sequence|pause_turn|
	// refusal|model_context_window_exceeded) or OpenAI `choices.0.finish_reason`
	// (stop|length|tool_calls|content_filter). "" when the response carried none.
	// StopTimeoutEstimated is the one value the proxy writes itself (#754).
	StopReason string `json:"stop_reason"`
}

// CacheBreakpoints is the total the provider's cap of four applies to.
func (m Meta) CacheBreakpoints() int {
	return m.CacheBPSystem + m.CacheBPTools + m.CacheBPMessages + m.CacheBPBlocks + m.CacheBPRoot
}

// The dialect map MetaFromBody normalizes. Two dialects onto one set of columns, because a
// column that means `reasoning_effort` for one client and nothing for another cannot be
// aggregated:
//
//	Anthropic  max_tokens, temperature, top_p, stream, tools[], tool_choice{type},
//	           system[] (blocks, or a bare string), thinking{type,budget_tokens},
//	           output_config{effort}
//	OpenAI     max_tokens / max_completion_tokens, temperature, top_p, stream, tools[],
//	           tool_choice (a bare string, or {function:{name}}), reasoning_effort,
//	           and no top-level system at all — it is a role=system message
//
// MetaFromBody reads the request's metadata off the raw body in ONE structural pass.
//
// One pass, and it is measured rather than assumed. This runs on the request goroutine, so
// the shape of the cost matters more than its absolute size:
//
//	one ForEach over the top-level object   0.55 ms/MB, 1 allocation
//	one gjson path query per field (12)     2.7  ms/MB, ~5 allocations
//
// The per-field version re-scans past the whole `messages` array once per field, which is
// 5x the work for a body of any interesting size — so the single pass wins, even though it
// pays for one body-length copy (`gjson.ParseBytes` is `Parse(string(json))`). For scale:
// the pipeline on this same path already runs seven whole-body path queries to count
// breakpoints, and re-serializes the body outright when a component rewrites a message.
// ponytail: costs one body-sized short-lived copy; swap for a zero-copy iterator if this
// ever appears in a profile.
//
// The ForEach never parses `messages` — each value is skipped structurally by byte — so the
// cost is linear in the body and independent of the transcript's depth or message count.
//
// Breakpoint counts are NOT read here: the pipeline already counts them to respect the
// provider's cap of four, and they reach the event on the trace for free (see FromTrace).
func MetaFromBody(body []byte) Meta {
	m := metaFromBodyRefs(body)
	// COPY EVERY STRING OUT OF THE BODY. gjson's String() returns a substring of `body` without
	// copying, so a Meta built from it holds the WHOLE request body alive for as long as the Meta
	// lives — and it lives far past the request: the event carries it to the writer, and the SSE
	// hub keeps the last 256 events for late subscribers. On CG2, 256 kept events from
	// 1,000-message sessions held 1-20 MB bodies each: four OOM kills at the 8 GiB limit on
	// 2026-10-19/20. strings.Clone makes each field a few bytes of its own.
	m.ReasoningEffort = strings.Clone(m.ReasoningEffort)
	m.ThinkingMode = strings.Clone(m.ThinkingMode)
	m.ToolChoice = strings.Clone(m.ToolChoice)
	return m
}

// metaFromBodyRefs reads the fields. Its strings ALIAS body; only MetaFromBody may return them.
func metaFromBodyRefs(body []byte) Meta {
	var m Meta
	gjson.ParseBytes(body).ForEach(func(k, v gjson.Result) bool {
		switch k.Str {
		case "max_tokens", "max_completion_tokens":
			// Either dialect's output cap. Anthropic's spelling wins if a body carries
			// both, which is what an Anthropic-shaped upstream would honour too.
			if m.MaxTokens == 0 || k.Str == "max_tokens" {
				m.MaxTokens = int(v.Int())
			}
		case "temperature":
			// Number-typed only, and by POINTER: "0" and absent are different facts, and a
			// non-numeric value is a malformed request rather than a temperature.
			if v.Type == gjson.Number {
				f := v.Float()
				m.Temperature = &f
			}
		case "top_p":
			if v.Type == gjson.Number {
				f := v.Float()
				m.TopP = &f
			}
		case "stream":
			m.Stream = v.Bool()
		case "reasoning_effort": // OpenAI: top level
			m.ReasoningEffort = v.String()
		case "output_config": // Anthropic: nested, never top level
			// Both accepted forms: the bare level string and {"type": "<level>"}.
			if e := v.Get("effort"); e.Exists() {
				if e.IsObject() {
					m.ReasoningEffort = e.Get("type").String()
				} else {
					m.ReasoningEffort = e.String()
				}
			}
		case "thinking":
			m.ThinkingMode = v.Get("type").String()
			m.ThinkingBudget = int(v.Get("budget_tokens").Int())
		case "tool_choice":
			m.ToolChoice = toolChoiceMode(v)
		case "tools":
			m.Tools = int(v.Get("#").Int())
		case "system":
			// A bare string is one block; an array is its length. A COUNT, never the text:
			// the system prompt is a request's most sensitive content, and its shape is
			// what explains a cache prefix. The text belongs to transcript capture, under
			// that gate and that redaction.
			if v.IsArray() {
				m.SystemBlocks = int(v.Get("#").Int())
			} else if v.Type == gjson.String && v.Str != "" {
				m.SystemBlocks = 1
			}
		}
		return true
	})
	return m
}

// toolChoiceMode normalizes the two dialects' tool_choice shapes to the forcing MODE.
// OpenAI sends a bare string ("auto"|"none"|"required") or an object naming a function;
// Anthropic sends an object ({"type":"auto"|"any"|"none"|"tool"}). The forced tool's name
// is deliberately dropped: it is unbounded client text and no aggregate asks for it.
func toolChoiceMode(v gjson.Result) string {
	if v.Type == gjson.String {
		return v.Str
	}
	if v.IsObject() {
		if t := v.Get("type"); t.Type == gjson.String {
			return t.Str
		}
		if v.Get("function.name").Exists() {
			return "tool" // OpenAI's object form forces one named function
		}
	}
	return ""
}
