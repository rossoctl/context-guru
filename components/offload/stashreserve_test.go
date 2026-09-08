package offload

import (
	"context"
	"encoding/json"
	"regexp"
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

// Every marker in the REQUEST AS MARSHALLED must resolve.
//
// This is the invariant #187 broke, and it is asserted on the rendered bytes rather than on
// the component's return value on purpose: a component can compute a key, hand it back to the
// pipeline, and still have written a marker into the message — so a test that reads only the
// returned keys would pass while the wire carried a marker nothing can serve. The store here
// holds a 2-slot reserve against three large outputs, so the third removal MUST be refused,
// and the message it would have cut MUST arrive verbatim.
func TestNoMarkerReachesTheWireWithoutItsPayload(t *testing.T) {
	st := store.NewMemory(store.Options{MaxEntries: 4}) // reserve = 2
	comp, err := newLinecap([]byte("max_line_chars: 40\nmin_size: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	req := &bschemas.BifrostChatRequest{}
	for _, tag := range []string{"a", "b", "c", "d", "e"} {
		m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool}
		schema.SetMessageText(&m, wideOutput(tag))
		req.Input = append(req.Input, m)
	}
	original := make([]string, len(req.Input))
	for i := range req.Input {
		original[i] = schema.MessageText(req.Input[i])
	}
	before := StashRefusals()
	var rep components.Report
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, MaxCachedIdx: -1}
	if _, err := comp.(*Linecap).Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}

	// The rendered request, which is what the model actually reads. Scanned with the ESCAPED
	// spelling as well as the plain one, because encoding/json HTML-escapes "<" by default and
	// every marker is appended after a newline — so a marker the model reads as <<cg:HASH>>
	// usually exists in the bytes on the wire only as \u003c\u003ccg:HASH\u003e\u003e. A test
	// that scanned for the plain form alone would report zero markers on a body full of them
	// and pass while the wire carried unbacked ones (expand.rawMarkerRe exists for the same
	// reason, on the proxy's side of the same problem).
	wire, err := json.Marshal(req.Input)
	if err != nil {
		t.Fatal(err)
	}
	wireMarkerRe := regexp.MustCompile(`(?:<|(?i:\\u003c)){2}cg:([A-Za-z0-9_-]{1,64})(?:>|(?i:\\u003e)){2}`)
	var markers []string
	for _, m := range wireMarkerRe.FindAllStringSubmatch(string(wire), -1) {
		markers = append(markers, m[1])
	}
	if len(markers) == 0 {
		t.Fatal("the fixture produced no markers at all, so it cannot show whether an " +
			"unbacked one reaches the wire — check the linecap config")
	}
	for _, id := range markers {
		if _, ok := expand.Resolve(st, id); !ok {
			t.Errorf("marker <<cg:%s>> is in the marshalled request and resolves to NOTHING: "+
				"the model is being told it can retrieve content this proxy cannot produce. "+
				"That is expand_unresolved_missing, from the inside (#187)", id)
		}
	}
	if len(markers) > 2 {
		t.Errorf("%d markers reached the wire against a 2-payload reserve; a refused removal "+
			"must leave the message VERBATIM, not stamp a marker anyway", len(markers))
	}
	// And the refusal is counted, so an operator sees the budget bind at the moment it binds
	// rather than whenever the model next happens to call expand.
	if got := StashRefusals() - before; got == 0 {
		t.Error("StashRefusals() did not move while removals were being declined; nothing " +
			"upstream of expand_unresolved_missing reports an exhausted reserve")
	}
	// Whatever was left verbatim must still be its original text — refusing is "leave it
	// alone", not "cut it and say nothing".
	verbatim := 0
	for i := range req.Input {
		txt := schema.MessageText(req.Input[i])
		if expand.HasPlaceholder(txt) {
			continue
		}
		verbatim++
		if txt != original[i] {
			t.Errorf("message %d carries no marker and is not its original text either: a "+
				"refused removal turned into a silent lossy drop, which is the other way to "+
				"break the invariant", i)
		}
	}
	if verbatim == 0 {
		t.Error("every message was rewritten, so no removal was refused; the reserve did not bind")
	}
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
