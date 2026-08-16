package reformat

import (
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

// toon is typed as a Reformat, and Reformat's contract is LOSSLESS repack: the type has no
// stash, no <<cg:HASH>> marker and no expand path, so anything it drops is gone for good.
// scalarCell used to collapse three distinct values onto the same cell text:
//
//	null      -> ""      indistinguishable from a real empty string
//	"1"       -> 1       string collapses onto the number
//	"true"    -> true    string collapses onto the boolean
//
// A model reading the table cannot tell which it was, and no marker lets anyone recover it.
// The fix keeps toon lossless by REFUSING to encode such an array at all — the never-worse
// path already leaves the output verbatim, so the only cost is a table we decline to shrink.
func TestToonRefusesToEncodeAmbiguousCells(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"null cell", `[{"note":null,"zip":"01234"},{"note":"x","zip":"02345"}]`},
		{"numeric string collapses onto a number", `[{"id":"1","n":2},{"id":"3","n":4}]`},
		{"bool string collapses onto a bool", `[{"f":"true","n":2},{"f":"false","n":4}]`},
		{"float-shaped string", `[{"v":"1.50","n":2},{"v":"2.0","n":4}]`},
	} {
		if out, ok := encodeTOON(tc.in); ok {
			t.Errorf("%s: encoded a lossy table instead of declining: %q", tc.name, out)
		}
	}

	// The case toon exists for must still work: all-distinct scalar types, no ambiguity.
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
