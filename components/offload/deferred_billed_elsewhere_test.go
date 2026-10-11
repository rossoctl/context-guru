package offload

import (
	"context"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/store"
)

// #455: a detail-only deferral (a call another record already bills) still produces its
// extraction_calls row on the next turn, but is NOT replayed into that turn's LLM-cost ledger.
// An ordinary deferral still is.
func TestDeferDetailOnlyListsTheCallButDoesNotBillItAgain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		detailOnly bool
		wantCalls  int64 // calls the next turn's ledger sink sees
	}{
		{"detail only", true, 0},
		{"ordinary", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory(store.Options{MaxEntries: 50})
			callCtx, callSink := cheapmodel.WithDetachedSink(context.Background())
			cheapmodel.ReplayUsage(callCtx, "claude-sonnet-5", 281, 683, 0, 43400)
			if tc.detailOnly {
				deferDetailOnly(st, "s1", "cache_aware_summarizer", "prefix_ask", "keepalive_substitute", 12, callSink)
			} else {
				deferUsage(st, "s1", "cache_aware_summarizer", "prefix_ask", "keepalive_substitute", 12, callSink)
			}

			turnSink := &cheapmodel.Sink{}
			c := &components.Ctx{Ctx: cheapmodel.WithSink(context.Background(), turnSink), Session: "s1", Store: st}
			rep := &components.Report{}
			takeDeferredUsage(c, rep)
			if len(rep.Calls) != 1 || rep.Calls[0].Strategy != "prefix_ask/keepalive_substitute" ||
				rep.Calls[0].CacheRead != 43400 {
				t.Fatalf("want one detail row for the call, got %+v", rep.Calls)
			}
			if calls, _, _ := turnSink.Totals(); calls != tc.wantCalls {
				t.Fatalf("next turn's ledger saw %d calls, want %d", calls, tc.wantCalls)
			}
		})
	}
}
