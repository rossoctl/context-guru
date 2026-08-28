package apply

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// A COMPONENT WHOSE COUNTERS ARE ALL EVENTS MUST STILL LOG THEM.
//
// Splitting Report.Gates into Gates and Events (#121) silently blinded this line: it rendered only
// Gates, so a component that records successes rather than refusals logged no counter information at
// all. Observed live on the worst possible turn — the one that adjudicated twelve outputs, removed
// twelve and saved 33,340 tokens logged `verdict=acted saved=33340` and nothing else, because every
// name it raised was an event. That is the diagnosis this line exists to provide, absent exactly when
// the component worked.
//
// The fixture is deliberately events-ONLY. A report carrying both would pass even with the events
// branch removed, since the gates field would still appear.
func TestDecisionLogCarriesEventsNotOnlyGates(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var eventsOnly components.Report
	eventsOnly.Component, eventsOnly.Kind = "extract_llm_sweep", "offload"
	eventsOnly.TokensBefore, eventsOnly.TokensAfter = 34317, 977
	eventsOnly.EventN("sweep_offered", 12)
	eventsOnly.EventN("sweep_dropped", 12)
	eventsOnly.Event("sweep_prefix_cache_read_ok")

	logDecisions(lg, &components.RunReport{Components: []components.Report{eventsOnly}})
	out := buf.String()

	// Precondition: the line was emitted at all, or the assertions below pass on an empty buffer.
	if !strings.Contains(out, "extract_llm_sweep") {
		t.Fatalf("no component line was logged, so the assertion is vacuous: %q", out)
	}
	for _, want := range []string{"sweep_offered=12", "sweep_dropped=12", "sweep_prefix_cache_read_ok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the decision line does not carry %q — a component that records only "+
				"successes logs no counters, which is the diagnosis this line exists for: %s",
				want, out)
		}
	}
	// Gates and events must be distinguishable in the output, not merged into one field: they answer
	// opposite questions and a reader cannot tell a refusal from a success otherwise.
	var both components.Report
	both.Component, both.Kind = "extract_llm", "offload"
	both.GateN("below_output_floor", 11)
	both.Event("reapplied_same_session")
	buf.Reset()
	logDecisions(lg, &components.RunReport{Components: []components.Report{both}})
	line := buf.String()
	if !strings.Contains(line, "gates=") || !strings.Contains(line, "events=") {
		t.Errorf("declines and successes must appear as separate fields; got %s", line)
	}
}
