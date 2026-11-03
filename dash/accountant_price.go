package dash

import (
	"sort"
	"strings"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// oneHourWriteInputMultiple is the 1h cache-write premium: such a token bills at 2x the input
// rate. It is the rule of modelinfo.Price.CostWithCacheWrite1h (which adds 2*Input - CacheWrite
// per 1h token on top of the 5m write rate); keep the two in step.
const oneHourWriteInputMultiple = 2

// tierUSD prices tokens by tier.
func tierUSD(p modelinfo.Price, t accountant.TierTokens) float64 {
	return float64(t.Input)*p.Input + float64(t.CacheRead)*p.CacheRead +
		float64(t.CacheWrite5m)*p.CacheWrite + float64(t.CacheWrite1h)*oneHourWriteInputMultiple*p.Input
}

// AccFigure is the request's savings in dollars, from the library's tokens. Only dollars are
// summed; token tiers are never collapsed into one total.
type AccFigure struct {
	ByComponent map[string]float64 // net of the break each component caused
	// DeltaUSD is the sum of ByComponent plus EndedBreakUSD. It excludes UnattributedBreakUSD,
	// KeepAliveSavedUSD, SplitUSD and TTLUSD.
	DeltaUSD          float64
	KeepAliveSavedUSD float64 // rescue effects
	SplitUSD          float64 // cachesplit effects (Saved - Caused); Task 11 decides where it lands
	TTLUSD            float64 // headttl effects: premium (negative) plus rescue; Task 11 decides where it lands
	// EndedBreakUSD is the break a change left behind when it ended (accountant design §5.2 row 1,
	// Q12): it has no ChangeID, so no component owns it, but the client body would have read that
	// prefix, so it is part of the bill difference. Negative: a cost.
	EndedBreakUSD float64
	// UnattributedBreakUSD is the breaks the client body has too (InBaseline: client edits, expired
	// caches, entries lost early): shown, never counted, never charged to a component.
	UnattributedBreakUSD float64
	Unpriced             []string // "opID:reason"; an unpriced operation is not a zero saving
	Estimated            bool     // any counted operation or effect is an estimate
}

// AccountantFigure prices the attached Result. A DryRun operation is skipped: computed,
// never counted. A spend operation (Call != nil) is not a saving and is priced elsewhere.
// Result.Interaction is ignored on purpose: the library attributes it to nobody.
func (e *Event) AccountantFigure(p modelinfo.Price) AccFigure {
	f := AccFigure{ByComponent: map[string]float64{}}
	if e.accRes == nil {
		return f
	}
	// An unpriced op is also in Ops, with zero tokens (compute.go price()/notEnded()). Counting it
	// would show "saved $0" for something unmeasured. OpIDs are host-supplied: they may be empty or
	// duplicated, and an Unpriced entry with an empty OpID can be request-level. So an op is skipped
	// only when its OpID is in the set AND it carries no tokens: that can never drop a real dollar.
	unpricedIDs := map[string]bool{}
	for _, u := range e.accRes.Unpriced {
		unpricedIDs[u.OpID] = true
	}
	owner := map[string]string{} // ChangeID -> component; empty ChangeIDs are never owners
	for _, o := range e.accRes.Ops {
		if o.DryRun || o.Call != nil {
			continue
		}
		c := strings.TrimPrefix(o.Operator, "context-guru.")
		// An unpriced op still owns the break it caused: a setting with no position is unpriced
		// (penalty_unknown) and its break is priced from the read the usage shows.
		if o.ChangeID != "" {
			if _, dup := owner[o.ChangeID]; !dup { // first owner wins; the library flags duplicates (unique_change)
				owner[o.ChangeID] = c
			}
		}
		if unpricedIDs[o.OpID] && o.Saved == (accountant.TierTokens{}) && o.Caused == (accountant.TierTokens{}) {
			continue
		}
		net := tierUSD(p, o.Saved) - tierUSD(p, o.Caused)
		f.ByComponent[c] += net
		f.DeltaUSD += net
		f.Estimated = f.Estimated || o.Estimated
	}
	for _, ce := range e.accRes.CacheEffects {
		// Sign convention (accountant internal/engine/compute.go): value = tierUSD(Saved) - tierUSD(Caused).
		//   break:      Saved is the read that did not happen, Caused the write that did: a cost,
		//               tierUSD(Caused) - tierUSD(Saved) > 0, charged to the owner of ChangeID.
		//   rescue:     Saved is the write avoided, Caused the read paid instead: a saving.
		//   split:      same shape as rescue: a saving.
		//   ttlPremium: Saved is the 5m write, Caused the dearer 1h write: value < 0 (a cost).
		//   ttlRescue:  Saved is the 5m write, Caused the read: a saving.
		value := tierUSD(p, ce.Saved) - tierUSD(p, ce.Caused)
		switch ce.Kind {
		case accountant.EffectBreak:
			switch c, ok := owner[ce.ChangeID]; {
			case ce.InBaseline: // the client body has this break too: shown, not counted
				f.UnattributedBreakUSD -= value
			case ok && ce.ChangeID != "":
				f.ByComponent[c] += value
				f.DeltaUSD += value
			case ce.ChangeID == "": // an ended change left it behind: counted, owned by no component
				f.EndedBreakUSD += value
				f.DeltaUSD += value
			default: // a ChangeID no counted op owns (a dry run): shown, never charged
				f.UnattributedBreakUSD -= value
			}
		case accountant.EffectRescue:
			f.KeepAliveSavedUSD += value
		case accountant.EffectSplit:
			f.SplitUSD += value
		case accountant.EffectTTLPremium, accountant.EffectTTLRescue:
			f.TTLUSD += value
		default:
			f.Unpriced = append(f.Unpriced, "effect:"+string(ce.Kind)+":unknown_kind")
			continue
		}
		f.Estimated = f.Estimated || ce.Estimated
	}
	for _, u := range e.accRes.Unpriced {
		f.Unpriced = append(f.Unpriced, u.OpID+":"+u.Reason)
	}
	return f
}

// Fallback reasons: why PriceMode kept the legacy figures of an event. AccountantFallback
// returns one of them ("" when the override applied).
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

// gatedChecks maps each library check (accountant internal/engine/compute.go checks() and conservation()) that
// means "the operation list or its attribution is wrong" to its fallback reason. A check not in the
// map is informational: it never gates.
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

// KindEnvelope is the CompRow.Kind of a row the accountant override adds: an envelope operator or
// the ended-change break, neither of which is a pipeline component.
const KindEnvelope = "envelope"

// EndedChangeComponent is the row that carries the break an ended change left behind.
const EndedChangeComponent = "ended_change"

// envelopeComponents are the operators that change the request envelope (tools, system, cache
// marks) and may have no pipeline row: the proxy reports them from a body diff
// (proxy/accountant_envelope.go). expand and adjudicate are never pipeline components; headttl is
// a host option; toolschema, toolfilter and cachesplit are components whose row can be absent when
// the rewrite ran from an equivalent gate (cacheinject runs the split too).
var envelopeComponents = map[string]bool{
	"expand": true, "adjudicate": true, "headttl": true,
	"toolschema": true, "toolfilter": true, "cachesplit": true,
}

// AccountantEstimated reports whether the applied figure is an estimate: the library could not rule
// out a break (penalty_unknown, accountant design §6.4). It is false when the override did not apply.
func (e *Event) AccountantEstimated() bool { return e.accEstimated }

// AccountantFallback is the reason PriceMode kept the legacy figures, or "" when the override
// applied. It is "" too on an event priced with Price alone, which never tries the override.
func (e *Event) AccountantFallback() string { return e.accFallback }

// PriceMode is Price, then an override of each component's
// SavedUSD and of BaselineCostUSD from the library's figure. KeepAliveSavedUSD, CachesplitSavedUSD,
// CacheSavedUSD, CostUSD and CGLLMCostUSD always stay as Price set them.
//
// The override is all-or-nothing per event, because a partial list would show an unmeasured
// component as a $0 saving. It applies only when: a Result is attached; no change was dropped
// from the operation list; the run was not observe mode; no operation or effect is unpriced; no
// gated library check failed; the figure is plausible; and every component that acted has a
// library figure. Otherwise the legacy figures stand and AccountantFallback says why.
func (e *Event) PriceMode(p modelinfo.Price, accountingComplete bool) {
	e.accFallback, e.accEstimated = "", false
	if !e.priceLegacy(p, accountingComplete) {
		return
	}
	e.accFallback = e.accOverride(p)
}

// accOverride applies the accountant override, or returns the reason it cannot and changes nothing.
func (e *Event) accOverride(p modelinfo.Price) string {
	switch {
	case e.accRes == nil:
		return FallbackNoResult
	case e.accOpsDropped != 0:
		return FallbackOpsDropped
	case e.accObserve:
		return FallbackObserve
	}
	for _, c := range e.accRes.Checks {
		if r, ok := gatedChecks[c.Check]; ok {
			return r
		}
	}
	fig := e.AccountantFigure(p)
	// penalty_unknown is not a missing price: the library priced every operation and could not
	// rule out a break, because the thread has no history or a setting has no position. The design
	// says to show such a figure and mark it estimated (accountant design §6.4), so it does not gate.
	// Any other unpriced reason means an operation has no price, and it does.
	estimated := false
	for _, u := range fig.Unpriced {
		if !strings.HasSuffix(u, ":"+accountant.ReasonPenaltyUnknown) {
			return FallbackUnpriced
		}
		estimated = true
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
	// the baseline: the sum invariant would break. An envelope operator (envelopeComponents) gets
	// its own row; for any other name the legacy figures stand.
	rows := map[string]bool{}
	for _, c := range e.Components {
		rows[c.Component] = true
	}
	var added []CompRow
	for name := range fig.ByComponent {
		switch {
		case rows[name]:
		case envelopeComponents[name]:
			added = append(added, CompRow{Component: name, Kind: KindEnvelope, Acted: true, Mutated: true})
		default:
			return FallbackFigureWithoutRow
		}
	}
	// The break an ended change left behind is counted and owned by no component: it gets a row
	// of its own, so the rows still sum to the baseline delta.
	if fig.EndedBreakUSD != 0 {
		if rows[EndedChangeComponent] {
			return FallbackDuplicateName
		}
		added = append(added, CompRow{Component: EndedChangeComponent, Kind: KindEnvelope, Acted: true})
		fig.ByComponent[EndedChangeComponent] = fig.EndedBreakUSD
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Component < added[j].Component })
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
	for i := range e.Components {
		e.Components[i].SavedUSD = saved[i]
	}
	for _, r := range added {
		r.SavedUSD = fig.ByComponent[r.Component]
		e.Components = append(e.Components, r)
	}
	e.BaselineCostUSD = e.CostUSD + fig.DeltaUSD
	e.accEstimated = estimated
	return ""
}

// countedSavedTokens is the Saved tokens, all four tiers, of the operations that feed
// AccountantFigure.ByComponent: not DryRun, not a spend.
func (e *Event) countedSavedTokens() int {
	n := 0
	for _, o := range e.accRes.Ops {
		if o.DryRun || o.Call != nil {
			continue
		}
		n += o.Saved.Input + o.Saved.CacheRead + o.Saved.CacheWrite5m + o.Saved.CacheWrite1h
	}
	return n
}
