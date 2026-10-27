package offload

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// FIXED RESTORE KEEPS EVERY OFFLOADER'S COMPACTION IN PLACE (#407).
//
// The first version of this fix only reached reapplyFrozen, so it covered the eight offloaders
// that replay through it and missed the case it was measured on: session e8f16627's output was
// compacted by extract, which re-derives every turn and stops at skipReduce, and extract_llm,
// which checks kept-verbatim before its replay. Both still flipped with the switch on. The fix
// now lives in isKeptVerbatim, which every one of them consults, so this table covers one offloader
// per route into it: reapplyFrozen (mask, failed_run), skipReduce (extract), and extract_llm's own
// check.
//
// Each row asserts its own control FIRST: without Ctx.Restored, the same expand flips the message
// back to the full original. Without that, "the bytes did not change" would also pass for an
// offloader that never consulted kept-verbatim at all.
func TestFixedRestoreKeepsTheCompactionAtTheOriginalPosition(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		// make returns the component and a context for one turn; tail is whether this turn's
		// target message is still in the uncached tail.
		make func(t *testing.T) (components.Offload, func(st store.Store, tail bool) *components.Ctx)
		// idx is the target message's index in req().
		idx int
		req func(body string) []bschemas.ChatMessage
	}{
		{name: "mask (reapplyFrozen)", body: strings.Repeat("verbose tool output line\n", 30), idx: 1,
			req: func(b string) []bschemas.ChatMessage {
				return []bschemas.ChatMessage{userMsg("go"), tool(b), tool("tail")}
			},
			make: func(t *testing.T) (components.Offload, func(store.Store, bool) *components.Ctx) {
				return maskFor(t), plainCtx
			}},
		{name: "failed_run (reapplyFrozen, then its own check)", idx: 1,
			body: strings.Repeat("--- FAIL: TestX\n", 40) + "3 failed\n",
			req: func(b string) []bschemas.ChatMessage {
				return []bschemas.ChatMessage{userMsg("go"), tool(b), tool(strings.Repeat("ok pkg\n", 40) + "5 passed\n")}
			},
			make: func(t *testing.T) (components.Offload, func(store.Store, bool) *components.Ctx) {
				c, err := newFailedRun([]byte("min_tokens: 5\n"))
				if err != nil {
					t.Fatal(err)
				}
				return c.(components.Offload), plainCtx
			}},
		{name: "extract (skipReduce) — the e8f16627 shape", body: strings.Repeat("verbose tool output line\n", 30), idx: 1,
			req: func(b string) []bschemas.ChatMessage {
				return []bschemas.ChatMessage{userMsg("go"), tool(b), tool("tail")}
			},
			make: func(t *testing.T) (components.Offload, func(store.Store, bool) *components.Ctx) {
				c, err := newExtract([]byte("min_tokens: 5\n"))
				if err != nil {
					t.Fatal(err)
				}
				return c.(components.Offload), plainCtx
			}},
		{name: "extract_llm (its own replay check)", idx: 1,
			body: strings.Repeat("2026-08-31T10:00:00Z INFO worker: processed batch\n", 400),
			req: func(b string) []bschemas.ChatMessage {
				return []bschemas.ChatMessage{userMsg("summarize the worker log"), toolResultMsg(b)}
			},
			make: func(t *testing.T) (components.Offload, func(store.Store, bool) *components.Ctx) {
				model := &summarizingModel{summary: basisSummary}
				return newTimeoutTestComponent(t, model), func(st store.Store, _ bool) *components.Ctx {
					return pricedCtx("fixed-restore-xllm", st, model)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// run drives turn 1 (compact, in the tail), the expand, and turn 2 (in the cached
			// prefix), and returns both turns' bytes for the target message.
			run := func(restored bool) (first, second string) {
				st := store.NewMemory(store.Options{MaxEntries: 400})
				off, ctxFor := tc.make(t)
				req := &bschemas.BifrostChatRequest{Input: tc.req(tc.body)}
				if _, err := off.Offload(req, &components.Report{}, ctxFor(st, true)); err != nil {
					t.Fatal(err)
				}
				first = schema.MessageText(req.Input[tc.idx])
				ids := expand.ParseMarkers(first)
				if first == tc.body || len(ids) == 0 {
					t.Fatalf("turn 1 did not compact the output behind a marker, so there is nothing to keep: %.120q", first)
				}
				c := ctxFor(st, false)
				MarkKeptVerbatim(st, c.Session, tc.body)
				if restored {
					c.Restored = map[string]string{contentKey(tc.body): ids[0]}
				}
				req2 := &bschemas.BifrostChatRequest{Input: tc.req(tc.body)}
				if _, err := off.Offload(req2, &components.Report{}, c); err != nil {
					t.Fatal(err)
				}
				return first, schema.MessageText(req2.Input[tc.idx])
			}
			// CONTROL: kept-verbatim, as before. This is what proves the row reaches the check.
			if first, second := run(false); second == first || second != tc.body {
				t.Fatalf("without fixed restore the expand did not revert the message to the original "+
					"(turn 2 = %.80q), so this row does not exercise the kept-verbatim path at all", second)
			}
			first, second := run(true)
			if second != first {
				t.Fatalf("with the original restored at an anchor, turn 2 changed the message at its own "+
					"position (%d -> %d bytes): the prefix flip #407 measured", len(first), len(second))
			}
		})
	}
}

func plainCtx(st store.Store, tail bool) *components.Ctx {
	maxCached := 1 // the target (index 1) is inside the cached prefix
	if tail {
		maxCached = -1
	}
	return &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, CacheAware: true, MaxCachedIdx: maxCached}
}

// summarize REPLACES a span instead of rewriting messages in place, so isKeptVerbatim's "no" would
// let it summarize the original away and, with it, the anchor right after the expanding turn. The
// span guard must still protect the original in BOTH of the forms summarize can meet it in: full
// (summarize ran before the offloader that compacts it) and compacted behind its marker (after).
func TestTheSpanGuardProtectsARestoredOriginalInEitherForm(t *testing.T) {
	body := strings.Repeat("verbose tool output line\n", 30)
	const id = "0123456789abcdef"
	c := &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{}),
		Restored: map[string]string{contentKey(body): id}}
	MarkKeptVerbatim(c.Store, "s", body)
	if isKeptVerbatim(c, contentKey(body)) {
		t.Fatal("a restored original still reads as kept-verbatim, so every offloader would flip it")
	}
	compacted := "[older tool output masked] " + expand.Marker(id) + " [full output: call " + expand.ToolName + "]"
	for name, text := range map[string]string{"full": body, "compacted": compacted} {
		if !restoredSpanGuard(c, text) {
			t.Errorf("the %s original is not protected from a summarize span", name)
		}
	}
	if restoredSpanGuard(c, "unrelated output") || restoredSpanGuard(&components.Ctx{}, body) {
		t.Error("the guard protects content that is not being restored")
	}
	// And through the real trim: a span over [user, original, tail] stops before the original.
	asst := userMsg("ok")
	asst.Role = bschemas.ChatMessageRoleAssistant
	msgs := []bschemas.ChatMessage{userMsg("go"), userMsg("more"), asst, userMsg(compacted), userMsg("tail")}
	if end := trimSpanForKeptVerbatim(msgs, 1, 5, func(s string) bool { return restoredSpanGuard(c, s) }); end != 3 {
		t.Fatalf("span end = %d, want 3 (stop before the restored original)", end)
	}
}
