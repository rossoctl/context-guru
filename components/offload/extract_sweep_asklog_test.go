package offload

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/store"
)

// The ask log line must carry a JOIN KEY. Iteration 022 measured 40% of every prefix ask charged fresh
// and could not explain it, because cg.sweep.ask carried no session and no cache boundary -- the one
// record holding the ask's economics could not be tied to the request that produced it. That left the
// benign explanation (the client's last cache_control breakpoint sits before the end of the body, so the
// tail past it is uncached and our appended question pays for it) indistinguishable from the alarming one
// (the prefix we send no longer matches what the provider cached).
//
// Asserted on the RENDERED line, not by reading the call site: a field that is passed but dropped by the
// handler is exactly the failure worth guarding.
func TestSweepAskLogCarriesAJoinKey(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	asker.cacheRead = 19595
	e := newSweep(t, "evidence: true\n")

	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := preExpiryCtx("session-xyz", asker, store.NewMemory(store.Options{}))
	c.Ctx = logging.With(context.Background(), lg)

	rep := &components.Report{}
	if _, err := e.Offload(sweepReqCoref(), rep, c); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	if !strings.Contains(line, "cg.sweep.ask") {
		t.Fatalf("no ask was logged, so there is nothing to join:\n%s", firstLines(line, 20))
	}
	for _, want := range []string{"session=session-xyz", "max_cached_idx", "req_tokens", "messages"} {
		if !strings.Contains(line, want) {
			t.Errorf("cg.sweep.ask is missing %q, so a run's asks cannot be attributed", want)
		}
	}
	// The economics must survive: the point is to ADD the join key, not to displace them.
	for _, want := range []string{"cache_read", "fresh", "offered", "verdicts"} {
		if !strings.Contains(line, want) {
			t.Errorf("cg.sweep.ask lost %q", want)
		}
	}
}
