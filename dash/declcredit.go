package dash

// Crediting the declarations that STOPPED being sent — and keeping two very different claims
// about them apart.
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
// # The two halves are not the same kind of claim, and are never added together
//
// FILTER — measured, and ours. `requests.filtered_decl_tokens`, written by the filter itself on
// every request it acted on. Those requests were really sent, really smaller, and smaller because
// of a component of ours. It goes into TotalSavedUSD, which is where our savings live.
//
// SELF — modelled, and the USER's. The account simply stopped declaring something: no component
// ran, no filter fired, and the only evidence is that the inventory is a time series with the name
// in the early part and not the late part. The reduction is real — the tokens genuinely were not
// billed — but it is NOT ours, and TotalSavedUSD says in as many words that its three addends are
// all ours. So this half is reported beside that total and added into a SECOND one,
// TotalReducedUSD, which is "everything that came off this bill, including what you did
// yourself". Two totals rather than one, because collapsing them would either overstate what the
// product did or refuse the user the number they asked for, and both were on offer.
//
// # Why the modelled half is not merely a weaker version of the measured one
//
// Measured on the production snapshot, the largest self-removal candidate in the window is
// `mcp__ide__executeCode` and `mcp__ide__getDiagnostics` — 224 tokens between them, last declared
// 2026-08-20 11:40, absent from the 14 MCP-bearing sessions that followed. The account did not
// remove those. They are the editor integration, and they are declared only when the session runs
// INSIDE the IDE. Nothing in the data distinguishes "I ran `claude mcp remove`" from "I worked in
// a terminal today", and no threshold fixes that, because the inference is not wrong about the
// facts — the declaration set really did change, and the tokens really were not sent — it is wrong
// only about WHO caused it.
//
// That is exactly why the halves are separated instead of being tuned. The measured half's
// attribution is a fact (a component of ours rewrote the body); the modelled half's is a guess,
// so it is labelled as one everywhere it appears and it is kept out of the figure that claims
// credit. Overlap is subtracted before it is even summed: a name on the account's own filter list
// is already counted in the measured half, and the two describe the same tokens.

import "github.com/rossoctl/context-guru/internal/modelinfo"

// DeclCredit is the reduction from declarations no longer sent, split by whose it is.
type DeclCredit struct {
	// The MEASURED half: what the declaration filter actually stopped sending.
	FilterReads    int64   `json:"filter_reads"`
	FilterUSD      float64 `json:"filter_usd"`
	FilterRequests int     `json:"filter_requests"`
	FilterSince    int64   `json:"filter_since,omitempty"`
	// The MODELLED half: what the account stopped declaring on its own. Items is how many
	// names, Overlap how many were dropped because the filter is already credited for them.
	SelfReads   int64   `json:"self_reads"`
	SelfUSD     float64 `json:"self_usd"`
	SelfItems   int     `json:"self_items"`
	SelfOverlap int     `json:"self_overlap"`
	// Priced is false when any contributing model had no rates, in which case the dollar
	// figures are the priced subset only and a consumer must show the token counts instead.
	Priced bool `json:"priced"`
}

// DeclCreditFor totals both halves over one scope.
//
// removed is the account's own server-side removal list, VERBATIM as the configuration stores it,
// used ONLY to drop overlapping rows out of the modelled half. It is passed in rather than read
// here for the reason dash reads no tenant configuration anywhere: the host owns it (see
// API.SetToolFilterState). Raw rather than pre-keyed, because the config's vocabulary and the
// report's differ and doing that translation per caller is what made the exclusion silently
// inert — see declFilteredSet.
func (d *DB) DeclCreditFor(f Filter, price func(string) (modelinfo.Price, bool),
	removed []string) (*DeclCredit, error) {
	out := &DeclCredit{Priced: true}
	if price == nil {
		// No rates: the token counts still stand and the dollars do not exist. Reported as
		// unpriced rather than as zero, which is this dashboard's rule everywhere.
		out.Priced = false
	}
	realized, err := d.DeclFilterSavings(f, price)
	if err != nil {
		return nil, err
	}
	out.FilterReads, out.FilterUSD = realized.Reads, realized.USD
	out.FilterRequests, out.FilterSince = realized.Requests, realized.Since
	if !realized.Priced {
		out.Priced = false
	}
	if price == nil {
		// SelfRemovals calls price() per session with no nil guard of its own, so this return is
		// load-bearing rather than an optimisation. DeclFilterSavings above checks for itself,
		// which is why the filter half is still computed here.
		return out, nil
	}
	self, err := d.SelfRemovals(f, price, removed)
	if err != nil {
		return nil, err
	}
	for _, r := range self {
		if r.Overlap {
			// The same tokens are in the measured half. Counted as overlap so the page can say
			// how many rows it declined to add, rather than the difference being invisible.
			out.SelfOverlap++
			continue
		}
		out.SelfItems++
		out.SelfReads += r.AvoidedReads
		out.SelfUSD += r.AvoidedUSD
		if !r.Priced {
			out.Priced = false
		}
	}
	return out, nil
}
