package campaign

import (
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/kvcache"
)

// PriceFunc is a host's rate lookup: a model's rates at a prompt size, in $ per token. A request above a
// price line reads the rates of its tier (modelinfo.Price.At). ok is false for a model with no price.
type PriceFunc func(model string, promptTokens int64) (pIn, pOut, pRead, pWrite float64, ok bool)

// DecisionPrices is what one decision point is worth, in gateway dollars. The replay (Rows) and the
// live campaign (the proxy) build it with PriceDecision, so both decide on the same economics.
type DecisionPrices struct {
	// PIn, POut, PRead and PWrite are the rates at the request's own prompt size.
	PIn, POut, PRead, PWrite float64
	// Stake is one avoided rewrite: the prefix at the 5-minute write rate minus the read rate, at the
	// request's own rates. It is in the calibrated units of the replay (BenefitScale), so it stays at
	// the 5-minute rate on a 1-hour thread too.
	Stake float64
	// Ping is one ping, at the rates of the ping's prompt size.
	Ping float64
	// Miss is the miss that the value rule weighs (keepalive.MissUSD): the same figure at the ping's
	// prompt size, at the write rate of the entry (5 minutes, or 1 hour when oneHour). 0 means unknown,
	// and the rule then fails open.
	Miss float64
}

// PriceDecision prices one decision point: a request of promptTokens prompt tokens that left a cached
// prefix of prefix tokens. oneHour says that the entry was billed at the 1-hour tier. ok is false when the
// model has no price. The caller applies its own gate on the rates (the replay drops a row whose write
// rate is not above its read rate; the live campaign tests Priced).
//
// The ping reads the prefix plus its own input (keepalive.PingPrompt), so it is priced at that size: a
// prefix just below a price line sends a ping above it. When the ping's size has no price, the
// request's own rates stand in. The accountant owns each formula (PingCost, MissCost).
func PriceDecision(price PriceFunc, model string, prefix, promptTokens int64, oneHour bool) (DecisionPrices, bool) {
	pIn, pOut, pRead, pWrite, ok := price(model, promptTokens)
	if !ok {
		return DecisionPrices{}, false
	}
	row := rates(pIn, pOut, pRead, pWrite)
	ping := row
	size := keepalive.PingPrompt(prefix)
	if in, out, read, write, ok := price(model, size); ok {
		ping = rates(in, out, read, write)
	}
	d := DecisionPrices{PIn: pIn, POut: pOut, PRead: pRead, PWrite: pWrite, Stake: Stake(prefix, pRead, pWrite)}
	d.Ping, _ = accountant.PingCost(accountant.PingInput{Model: model, Prefix: prefix, FreshInput: kvcache.DefaultPingInputTokens, Output: 1}, ping.Pricer())
	d.Miss, _ = accountant.MissCost(accountant.MissInput{Model: model, Prefix: prefix, OneHour: oneHour}, ping.Pricer())
	return d, true
}

// rates is the price of one set of per-token rates. The 1-hour write rate is derived by the model
// rules (modelinfo.Price.CacheWrite1h), as for every other price.
func rates(in, out, read, write float64) modelinfo.Price {
	return modelinfo.Price{Input: in, Output: out, CacheRead: read, CacheWrite: write}
}
