package apply_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
)

// SWEEP V3 dry_run, end to end through the real request path. A dry run decides every cut and applies none, so
// the request that goes upstream must equal the request of a pipeline whose sweep drops nothing.

const dryBase = "pipeline: [extract_llm_sweep]\ncomponents:\n  extract_llm_sweep:\n    min_tokens: 200\n"

// dryOpts is the shared request shape: the same body for both arms.
func dryOpts(session string, body []byte, now time.Time, asker *dropAsker, tr *modes.Tracker) apply.Opts {
	return apply.Opts{Provider: bschemas.Anthropic, Body: body, Session: session, CacheMode: "on", Tracker: tr, Now: now,
		PrefixAsk: asker, CanKeepAlive: func(_, _ string) bool { return true }}
}

// runCold plays the cold-apply scenario of the #752 test (turn 1 registers, a ping holds the answer, turn 2 is
// past the TTL) with the given pipeline, and returns the two forwarded bodies. With sweep false the pipeline has
// no sweep: there is no ping to hold, and the two turns just go through.
func runCold(t *testing.T, session, sweepCfg string, sweep bool) (body1, body2 []byte, st store.Store) {
	t.Helper()
	cfg := pipe(t, sweepCfg+"mode: sync\n")
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st = store.NewMemory(store.Options{})
	tr := modes.NewTracker(0)
	asker := &dropAsker{}
	base := time.Unix(1_700_000_000, 0)
	ends := []string{"\n", "\n\n", "\r\n"}
	res1 := apply.BodyOpts(context.Background(), p, st, dryOpts(session, sweepNewlineBody(ends, false), base, asker, tr))
	if sweep {
		dispatch, _, reason, ok := offload.SweepPingSubstitute(res1.Session)
		if !ok {
			t.Fatalf("no sweep question offered (reason %q)", reason)
		}
		if held := dispatch(10*time.Second, offload.SweepPingWait{WaitingFor: "human"}); !held.Held {
			t.Fatalf("the ping held nothing: %+v", held)
		}
	}
	res2 := apply.BodyOpts(context.Background(), p, st, dryOpts(session, sweepNewlineBody(ends, true), base.Add(7*time.Minute), asker, tr))
	return res1.Body, res2.Body, st
}

// ⭐ A cold apply: with dry_run the forwarded body is the body of a run that dropped nothing, and with the real v3
// it is not (the control: the same scenario really drops).
func TestDryRunColdApplyForwardsTheRequestUntouched(t *testing.T) {
	offload.ResetV3ForTest()
	cfg := func(extra string) string {
		return dryBase + "    warm: true\n    selector: judge\n    protect_last_n: 0\n" + extra
	}
	// The arm with no drops: the sweep sees the same ping and the same answer but is told never to apply. A
	// pipeline with the sweep absent would also do; this keeps every other component identical.
	_, noSweep2, _ := runCold(t, "dry-nosweep", "pipeline: []\n", false)
	_, real2, realSt := runCold(t, "dry-real", cfg(""), true)
	_, dry2, dryStore := runCold(t, "dry-dry", cfg("    dry_run: true\n"), true)

	// The marker is written with its angle brackets JSON-escaped on the wire, so look for the id part.
	if bytes.Equal(real2, noSweep2) || !strings.Contains(string(real2), "cg:") {
		t.Fatalf("control: the real v3 run must change the request (it drops outputs)\n real: %.200s\n none: %.200s", real2, noSweep2)
	}
	if !bytes.Equal(dry2, noSweep2) {
		t.Fatalf("the dry run changed the forwarded request.\n dry: %.300s\nnone: %.300s", dry2, noSweep2)
	}
	if strings.Contains(string(dry2), "cg:") {
		t.Fatal("the dry run left a marker in the request")
	}
	// Nothing was recorded: the thread's drop index is empty for the dry run and not for the real run.
	if n := offload.DropRecordsForTest(dryStore, sessionThreadKey("dry-dry")); n != 0 {
		t.Fatalf("the dry run wrote %d drop records", n)
	}
	if n := offload.DropRecordsForTest(realSt, sessionThreadKey("dry-real")); n == 0 {
		t.Fatal("control: the real run wrote no drop record")
	}
	if got := offload.V3DryRunCutsForTest(); got < 1 {
		t.Fatalf("v3.dry_run_cuts = %d, want the would-be cut counted", got)
	}
}

func sessionThreadKey(session string) string { return thread.Key(session, "") }

// The first request of a dry run is also unchanged: the tool injection of the proxy is not in this path, and the
// pipeline itself changes nothing on turn 1 with or without dry_run.
func TestDryRunFirstRequestMatchesARealRun(t *testing.T) {
	offload.ResetV3ForTest()
	cfg := dryBase + "    warm: true\n    selector: judge\n    protect_last_n: 0\n"
	b1real, _, _ := runCold(t, "dry-first-real", cfg, true)
	b1dry, _, _ := runCold(t, "dry-first-dry", cfg+"    dry_run: true\n", true)
	if !bytes.Equal(b1real, b1dry) {
		t.Fatalf("turn 1 differs between a real and a dry run:\n%.200s\n%.200s", b1real, b1dry)
	}
}
