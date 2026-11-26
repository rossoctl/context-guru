package accrec

import (
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/tooldefer"
)

// DeferSpendOps turns the search-turn costs of one deferred request into accountant spend operations on the
// tooldefer operator (#1441). Each is a Call with its own usage:
//
//   - defer-search-output: the search calls' output tokens;
//   - defer-search-second-pass: the API's second pass reads the loaded definitions once without the cache (input);
//   - defer-search-next-write: the next request writes the loaded definitions to the cache once (cache write).
//
// The library prices each Call at its own usage and size. The second and third are estimates. The first is an estimate too when usage did not give the output. A cost of 0
// tokens makes no operation. The ChangeID is empty: a spend is booked once, on the request that paid it.
func DeferSpendOps(model string, s tooldefer.SearchSpend) []accountant.Operation {
	op := func(id string, u accountant.CallUsage, estimated bool) accountant.Operation {
		params := map[string]string{}
		if estimated {
			params[accwire.SpendParamEstimated] = "true"
		}
		return accountant.Operation{ID: id, Kind: accountant.KindSpend, Operator: accwire.Operator(accwire.OpToolDefer),
			Call: &accountant.ModelCall{Model: model, Params: params, Usage: u}}
	}
	var ops []accountant.Operation
	if s.Output > 0 {
		ops = append(ops, op("defer-search-output", accountant.CallUsage{Output: int(s.Output)}, s.OutputEstimated))
	}
	if s.Loaded > 0 {
		ops = append(ops, op("defer-search-second-pass", accountant.CallUsage{Usage: accountant.Usage{Input: int(s.Loaded)}}, true))
		w := accountant.Usage{CacheWrite: int(s.Loaded)}
		if s.Write1h {
			w.CacheWrite1h = int(s.Loaded)
		}
		ops = append(ops, op("defer-search-next-write", accountant.CallUsage{Usage: w}, true))
	}
	return ops
}

// DeferMoney maps the accountant's figure for tool deferral to the ledger's three numbers (#1441). The figure is
// ByComponent at the tooldefer operator. The library books a spend call as a separate bill, so the search turns are
// its spend lines on that operator, and they come off the net. Gross is the saved side (first plus replay). Cost is
// gross less net. No pricing here: it only reads the library's figure.
func DeferMoney(f accountant.USDFigure) tooldefer.Money {
	op := accwire.Operator(accwire.OpToolDefer)
	var spend float64
	for _, l := range f.Lines {
		if l.Kind == accountant.KindSpend && l.Operator == op && l.Direction == accountant.DirAdded {
			spend += l.USD
		}
	}
	net := f.ByComponent[op] - spend
	gross := f.ByComponentFirst[op] + f.ByComponentReplay[op]
	return tooldefer.Money{Gross: gross, Cost: gross - net, Net: net}
}

// FigureWithheld says that a request books no accountant figure in dollars: an observe run, a trace that dropped
// operations, or a result that fails a gate of accountant.Withhold (the size gate is the dashboard's).
func FigureWithheld(res accountant.Result, observe bool, opsDropped int) bool {
	return observe || opsDropped != 0 || accountant.Withhold(res, accountant.SizeNotGiven) != ""
}

// Held says that the library could not rule out a break (penalty_unknown): the thread had no history. The dashboard
// books no saving for such a request (issue #915), the deferral's figure with the rest, so the /stats ledger books
// none either: one figure on every surface (#1441).
func Held(res accountant.Result) bool {
	for _, u := range res.Unpriced {
		if u.Reason == accountant.ReasonPenaltyUnknown {
			return true
		}
	}
	return false
}
