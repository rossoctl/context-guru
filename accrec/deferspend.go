package accrec

import (
	"strconv"

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
// The second and third are estimates. The first is an estimate too when usage did not give the output. A cost of 0
// tokens makes no operation. The ChangeID is empty: a spend is booked once, on the request that paid it.
func DeferSpendOps(model string, s tooldefer.SearchSpend) []accountant.Operation {
	op := func(id string, u accountant.CallUsage, estimated bool) accountant.Operation {
		params := map[string]string{}
		if s.PromptTokens > 0 {
			params[accwire.SpendParamPromptTokens] = strconv.FormatInt(s.PromptTokens, 10)
		}
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
