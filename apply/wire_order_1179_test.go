package apply_test

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// keepTail2 is a count-changing component like the summarizer: it keeps message 0, replaces every
// message up to the last two with one summary, and keeps the last two (#1179).
type keepTail2 struct{}

func (keepTail2) Name() string                 { return "keeptail2" }
func (keepTail2) Enabled(*components.Ctx) bool { return true }
func (keepTail2) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	in := req.Input
	if len(in) < 5 {
		return nil
	}
	sum := in[1]
	sum.ChatAssistantMessage = nil
	sum.Role = bschemas.ChatMessageRoleAssistant
	schema.SetMessageText(&sum, "summary")
	req.Input = []bschemas.ChatMessage{in[0], sum, in[len(in)-2], in[len(in)-1]}
	return nil
}

func init() {
	components.Register("keeptail2", func([]byte) (components.Component, error) { return keepTail2{}, nil })
}

func think(sig string) string {
	return `{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"` + sig + `"}]}`
}

func checkKeepTail2(t *testing.T, b []string) {
	t.Helper()
	body := `{"model":"claude-x","messages":[` + strings.Join(b, ",") + `]}`
	res := traceOf(t, "keeptail2", bschemas.Anthropic, body)
	if !res.Changed {
		t.Fatalf("the summary was not forwarded (fell back to the original): ops=%+v dropped=%d", res.Trace.Ops, res.Trace.OpsDropped)
	}
	n := len(b)
	checkWireOrder(t, res.Body, []string{b[0], summaryMsg(), b[n-2], b[n-1]})
}

// A kept user text at the end has the same bytes as a removed one near the start.
func TestSpliceKeptDuplicateTextAtEndKeepsOrder(t *testing.T) {
	u := `{"role":"user","content":"same words"}`
	a := `{"role":"assistant","content":"ok"}`
	a2 := `{"role":"assistant","content":"ok2"}`
	checkKeepTail2(t, []string{`{"role":"user","content":"first"}`, u, a, `{"role":"user","content":"x"}`, a2, `{"role":"user","content":"more"}`, u})
}

// The thinking-only assistant shape: same thinking text and the same signature.
func TestSpliceKeptThinkingOnlyDuplicateKeepsOrder(t *testing.T) {
	u := `{"role":"user","content":"go on"}`
	th := think("sigA")
	checkKeepTail2(t, []string{`{"role":"user","content":"first"}`, th, u, th, u, th, u, th})
}

// The same shape with a different signature on each message.
func TestSpliceKeptThinkingOnlyOtherSignaturesKeepsOrder(t *testing.T) {
	u := `{"role":"user","content":"go on"}`
	checkKeepTail2(t, []string{`{"role":"user","content":"first"}`, think("s1"), u, think("s2"), u, think("s3"), u, think("s4")})
}
