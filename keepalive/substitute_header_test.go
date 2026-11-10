package keepalive

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/store"
)

// cheapClient is a real cheapmodel.Anthropic client on a fake host. Each answer carries the
// subscription limit headers, and its text comes from reply, which sees the request body. The
// substitute tests below send their call through it, so the headers travel the production path:
// the host's Dispatcher, cheapmodel's send, the call's sink, the offload result, the keeper (#1016).
func cheapClient(t *testing.T, reply func(body []byte) string) cheapmodel.Anthropic {
	t.Helper()
	f := dispatchtest.NewFake(func(r dispatch.Request) (dispatch.Response, error) {
		text, _ := json.Marshal(reply(r.Body))
		body := fmt.Sprintf(`{"content":[{"type":"text","text":%s}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":600,"cache_read_input_tokens":48576,"output_tokens":300}}`, text)
		return dispatch.Response{Status: 200, Header: limitHeader(), Body: []byte(body)}, nil
	})
	return cheapmodel.Anthropic{D: f, Ref: f.Pin(), Model: "claude-sonnet-5"}
}

// cheapSummaryModel is the summarizer's model, sent through cheapClient.
type cheapSummaryModel struct{ cli cheapmodel.Anthropic }

func (m cheapSummaryModel) Complete(ctx context.Context, p string) (string, error) {
	return m.cli.Complete(ctx, p)
}

func (m cheapSummaryModel) CompleteMessages(ctx context.Context, _ string, _ []bschemas.ChatMessage) (string, error) {
	return m.cli.Complete(ctx, "summarize")
}

// cheapAsker is the host's PrefixAsker, sent through cheapClient: it asks over a fixed prefix body.
type cheapAsker struct{ cli cheapmodel.Anthropic }

func (a cheapAsker) Ask(ctx context.Context, _ string, ask string) (string, components.PrefixUsage, error) {
	reply, u, err := a.cli.CompletePrefixed(ctx, []byte(kaToolBody), ask)
	return reply, components.PrefixUsage(u), err
}

// dropAll answers "drop" for every label in the question, as sweepAsker does.
func dropAll(body []byte) string {
	var parts []string
	for i := 0; strings.Contains(string(body), fmt.Sprintf("[%d] ", i)); i++ {
		parts = append(parts, fmt.Sprintf(`{"i":%d,"needed_by":"none","quote":"","verdict":"drop"}`, i))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// substituteKeeper is a keeper whose reports go to a sink, for a thread that ends at a tool result.
func substituteKeeper(t *testing.T) (*Keeper, *fakeDispatcher, *reportSink, *testClock) {
	t.Helper()
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}}})
	return k, fd, &reportSink{}, clock
}

// A summary call in the place of a ping reports its answer's headers on the ping row, so the host
// reads the subscription limit headers from it as from an ordinary ping (#1016).
func TestSummarySubstituteReportCarriesTheResponseHeader(t *testing.T) {
	k, fd, sink, clock := substituteKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	kaCandidate(t, "sess-1", st, cheapSummaryModel{cheapClient(t, func([]byte) string {
		return "<summary>explored the handler, 3 tests fail.</summary>"
	})})
	recordBodyWithSink(k, sink, sess1, clock.now(), kaToolBody)
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("the due ping did not fire (%d)", n)
	}
	if k.summarySubstituted.Load() != 1 || fd.n() != 0 {
		t.Fatalf("summarySubstituted = %d, ordinary pings = %d; want the summary in the ping's place",
			k.summarySubstituted.Load(), fd.n())
	}
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	if got := reps[0].Header.Get("anthropic-ratelimit-unified-5h-utilization"); got != "0.42" {
		t.Errorf("report header utilization = %q, want 0.42", got)
	}
}

// A sweep question in the place of a ping reports its answer's headers on the ping row too.
func TestSweepSubstituteReportCarriesTheResponseHeader(t *testing.T) {
	k, fd, sink, clock := substituteKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	registerSweep(t, st, cheapAsker{cheapClient(t, dropAll)}, "claude-sonnet-5", sweepConversation(3))
	recordBodyWithSink(k, sink, sess1, clock.now(), kaToolBody)
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("the due ping did not fire (%d)", n)
	}
	waitSweepOutcome(t, k, fd)
	if k.sweepSubstituted.Load() != 1 || fd.n() != 0 {
		t.Fatalf("sweepSubstituted = %d, ask failed %d, ordinary pings = %d; want the question in the ping's place",
			k.sweepSubstituted.Load(), k.sweepAskFailed.Load(), fd.n())
	}
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	if got := reps[0].Header.Get("anthropic-ratelimit-unified-5h-status"); got != "allowed" {
		t.Errorf("report header status = %q, want allowed", got)
	}
}

// A substitute call whose answer carried no headers (a host can send none) books the ping row as
// before, with no header: nothing is made up and nothing fails.
func TestSubstituteReportWithoutResponseHeaderIsEmpty(t *testing.T) {
	k, fd, sink, clock := substituteKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	kaCandidate(t, "sess-1", st, &fakeSummaryModel{out: "<summary>explored the handler.</summary>"})
	recordBodyWithSink(k, sink, sess1, clock.now(), kaToolBody)
	k.Sweep(clock.advance(281 * time.Second))
	if k.summarySubstituted.Load() != 1 || fd.n() != 0 {
		t.Fatalf("summarySubstituted = %d, ordinary pings = %d", k.summarySubstituted.Load(), fd.n())
	}
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	if reps[0].Header != nil {
		t.Errorf("report header = %v, want nil", reps[0].Header)
	}
}
