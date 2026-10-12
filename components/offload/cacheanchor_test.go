package offload

import (
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func anchorBlockAssistant(text string) bschemas.ChatMessage {
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
		Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{{Type: bschemas.ChatContentBlockTypeText, Text: &text}}}}
}

// anchorConv: user, then n (assistant tool turn, tool result) pairs; a request ends on its last tool result.
func anchorConv(n int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("go")}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input, anchorBlockAssistant("step "+strconv.Itoa(i)),
			toolResultMsgWithID("toolu_a"+strconv.Itoa(i), "output "+strconv.Itoa(i)+"\n"+strings.Repeat("line\n", 200)))
	}
	return req
}

func marks(req *bschemas.BifrostChatRequest) []int {
	var at []int
	for i := range req.Input {
		if req.Input[i].Content == nil {
			continue
		}
		for _, b := range req.Input[i].Content.ContentBlocks {
			if b.CacheControl != nil {
				at = append(at, i)
			}
		}
	}
	return at
}

func runAnchor(t *testing.T, c *components.Ctx, req *bschemas.BifrostChatRequest) *components.Report {
	t.Helper()
	comp, _ := newCacheAnchor(nil)
	rep := &components.Report{Component: "cacheanchor"}
	if _, err := comp.(components.Offload).Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	return rep
}

func anchorCtx(st store.Store) *components.Ctx {
	return &components.Ctx{Session: "anc", Store: st, ExistingBreakpoints: 3}
}

func TestCacheAnchorMarksTheTurnAfterTheLastValidEntryBeforeAnEdit(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	anchorNow = func() time.Time { return now }
	defer func() { anchorNow = time.Now }()
	st := store.NewMemory(store.Options{MaxEntries: 100})
	// three appends: each request ends one pair later
	for n := 3; n <= 6; n++ {
		if rep := runAnchor(t, anchorCtx(st), anchorConv(n)); rep.Events["anchor_placed"] != 0 {
			t.Fatalf("an append must not be anchored (n=%d)", n)
		}
		now = now.Add(20 * time.Second)
	}
	// the next request edits the tool result of pair 5 (message 10); requests ended at 6, 8, 10 and 12 before
	req := anchorConv(7)
	schema.SetMessageText(&req.Input[10], "[masked]")
	rep := runAnchor(t, anchorCtx(st), req)
	got := marks(req)
	// first change p=10; latest earlier end e<=8 is 8 -> mark message 9 (the assistant turn after it)
	if rep.Events["anchor_placed"] != 1 || len(got) != 1 || got[0] != 9 {
		t.Fatalf("want one mark on message 9, got %v (events %v gates %v)", got, rep.Events, rep.Gates)
	}
}

func TestCacheAnchorRespectsTheCapTheClockAndColdRequests(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	anchorNow = func() time.Time { return now }
	defer func() { anchorNow = time.Now }()
	prime := func(st store.Store) {
		for n := 3; n <= 6; n++ {
			runAnchor(t, anchorCtx(st), anchorConv(n))
			now = now.Add(20 * time.Second)
		}
	}
	edited := func() *bschemas.BifrostChatRequest {
		r := anchorConv(7)
		schema.SetMessageText(&r.Input[10], "[masked]")
		return r
	}
	// no free slot
	st := store.NewMemory(store.Options{MaxEntries: 100})
	prime(st)
	c := anchorCtx(st)
	c.ExistingBreakpoints = 4
	r := edited()
	if rep := runAnchor(t, c, r); len(marks(r)) != 0 || rep.Gates["anchor_no_free_slot"] != 1 {
		t.Errorf("a fifth breakpoint is a 400: %v %v", marks(r), rep.Gates)
	}
	// the entries have expired
	st = store.NewMemory(store.Options{MaxEntries: 100})
	prime(st)
	now = now.Add(10 * time.Minute)
	r = edited()
	if runAnchor(t, anchorCtx(st), r); len(marks(r)) != 0 {
		t.Errorf("entries older than the TTL must not be trusted: %v", marks(r))
	}
	// cold request: the whole prefix is rewritten anyway
	st = store.NewMemory(store.Options{MaxEntries: 100})
	prime(st)
	c = anchorCtx(st)
	c.ColdCache = true
	r = edited()
	if runAnchor(t, c, r); len(marks(r)) != 0 {
		t.Errorf("no anchor on a cold request: %v", marks(r))
	}
}
