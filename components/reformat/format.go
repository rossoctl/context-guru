package reformat

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("format", newFormat) }

// Format re-encodes JSON tool outputs denser without losing data (a Reformat):
// pretty-printed JSON is re-marshaled compact. It's strictly lossless — same
// value, fewer whitespace tokens — so no stash is needed. (For a denser tabular
// re-encoding of uniform object arrays, see the `toon` component.)
//
// It also descends one level into a tool-runner envelope (see descendEnvelope):
// the payload usually lives JSON-escaped inside a string field, where re-encoding
// only the envelope wins nothing.
type Format struct{ minTokens int }

type formatConfig struct {
	MinTokens int `yaml:"min_tokens"`
}

func newFormat(raw []byte) (components.Component, error) {
	cfg := formatConfig{MinTokens: 50}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &Format{minTokens: cfg.MinTokens}, nil
}

func (Format) Name() string                 { return "format" }
func (Format) Enabled(*components.Ctx) bool { return true }

func (f *Format) Reformat(req *schemas.BifrostChatRequest, rep *components.Report, _ *components.Ctx) error {
	acted := false
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue
		}
		if !schema.Rewritable(*m) {
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
		}
		content := schema.MessageText(*m)
		trimmed := strings.TrimSpace(content)
		if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
			rep.Gate("not_json_shaped")
			continue
		}
		if schema.TextTokens(content) < f.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		compact, ok := compactJSON(trimmed)
		if !ok {
			rep.Gate("json_parse_failed") // not valid JSON — leave untouched
			continue
		}
		// Compacting the OUTER document is usually worth ~nothing: of the measured
		// large low-reduction blobs, 673/673 carry their real payload JSON-escaped
		// inside a string field, so an envelope-only re-encode saved 9 tokens of
		// 6,459. Descend into that field and compact the payload too.
		best, why := compact, ""
		if out, w := descendEnvelope(compact, compactJSON); w == "" {
			best = out
		} else {
			why = w
		}
		if schema.TextTokens(best) >= schema.TextTokens(content) {
			if why != "" {
				rep.Gate(why) // nothing to descend into, or the payload didn't shrink
			} else {
				rep.Gate("already_compact")
			}
			continue
		}
		schema.SetMessageText(m, best)
		acted = true
	}
	if !acted {
		rep.Skipped = true
	}
	return nil
}

// compactJSON re-encodes a JSON document as compact JSON. UseNumber keeps numbers
// byte-exact: a float64 round-trip rewrites big integers in exponent form, which a
// lossless Reformat may not do. ok=false means "not usable".
//
// The dec.More() check is the losslessness guard, and it is not theoretical. A
// json.Decoder reads ONE value and ignores whatever follows, so a tool output that is a
// JSON document followed by anything else — `[1, 2]\nwarnings: 0 []`, a jq document plus
// a stderr line, an NDJSON stream — used to be "compacted" to just its first document,
// silently DELETING the rest. Two such outputs exist in the captures measured here; only
// the min_tokens floor kept the deletion from firing. A Reformat may not drop content, so
// trailing content means decline.
func compactJSON(s string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	if dec.More() {
		return "", false // more than one document: compacting would keep only the first
	}
	b, err := marshalJSON(v)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// minEmbeddedJSON is the byte floor for trying to parse a string field as JSON.
// Below it there is no reduction worth having, and it keeps the prefix check from
// turning into a parse attempt on every short string of every object.
const minEmbeddedJSON = 64

// descendEnvelope applies tr to JSON payloads carried inside a *string* field of a
// JSON object — the Claude Code tool-runner envelope:
//
//	{"ok":true,"exit_code":0,"stdout":"{\n  \"total\": 50, ... \"tasks\": [ ... ]}"}
//
// where the compressible value is one level down, JSON-escaped in a string. `stdout`
// is the measured case (673/673 of the large low-reduction blobs), but the field name
// is NOT special-cased: any string field that cheaply looks like JSON (leading `{`/`[`
// after trimming, ≥ minEmbeddedJSON bytes) is tried. Descent stops here — one level,
// no recursive walker — and a field is only replaced if tr's output is smaller.
//
// Fidelity: the object is decoded into json.RawMessage, so every field tr does not
// touch is re-emitted byte-exact (HTML escaping is off for the same reason); only key
// order is normalised. The replacement is re-escaped by encoding/json, so the result
// still parses for the agent.
//
// The second return is "" on success, else the gate reason for declining.
func descendEnvelope(content string, tr func(string) (string, bool)) (string, string) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || trimmed[0] != '{' {
		return "", "envelope_not_object"
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return "", "envelope_not_object"
	}
	found, acted := false, false
	for k, raw := range obj {
		inner, ok := embeddedJSON(raw)
		if !ok {
			continue
		}
		found = true
		out, ok := tr(inner)
		if !ok {
			continue
		}
		repl, err := marshalJSON(out)
		if err != nil || len(repl) >= len(raw) {
			continue // no win here: leave the field exactly as it arrived
		}
		obj[k] = repl
		acted = true
	}
	if !acted {
		if found {
			return "", "envelope_inner_not_smaller"
		}
		return "", "envelope_no_embedded_json"
	}
	out, err := marshalJSON(obj)
	if err != nil {
		return "", "envelope_marshal_failed"
	}
	return string(out), ""
}

// embeddedJSON reports whether a raw field value is a JSON string whose contents are
// themselves JSON, and returns those contents. The checks are ordered cheapest first
// (length, leading quote) so no unquoting happens for the common short field.
func embeddedJSON(raw json.RawMessage) (string, bool) {
	if len(raw) < minEmbeddedJSON || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return "", false
	}
	return s, true
}

// marshalJSON is json.Marshal without HTML escaping, so a payload containing <, > or &
// round-trips byte-exact instead of growing backslash-u escapes.
func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func init() {
	components.RegisterFields("format", formatConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 50, Min: 1,
			Hint: "Only re-encode content estimated above this many tokens. Below it the repack cannot pay for the work."},
	})
}
