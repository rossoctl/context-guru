package reformat

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("toon", newToon) }

// Toon re-encodes a JSON array of uniform, flat objects as TOON (Token-Oriented
// Object Notation): one header listing the field names once, then one
// comma-separated row per element. It drops the braces, repeated keys, and
// quotes that dominate a JSON array's token cost. It's a Reformat (repack in
// place, nothing stashed), so it must be LOSSLESS: every scalar value is preserved and
// stays distinguishable from every other. Arrays are encoded only when the element key
// sets match, the values are scalars, and no cell would be AMBIGUOUS once its quotes are
// dropped — a null, or a string that reads back as a number or a bool, makes the whole
// array non-encodable (see scalarCell). Anything nested, ragged or non-array is left
// untouched, and the pipeline's never-worse guard reverts any case that fails to shrink.
//
//	[{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]
//	=>
//	[2]{id,name}:
//	1,Alice
//	2,Bob
//
// The array is rarely at the top level of a tool result: it usually sits inside a
// tool-runner envelope's string field (see descendEnvelope and encodeTOONIn), which is
// why toon looked inert on real traffic.
type Toon struct{ minTokens int }

type toonConfig struct {
	MinTokens int `yaml:"min_tokens"`
}

func newToon(raw []byte) (components.Component, error) {
	cfg := toonConfig{MinTokens: 50}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	return &Toon{minTokens: cfg.MinTokens}, nil
}

func (Toon) Name() string                 { return "toon" }
func (Toon) Enabled(*components.Ctx) bool { return true }

func (t *Toon) Reformat(req *schemas.BifrostChatRequest, rep *components.Report, _ *components.Ctx) error {
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
		if schema.TextTokens(content) < t.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		toon, ok := encodeTOON(content)
		if !ok {
			// The array is usually not at the top level: of the measured large
			// low-reduction blobs, 673/673 carry their payload JSON-escaped inside a
			// string field of a tool-runner envelope, and 537 of those (2,098,762
			// tokens, 89% of the mass) hold a repeated-record array in there. That is
			// what `not_uniform_object_array` was really reporting at 72.8%.
			var why string
			if toon, why = descendEnvelope(content, encodeTOONIn); why != "" {
				if why == "envelope_not_object" {
					why = "not_uniform_object_array" // no table, and no envelope to open
				}
				rep.Gate(why)
				continue
			}
		}
		if schema.TextTokens(toon) >= schema.TextTokens(content) {
			rep.Gate("already_dense")
			continue
		}
		schema.SetMessageText(m, toon)
		acted = true
	}
	if !acted {
		rep.Skipped = true
	}
	return nil
}

// encodeTOONIn encodes the payload found inside an envelope string field. Two shapes
// occur in the measured traffic: the payload IS the record array, or the array is a
// field of a wrapper object — {"total":50,...,"tasks":[{...} x50]}, the `stdout` case.
// In the second shape the TOON text replaces the array as a JSON *string* value, so
// the payload still parses as JSON for the agent. Descent stops here: two levels
// counting the string field, no unbounded walk.
func encodeTOONIn(inner string) (string, bool) {
	if out, ok := encodeTOON(inner); ok {
		return out, true
	}
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" || trimmed[0] != '{' {
		return "", false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return "", false
	}
	acted := false
	for k, raw := range obj {
		if len(raw) == 0 || raw[0] != '[' {
			continue // only an array can be a TOON table; siblings stay byte-exact
		}
		out, ok := encodeTOON(string(raw))
		if !ok {
			continue
		}
		repl, err := marshalJSON(out)
		if err != nil || len(repl) >= len(raw) {
			continue
		}
		obj[k] = repl
		acted = true
	}
	if !acted {
		return "", false
	}
	out, err := marshalJSON(obj)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// encodeTOON renders a JSON array of uniform scalar-valued objects as TOON.
// ok=false (leave the content untouched) for anything else: non-array, empty,
// ragged key sets, or a nested/complex value.
func encodeTOON(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || trimmed[0] != '[' {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber() // keep numbers byte-exact rather than float64
	var arr []map[string]any
	if err := dec.Decode(&arr); err != nil || len(arr) == 0 {
		return "", false
	}

	keys := make([]string, 0, len(arr[0]))
	for k := range arr[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic column order; header preserves the mapping

	var b strings.Builder
	b.WriteString("[")
	b.WriteString(strconv.Itoa(len(arr)))
	b.WriteString("]{")
	b.WriteString(strings.Join(keys, ","))
	b.WriteString("}:\n")

	for _, row := range arr {
		if len(row) != len(keys) {
			return "", false // ragged: a row has extra/missing keys
		}
		cells := make([]string, len(keys))
		for j, k := range keys {
			v, ok := row[k]
			if !ok {
				return "", false
			}
			cell, ok := scalarCell(v)
			if !ok {
				return "", false // nested object/array — not a flat table
			}
			cells[j] = cell
		}
		b.WriteString(strings.Join(cells, ","))
		b.WriteByte('\n')
	}
	return b.String(), true
}

// scalarCell renders one JSON scalar as a TOON cell, quoting CSV-style when the
// value contains a delimiter. ok=false means "not encodable" — the caller then leaves the
// whole array untouched.
//
// It refuses three cases that would make the encoding LOSSY, because toon is a Reformat
// and Reformat's contract is a lossless repack: the type carries no stash, no
// <<cg:HASH>> marker and no expand path, so a value it flattens is unrecoverable.
//   - null: would render as an empty cell, indistinguishable from a real "".
//   - a string that reads back as a number ("1", "1.50", "01234"): collapses onto the
//     number cell, so the reader cannot tell 1 from "1".
//   - a string that reads back as a bool ("true"): same collapse.
//
// Refusing costs only a table we decline to compress; encoding it would silently change
// what the model is told, which is the one thing a lossless component may not do.
func scalarCell(v any) (string, bool) {
	var s string
	switch x := v.(type) {
	case nil:
		return "", false // ambiguous with a real empty string, and unrecoverable
	case bool:
		s = strconv.FormatBool(x)
	case json.Number:
		s = x.String()
	case string:
		if ambiguousScalarString(x) {
			return "", false
		}
		s = x
	default:
		return "", false
	}
	if strings.ContainsAny(s, ",\"\n\r") || s != strings.TrimSpace(s) {
		s = `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s, true
}

// ambiguousScalarString reports whether a string cell would be indistinguishable from a
// number or boolean cell once the quotes are dropped. Uses the same parsers the JSON
// decoder would, so the test is "does this round-trip as a different type", not a regex
// guess. Bare "" is fine: it is only ambiguous with null, which scalarCell already refuses.
func ambiguousScalarString(s string) bool {
	if s == "" {
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseBool(s); err == nil {
		return true
	}
	return false
}

func init() {
	components.RegisterFields("toon", toonConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 50, Min: 1,
			Hint: "Only convert a uniform JSON array to TOON above this many tokens; below it the saving is noise."},
	})
}
