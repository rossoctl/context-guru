package all

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// context-guru's own plugin output — the adapt report, the cost table — is the product's
// answer, printed verbatim by the agent. No component may change it; every other tool result
// is compacted as before.
func TestOwnPluginOutputIsNeverCompacted(t *testing.T) {
	rows := make([]map[string]any, 30)
	for i := range rows {
		rows[i] = map[string]any{"id": i, "name": "row", "ok": true}
	}
	b, _ := json.MarshalIndent(rows, "", "        ")
	pretty := string(b)
	req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic}
	call := func(id, cmd string) {
		name, args := "Bash", `{"command":`+string(must(json.Marshal(cmd)))+`}`
		req.Input = append(req.Input, schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant,
			ChatAssistantMessage: &schemas.ChatAssistantMessage{ToolCalls: []schemas.ChatAssistantMessageToolCall{{
				ID: &id, Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: args}}}}})
		text := pretty
		req.Input = append(req.Input, schemas.ChatMessage{Role: schemas.ChatMessageRoleTool,
			Content:         &schemas.ChatMessageContent{ContentStr: &text},
			ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: &id}})
	}
	call("t1", `python3 "/home/u/.claude/plugins/cache/m/context-guru/0.5.0/scripts/cg.py" adapt compaction`)
	call("t2", `cat data.json`)
	call("t3", `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" savings`)
	call("t4", `python3 ../../scripts/cg.py adapt compaction --days 7`)              // Codex, relative
	call("t5", `python3 "/tmp/my plugin dir/scripts/cg.py" revert compaction`)       // --plugin-dir, any name
	call("t6", `python3 scripts/cg.py --help && python3 tools/scripts/build.py all`) // someone else's scripts
	cfg, err := config.LoadBytes([]byte("pipeline: [format]\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := p.Run(req, &components.Ctx{Ctx: context.Background(), Session: "s",
		Store: store.NewMemory(store.Options{}), MaxCachedIdx: -1})
	for i, want := range map[int]bool{1: true, 3: false, 5: true, 7: true, 9: true, 11: false} {
		if same := schema.MessageText(req.Input[i]) == pretty; same != want {
			t.Errorf("message %d unchanged = %v, want %v", i, same, want)
		}
	}
	if len(rr.Components) != 1 || rr.Components[0].Gates["own_output"] == 0 {
		t.Errorf("the restore was not reported: %+v", rr.Components)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
