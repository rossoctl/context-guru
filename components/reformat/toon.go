package reformat

import (
	"encoding/json"
	"reflect"
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
// stays distinguishable from every other. Ambiguity is resolved by QUOTING, not by
// refusing: a string that would read back as a number or a bool is quoted ("1"), a bare
// empty cell is null and `""` is the empty string (see scalarCell for the full cell
// grammar). Arrays are encoded when the element key sets match and every value is a
// scalar; anything nested or non-array is left untouched. Every candidate table is
// decoded again before it is adopted (decodeTOON) and dropped unless it reproduces the
// input exactly, and the pipeline's never-worse guard reverts any case that fails to shrink.
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
	if dec.More() {
		// A json.Decoder reads one value and ignores the rest, so an array followed by
		// anything else would be re-encoded as a table with the remainder DELETED. The
		// round-trip check below cannot catch that (it compares against the value that
		// was parsed), so it has to be refused here.
		return "", false
	}

	keys := make([]string, 0, len(arr[0]))
	for k := range arr[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic column order; header preserves the mapping

	hdr := make([]string, len(keys))
	for j, k := range keys {
		hdr[j] = k // a key carrying a delimiter is quoted like a cell, and unquoted back
		if k == "" || needsQuote(k) {
			hdr[j] = quoteCell(k)
		}
	}
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(strconv.Itoa(len(arr)))
	b.WriteString("]{")
	b.WriteString(strings.Join(hdr, ","))
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
	out := b.String()
	// Verify-then-adopt: decode the candidate and adopt it ONLY IF it reproduces the
	// input exactly. Losslessness is then a property this function checked, not one a
	// comment claims — and a cell shape the encoder mishandles (a literal newline, an
	// unanticipated key) costs a declined table instead of silently corrupting one.
	if back, ok := decodeTOON(out); !ok || !reflect.DeepEqual(back, arr) {
		return "", false
	}
	return out, true
}

// scalarCell renders one JSON scalar as a TOON cell. Every scalar is encodable: the
// ambiguity that would make the encoding lossy is removed by QUOTING, which is the
// same CSV-style mechanism the delimiter case already used, rather than by refusing
// the whole table.
//
// The cell grammar, and what makes each value distinguishable from every other:
//
//   - a bare empty cell is null;
//   - `""` (quoted, empty) is the empty string;
//   - a bare `true`/`false` is a bool, a bare number is a number;
//   - a string that would read back as a number or a bool ("1", "1.50", "true") is
//     QUOTED, so it reads back as a string;
//   - anything containing a delimiter, or with leading/trailing space, is quoted too.
//
// ok=false only for a value that is not a scalar at all (a nested object or array),
// which means the array is not a flat table. decodeTOON is the inverse, and
// encodeTOON refuses to adopt any table that does not survive it (verify-then-adopt),
// so a shape this comment has not anticipated — a cell holding a literal newline, say,
// which quoting cannot rescue because rows are newline-separated — costs a declined
// table, never a corrupted one.
func scalarCell(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", true // bare empty cell: null
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return x.String(), true
	case string:
		if x == "" || ambiguousScalarString(x) || needsQuote(x) {
			return quoteCell(x), true
		}
		return x, true
	default:
		return "", false // nested object/array — not a flat table
	}
}

// needsQuote reports whether a cell's text would not survive the row/cell split
// unquoted: it carries a delimiter, or space the split would not preserve.
func needsQuote(s string) bool {
	return strings.ContainsAny(s, ",\"\n\r") || s != strings.TrimSpace(s)
}

// quoteCell applies CSV-style quoting: wrap in quotes, double any interior quote.
func quoteCell(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// ambiguousScalarString reports whether a string cell would be indistinguishable from a
// number or boolean cell if it were emitted bare. Uses the same parsers the JSON decoder
// would, so the test is "does this read back as a different type", not a regex guess.
// Such a cell is quoted (see scalarCell); over-quoting costs two characters, and
// decodeTOON reads a quoted cell back as a string unconditionally.
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

// decodeTOON is encodeTOON's inverse: it parses a TOON table back into the value it
// was built from, or reports ok=false. It exists so losslessness is PROVEN rather
// than argued — encodeTOON runs it on its own output and adopts the table only if
// the result is deep-equal to the input (headroom's verify-then-adopt discipline).
// It is also what the round-trip tests decode with.
func decodeTOON(s string) ([]map[string]any, bool) {
	nl := strings.IndexByte(s, '\n')
	if nl < 0 || len(s) == 0 || s[0] != '[' {
		return nil, false
	}
	hdr, body := s[:nl], s[nl+1:]
	brace := strings.Index(hdr, "]{")
	if brace < 0 || !strings.HasSuffix(hdr, "}:") {
		return nil, false
	}
	n, err := strconv.Atoi(hdr[1:brace])
	if err != nil || n <= 0 {
		return nil, false
	}
	keys, _, ok := splitCells(hdr[brace+2 : len(hdr)-2])
	if !ok {
		return nil, false
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != n {
		return nil, false
	}
	out := make([]map[string]any, 0, n)
	for _, ln := range lines {
		cells, quoted, ok := splitCells(ln)
		if !ok || len(cells) != len(keys) {
			return nil, false
		}
		row := make(map[string]any, len(keys))
		for j, k := range keys {
			row[k] = decodeCell(cells[j], quoted[j])
		}
		if len(row) != len(keys) {
			return nil, false // duplicate key in the header
		}
		out = append(out, row)
	}
	return out, true
}

// splitCells splits one TOON row (or the header's key list) into cells, honouring the
// CSV-style quoting scalarCell applies. It returns each cell's text with its quotes
// removed plus whether it ARRIVED quoted — that flag is what keeps "1" distinguishable
// from 1 on the way back.
func splitCells(row string) (cells []string, quoted []bool, ok bool) {
	for {
		if strings.HasPrefix(row, `"`) {
			var b strings.Builder
			i := 1
			for {
				j := strings.IndexByte(row[i:], '"')
				if j < 0 {
					return nil, nil, false // unterminated quote
				}
				b.WriteString(row[i : i+j])
				i += j + 1
				if i < len(row) && row[i] == '"' { // doubled quote: a literal one
					b.WriteByte('"')
					i++
					continue
				}
				break
			}
			cells, quoted = append(cells, b.String()), append(quoted, true)
			row = row[i:]
			if row == "" {
				return cells, quoted, true
			}
			if row[0] != ',' {
				return nil, nil, false // trailing junk after a closing quote
			}
			row = row[1:]
			continue
		}
		k := strings.IndexByte(row, ',')
		if k < 0 {
			return append(cells, row), append(quoted, false), true
		}
		cells, quoted = append(cells, row[:k]), append(quoted, false)
		row = row[k+1:]
	}
}

// decodeCell maps one cell back to its JSON scalar, mirroring scalarCell exactly: a
// quoted cell is always a string, a bare empty cell is null, and a bare cell is typed
// by the same parsers that decided it did not need quoting.
func decodeCell(cell string, quoted bool) any {
	switch {
	case quoted:
		return cell
	case cell == "":
		return nil
	case cell == "true":
		return true
	case cell == "false":
		return false
	}
	if _, err := strconv.ParseFloat(cell, 64); err == nil {
		return json.Number(cell)
	}
	return cell
}

func init() {
	components.RegisterFields("toon", toonConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 50, Min: 1,
			Hint: "Only convert a uniform JSON array to TOON above this many tokens; below it the saving is noise."},
	})
}
