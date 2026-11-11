package dash

// Crediting the declarations that STOPPED being sent.
//
// This is the one saving on this dashboard that had nowhere to appear. Everything else here is
// measured as content a COMPONENT removed from a message, and the whole savings walk is built on
// `tokens_before − tokens_after`, which is `schema.MessagesTokens`: message text only. A tool
// declaration is not in `messages`. So when the declaration filter stops sending 4,000 tokens of
// MCP schemas on every request, `tokens_before` does not move, `baseline_cost_usd` does not move,
// and the reduction is absent from NetSavedUSD, from TotalSavedUSD and from the waterfall. It was
// visible on the Inventory tab and in no total anywhere. That is the bug this file fixes, and it
// hid the LARGEST measured lever in the product behind the smallest surface.
//
// The credit is MEASURED and OURS: `requests.filtered_decl_tokens`, written by the filter itself
// on every request it acted on. Those requests were really sent, really smaller, and smaller
// because of a component of ours. It goes into TotalSavedUSD, which is where our savings live.
//
// What an account stops declaring on its own is not credited anywhere. No component ran, and the
// only evidence is that the inventory is a time series with a name in the early part and not the
// late part, which cannot tell a removal from a session that ran in another environment. That
// attribution is a guess and the reduction is not ours (#583, #584).

import "github.com/rossoctl/context-guru/internal/modelinfo"

// DeclCredit is the reduction from declarations the filter stopped sending.
type DeclCredit struct {
	// The MEASURED half: what the declaration filter actually stopped sending.
	FilterReads    int64   `json:"filter_reads"`
	FilterUSD      float64 `json:"filter_usd"`
	FilterRequests int     `json:"filter_requests"`
	FilterSince    int64   `json:"filter_since,omitempty"`
	// Priced is false when any contributing model had no rates, in which case the dollar
	// figures are the priced subset only and a consumer must show the token counts instead.
	Priced bool `json:"priced"`
}

// DeclCreditFor totals the filter's saving over one scope.
func (d *DB) DeclCreditFor(f Filter, price func(string) (modelinfo.Price, bool)) (*DeclCredit, error) {
	out := &DeclCredit{Priced: price != nil}
	// No rates: the token counts still stand and the dollars do not exist. Reported as unpriced
	// rather than as zero, which is this dashboard's rule everywhere.
	realized, err := d.DeclFilterSavings(f, price)
	if err != nil {
		return nil, err
	}
	out.FilterReads, out.FilterUSD = realized.Reads, realized.USD
	out.FilterRequests, out.FilterSince = realized.Requests, realized.Since
	if !realized.Priced {
		out.Priced = false
	}
	return out, nil
}
