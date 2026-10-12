package extract

import "testing"

func TestParseRideDecisions(t *testing.T) {
	d, ok := ParseRideDecisions("```json\n[{\"label\":0,\"action\":\"Keep\"},{\"label\":1,\"action\":\"extract\",\"extract\":\"a b [c]\"}]\n```")
	if !ok || len(d) != 2 || d[0].Action != "keep" || d[1].Extract != "a b [c]" {
		t.Fatalf("got %v %v", d, ok)
	}
	if d, ok := ParseRideDecisions("[]"); !ok || len(d) != 0 {
		t.Fatal("empty array must parse as keep-everything")
	}
	if _, ok := ParseRideDecisions("I will continue the task."); ok {
		t.Fatal("prose must not parse")
	}
	if _, ok := ParseRideDecisions(`[{"label":0,"action":"park"`); ok {
		t.Fatal("truncated reply must not parse")
	}
}

func TestGroundedExtract(t *testing.T) {
	orig := "alpha line one\nbeta line two\ngamma line three"
	if !GroundedExtract(orig, "SUMMARY: dropped noise\nbeta line two\ngamma line three", 0.8) {
		t.Fatal("verbatim lines must ground")
	}
	if GroundedExtract(orig, "beta line two\ninvented line here\nanother made up one", 0.8) {
		t.Fatal("invented lines must not ground")
	}
	if GroundedExtract(orig, "SUMMARY: only", 0.8) {
		t.Fatal("no substantive lines must not ground")
	}
}
