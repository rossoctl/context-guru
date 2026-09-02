package proxy

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/metrics"
)

// TestMutatedAndGateDeclinesAreExported.
//
// Live, cachesplit read `runs 353, acted 0, mutated 53` — and /metrics carried only the
// first two, so the component with the largest measured cost effect in the whole pipeline
// rendered as a dead red bar and Prometheus held no series that could say otherwise. The
// gate histogram had the same problem from the other end: `acted: 0` cannot tell a
// misfiring guard from a workload with nothing to do, and the only copy of that answer was
// a single log string that no query can group by.
func TestMutatedAndGateDeclinesAreExported(t *testing.T) {
	agg := metrics.NewAggregator()
	// A cachesplit-shaped run: it changed the request but removed no content tokens, so it
	// is mutated-never-acted.
	agg.Component(components.Report{
		Component: "cachesplit", Kind: "reformat", TokensBefore: 100, TokensAfter: 100,
	})
	// A gated component: it ran, declined every candidate, and says which gate did it.
	// The gate name carries the awkward characters the exposition format has to escape.
	agg.Component(components.Report{
		Component: "toon", Kind: "reformat", TokensBefore: 100, TokensAfter: 100, Skipped: true,
		Gates: map[string]int{`not_uniform"object\array` + "\n": 14675, "below_min_size": 9063},
	})
	h := New(nil, nil, agg, Options{})
	body := h.renderMetrics()

	for _, want := range []string{
		`cg_component_runs_total{component="cachesplit",outcome="ran"} 1`,
		`cg_component_runs_total{component="cachesplit",outcome="acted"} 0`,
		`cg_component_runs_total{component="cachesplit",outcome="mutated"} 1`,
		`cg_component_runs_total{component="cachesplit",outcome="discarded"} 0`,
		`cg_component_gate_declines_total{component="toon",gate="below_min_size"} 9063`,
		// Escaped: backslash, quote and newline all survive as escapes rather than as a
		// broken line that would corrupt every series after it.
		`cg_component_gate_declines_total{component="toon",gate="not_uniform\"object\\array\n"} 14675`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing from /metrics:\n  %s", want)
		}
	}
	// Sorted, so a scrape diff does not reorder every line.
	if a, b := strings.Index(body, `gate="below_min_size"`), strings.Index(body, `gate="not_uniform`); a > b {
		t.Errorf("gate labels are not sorted: below_min_size at %d, not_uniform at %d", a, b)
	}
	// One line per series, and the escaped newline must not have split one.
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "not_uniform") || strings.HasPrefix(ln, `object\array`) {
			t.Errorf("a label value broke out of its line: %q", ln)
		}
	}
}

// TestExtractEconomicsAreExported. NetValueUSD was -$0.7085 live and appeared only in
// /stats, which nothing scrapes and nothing alerts on.
func TestExtractEconomicsAreExported(t *testing.T) {
	metrics.RecordExtractionSuppressed("extract_llm", "gate:not_worth_it")
	metrics.RecordExtractionCacheLookup("extract_llm", true)
	h := New(nil, nil, metrics.NewAggregator(), Options{})
	body := h.renderMetrics()
	for _, want := range []string{
		`cg_extract_calls_total{outcome="made"}`,
		`cg_extract_calls_total{outcome="avoided"} 1`,
		`cg_extract_calls_total{outcome="suppressed"} 1`,
		"cg_extract_cost_usd ",
		"cg_extract_net_value_usd ",
		"cg_extract_latency_ms ",
		`cg_extract_gate_declines_total{reason="gate:not_worth_it"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing from /metrics:\n  %s", want)
		}
	}
	// A negative net value must be exported as a negative number, not clamped or dropped:
	// the whole point is that an underwater component looks underwater.
	if strings.Contains(body, "cg_extract_net_value_usd NaN") {
		t.Error("cg_extract_net_value_usd is NaN")
	}
}

// TestMonthToDateFamiliesAreGauges. These four are re-read for the current calendar month
// and SHRINK when rows migrate to cold storage, so a counter TYPE makes every rate() over
// them extrapolate a spike at the moment the value fell.
func TestMonthToDateFamiliesAreGauges(t *testing.T) {
	h := New(nil, nil, metrics.NewAggregator(), Options{
		TenantMetrics: fakeTenantMetrics{{TenantID: "t1", Label: "acme", Requests: 3}},
	})
	body := h.renderMetrics()
	for _, name := range []string{
		"cg_tenant_requests_total", "cg_tenant_tokens_total",
		"cg_tenant_saved_tokens_unique_total", "cg_tenant_billed_tokens_total",
	} {
		if !strings.Contains(body, "# TYPE "+name+" gauge") {
			t.Errorf("%s is not declared a gauge; rate() over it will spike at the month "+
				"boundary and at every archival migration", name)
		}
		if help := helpLine(body, name); !strings.Contains(help, "rate()") {
			t.Errorf("HELP for %s does not warn against rate():\n  %s", name, help)
		}
	}
}

type fakeTenantMetrics []TenantMetricRow

func (f fakeTenantMetrics) TenantMetrics(int64) ([]TenantMetricRow, error) { return f, nil }
