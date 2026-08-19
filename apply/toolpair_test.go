package apply

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/schema"
)

// TestPairsRealAnthropicToolUseWithItsResult is the end-to-end proof that the command
// that produced a tool_result IS reachable from the proxy's view of the transcript —
// the premise cmdfilter's filter comment used to deny.
//
// The fixture is a REAL captured Anthropic request (a SWE-bench run through the proxy),
// trimmed to two turns: a Bash `grep -n …` call and a Read call, each with its own
// tool_result. It has to be real traffic in this dialect, because bifrost's chat schema
// does not model `tool_use` at all — unmarshalling one keeps the block's type and drops
// its id, name and input — so the pairing works only because normalize lifts the blocks
// into ToolCalls. A hand-written OpenAI-shaped fixture would pass without that lift and
// prove nothing.
func TestPairsRealAnthropicToolUseWithItsResult(t *testing.T) {
	raw, err := os.ReadFile("testdata/anthropic_tool_use.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Provider string          `json:"provider"`
		Body     json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.Provider != string(bschemas.Anthropic) {
		t.Fatalf("fixture provider %q: the lift is Anthropic-dialect", fx.Provider)
	}
	msgs := gjson.GetBytes(fx.Body, "messages").Array()
	norm, _ := normalize(bschemas.Anthropic, msgs)

	pairs := schema.ToolCalls(&bschemas.BifrostChatRequest{Input: norm})
	if len(pairs) != 2 {
		t.Fatalf("paired %d tool results, want 2 (%v)", len(pairs), pairs)
	}
	var cmds []string
	for i, tc := range pairs {
		if norm[i].Role != bschemas.ChatMessageRoleTool {
			t.Errorf("pair points at a %q message, not a tool result", norm[i].Role)
		}
		if schema.MessageText(norm[i]) == "" {
			t.Errorf("paired tool result %d has no text", i)
		}
		cmds = append(cmds, tc.Name+" | "+tc.Command())
	}
	wantOne := func(sub string) {
		t.Helper()
		for _, c := range cmds {
			if strings.Contains(c, sub) {
				return
			}
		}
		t.Errorf("no paired call rendered %q; got %v", sub, cmds)
	}
	wantOne("grep -n") // the Bash command string, not the JSON arguments
	wantOne("Read")    // a non-shell tool renders as name + arguments
	wantOne("/testbed/astropy/units/core.py")
}
