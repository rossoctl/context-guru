package deferred

import (
	"strings"
	"testing"
)

func TestRegionsEndRules(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"blank line":      {`"` + Header + `\nmcp__a__b\n\nmore"`, Header + `\nmcp__a__b`},
		"literal tag":     {`"` + Header + `\nmcp__a__b\n</system-reminder>"`, Header + `\nmcp__a__b`},
		"escaped tag":     {`"` + Header + `\nmcp__a__b\n\u003c/system-reminder\u003e"`, Header + `\nmcp__a__b`},
		"string end":      {`"` + Header + `\nmcp__a__b" , "x"`, Header + `\nmcp__a__b`},
		"escaped quote":   {`"` + Header + ` \"q\"\nmcp__a__b"`, Header + ` \"q\"\nmcp__a__b`},
		"crlf blank line": {`"` + Header + `\r\nmcp__a__b\r\n\r\nx"`, Header + `\r\nmcp__a__b`},
	} {
		got := Regions([]byte(tc.body), 1<<20)
		if len(got) != 1 || string(got[0]) != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
	if Regions([]byte(`"nothing"`), 100) != nil {
		t.Error("a body without a header returned a region")
	}
	if r := Regions([]byte(`"`+Header+`\nmcp__a__b"`), 10); len(r) != 1 || len(r[0]) != 10 {
		t.Errorf("budget not applied: %q", r)
	}
}

func TestListsVoidsAQuotedOrBadList(t *testing.T) {
	good := Header + "\nmcp__a__b\nmcp__a__c\n\nafter"
	l, v := Lists(good)
	if v != 0 || len(l) != 1 || strings.Join(l[0].Names, ",") != "mcp__a__b,mcp__a__c" {
		t.Fatalf("good list: %+v voided=%d", l, v)
	}
	if got := good[l[0].Start:l[0].End]; got != Header+"\nmcp__a__b\nmcp__a__c" {
		t.Errorf("range = %q", got)
	}
	l, v = Lists(Header + "\nmcp__a__b\nnot a name!\n")
	if len(l) != 0 || v != 1 {
		t.Errorf("bad line: %+v voided=%d", l, v)
	}
	l, v = Lists("I said: " + Header + "\nmcp__a__b\n")
	if len(l) != 0 || v != 0 {
		t.Errorf("quoted mid-line: %+v voided=%d", l, v)
	}
	l, _ = Lists(Header + "\r\nmcp__a__b\r\n</system-reminder>")
	if len(l) != 1 {
		t.Errorf("CRLF list not parsed: %+v", l)
	}
}

func TestTextsSkipsToolResults(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"` + Header + `"}]},` +
		`{"role":"assistant","content":"` + Header + `"},` +
		`{"role":"user","content":[{"type":"text","text":"` + Header + `\nmcp__a__b"}]}]}`
	if got := Texts([]byte(body)); len(got) != 1 {
		t.Errorf("Texts = %q, want only the user text block", got)
	}
}

func TestTextsResponses(t *testing.T) {
	l := Header + `\nmcp__a__b\n`
	body := `{"instructions":"` + l + `","input":[
	  {"role":"user","content":"` + l + `"},
	  {"role":"developer","content":[{"type":"input_text","text":"` + l + `"}]},
	  {"role":"assistant","content":"` + l + `"},
	  {"type":"function_call_output","output":"` + l + `"}]}`
	if got := len(TextsResponses([]byte(body))); got != 3 {
		t.Errorf("texts = %d, want 3 (instructions, user, developer; not assistant or a tool result)", got)
	}
}
