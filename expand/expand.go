// Package expand holds the host-agnostic half of reversibility (design D6,
// after headroom's CCR): the marker format Offload components write, the
// expand(id) tool definition injected per provider, and resolution of a stashed
// original from the Store.
//
// The continuation LOOP that actually answers an expand tool call is host glue
// (the bifrost proxy wraps its chat route; the AuthBridge plugin does it in
// OnResponse) — but every host reuses ParseMarkers, ToolDef, and Resolve from
// here so the wire contract stays identical across integrations.
package expand

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// ToolName is the model-callable tool that retrieves offloaded content.
const ToolName = "context_guru_expand"

// SummaryMarker is the non-resolvable sentinel an Offload component writes under
// marker_mode "summary": it signals "content was compacted here" — to the model
// and to cross-turn skip-detection — but carries no store key, so there is
// nothing to expand (restoration is off). The human-readable description of what
// was dropped stays inline in the message (the component's own note).
const SummaryMarker = "⟪cg⟫"

// Marker is the sentinel an Offload component writes in place of dropped
// content. HASH is the store key. Sticky-on per session (headroom's golden
// tool-bytes rule) so injecting the tool never busts the provider prefix cache.
//
//	<<cg:HASH>>
var markerRe = regexp.MustCompile(`<<cg:([A-Za-z0-9_-]{1,64})>>`)

// Marker renders the sentinel for a given store key.
func Marker(key string) string { return "<<cg:" + key + ">>" }

// HasPlaceholder reports whether s already carries a context-guru placeholder —
// a resolvable <<cg:HASH>> marker (full mode) OR a SummaryMarker sentinel
// (summary mode). Components use it to skip content an earlier component/turn
// already reduced, so a later turn re-sending the same message reduces to the
// same bytes (provider prefix-cache stays warm).
func HasPlaceholder(s string) bool {
	return strings.Contains(s, SummaryMarker) || markerRe.MatchString(s)
}

// rawMarkerRe matches a full <<cg:HASH>> marker in RAW, un-decoded request bytes,
// where the angle brackets may arrive HTML-escaped as < / >.
//
// BOTH spellings are load-bearing, and the escaped one is the COMMON case, not an
// exotic client quirk: Go's encoding/json HTML-escapes "<" by default (unless a caller
// opts out via Encoder.SetEscapeHTML(false)), and sjson escapes it whenever the value
// being set contains a newline — and every Offload marker is appended after a newline.
// So a marker the MODEL reads as <<cg:HASH>> usually exists in the bytes on the wire
// only as <<cg:HASH>>. Any check matching markers against a raw body must
// accept both forms deliberately.
//
// The escape alternatives are case-insensitive: \u003C is as valid as \u003c, and a miss
// here is a FALSE NEGATIVE — a real expand call streamed past uninspected, which is
// worse than the over-buffering this regexp exists to prevent.
var rawMarkerRe = regexp.MustCompile(`(?:<|(?i:\\u003c)){2}cg:([A-Za-z0-9_-]{1,64})(?:>|(?i:\\u003e)){2}`)

// HasMarkersInMessages reports whether a request body carries a context-guru
// placeholder in content the MODEL can see and reference — the messages array and
// the system prompt.
//
// It deliberately does NOT scan the whole body. The `tools` array holds our own
// injected expand tool, whose description quotes the marker syntax ("…replaced by a
// <<cg:HASH>> marker"), HTML-escaped by ToolDefRaw's encoding/json. A whole-body check
// therefore matched the tool we had just injected and was a tautology: every request
// looked marker-bearing, so every streaming response was fully buffered and the
// documented zero-added-latency fast path never engaged. Requiring the full marker
// shape is not sufficient on its own — the tool description contains the full shape
// too. Scoping to model-visible content is what fixes it.
func HasMarkersInMessages(body []byte) bool {
	for _, field := range [...]string{"messages", "system"} {
		if r := gjson.GetBytes(body, field); r.Exists() &&
			(rawMarkerRe.MatchString(r.Raw) || strings.Contains(r.Raw, SummaryMarker)) {
			return true
		}
	}
	return false
}

// ParseMarkers returns the distinct store keys referenced by any markers in s,
// in first-seen order.
func ParseMarkers(s string) []string {
	m := markerRe.FindAllStringSubmatch(s, -1)
	seen := map[string]struct{}{}
	var out []string
	for _, g := range m {
		k := g[1]
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// Resolve returns the original stashed under key, or ("",false) if it's gone
// (expired/evicted). Callers must handle the miss gracefully — an expired
// original silently turns a lossless offload lossy (headroom's known TTL edge).
func Resolve(s store.Store, key string) (string, bool) {
	b, ok := s.Get(key)
	if !ok {
		return "", false
	}
	return string(b), true
}

// toolDesc / the JSON schema for the expand tool's one argument. Kept as typed
// structs (not map[string]any) so the serialized bytes have a FIXED key order —
// injecting the same tool on every turn produces byte-identical `tools` entries,
// which is what keeps the provider prefix cache warm across turns (Inject relies on this).
const toolDesc = "Retrieve the full original content that was compressed and replaced by a <<cg:HASH>> marker."

type idProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}
type toolSchema struct {
	Type       string            `json:"type"`
	Properties map[string]idProp `json:"properties"`
	Required   []string          `json:"required"`
}

func schemaLiteral() toolSchema {
	return toolSchema{
		Type: "object",
		Properties: map[string]idProp{
			"id": {Type: "string", Description: "The HASH from a <<cg:HASH>> marker to retrieve in full."},
		},
		Required: []string{"id"},
	}
}

// openAIToolDef / anthropicToolDef are the ordered wire shapes.
type openAIFn struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Parameters  toolSchema `json:"parameters"`
}
type openAIToolDef struct {
	Type     string   `json:"type"`
	Function openAIFn `json:"function"`
}
type anthropicToolDef struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputSchema toolSchema `json:"input_schema"`
}

// ToolDefRaw returns the expand tool definition for a provider as deterministic
// JSON bytes (stable key order). Inject appends these to the request's tools array.
func ToolDefRaw(provider string) json.RawMessage {
	var v any
	if provider == "anthropic" {
		v = anthropicToolDef{Name: ToolName, Description: toolDesc, InputSchema: schemaLiteral()}
	} else {
		v = openAIToolDef{Type: "function", Function: openAIFn{Name: ToolName, Description: toolDesc, Parameters: schemaLiteral()}}
	}
	b, _ := json.Marshal(v)
	return b
}
