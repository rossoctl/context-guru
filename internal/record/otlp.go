package record

// The OTLP rendering of a recorded turn: what the proxy exports.
//
// WHAT THIS IS FOR. The proxy's record of truth is wire (wire.go). This file is the optional live
// view: with --record-otlp, each turn is also sent as an OTLP span, so Phoenix, Jaeger and other
// collectors can show a session while it runs.
//
// WHAT IS ADDED FOR OTLP, beyond the corpus attributes:
//
//   - Valid ids. OTLP needs a 16-byte trace id and an 8-byte span id. A session UUID loses its
//     dashes; any other session key (cg-derived-..., cg-unkeyed-...) is hashed to 16 bytes. The
//     original key goes in session.id, which is what Phoenix groups sessions by.
//   - The current semconv provider name. gen_ai.system is deprecated in favour of
//     gen_ai.provider.name; both are written for one release.
//   - The semconv message schema. gen_ai.input.messages / gen_ai.output.messages carry role
//     plus typed parts, with tool_call.arguments as a JSON value rather than a string;
//     OTLPMessages converts from the translated form.
//   - OpenInference attributes (openinference.span.kind, llm.*), so Phoenix shows model, token
//     counts and messages in its LLM view instead of as raw attributes.
//
// None of this changes what is recorded. Redaction has already run on every body by the time a
// span is built here, exactly as for the corpus path.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// OTLP and OpenInference attribute keys added on the exported spans.
const (
	AttrProviderName = "gen_ai.provider.name"
	AttrSessionID    = "session.id"

	// OpenInference (Arize Phoenix). See github.com/Arize-ai/openinference, semantic conventions.
	AttrOISpanKind       = "openinference.span.kind"
	AttrOIModelName      = "llm.model_name"
	AttrOIProvider       = "llm.provider"
	AttrOITokenPrompt    = "llm.token_count.prompt"
	AttrOITokenComplete  = "llm.token_count.completion"
	AttrOITokenTotal     = "llm.token_count.total"
	AttrOITokenCacheRead = "llm.token_count.prompt_details.cache_read"
	AttrOITokenCacheNew  = "llm.token_count.prompt_details.cache_write"
	AttrOIInputValue     = "input.value"
	AttrOIInputMime      = "input.mime_type"
	AttrOIOutputValue    = "output.value"
	AttrOIOutputMime     = "output.mime_type"

	OISpanKindLLM   = "LLM"
	OISpanKindChain = "CHAIN"

	// Resource attributes.
	ResService       = "service.name"
	ResServiceVer    = "service.version"
	ResClient        = "cg.client.name"
	ResTenant        = "cg.tenant.id"
	ServiceNameProxy = "context-guru-proxy"
)

// TraceIDFor maps a session key to a 16-byte OTLP trace id, as 32 hex characters.
//
// A UUID is used as is, without its dashes, so a session's trace id in Phoenix reads as its
// Claude Code session id. Any other key is hashed: it is stable, so every call of one session
// gets the same trace id, and two sessions never collide in practice.
func TraceIDFor(sessionKey string) string {
	if h := strings.ReplaceAll(sessionKey, "-", ""); len(h) == 32 && isHex(h) {
		return strings.ToLower(h)
	}
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:16])
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// OTLPSpan is one span ready for export: everything an OTLP exporter needs, with no SDK types,
// so this package stays free of I/O and unit-testable on values alone. The proxy turns it into
// an SDK span.
type OTLPSpan struct {
	TraceID      string // 32 hex
	SpanID       string // 16 hex
	ParentSpanID string // 16 hex, or ""
	Name         string
	Start, End   time.Time
	Attributes   map[string]any
	StatusError  string // "" = OK
}

// ToOTLP renders the turn for export. It starts from ToSpan, so the two renderings cannot drift
// on any attribute they share, and adds what OTLP and Phoenix need.
func (t Turn) ToOTLP() OTLPSpan {
	sp := t.ToSpan()
	at := sp.Attributes
	at[AttrSessionID] = t.SessionID
	if t.System != "" {
		at[AttrProviderName] = t.System
		at[AttrOIProvider] = t.System
	}
	if v, ok := at[AttrInputMsgs].(string); ok {
		at[AttrInputMsgs] = OTLPMessages(v)
		at[AttrOIInputValue] = v
		at[AttrOIInputMime] = "application/json"
	}
	if v, ok := at[AttrOutputMsgs].(string); ok {
		at[AttrOutputMsgs] = OTLPMessages(v)
		at[AttrOIOutputValue] = v
		at[AttrOIOutputMime] = "application/json"
	}
	at[AttrOISpanKind] = OISpanKindLLM
	at[AttrOIModelName] = t.Model
	at[AttrOITokenPrompt] = t.InputTokens()
	at[AttrOITokenComplete] = t.Output
	at[AttrOITokenTotal] = t.InputTokens() + t.Output
	at[AttrOITokenCacheRead] = t.CacheRead
	at[AttrOITokenCacheNew] = t.CacheWrite
	return OTLPSpan{
		TraceID:      TraceIDFor(t.SessionID),
		SpanID:       t.SpanID,
		ParentSpanID: t.ParentID,
		Name:         sp.Name,
		Start:        t.Start,
		End:          t.End,
		Attributes:   at,
		StatusError:  t.Err,
	}
}

// otlpPart and otlpMessage are Part and Message with a tool_call's arguments as a JSON value.
// Same fields in the same order, so a round trip gives back the corpus bytes exactly.
type otlpPart struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  string          `json:"response,omitempty"`
}

type otlpMessage struct {
	Role         string     `json:"role"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Content      string     `json:"content,omitempty"`
	Parts        []otlpPart `json:"parts,omitempty"`
}

// OTLPMessages converts the corpus message list (a JSON string) to the semconv message schema.
// The only difference is that a tool_call's arguments become a JSON value instead of a string.
// Input that does not parse is returned as is: a trace with a malformed message attribute is
// still a trace.
func OTLPMessages(corpus string) string {
	var msgs []Message
	if err := json.Unmarshal([]byte(corpus), &msgs); err != nil {
		return corpus
	}
	out := make([]otlpMessage, len(msgs))
	for i, m := range msgs {
		out[i] = otlpMessage{Role: m.Role, FinishReason: m.FinishReason, Content: m.Content}
		for _, p := range m.Parts {
			op := otlpPart{Type: p.Type, Content: p.Content, ID: p.ID, Name: p.Name, Response: p.Response}
			if p.Arguments != "" {
				if json.Valid([]byte(p.Arguments)) {
					op.Arguments = json.RawMessage(p.Arguments)
				} else {
					op.Arguments, _ = json.Marshal(p.Arguments) // keep it, as a JSON string
				}
			}
			out[i].Parts = append(out[i].Parts, op)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return corpus
	}
	return string(b)
}
