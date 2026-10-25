package counterfactual

// Result is the output of Compute (§6.3).
type Result struct {
	Version     int
	Ops         []PricedOp // one for each input operation, same order, then the cacheChanges the library added
	Interaction TierTokens // §6.2: on a rescued request; attributed to nobody
	Unpriced    []Unpriced // with a machine-stable reason
	Checks      []CheckFailure
	Boundary    *Position  // the B* used; nil when unknown
	Recomputed  bool       // another request replaced the ThreadState first (R53, §6.1)
	Phase       CachePhase // the cache phase it used (§5.2); for the host's own checks, never an input
}

// Unpriced names an operation the library did not price and why.
type Unpriced struct {
	OpID   string
	Reason string // "unknown_kind", "negative_size", "operator_not_allowed", "penalty_unknown", ...
}

// CheckFailure is a failed check (§6.5). A failure does not stop pricing.
type CheckFailure struct {
	Check    string // "sum" | "order" | "unique_change" | "spans" | "attribution" | "conservation"
	Operator string // "" when the check is not about one operator
	Detail   string // numbers only, no content
}

// PricedOp is one operation in tokens for each tier.
type PricedOp struct {
	OpID      string
	Operator  string
	ChangeID  string
	DryRun    bool
	Saved     TierTokens // tokens the provider did not bill, by tier; never negative
	Caused    TierTokens // tokens the provider billed because of this operation; never negative
	Call      *ModelCall // spend only: copied from the operation, for the host to price
	Estimated bool
}

// TierTokens is a token count for each billing tier.
type TierTokens struct {
	Input        int `json:"input,omitempty"`
	CacheWrite5m int `json:"cacheWrite5m,omitempty"`
	CacheWrite1h int `json:"cacheWrite1h,omitempty"`
	CacheRead    int `json:"cacheRead,omitempty"`
}
