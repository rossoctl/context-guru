package schema

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func strp(s string) *string { return &s }

func TestRewritable(t *testing.T) {
	text := "hello"
	cases := []struct {
		name string
		msg  bschemas.ChatMessage
		want bool
	}{
		{"nil content", bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool}, true},
		{"string content", bschemas.ChatMessage{Content: &bschemas.ChatMessageContent{ContentStr: strp("x")}}, true},
		{"all text blocks", bschemas.ChatMessage{Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{
			{Type: bschemas.ChatContentBlockTypeText, Text: &text},
		}}}, true},
		{"has image block", bschemas.ChatMessage{Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{
			{Type: bschemas.ChatContentBlockTypeText, Text: &text},
			{Type: bschemas.ChatContentBlockTypeImage},
		}}}, false},
		{"unmodeled tool_result block", bschemas.ChatMessage{Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{
			{Type: bschemas.ChatContentBlockType("tool_result")},
		}}}, false},
	}
	for _, c := range cases {
		if got := Rewritable(c.msg); got != c.want {
			t.Errorf("%s: Rewritable=%v want %v", c.name, got, c.want)
		}
	}
}

func TestMessagesTokens(t *testing.T) {
	if MessagesTokens(nil) != 0 {
		t.Fatal("nil request => 0 tokens")
	}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		{Content: &bschemas.ChatMessageContent{ContentStr: strp("hello world this is some text")}},
	}}
	if MessagesTokens(req) <= 0 {
		t.Fatal("expected a positive token count for non-empty content")
	}
	if TextTokens("") != 0 {
		t.Fatal("empty string => 0 tokens")
	}
}

func TestCloneMessagesIndependent(t *testing.T) {
	orig := []bschemas.ChatMessage{{
		Role:    bschemas.ChatMessageRoleTool,
		Content: &bschemas.ChatMessageContent{ContentStr: strp("x")},
	}}
	clone := CloneMessages(orig)
	SetMessageText(&clone[0], "mutated")
	if MessageText(orig[0]) != "x" {
		t.Fatal("clone must be independent of the original")
	}
	if CloneMessages(nil) != nil {
		t.Fatal("nil in => nil out")
	}
}

// TestCloneMessagesIndependentWhenJSONFails: the JSON round-trip DOES fail in practice —
// bifrost's ChatMessageContent.MarshalJSON errors when a message carries both string and
// block content, which a component can produce. The clone used to return the input slice
// itself on that path, so the snapshot ALIASED the live messages: a component mutating in
// place also mutated the snapshot, and the pipeline's revert (error/panic/never-worse)
// became a no-op. Isolation must hold on the error path too.
func TestCloneMessagesIndependentWhenJSONFails(t *testing.T) {
	orig := []bschemas.ChatMessage{{
		Role: bschemas.ChatMessageRoleTool,
		Content: &bschemas.ChatMessageContent{
			ContentStr:    strp("x"),
			ContentBlocks: []bschemas.ChatContentBlock{{Type: bschemas.ChatContentBlockTypeText, Text: strp("x")}},
		},
	}}
	clone := CloneMessages(orig)
	if len(clone) != 1 {
		t.Fatalf("clone lost messages: %+v", clone)
	}
	*clone[0].Content.ContentStr = "mutated"
	clone[0].Content.ContentBlocks[0].Text = strp("mutated")
	if got := *orig[0].Content.ContentStr; got != "x" {
		t.Errorf("clone aliases the original's content string (got %q) — revert would be a no-op", got)
	}
	if got := *orig[0].Content.ContentBlocks[0].Text; got != "x" {
		t.Errorf("clone aliases the original's content blocks (got %q)", got)
	}
}

// TestFallbackCloneKeepsCacheFields: bifrost's DeepCopyChatMessage drops
// CacheControl/Citations/CachePoint from content blocks, so the fallback clone path used
// to hand back a snapshot with no prompt-cache breakpoints. Reverting a component then
// DELETED breakpoints the request arrived with — the never-worse invariant violated on
// the path that only runs when something already went wrong.
func TestFallbackCloneKeepsCacheFields(t *testing.T) {
	ttl, scope, enabled := "1h", "global", true
	orig := []bschemas.ChatMessage{{
		Role: bschemas.ChatMessageRoleUser,
		Content: &bschemas.ChatMessageContent{
			// Both set => ChatMessageContent.MarshalJSON errors => fallbackClone path.
			ContentStr: strp("x"),
			ContentBlocks: []bschemas.ChatContentBlock{{
				Type:         bschemas.ChatContentBlockTypeText,
				Text:         strp("cached prefix"),
				CacheControl: &bschemas.CacheControl{Type: bschemas.CacheControlTypeEphemeral, TTL: &ttl, Scope: &scope},
				Citations:    &bschemas.Citations{Enabled: &enabled},
				CachePoint:   &bschemas.CachePoint{Type: "default", TTL: &ttl},
			}},
		},
	}}
	snap := CloneMessages(orig)
	if len(snap) != 1 || snap[0].Content == nil || len(snap[0].Content.ContentBlocks) != 1 {
		t.Fatalf("fallback clone lost structure: %+v", snap)
	}

	// A component mutates the live message in place and strips the breakpoint...
	live := orig[0].Content.ContentBlocks
	live[0].Text = strp("rewritten")
	live[0].CacheControl = nil
	live[0].Citations = nil
	live[0].CachePoint = nil

	// ...the pipeline reverts by assigning the snapshot back.
	got := snap[0].Content.ContentBlocks[0]
	if got.CacheControl == nil {
		t.Fatal("revert dropped cache_control: a prompt-cache breakpoint is gone from the wire")
	}
	if got.CacheControl.Type != bschemas.CacheControlTypeEphemeral {
		t.Errorf("cache_control type = %q want ephemeral", got.CacheControl.Type)
	}
	if got.CacheControl.TTL == nil || *got.CacheControl.TTL != "1h" {
		t.Errorf("cache_control ttl = %v want 1h", got.CacheControl.TTL)
	}
	if got.CacheControl.Scope == nil || *got.CacheControl.Scope != "global" {
		t.Errorf("cache_control scope = %v want global", got.CacheControl.Scope)
	}
	if got.Citations == nil || got.Citations.Enabled == nil || !*got.Citations.Enabled {
		t.Errorf("revert dropped citations: %+v", got.Citations)
	}
	if got.CachePoint == nil || got.CachePoint.Type != "default" || got.CachePoint.TTL == nil || *got.CachePoint.TTL != "1h" {
		t.Errorf("revert dropped cachePoint: %+v", got.CachePoint)
	}

	// The snapshot's pointers must not alias the live ones either: mutating the clone
	// must not reach the original (and the original's TTL string is still "1h").
	*got.CacheControl.TTL = "5m"
	if ttl != "1h" {
		t.Error("clone shares the original's cache_control TTL pointer")
	}
}

func TestBlockTextNonText(t *testing.T) {
	if BlockText(bschemas.ChatContentBlock{Type: bschemas.ChatContentBlockTypeImage}) != "" {
		t.Fatal("a non-text (image) block has no text")
	}
	if MessageText(bschemas.ChatMessage{}) != "" {
		t.Fatal("nil content => empty text")
	}
}

func TestMessageTextBlocks(t *testing.T) {
	a, b := "foo", "bar"
	m := bschemas.ChatMessage{Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{
		{Type: bschemas.ChatContentBlockTypeText, Text: &a},
		{Type: bschemas.ChatContentBlockTypeText, Text: &b},
	}}}
	if got := MessageText(m); got != "foobar" {
		t.Fatalf("MessageText=%q want foobar", got)
	}
}
