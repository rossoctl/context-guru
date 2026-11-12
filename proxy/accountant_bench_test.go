package proxy

import (
	"fmt"
	"github.com/rossoctl/accountant"
	"strings"
	"testing"
	"time"
)

// benchBody is a Claude Code shaped request of about 600 KB: 150 messages, each with a block of
// text, and a cache mark on the newest block.
func benchBody() []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[`)
	for i := 0; i < 150; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		mark := ""
		if i == 149 {
			mark = `,"cache_control":{"type":"ephemeral"}`
		}
		fmt.Fprintf(&b, `{"role":%q,"content":[{"type":"text","text":%q%s}]}`, role, strings.Repeat("word ", 800)+fmt.Sprint(i), mark)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkAccBodies(b *testing.B) {
	raw := benchBody()
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		accBodies(raw)
	}
}

func BenchmarkAccBreakpoints(b *testing.B) {
	raw := benchBody()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		accBreakpoints(raw)
	}
}

// BenchmarkBeginEndUnchanged is the accountant's cost on the request path for a request forwarded as sent.
func BenchmarkBeginEndUnchanged(b *testing.B) {
	raw := benchBody()
	s := newSavingsTracker()
	meta := accountant.RequestMeta{RequestID: "r", Session: "s", At: time.Unix(1000, 0), Billing: accountant.Billing{Mode: accountant.BillingAPI}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := s.begin(meta, raw)
		s.end(r, accountant.Thread{ID: "main"}, raw, nil, accFacts)
	}
}
