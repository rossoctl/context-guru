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

// wideOutput is a tool output linecap will certainly rewrite: one very long line.
func wideOutput(tag string) string {
	return "line " + tag + ": " + strings.Repeat(tag+"x", 900)
}

// cmdfilter builds its marker token inline instead of calling commitMark, so the reserve
// contract has to be honored a second time in that file — which is exactly the kind of
// second copy a fix forgets. One filtered output, a reserve with no room at all, and the
// message must arrive unchanged rather than carrying a marker nothing can serve.
func TestCmdfilterRefusesRatherThanStampingAnUnbackedMarker(t *testing.T) {
	// The pip advisory pair: a real captured output the shipped filters match.
	const pipNag = "WARNING: Running pip as the 'root' user can result in broken permissions and " +
		"conflicting behaviour with the system package manager, possibly rendering your system " +
		"unusable. It is recommended to use a virtual environment instead: " +
		"https://pip.pypa.io/warnings/venv\n" +
		"WARNING: You are using pip version 21.0.1; however, version 23.0.1 is available.\n" +
		"You should consider upgrading via the '/usr/bin/python3 -m pip install --upgrade pip' command.\n"
	comp, err := newCmdfilter([]byte("min_size: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	// A reserve of zero slots: max_entries 1 => stashCap 0, so no payload can ever be held.
	st := store.NewMemory(store.Options{MaxEntries: 1})
	req := &bschemas.BifrostChatRequest{Provider: bschemas.Anthropic,
		Input: []bschemas.ChatMessage{cmdToolMsg(pipNag)}}
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, MaxCachedIdx: -1}
	var rep components.Report
	if _, err := comp.(components.Offload).Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[0])
	if expand.HasPlaceholder(got) {
		for _, id := range expand.ParseMarkers(got) {
			if _, ok := expand.Resolve(st, id); !ok {
				t.Errorf("cmdfilter stamped <<cg:%s>> with a reserve of zero slots; the payload "+
					"is nowhere and the filter was not reversible", id)
			}
		}
	}
	if got != pipNag {
		t.Errorf("cmdfilter rewrote the output although its original could not be stored:\n%q", got)
	}
}

// summarize REPLACES the span it covers, so it cannot leave "this message" verbatim the way
// the per-message offloaders can — refusing means skipping the checkpoint entirely. The
// failure it must not have is emitting the summary anyway, since the marker inside it is then
// the only route back to a span that is no longer in the transcript at all.
func TestSummarizeSkipsTheCheckpointWhenTheSpanCannotBeStashed(t *testing.T) {
	s := newSummarizeKeepLast(t, 1)
	st := store.NewMemory(store.Options{MaxEntries: 1}) // reserve = 0 slots
	msgs := []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser},
		callMsg("t1"), bulkResult("t1"),
		callMsg("t2"), bulkResult("t2"),
		callMsg("t3"), bulkResult("t3"),
	}
	schema.SetMessageText(&msgs[0], "fix the failing tests")
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, MaxCachedIdx: -1}
	var rep components.Report
	if _, err := s.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != len(msgs) {
		t.Errorf("summarize restructured the transcript (%d messages, was %d) although the span "+
			"it replaced could not be stashed: the summary's marker is the only route back to a "+
			"span that is now gone", len(req.Input), len(msgs))
	}
	for i := range req.Input {
		for _, id := range expand.ParseMarkers(schema.MessageText(req.Input[i])) {
			if _, ok := expand.Resolve(st, id); !ok {
				t.Errorf("message %d carries <<cg:%s>> and the store cannot produce it", i, id)
			}
		}
	}
}
