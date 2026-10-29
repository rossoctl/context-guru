package counterfactual

// Result is the output of Compute (§6.3).
type Result struct {
	Version int
	Ops     []PricedOp // one for each input operation, same order: the reported ones, then the active ones applied again
	// CacheEffects are the breaks, rescues and breakpoint changes the library found. The library
	// writes them and a host never reports one, so they have no input form (§4.2 rule 4).
	CacheEffects []CacheEffect
	Interaction  TierTokens // §6.2: on a rescued request; attributed to nobody
	Unpriced     []Unpriced // with a machine-stable reason
	Checks       []CheckFailure
	Boundary     *Position  // the B* used; nil when unknown
	Recomputed   bool       // another request replaced the ThreadState first (R53, §6.1)
	Phase        CachePhase // the cache phase it used (§5.2); for the host's own checks, never an input
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

// CacheEffectKind says what a CacheEffect is. The host needs it to settle the effect: a break goes to
// the change that caused it, a rescue goes to the keeper by its window.
type CacheEffectKind string

const (
	EffectBreak      CacheEffectKind = "break"      // a prefix the provider read is written again: a cost
	EffectRescue     CacheEffectKind = "rescue"     // a keep-alive ping kept an entry alive: write becomes read
	EffectSplit      CacheEffectKind = "split"      // cachesplit: the stable half is read, not written
	EffectTTLPremium CacheEffectKind = "ttlPremium" // headttl, cost leg: on every request that writes the head at 1h
	EffectTTLRescue  CacheEffectKind = "ttlRescue"  // headttl, read leg: a request reads the 1h entry after the 5m TTL
)

// CacheEffect is a change in how the provider bills content that is already there (§4.4). It
// changes no content. It is part of the output only: the library finds it by comparing requests
// (§5), so no operator reports one and the input type has no form for it.
type CacheEffect struct {
	// ID is unique inside the Result: "ce:" + a number.
	ID   string          `json:"id"`
	Kind CacheEffectKind `json:"kind"`
	// ChangeID is the change that caused a break, the keep-alive window of a rescue, or the head
	// entry of a TTL leg. Empty on a break that nobody caused (R31).
	ChangeID string `json:"changeId,omitempty"`
	// From and To are the ends of the prefix range the effect covers. A nil From is the start of the
	// prompt; a nil To is the end of the body. For a break, From is the end of what the provider
	// still read and To is B* (§6.2).
	From *Position `json:"from,omitempty"`
	To   *Position `json:"to,omitempty"`
	// TTL is the lifetime of the entry the effect is about, "5m" or "1h". Set on the TTL kinds.
	TTL string `json:"ttl,omitempty"`
	// Cause says why a break nobody reported happened, when the library can tell. Empty when it
	// cannot: an entry lost early has no visible reason. It changes no figure.
	Cause     CacheEffectCause `json:"cause,omitempty"`
	Saved     TierTokens       `json:"saved"`
	Caused    TierTokens       `json:"caused"`
	Estimated bool             `json:"estimated,omitempty"`
}

// CacheEffectCause is the reason the library can give for a break that no operation reported.
type CacheEffectCause string

// CauseModelChanged: this request names a different model from the thread's previous one, and
// the provider keeps a separate cache for each model, so nothing was read. Nobody reported the
// change, so the break has no ChangeID (R50 says the library does not price the change itself).
const CauseModelChanged CacheEffectCause = "model_changed"
