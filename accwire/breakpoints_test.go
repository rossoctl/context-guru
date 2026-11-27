package accwire

import (
	"testing"

	"github.com/rossoctl/accountant"
)

// The accountant reads a breakpoint by schema's rule: an OBJECT under cache_control, or under
// cachePoint on a tool, a system block or a content block. A null or a scalar is no mark, and a
// message carries cache_control only.
func TestBreakpointsFollowSchemaMarkRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []accountant.Breakpoint
	}{
		{"cachePoint on a content block",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"cachePoint":{"type":"default"}}]}]}`,
			[]accountant.Breakpoint{{Pos: accountant.Position{Section: accountant.SectionMessages, Index: 0}, TTL: "5m"}}},
		{"cachePoint with a 1h ttl",
			`{"messages":[{"role":"user","content":[{"cachePoint":{"type":"default","ttl":"1h"}}]}]}`,
			[]accountant.Breakpoint{{Pos: accountant.Position{Section: accountant.SectionMessages, Index: 0}, TTL: "1h"}}},
		{"cachePoint as its own system entry",
			`{"system":[{"text":"s"},{"cachePoint":{"type":"default"}}],"messages":[{"role":"user","content":"hi"}]}`,
			[]accountant.Breakpoint{{Pos: accountant.Position{Section: accountant.SectionSystem, Index: 1}, TTL: "5m"}}},
		{"cachePoint as its own tool entry",
			`{"tools":[{"name":"t"},{"cachePoint":{"type":"default"}}],"messages":[{"role":"user","content":"hi"}]}`,
			[]accountant.Breakpoint{{Pos: accountant.Position{Section: accountant.SectionTools, Index: 1}, TTL: "5m"}}},
		{"null cache_control on a block is no mark",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":null}]}]}`, nil},
		{"null cache_control on a message is no mark",
			`{"messages":[{"role":"user","content":"a","cache_control":null}]}`, nil},
		{"cachePoint on a message is no mark",
			`{"messages":[{"role":"user","content":"a","cachePoint":{"type":"default"}}]}`, nil},
		{"cache_control object still counts",
			`{"messages":[{"role":"user","content":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
			[]accountant.Breakpoint{{Pos: accountant.Position{Section: accountant.SectionMessages, Index: 0}, TTL: "1h"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Messages.Breakpoints([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}
