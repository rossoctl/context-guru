package schema

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestContentTextAndToolResultText(t *testing.T) {
	for _, tc := range []struct{ name, json, want string }{
		{"string", `{"content":"a \"b\""}`, `a "b"`},
		{"one text block", `{"content":[{"type":"text","text":"hi"}]}`, "hi"},
		{"two blocks, no separator", `{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, "ab"},
		{"image skipped", `{"content":[{"type":"image","source":{}},{"type":"text","text":"x"}]}`, "x"},
		{"non-string text skipped", `{"content":[{"type":"text","text":5}]}`, ""},
		{"empty array", `{"content":[]}`, ""},
		{"null", `{"content":null}`, ""},
		{"missing", `{}`, ""},
		{"object", `{"content":{"text":"x"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolResultText(gjson.Parse(tc.json)); got != tc.want {
				t.Fatalf("ToolResultText = %q, want %q", got, tc.want)
			}
			if got := ContentText(gjson.Get(tc.json, "content")); got != tc.want {
				t.Fatalf("ContentText = %q, want %q", got, tc.want)
			}
		})
	}
}
