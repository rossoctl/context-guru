package bifrost

import (
	"context"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func toolMsg(text string) bschemas.ChatMessage {
	t := text
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool, Content: &bschemas.ChatMessageContent{ContentStr: &t}}
}

func newPlugin(t *testing.T, yaml string) *Plugin {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(pipe, store.NewMemory(store.Options{}))
}

func TestPreRequestHookRunsPipeline(t *testing.T) {
	p := newPlugin(t, "pipeline: [dedup]\n")
	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	chat := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{toolMsg(dump), toolMsg(dump)}}
	req := &bschemas.BifrostRequest{ChatRequest: chat}
	ctx := bschemas.NewBifrostContext(context.Background(), time.Time{})

	before := schema.MessagesTokens(chat)
	if err := p.PreRequestHook(ctx, req); err != nil {
		t.Fatal(err)
	}
	if schema.MessagesTokens(chat) >= before {
		t.Fatal("PreRequestHook did not reduce the duplicate tool output")
	}
}

func TestNonChatRequestPassesThrough(t *testing.T) {
	p := newPlugin(t, "pipeline: [dedup]\n")
	ctx := bschemas.NewBifrostContext(context.Background(), time.Time{})
	if err := p.PreRequestHook(ctx, &bschemas.BifrostRequest{}); err != nil {
		t.Fatalf("nil ChatRequest should be a no-op, got %v", err)
	}
}

func TestSessionResolutionPrefersExplicit(t *testing.T) {
	p := newPlugin(t, "pipeline: [dedup]\n")
	ctx := bschemas.NewBifrostContext(context.Background(), time.Time{})
	ctx.SetValue(SessionContextKey, "explicit-123")
	chat := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{toolMsg("hi")}}
	if got := p.resolveSession(ctx, chat); got != "explicit-123" {
		t.Fatalf("explicit session id should win, got %q", got)
	}
}

func TestBypassSkips(t *testing.T) {
	p := newPlugin(t, "pipeline: [dedup]\n")
	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	chat := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{toolMsg(dump), toolMsg(dump)}}
	ctx := bschemas.NewBifrostContext(context.Background(), time.Time{})
	ctx.SetValue(BypassContextKey, true)
	before := schema.MessagesTokens(chat)
	_ = p.PreRequestHook(ctx, &bschemas.BifrostRequest{ChatRequest: chat})
	if schema.MessagesTokens(chat) != before {
		t.Fatal("bypass should have left the request unchanged")
	}
}

// The agent's own id in the request metadata survives compaction, which changes the head
// messages. Bifrost must read it, like the proxy does.
func TestSessionResolutionReadsMetadataAcrossCompaction(t *testing.T) {
	p := newPlugin(t, "pipeline: [dedup]\n")
	ctx := bschemas.NewBifrostContext(context.Background(), time.Time{})
	mk := func(head string, md map[string]any) *bschemas.BifrostChatRequest {
		r := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{toolMsg(head)}}
		if md != nil {
			r.Params = &bschemas.ChatParameters{Metadata: &md}
		}
		return r
	}
	cc := func(id string) map[string]any {
		return map[string]any{"user_id": `{"device_id":"d","account_uuid":"","session_id":"` + id + `"}`}
	}
	before := p.resolveSession(ctx, mk("original head", cc("sess-1")))
	after := p.resolveSession(ctx, mk("This session is being continued", cc("sess-1")))
	if before != after || before != "sess-1" {
		t.Fatalf("metadata id must survive a changed head: before %q after %q", before, after)
	}
	bob := map[string]any{"taskId": "task-9"}
	if got := p.resolveSession(ctx, mk("x", bob)); got != "task-9" {
		t.Fatalf("taskId should resolve, got %q", got)
	}
	// header wins over metadata
	ctx.SetValue(SessionContextKey, "hdr")
	if got := p.resolveSession(ctx, mk("x", cc("sess-1"))); got != "hdr" {
		t.Fatalf("header should win, got %q", got)
	}
	// non-string metadata falls back to the derived hash (16 hex)
	ctx2 := bschemas.NewBifrostContext(context.Background(), time.Time{})
	if got := p.resolveSession(ctx2, mk("x", map[string]any{"user_id": 5})); len(got) != 16 {
		t.Fatalf("non-string metadata should fall back to the hash, got %q", got)
	}
}
