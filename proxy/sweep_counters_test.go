package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/metrics"
)

// sweepGoldenCounters is the cold-sweep adjudicator's counter contract, and it is a contract for the
// same reason statsGoldenTopLevel is one: these names are what an operator's alert rule and dashboard
// query are written against, so a rename breaks monitoring silently rather than loudly.
//
// The two marked ALERTABLE are the point of the list. The rest describe volume; those two describe
// the model misbehaving, and each names a distinct misbehaviour:
//
//   - sweep_drop_refused_obligation: the model tried to remove an output it had, in the same reply,
//     just said was still needed. The removal did not happen — that is the invariant — but a
//     non-zero rate means the contract is not holding and the only thing standing between the
//     workload and silent context loss is our refusal.
//   - sweep_quote_fabricated: the model cited transcript text that is not in the transcript. It
//     argues for KEEPING, so it is not dangerous, but on this design it is the ONLY remaining signal
//     that the model is inventing, because nothing else it returns is content. If the verdicts are
//     to be trusted at all, this is the number that says whether they can be.
var sweepGoldenCounters = []string{
	"sweep_adjudicated",
	"sweep_criterion_missing",
	"sweep_dropped",
	"sweep_drop_refused_obligation", // ALERTABLE
	"sweep_kept",
	"sweep_quote_fabricated", // ALERTABLE
}

// The counters must reach BOTH surfaces, because they answer different questions for different
// consumers: /stats is what the benchmark harness parses, /metrics is what an alert rule fires on.
// A counter that exists in the component and reaches neither is a log line.
func TestSweepCountersReachStatsAndMetrics(t *testing.T) {
	gates := map[string]int{}
	for i, name := range sweepGoldenCounters {
		gates[name] = i + 1 // distinct values, so a mixed-up mapping is visible
	}
	agg := metrics.NewAggregator()
	agg.Component(components.Report{
		Component: "extract_llm_sweep", Kind: "offload",
		TokensBefore: 10_000, TokensAfter: 400, Gates: gates,
	})
	h := New(nil, nil, agg, Options{})

	// /stats: under the component's own object, so a multi-component pipeline's counters stay
	// attributable rather than being summed into one pool.
	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))
	var got struct {
		Components map[string]struct {
			Gates map[string]int64 `json:"gates"`
		} `json:"components"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("/stats is not the expected shape: %v\n%s", err, w.Body.String())
	}
	comp, ok := got.Components["extract_llm_sweep"]
	if !ok {
		t.Fatalf("extract_llm_sweep is absent from /stats: %s", w.Body.String())
	}
	// PRECONDITION: the gate histogram survived the rollup at all. Without it every assertion
	// below would fail for one uninformative reason, and a rollup that dropped `gates` entirely
	// would look identical to six renamed counters.
	if len(comp.Gates) == 0 {
		t.Fatal("the component's gate histogram did not reach /stats, so the counter names cannot be checked")
	}
	for i, name := range sweepGoldenCounters {
		if comp.Gates[name] != int64(i+1) {
			t.Errorf("/stats lost or renamed %q (got %d, want %d) — an operator's query breaks silently",
				name, comp.Gates[name], i+1)
		}
	}

	// /metrics: the generic per-gate series, which is what makes the two alertable counters
	// alertable without a bespoke metric each.
	body := h.renderMetrics()
	for i, name := range sweepGoldenCounters {
		want := `cg_component_gate_declines_total{component="extract_llm_sweep",gate="` + name + `"} ` +
			strconv.Itoa(i+1)
		if !strings.Contains(body, want) {
			t.Errorf("missing from /metrics:\n  %s", want)
		}
	}
}

// The other end of this contract is stated in components/offload, where
// TestSweepRaisesTheContractedCounterNames drives the real component and asserts it raises these same
// literal strings. Deliberately spelled out at BOTH ends rather than shared through one exported
// list: a rename that edited a single shared list would keep every test passing while breaking every
// deployed alert rule, which is the failure a contract test exists to prevent.
