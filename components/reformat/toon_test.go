package reformat

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestEncodeTOON_UniformArrayShrinks(t *testing.T) {
	in := `[{"id":1,"name":"Alice","role":"admin"},{"id":2,"name":"Bob","role":"user"}]`
	out, ok := encodeTOON(in)
	if !ok {
		t.Fatal("expected uniform scalar array to encode")
	}
	// Header lists count + sorted keys once; each row is comma-separated.
	if !strings.HasPrefix(out, "[2]{id,name,role}:\n") {
		t.Fatalf("unexpected header, got:\n%s", out)
	}
	for _, want := range []string{"1,Alice,admin", "2,Bob,user"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing row %q in:\n%s", want, out)
		}
	}
	if len(out) >= len(in) {
		t.Fatalf("TOON (%d) not smaller than JSON (%d)", len(out), len(in))
	}
}

func TestEncodeTOON_QuotesDelimiters(t *testing.T) {
	out, ok := encodeTOON(`[{"a":"x,y"},{"a":"he said \"hi\""}]`)
	if !ok {
		t.Fatal("expected encode")
	}
	if !strings.Contains(out, `"x,y"`) || !strings.Contains(out, `"he said ""hi"""`) {
		t.Fatalf("bad CSV quoting:\n%s", out)
	}
}

func TestEncodeTOON_SkipsNonTable(t *testing.T) {
	cases := map[string]string{
		"object":       `{"id":1}`,
		"empty array":  `[]`,
		"nested value": `[{"a":{"b":1}}]`,
		"ragged keys":  `[{"a":1},{"a":1,"b":2}]`,
		"not json":     `just some log output`,
	}
	for name, in := range cases {
		if _, ok := encodeTOON(in); ok {
			t.Errorf("%s: expected ok=false (leave untouched)", name)
		}
	}
}

// toon is typed as a Reformat, and Reformat's contract is LOSSLESS repack: the type has
// no stash, no <<cg:HASH>> marker and no expand path, so anything it drops is gone for
// good. scalarCell once collapsed three distinct values onto the same cell text —
// null/"" , "1"/1, "true"/true — and the first fix for that was to REFUSE the whole
// array. Refusing was expensive: 89% of the measured envelope mass is record arrays.
// The cells are now QUOTED instead, which removes the ambiguity outright, and the pairs
// below are the exact ones the refusal existed to protect: each must encode, and each
// must decode back to its own value.
func TestToonKeepsAmbiguousCellsDistinct(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"null vs empty string", `[{"v":null},{"v":null}]`, `[{"v":""},{"v":""}]`},
		{"numeric string vs number", `[{"v":1},{"v":2}]`, `[{"v":"1"},{"v":"2"}]`},
		{"bool string vs bool", `[{"v":true},{"v":false}]`, `[{"v":"true"},{"v":"false"}]`},
		{"float-shaped string", `[{"v":1.50},{"v":2.0}]`, `[{"v":"1.50"},{"v":"2.0"}]`},
	} {
		outA, okA := encodeTOON(tc.a)
		outB, okB := encodeTOON(tc.b)
		if !okA || !okB {
			t.Errorf("%s: declined (%v/%v) — quoting should make both encodable", tc.name, okA, okB)
			continue
		}
		if outA == outB {
			t.Errorf("%s: both values produced the same table %q", tc.name, outA)
		}
		assertRoundTrip(t, tc.name+"/a", tc.a, outA)
		assertRoundTrip(t, tc.name+"/b", tc.b, outB)
	}

	// The case toon exists for must still work: all-distinct scalar types, no quoting.
	out, ok := encodeTOON(`[{"id":1,"name":"Alice","ok":true},{"id":2,"name":"Bob","ok":false}]`)
	if !ok {
		t.Fatal("declined the unambiguous uniform table toon exists to compress")
	}
	for _, want := range []string{"[2]{id,name,ok}:", "1,Alice,true", "2,Bob,false"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// assertRoundTrip is the lossless proof used everywhere toon is called lossless: decode
// the emitted table and require the result to be deep-equal to the JSON it came from,
// numbers included (json.Number keeps them byte-exact, so 1.50 stays 1.50).
func assertRoundTrip(t *testing.T, name, inputJSON, toon string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(inputJSON))
	dec.UseNumber()
	var want []map[string]any
	if err := dec.Decode(&want); err != nil {
		t.Fatalf("%s: test input is not a JSON object array: %v", name, err)
	}
	got, ok := decodeTOON(toon)
	if !ok {
		t.Fatalf("%s: emitted table does not parse back:\n%s", name, toon)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: round-trip changed the value\ninput %s\ntoon  %q\nback  %#v", name, inputJSON, toon, got)
	}
}

// nastyCells are the values that make a table hard to encode without losing something:
// the type-collapse pairs, delimiters, embedded quotes, padding, big integers a float
// round-trip would rewrite, and cells quoting cannot rescue (newline/CR), which must be
// DECLINED rather than corrupted.
var nastyCells = []any{
	nil, "", "1", "1.50", "01234", "true", "False", "t", "-", "a,b", `he said "hi"`,
	" pad ", "plain", "line\nbreak", "crlf\r", json.Number("0"), json.Number("1.50"),
	json.Number("-3e10"), json.Number("123456789012345678901234567890"), true, false, "日本", "null",
}

// TestToonRoundTripsRandomTables is the property that makes "lossless" a checked claim
// rather than an argued one: over random tables built from nastyCells, every table toon
// agrees to encode must decode back to exactly the value it came from. Whatever it
// declines is fine — declining costs a compression, not a fact.
func TestToonRoundTripsRandomTables(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	keysets := [][]string{{"a"}, {"id", "name"}, {"a,b", `c"d`, "", "z"}, {"x", "y", "z", "w"}}
	encoded, declined := 0, 0
	for iter := 0; iter < 20000; iter++ {
		ks := keysets[rng.Intn(len(keysets))]
		arr := make([]map[string]any, 1+rng.Intn(4))
		for i := range arr {
			row := map[string]any{}
			for _, k := range ks {
				row[k] = nastyCells[rng.Intn(len(nastyCells))]
			}
			arr[i] = row
		}
		raw, err := json.Marshal(arr)
		if err != nil {
			t.Fatal(err)
		}
		out, ok := encodeTOON(string(raw))
		if !ok {
			declined++
			continue
		}
		encoded++
		assertRoundTrip(t, "fuzz", string(raw), out)
		if t.Failed() {
			t.FailNow()
		}
	}
	if encoded == 0 {
		t.Fatal("encoded nothing — the property proved nothing")
	}
	t.Logf("encoded=%d declined=%d", encoded, declined)
}

// TestToonIsDeterministic guards the cache: a Reformat whose output varies between
// calls or process restarts re-anchors the prompt cache on every request, which is a
// pure loss. Column order is sorted, so the same input must give byte-identical output.
func TestToonIsDeterministic(t *testing.T) {
	in := `[{"b":1,"a":"x","c":null},{"b":2,"a":"y","c":"1"}]`
	first, ok := encodeTOON(in)
	if !ok {
		t.Fatal("declined")
	}
	for i := 0; i < 200; i++ {
		if got, ok := encodeTOON(in); !ok || got != first {
			t.Fatalf("non-deterministic: %q vs %q", got, first)
		}
	}
}
