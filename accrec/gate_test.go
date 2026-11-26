package accrec

import (
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
)

// The gate is one decision for the dashboard row and the /stats ledger. Each gate names its reason.
func TestGate(t *testing.T) {
	mask := accwire.Operator("mask")
	op := accountant.PricedOp{Operator: mask, ChangeID: "c", Saved: accountant.TierTokens{CacheRead: 100}}
	res := accountant.Result{Ops: []accountant.PricedOp{op}}
	fig := accountant.USDFigure{ByComponent: map[string]float64{mask: 1}, ByComponentFirst: map[string]float64{}, ByComponentReplay: map[string]float64{}}
	rows := []GateRow{{Name: "mask", Acted: true}}
	ok := GateIn{TokensBefore: 1000, Rows: rows}
	if r, _ := Gate(res, fig, ok); r != "" {
		t.Fatalf("a clean request is gated: %q", r)
	}
	for name, tc := range map[string]struct {
		res  accountant.Result
		fig  accountant.USDFigure
		in   GateIn
		want string
	}{
		"ops dropped": {res, fig, GateIn{OpsDropped: 1, TokensBefore: 1000, Rows: rows}, GateOpsDropped},
		"observe":     {res, fig, GateIn{Observe: true, TokensBefore: 1000, Rows: rows}, GateObserve},
		"size":        {res, fig, GateIn{Rows: rows}, GateSizeUnknown},
		"implausible": {res, fig, GateIn{TokensBefore: 10, Rows: rows}, GateImplausibleSize},
		"no row":      {res, fig, GateIn{TokensBefore: 1000}, GateFigureWithoutRow},
		"duplicate":   {res, fig, GateIn{TokensBefore: 1000, Rows: append(rows[:1:1], rows[0])}, GateDuplicateName},
		"acted":       {res, accountant.USDFigure{ByComponent: map[string]float64{}}, GateIn{TokensBefore: 1000, Rows: rows}, GateActedWithoutFigure},
		"unpriced":    {res, accountant.USDFigure{ByComponent: fig.ByComponent, Unpriced: []string{"o1:no_price"}}, ok, GateUnpriced},
		"fatal check": {accountant.Result{Ops: res.Ops, Checks: []accountant.CheckFailure{{Check: accountant.CheckSum}}}, fig, ok, GateCheckSum},
	} {
		if r, _ := Gate(tc.res, tc.fig, tc.in); r != tc.want {
			t.Errorf("%s: reason %q, want %q", name, r, tc.want)
		}
	}
	// penalty_unknown does not gate: the figure is held, not refused.
	held := accountant.USDFigure{ByComponent: fig.ByComponent, Unpriced: []string{"o1:" + accountant.ReasonPenaltyUnknown}}
	if r, _ := Gate(res, held, ok); r != "" {
		t.Errorf("penalty_unknown gated: %q", r)
	}
	// A fatal check this host has no name for still gates.
	if got := fatalCheckReason("some_future_check"); got != GateCheckOther {
		t.Errorf("unnamed fatal check = %q", got)
	}
}
