package reformat

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Tool-schema annotation strip: the one envelope lever that applies to EVERY
// request, including the very first.
//
// A coding agent sends its whole tool catalogue on every call — 31 tools / 168
// descriptions / 131 KB of JSON on real Claude Code traffic — and JSON Schema's
// annotation vocabulary is, by the spec's own words, inert to validation:
//
//	`title`, `description`, `default`, `deprecated`, `readOnly`, `writeOnly`,
//	`examples` are "Basic Meta-Data Annotations" (json-schema-validation §9):
//	"they do not affect validation". `$comment` (json-schema-core §8.3) "MUST NOT
//	be presented to end users" and has no validation effect. `example` and
//	`markdownDescription` are not JSON Schema keywords at all (OpenAPI / VS Code)
//	and are ignored by every validator.
//
// So removing them cannot change the set of valid inputs, which is the property
// Anthropic's tool-use contract needs: `input_schema` constrains `tool_use.input`,
// and under `strict: true` it drives constrained decoding. `description` and
// `default` are NOT dropped even though they are annotations too, because they are
// the two annotations the MODEL reads: they change what it knows about the tool,
// not what validates.
//
// # Where the cache stands
//
// `tools` is rendered FIRST, ahead of `system` and `messages`, so any byte changed
// here invalidates the entire cached prefix — exactly once, and then never again.
// With the provider's multipliers (cache read R = 0.1x, 5m write W = 1.25x, plain
// 1.0x), a prefix of P tokens and a saving of s tokens:
//
//   - COLD start (nothing cached): the prefix is being written either way, so the
//     transform costs nothing and saves W·s = 1.25s immediately, plus R·s = 0.1s on
//     every later request of the session. Net-positive at request 1.
//   - WARM (an entry for the untransformed prefix exists): the re-anchor costs
//     (W−R)·P − W·s = 1.15P − 1.25s once, and each later request recovers 0.1s.
//     Break-even at n = (1.15P − 1.25s)/(0.1s) ≈ 11.5·P/s requests.
//
// Measured on the capture corpus with the repo's own tokenizer: 31 tools/request,
// s = 473 tokens, P = 57k–85k ⇒ n = 1,371–2,062 requests (24 tools and s = 389 on the
// older SWE-bench capture, P = 30k ⇒ n = 880). Real sessions are tens of requests.
// The arithmetic is therefore unambiguous: this pays on a prefix nobody has cached
// yet, and loses badly on one that is warm.
//
// That is NOT an argument for gating on Ctx.ColdCache, and gating on it would be a
// bug: a prefix transform must be all-or-nothing over a session. Applied only on
// cold turns, request 1 sends the compacted tools and request 2 sends the original
// — so request 2 re-anchors, request 3 re-anchors back, and the component pays the
// 1.15P penalty every other turn forever. The only safe shapes are "always" and
// "never".
//
// Hence: deterministic, unconditional while enabled, and OFF in the default presets.
// The 1.15P re-anchor is then paid once per DEPLOYMENT (by whichever sessions happen
// to be in flight when an operator turns it on), not once per session — and from
// then on every session's first request is strictly cheaper. Flipping it mid-sweep
// is the only way to lose money with it, which is why it is an operator decision.
//
// Determinism is what makes that true, so it is enforced rather than assumed: the
// result is memoized by SHA-256 of the incoming tools array, and the rebuild goes
// through encoding/json, whose object encoder sorts keys — Go map ranging is
// randomized and a ranged rebuild would emit a different byte order every process.
// See TestCompactToolSchemasByteStable.
//
// Not implemented, deliberately: headroom's L2/L3 (truncating tool descriptions,
// dropping "self-explanatory" ones). Those change what the model is told a tool
// does; they are lossy, they default to disabled even in headroom, and 168
// descriptions are 110 KB of the 131 KB of tools — which makes them the tempting
// lever and the one whose failure mode is a wrong tool call. Also not implemented:
// `description` whitespace normalization. Measured on the same corpus, collapsing
// runs of whitespace saves 844 bytes (0.6% of the tools array) and does it by
// flattening the markdown lists and tables agent tool descriptions are written in —
// so it is not inert. The safe form (strip trailing spaces per line, collapse 3+
// blank lines) saves 19 bytes of 110,401. Not worth a line of code.

// toolSchemaDropKeys is the annotation vocabulary removed from every schema NODE.
// Sourced from headroom's tool_schema_compaction.py:44 and re-verified against
// json-schema-validation §9 / json-schema-core §8.3 (see the type doc).
var toolSchemaDropKeys = map[string]bool{
	"$comment": true, "$id": true, "$schema": true, "deprecated": true,
	"example": true, "examples": true, "markdownDescription": true,
	"readOnly": true, "title": true, "writeOnly": true,
}

// The three shapes a JSON Schema keyword can hold a SUBSCHEMA in. Recursion is
// whitelisted to these positions, which is what keeps the strip out of places where
// the same words are DATA rather than keywords:
//
//   - a property literally named `title`, `examples` or `readOnly` lives under
//     `properties`, whose keys are names and whose values are schemas — so keys are
//     never inspected and values always are. (headroom special-cases this at :230;
//     positional recursion gets it for free.)
//   - `const`, `default` and `enum` hold arbitrary instance data, which may itself be
//     an object with a `title` field. They are not in any list below, so nothing
//     inside them is ever touched.
var (
	subschemaMaps = map[string]bool{ // name -> schema
		"properties": true, "patternProperties": true, "$defs": true,
		"definitions": true, "dependentSchemas": true,
	}
	subschemaValues = map[string]bool{ // a single schema (or, for items, a list of them)
		"additionalItems": true, "additionalProperties": true, "contains": true,
		"contentSchema": true, "else": true, "if": true, "items": true, "not": true,
		"propertyNames": true, "then": true, "unevaluatedItems": true,
		"unevaluatedProperties": true,
	}
	subschemaLists = map[string]bool{ // a list of schemas
		"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true,
	}
)

// pruneSchema strips annotation keywords from a schema node in place and recurses
// into subschema positions only. keepIDs leaves `$id`/`$schema` alone: they are
// base-URI and dialect declarations, not annotations, and a schema containing a
// reference may resolve it against them.
func pruneSchema(node any, keepIDs bool) {
	m, ok := node.(map[string]any)
	if !ok {
		return // a boolean schema (`true`/`false`) or a non-object; nothing to strip
	}
	for k := range toolSchemaDropKeys {
		if keepIDs && (k == "$id" || k == "$schema") {
			continue
		}
		delete(m, k)
	}
	for k, v := range m {
		switch {
		case subschemaMaps[k]:
			if sub, ok := v.(map[string]any); ok {
				for _, s := range sub { // KEYS are property names, never keywords
					pruneSchema(s, keepIDs)
				}
			}
		case subschemaValues[k]:
			if list, ok := v.([]any); ok { // draft-04/07 tuple form of `items`
				for _, s := range list {
					pruneSchema(s, keepIDs)
				}
				continue
			}
			pruneSchema(v, keepIDs)
		case subschemaLists[k]:
			if list, ok := v.([]any); ok {
				for _, s := range list {
					pruneSchema(s, keepIDs)
				}
			}
		}
	}
}

// refMarkers are the keywords whose resolution depends on `$id`/`$schema`. Their
// presence anywhere in a tool's JSON makes those two too risky to drop, so that
// tool keeps them. A substring scan is enough: a false positive costs one missed
// annotation, a false negative would break reference resolution.
var refMarkers = [][]byte{[]byte(`"$ref"`), []byte(`"$dynamicRef"`), []byte(`"$anchor"`), []byte(`"$dynamicAnchor"`)}

// CompactToolSchemas strips JSON-Schema annotation keywords from every tool's
// `input_schema` in a raw request body. Returns the rewritten body and whether
// anything changed; any parse problem returns the input untouched (fail open).
//
// Deterministic and memoized by digest of the incoming `tools` array — 31 schemas
// per call on real traffic, and the array is byte-identical on every turn of a
// session, so the walk runs once per distinct catalogue rather than once per
// request.
func CompactToolSchemas(body []byte) ([]byte, bool) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || len(tools.Array()) == 0 {
		return body, false
	}
	raw := []byte(tools.Raw)
	out, ok := compactedTools(raw)
	// Verify-then-adopt, headroom's most reusable idea: take the rewrite only if it is
	// STRICTLY smaller. The rebuild goes through encoding/json, which sorts object keys,
	// so a tools array with nothing to strip still comes back with different bytes — and
	// adopting that would re-anchor the whole cached prefix for a saving of zero. Real
	// traffic hits this: an agent whose tools carry no annotations at all, and every
	// schema-less Anthropic server tool.
	if !ok || len(out) >= len(raw) {
		return body, false
	}
	next, err := sjson.SetRawBytes(body, "tools", out)
	if err != nil {
		return body, false
	}
	return next, true
}

var toolCache sync.Map // [32]byte digest -> []byte compacted tools array

func compactedTools(raw []byte) ([]byte, bool) {
	key := sha256.Sum256(raw)
	if v, hit := toolCache.Load(key); hit {
		b, ok := v.([]byte)
		return b, ok
	}
	out, ok := stripToolAnnotations(raw)
	if !ok {
		toolCache.Store(key, nil) // remember the failure too; it will not improve
		return nil, false
	}
	toolCache.Store(key, out)
	return out, true
}

// stripToolAnnotations is the uncached transform: decode, prune, re-encode. The
// re-encode is a full rewrite of the array rather than byte surgery, which is fine
// precisely BECAUSE this re-anchors the prefix once by design — and encoding/json
// sorts object keys, so the output is byte-stable across processes. json.Number
// keeps numeric literals exactly as they arrived (1.0 stays 1.0, a 20-digit integer
// stays itself) so a schema bound is never silently reformatted.
func stripToolAnnotations(raw []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var arr []any
	if err := dec.Decode(&arr); err != nil {
		return nil, false
	}
	for _, t := range arr {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		schemaNode, ok := tool["input_schema"]
		if !ok {
			// Anthropic server tools (web_search, bash_20250124, ...) are schema-less,
			// and OpenAI-shaped bodies nest it under function.parameters.
			if fn, isFn := tool["function"].(map[string]any); isFn {
				schemaNode, ok = fn["parameters"]
			}
			if !ok {
				continue
			}
		}
		keepIDs := false
		if b, err := json.Marshal(schemaNode); err == nil {
			for _, mk := range refMarkers {
				if bytes.Contains(b, mk) {
					keepIDs = true
					break
				}
			}
		}
		pruneSchema(schemaNode, keepIDs)
	}
	out, err := json.Marshal(arr)
	if err != nil {
		return nil, false
	}
	return out, true
}

// --------------------------------------------------------------------------- //

func init() { components.Register("toolschema", newToolschema) }

// Toolschema is a marker component: `tools` is a top-level body field the pipeline
// never sees (it operates on `messages`), so the rewrite lives in apply and is gated
// on this name being present — the same arrangement cachesplit uses for the
// system-array split. See CompactToolSchemas for the mechanism and the break-even.
type Toolschema struct{}

func newToolschema(raw []byte) (components.Component, error) {
	var none struct{}
	if err := components.Decode(raw, &none); err != nil {
		return nil, fmt.Errorf("toolschema: takes no configuration: %w", err)
	}
	return Toolschema{}, nil
}

func (Toolschema) Name() string { return "toolschema" }

func (Toolschema) Enabled(*components.Ctx) bool { return true }

// Reformat is a no-op: the rewrite already happened in apply, which reports it here
// through Ctx.ToolSchema so this component does not read "declined" on the requests
// where it just ran (the mistake cachesplit's doc records).
func (Toolschema) Reformat(_ *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) error {
	rep.Skipped = c == nil || !c.ToolSchema
	return nil
}

func init() { components.RegisterFields("toolschema", struct{}{}, nil) }
