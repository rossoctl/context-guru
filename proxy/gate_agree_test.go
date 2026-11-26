package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// #1519: the dashboard row, the /stats ledger and the metrics push gate one request with one rule. The size bound is the
// library's (accountant.Withhold with no size). The Result is built by hand: the library measures the client messages
// into MessageTokens and each operation's SavedMessages, and a real request cannot be made to save more than it had.
func TestRowStatsAndPushAgreeOnTheSizeBound(t *testing.T) {
	op := func(savedMessages int) accountant.PricedOp {
		return accountant.PricedOp{Operator: "context-guru.mask", OpID: "o1", ChangeID: "c1", Kind: accountant.KindRewrite,
			Saved:         accountant.TierTokens{CacheRead: 1000},
			SavedMessages: accountant.TierTokens{CacheRead: savedMessages}}
	}
	for name, tc := range map[string]struct {
		res  accountant.Result
		want string // the withheld reason; "" books
	}{
		"implausible": {accountant.Result{Ops: []accountant.PricedOp{op(1000)}, MessageTokens: 100}, accountant.WithheldImplausibleSize},
		"healthy":     {accountant.Result{Ops: []accountant.PricedOp{op(1000)}, MessageTokens: 5000}, ""},
		// Tool declarations are in no SavedMessages (#907): a deferral or a filter saving is not bounded by the messages.
		"tool section": {accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.toolfilter", OpID: "o1",
			ChangeID: "c1", Kind: accountant.KindRewrite, Saved: accountant.TierTokens{CacheRead: 2188}}}, MessageTokens: 121}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			// The dashboard row.
			// The mask cases have a mask row that acted. The toolfilter operator is an envelope one: no row.
			rows, run := []dash.CompRow{{Component: "mask", Acted: true}}, []components.Report{{Component: "mask", TokensBefore: 2000, TokensAfter: 1000}}
			if name == "tool section" {
				rows, run = nil, nil
			}
			e := &dash.Event{Model: "claude-sonnet-5-5", FreshInput: 100, CacheRead: 1000, OutputTokens: 10, Components: rows}
			e.SetAccountant(tc.res)
			e.PriceMode(modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}, true)
			if got := e.AccountantFallback(); got != tc.want {
				t.Errorf("row fallback %q, want %q", got, tc.want)
			}
			// The /stats ledger.
			c := &capture{model: "claude-sonnet-5-5", pricer: tdPrices{}, accRes: &tc.res}
			c.trace.Run = &components.RunReport{Components: run}
			if _, ok := c.accFigure(context.Background(), Usage{CacheRead: 1000}, true); ok != (tc.want == "") {
				t.Errorf("/stats books %v, want %v", ok, tc.want == "")
			}
			// The metrics push.
			r := sdk.NewManualReader()
			s, err := acmetrics.New(sdk.NewMeterProvider(sdk.WithReader(r)).Meter("test"), metricsPricer{prices: tdPrices{}})
			if err != nil {
				t.Fatal(err)
			}
			s.Observe(acmetrics.Observation{Meta: accountant.RequestMeta{Model: "claude-sonnet-5-5", At: time.Unix(1700000000, 0),
				Billing: accountant.Billing{Mode: accountant.BillingAPI}}, Usage: accountant.Usage{CacheRead: 1000}, Result: tc.res})
			var rm metricdata.ResourceMetrics
			if err := r.Collect(context.Background(), &rm); err != nil {
				t.Fatal(err)
			}
			got := ""
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if d, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "llm.savings.withheld" {
						for _, dp := range d.DataPoints {
							v, _ := dp.Attributes.Value("reason")
							got = v.AsString()
						}
					}
				}
			}
			if got != tc.want {
				t.Errorf("push withheld %q, want %q", got, tc.want)
			}
		})
	}
}
