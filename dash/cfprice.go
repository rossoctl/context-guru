package dash

import (
	"strings"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// oneHourWriteInputMultiple is the 1h cache-write premium: such a token bills at 2x the input
// rate. It is the rule of modelinfo.Price.CostWithCacheWrite1h (which adds 2*Input - CacheWrite
// per 1h token on top of the 5m write rate); keep the two in step.
const oneHourWriteInputMultiple = 2

// tierUSD prices tokens by tier.
func tierUSD(p modelinfo.Price, t cf.TierTokens) float64 {
	return float64(t.Input)*p.Input + float64(t.CacheRead)*p.CacheRead +
		float64(t.CacheWrite5m)*p.CacheWrite + float64(t.CacheWrite1h)*oneHourWriteInputMultiple*p.Input
}

// CFFigure is the request's savings in dollars, from the library's tokens. Only dollars are
// summed; token tiers are never collapsed into one total.
type CFFigure struct {
	ByComponent map[string]float64 // net of the break each component caused
	// DeltaUSD is the sum of ByComponent: net of the ATTRIBUTED breaks only. It excludes
	// UnattributedBreakUSD, KeepAliveSavedUSD, SplitUSD and TTLUSD.
	DeltaUSD             float64
	KeepAliveSavedUSD    float64  // rescue effects
	SplitUSD             float64  // cachesplit effects (Saved - Caused); Task 11 decides where it lands
	TTLUSD               float64  // headttl effects: premium (negative) plus rescue; Task 11 decides where it lands
	UnattributedBreakUSD float64  // client edits, expired caches: shown, never charged to a component
	Unpriced             []string // "opID:reason"; an unpriced operation is not a zero saving
	Estimated            bool     // any counted operation or effect is an estimate
}

// CounterfactualFigure prices the attached Result. A DryRun operation is skipped: computed,
// never counted. A spend operation (Call != nil) is not a saving and is priced elsewhere.
// Result.Interaction is ignored on purpose: the library attributes it to nobody.
func (e *Event) CounterfactualFigure(p modelinfo.Price) CFFigure {
	f := CFFigure{ByComponent: map[string]float64{}}
	if e.cfRes == nil {
		return f
	}
	// An unpriced op is also in Ops, with zero tokens (compute.go price()/notEnded()). Counting it
	// would show "saved $0" for something unmeasured. OpIDs are host-supplied: they may be empty or
	// duplicated, and an Unpriced entry with an empty OpID can be request-level. So an op is skipped
	// only when its OpID is in the set AND it carries no tokens: that can never drop a real dollar.
	unpricedIDs := map[string]bool{}
	for _, u := range e.cfRes.Unpriced {
		unpricedIDs[u.OpID] = true
	}
	owner := map[string]string{} // ChangeID -> component; empty ChangeIDs are never owners
	for _, o := range e.cfRes.Ops {
		if o.DryRun || o.Call != nil {
			continue
		}
		if unpricedIDs[o.OpID] && o.Saved == (cf.TierTokens{}) && o.Caused == (cf.TierTokens{}) {
			continue
		}
		c := strings.TrimPrefix(o.Operator, "context-guru.")
		if o.ChangeID != "" {
			if _, dup := owner[o.ChangeID]; !dup { // first owner wins; the library flags duplicates (unique_change)
				owner[o.ChangeID] = c
			}
		}
		net := tierUSD(p, o.Saved) - tierUSD(p, o.Caused)
		f.ByComponent[c] += net
		f.DeltaUSD += net
		f.Estimated = f.Estimated || o.Estimated
	}
	for _, ce := range e.cfRes.CacheEffects {
		// Sign convention (counterfactual/compute.go): value = tierUSD(Saved) - tierUSD(Caused).
		//   break:      Saved is the read that did not happen, Caused the write that did: a cost,
		//               tierUSD(Caused) - tierUSD(Saved) > 0, charged to the owner of ChangeID.
		//   rescue:     Saved is the write avoided, Caused the read paid instead: a saving.
		//   split:      same shape as rescue: a saving.
		//   ttlPremium: Saved is the 5m write, Caused the dearer 1h write: value < 0 (a cost).
		//   ttlRescue:  Saved is the 5m write, Caused the read: a saving.
		value := tierUSD(p, ce.Saved) - tierUSD(p, ce.Caused)
		switch ce.Kind {
		case cf.EffectBreak:
			// An empty ChangeID is a break nobody caused (client edit, cache expiry): unowned.
			if c, ok := owner[ce.ChangeID]; ok && ce.ChangeID != "" {
				f.ByComponent[c] += value
				f.DeltaUSD += value
			} else {
				f.UnattributedBreakUSD -= value
			}
		case cf.EffectRescue:
			f.KeepAliveSavedUSD += value
		case cf.EffectSplit:
			f.SplitUSD += value
		case cf.EffectTTLPremium, cf.EffectTTLRescue:
			f.TTLUSD += value
		default:
			f.Unpriced = append(f.Unpriced, "effect:"+string(ce.Kind)+":unknown_kind")
			continue
		}
		f.Estimated = f.Estimated || ce.Estimated
	}
	for _, u := range e.cfRes.Unpriced {
		f.Unpriced = append(f.Unpriced, u.OpID+":"+u.Reason)
	}
	return f
}

// SavingsMode picks where the stored per-component savings and the baseline come from.
type SavingsMode int

const (
	SavingsLegacy         SavingsMode = iota // the rules in this package
	SavingsShadow                            // legacy figures; the host logs the library's beside them
	SavingsCounterfactual                    // the library's figures, when the event qualifies
)

// Fallback reasons: why PriceMode kept the legacy figures of an event in SavingsCounterfactual
// mode. CounterfactualFallback returns one of them ("" when the override applied, or in any other
// mode).
const (
	FallbackNoResult           = "no_result"            // no Result attached (no usage, skipped, expand rounds)
	FallbackOpsDropped         = "ops_dropped"          // a change could not be placed as an operation
	FallbackObserve            = "observe"              // the run was observe mode
	FallbackUnpriced           = "unpriced"             // an operation or effect is unpriced
	FallbackCheckSum           = "check_sum"            // the library's sum check failed
	FallbackCheckUniqueChange  = "check_unique_change"  // two operations share a ChangeID
	FallbackCheckSpans         = "check_spans"          // an operation's spans do not add up to its sizes
	FallbackSizeUnknown        = "size_unknown"         // TokensBefore is 0: the plausibility bound cannot be checked
	FallbackImplausibleSize    = "implausible_size"     // the library saved more tokens than the request had
	FallbackFigureWithoutRow   = "figure_without_row"   // a library figure for a component with no row
	FallbackDuplicateName      = "duplicate_name"       // two rows share a component name
	FallbackActedWithoutFigure = "acted_without_figure" // a component acted and the library has no figure for it
)

// gatedChecks maps each library check (counterfactual/compute.go checks() and conservation()) that
// means "the operation list or its attribution is wrong" to its fallback reason. A check not in the
// map is informational: it is logged in cf_shadow and never gates.
//
//   - sum: the operations' byte changes per section do not equal the client-minus-forwarded body
//     bytes. The op list is wrong (an op missing, a change reported twice under two ChangeIDs, sizes
//     in the wrong unit, thread state lost on restart). Gated.
//   - unique_change: two live operations share a ChangeID; the figure would follow whichever owner
//     comes first. Gated.
//   - spans: an operation's spans do not add up to its own sizes; its per-span tiers are then
//     priced from numbers that contradict its total. The proxy reports no spans today, so it cannot
//     fire, but if it does the op list is malformed. Gated.
//   - conservation: the token form of sum (client tokens minus billed tokens against the net of the
//     operations). With sum passing it differs only by the ratio's rounding beyond its tolerance, or
//     by a usage that does not bill the forwarded body; the second case is what the plausibility
//     bound below catches where it matters. Informational, not gated.
//   - order, attribution: named in result.go but never emitted by compute.go (attribution holds by
//     construction). Not gated; would be informational if ever emitted.
var gatedChecks = map[string]string{
	"sum":           FallbackCheckSum,
	"unique_change": FallbackCheckUniqueChange,
	"spans":         FallbackCheckSpans,
}

// CounterfactualFallback is the reason PriceMode kept the legacy figures in SavingsCounterfactual
// mode, or "" when the override applied. In SavingsShadow mode it is the reason counterfactual mode
// WOULD have fallen back (the figures are not touched), so the shadow log can show it. "" in
// legacy mode.
func (e *Event) CounterfactualFallback() string { return e.cfFallback }

// PriceMode is Price, then, in SavingsCounterfactual mode, an override of each component's
// SavedUSD and of BaselineCostUSD from the library's figure. KeepAliveSavedUSD, CachesplitSavedUSD,
// CacheSavedUSD, CostUSD and CGLLMCostUSD always stay as Price set them.
//
// The override is all-or-nothing per event, because a partial list would show an unmeasured
// component as a $0 saving. It applies only when: a Result is attached; no change was dropped
// from the operation list; the run was not observe mode; no operation or effect is unpriced; no
// gated library check failed; the figure is plausible; and every component that acted has a
// library figure. Otherwise the legacy figures stand and CounterfactualFallback says why.
func (e *Event) PriceMode(p modelinfo.Price, accountingComplete bool, mode SavingsMode) {
	e.cfFallback = ""
	if !e.priceLegacy(p, accountingComplete) || mode == SavingsLegacy {
		return
	}
	e.cfFallback = e.cfOverride(p, mode == SavingsCounterfactual)
}

// cfOverride applies the counterfactual override when apply is true, or returns the reason it
// cannot. With apply false it only returns the reason ("" = it would apply) and changes nothing.
func (e *Event) cfOverride(p modelinfo.Price, apply bool) string {
	switch {
	case e.cfRes == nil:
		return FallbackNoResult
	case e.cfOpsDropped != 0:
		return FallbackOpsDropped
	case e.cfObserve:
		return FallbackObserve
	}
	for _, c := range e.cfRes.Checks {
		if r, ok := gatedChecks[c.Check]; ok {
			return r
		}
	}
	fig := e.CounterfactualFigure(p)
	if len(fig.Unpriced) > 0 {
		return FallbackUnpriced
	}
	// Plausibility bound on the library's INPUT: a request cannot remove more tokens than it had.
	// The library converts removed bytes to tokens with the request's own billed-token ratio, so a
	// usage that does not match the forwarded body (a wrong bytes-to-tokens ratio) would otherwise
	// publish an inflated figure. Over the bound, or with TokensBefore unknown (0), the bound cannot
	// be met or checked and the legacy figures stand. This is a gate, never a rescale of the figure.
	if e.TokensBefore <= 0 {
		return FallbackSizeUnknown
	}
	if e.countedSavedTokens() > e.TokensBefore {
		return FallbackImplausibleSize
	}
	// A library figure for a component the event has no row for would leave the rows short of
	// the baseline: the sum invariant would break. Keep the legacy figures instead.
	rows := map[string]bool{}
	for _, c := range e.Components {
		rows[c.Component] = true
	}
	for name := range fig.ByComponent {
		if !rows[name] {
			return FallbackFigureWithoutRow
		}
	}
	// Two rows with one name would each take the full figure and break the sum invariant.
	seen := map[string]bool{}
	for _, c := range e.Components {
		if _, ok := fig.ByComponent[c.Component]; ok {
			if seen[c.Component] {
				return FallbackDuplicateName
			}
			seen[c.Component] = true
		}
	}
	saved := make([]float64, len(e.Components))
	for i, c := range e.Components {
		v, ok := fig.ByComponent[c.Component]
		if !ok && c.Acted {
			return FallbackActedWithoutFigure
		}
		saved[i] = v
	}
	if !apply {
		return ""
	}
	for i := range e.Components {
		e.Components[i].SavedUSD = saved[i]
	}
	e.BaselineCostUSD = e.CostUSD + fig.DeltaUSD
	return ""
}

// countedSavedTokens is the Saved tokens, all four tiers, of the operations that feed
// CounterfactualFigure.ByComponent: not DryRun, not a spend.
func (e *Event) countedSavedTokens() int {
	n := 0
	for _, o := range e.cfRes.Ops {
		if o.DryRun || o.Call != nil {
			continue
		}
		n += o.Saved.Input + o.Saved.CacheRead + o.Saved.CacheWrite5m + o.Saved.CacheWrite1h
	}
	return n
}
