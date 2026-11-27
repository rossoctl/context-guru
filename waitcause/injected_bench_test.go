package waitcause

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/agent"
)

// The wake check runs on the last message of every request. A long pasted prompt is the worst case:
// no marker is found, so every tag is scanned over the whole text.
var benchText = strings.Repeat("please fix the failing test in the parser and then run the suite again. ", 1500)

func BenchmarkIsInjectedText(b *testing.B) {
	cl := agent.IdentifyUserAgent("claude-cli/2.1.319 (external, cli)")
	b.SetBytes(int64(len(benchText)))
	for i := 0; i < b.N; i++ {
		if isInjectedText(cl, benchText) {
			b.Fatal("plain text read as injected")
		}
	}
}
