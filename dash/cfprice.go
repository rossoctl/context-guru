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
	ByComponent          map[string]float64 // net of the break each component caused
	DeltaUSD             float64            // sum of ByComponent: the baseline delta
	KeepAliveSavedUSD    float64            // rescue effects
	SplitUSD             float64            // cachesplit effects (Saved - Caused); Task 11 decides where it lands
	TTLUSD               float64            // headttl effects: premium (negative) plus rescue; Task 11 decides where it lands
	UnattributedBreakUSD float64            // client edits, expired caches: shown, never charged to a component
	Unpriced             []string           // "opID:reason"; an unpriced operation is not a zero saving
	Estimated            bool               // any counted operation or effect is an estimate
}

// CounterfactualFigure prices the attached Result. A DryRun operation is skipped: computed,
// never counted. A spend operation (Call != nil) is not a saving and is priced elsewhere.
// Result.Interaction is ignored on purpose: the library attributes it to nobody.
func (e *Event) CounterfactualFigure(p modelinfo.Price) CFFigure {
	f := CFFigure{ByComponent: map[string]float64{}}
	if e.cfRes == nil {
		return f
	}
	owner := map[string]string{} // ChangeID -> component
	for _, o := range e.cfRes.Ops {
		if o.DryRun || o.Call != nil {
			continue
		}
		c := strings.TrimPrefix(o.Operator, "context-guru.")
		owner[o.ChangeID] = c
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
			if c, ok := owner[ce.ChangeID]; ok {
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
