package all_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func run(t *testing.T, yaml string, req *schemas.BifrostChatRequest) (*components.RunReport, store.Store) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st}
	return pipe.Run(req, c), st
}

func TestFormatCompactsJSON(t *testing.T) {
	type row struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	rows := make([]row, 20)
	for i := range rows {
		rows[i] = row{ID: i, Name: "item-with-a-longish-name"}
	}
	pretty, _ := json.MarshalIndent(rows, "", "    ")
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(string(pretty))}}
	before := schema.MessagesTokens(req)
	run(t, "pipeline: [format]\n", req)
	after := schema.MessagesTokens(req)
	if after >= before {
		t.Fatalf("format did not compact JSON: before=%d after=%d", before, after)
	}
	// still valid JSON (lossless)
	var back []row
	if err := json.Unmarshal([]byte(schema.MessageText(req.Input[0])), &back); err != nil || len(back) != 20 {
		t.Fatalf("format broke JSON validity: %v", err)
	}
}
