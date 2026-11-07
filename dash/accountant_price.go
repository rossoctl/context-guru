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
	// ByComponentFirst and ByComponentReplay split each component's SAVED tokens by tier: the write
	// and input tiers (content the provider had not cached: a first removal) and the read tier (a
	// replay of content in the cached prefix). ByComponent minus both is its costs (Caused, breaks).
	ByComponentFirst, ByComponentReplay map[string]float64
	// DeltaUSD is the sum of ByComponent plus EndedBreakUSD. It excludes UnattributedBreakUSD,
	// KeepAliveSavedUSD, SplitUSD and TTLUSD.
	DeltaUSD          float64
	KeepAliveSavedUSD float64 // rescue effects
	SplitUSD          float64 // cachesplit effects (Saved - Caused): CachesplitSavedUSD
	TTLUSD            float64 // headttl effects: premium (negative) plus rescue: the headttl row
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
	f := AccFigure{ByComponent: map[string]float64{}, ByComponentFirst: map[string]float64{}, ByComponentReplay: map[string]float64{}}
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
		read := tierUSD(p, accountant.TierTokens{CacheRead: o.Saved.CacheRead})
		f.ByComponentReplay[c] += read
		f.ByComponentFirst[c] += tierUSD(p, o.Saved) - read
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
		//   markPremium: Saved is the input the client body would have billed, Caused the dearer write the
		//               host's added mark made the provider bill: value < 0 (a cost).
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
		case accountant.EffectMarkPremium:
			// The write premium of a breakpoint the host added: a cost (value < 0), charged to the
			// operation whose span covers the mark. With no owner it belongs to the tail breakpoint,
			// the only component that adds a mark (accountant design §4.4). Without this case the
			// effect was unpriced and every request with the default tail breakpoint booked no saving.
			c, ok := owner[ce.ChangeID]
			if !ok || ce.ChangeID == "" {
				c = "tailbreakpoint"
			}
			f.ByComponent[c] += value
			f.DeltaUSD += value
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

// Fallback reasons: why PriceMode booked no saving on an event. AccountantFallback
// returns one of them ("" when the override applied).
const (
	FallbackNoResult           = "no_result"            // no Result attached (no usage, skipped, expand rounds)
	FallbackOpsDropped         = "ops_dropped"          // a change could not be placed as an operation
	FallbackObserve            = "observe"              // the run was observe mode
	FallbackUnpriced           = "unpriced"             // no usage or no price for the model, or an operation or effect is unpriced
	FallbackCheckSum           = "check_sum"            // the library's sum check failed
	FallbackCheckUniqueChange  = "check_unique_change"  // two operations share a ChangeID
	FallbackCheckSpans         = "check_spans"          // an operation's spans do not add up to its sizes
	FallbackSizeUnknown        = "size_unknown"         // TokensBefore is 0: the plausibility bound cannot be checked
	FallbackImplausibleSize    = "implausible_size"     // the library saved more tokens than the request had
	FallbackFigureWithoutRow   = "figure_without_row"   // a library figure for a component with no row
	FallbackDuplicateName      = "duplicate_name"       // two rows share a component name
	FallbackActedWithoutFigure = "acted_without_figure" // a component acted and the library has no figure for it
	FallbackUnsupportedWire    = "unsupported_wire"     // the request is on a wire the accountant does not read yet (OpenAI Responses)
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

// PricedByAccountant is CompRow.PricedBy on a row the accountant library priced.
const PricedByAccountant = "accountant"

// PricedByNone is CompRow.PricedBy on a row of a request the accountant path handled but could not
// price (a fallback): it books no saving, and the read-time passes must not value it with the
// legacy rule. Any non-empty PricedBy means the stored split stands as written; only a row written
// before the switch has it empty.
const PricedByNone = "none"

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
	"tailbreakpoint": true, "tooldefer": true,
}

// AccountantEstimated reports whether the applied figure is an estimate: the library could not rule
// out a break (penalty_unknown, accountant design §6.4). It is false when the override did not apply.
// It reads PriceEstimated, the stored field the dashboard marks the figure by.
func (e *Event) AccountantEstimated() bool { return e.PriceEstimated }

// AccountantFallback is the reason PriceMode booked no saving, or "" when the override
// applied. It is "" too on an event priced with Price alone, which never tries the override.
// An event with no usage or no price books no saving too, and its reason is FallbackUnpriced.
func (e *Event) AccountantFallback() string { return e.accFallback }

// PriceMode is Price, then an override of each component's SavedUSD, of BaselineCostUSD, of
// KeepAliveSavedUSD and of CachesplitSavedUSD from the library's figure. CacheSavedUSD (the
// provider's own cache, a measurement), CostUSD and CGLLMCostUSD always stay as Price set them.
//
// The override is all-or-nothing per event, because a partial list would show an unmeasured
// component as a $0 saving. It applies only when: a Result is attached; no change was dropped
// from the operation list; the run was not observe mode; no operation or effect is unpriced; no
// gated library check failed; the figure is plausible; and every component that acted has a
// library figure. Otherwise the event books no saving (Price set every saving to 0) and
// AccountantFallback says why: a figure nothing measured is not shown.
func (e *Event) PriceMode(p modelinfo.Price, accountingComplete bool) {
	e.accFallback, e.PriceEstimated, e.AccountantPriced, e.EstimatedSavedUSD, e.EstimateHeld = "", false, false, 0, false
	if !e.priceCost(p, accountingComplete) {
		e.accFallback = FallbackUnpriced
		return
	}
	e.accFallback = e.accOverride(p)
	if e.accFallback != "" {
		for i := range e.Components {
			e.Components[i].PricedBy = PricedByNone
		}
	}
}

// accOverride applies the accountant override, or returns the reason it cannot and changes nothing.
func (e *Event) accOverride(p modelinfo.Price) string {
	switch {
	case e.accUnsupportedWire:
		return FallbackUnsupportedWire // before no_result: say the real reason, not that nothing was attached
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
	// be met or checked and the event books no saving. This is a gate, never a rescale of the figure.
	if e.TokensBefore <= 0 {
		return FallbackSizeUnknown
	}
	if e.countedSavedTokens() > e.TokensBefore {
		return FallbackImplausibleSize
	}
	// A library figure for a component the event has no row for would leave the rows short of
	// the baseline: the sum invariant would break. An envelope operator (envelopeComponents) gets
	// its own row; for any other name the event books no saving.
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
	// The head-TTL legs (premium and rescue) are a lever's figure like any component's: they go on
	// the headttl row, inside the baseline delta. Legacy had no figure for them.
	if fig.TTLUSD != 0 {
		fig.ByComponent["headttl"] += fig.TTLUSD
		fig.DeltaUSD += fig.TTLUSD
		if !rows["headttl"] && !inAdded(added, "headttl") {
			added = append(added, CompRow{Component: "headttl", Kind: KindEnvelope, Acted: true, Mutated: true})
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
	for _, r := range added {
		e.Components = append(e.Components, r)
		saved = append(saved, fig.ByComponent[r.Component])
	}
	for i := range e.Components {
		c := &e.Components[i]
		c.SavedUSD, c.PricedBy = saved[i], PricedByAccountant
		c.SavedFirstUSD, c.SavedReplayUSD = fig.ByComponentFirst[c.Component], fig.ByComponentReplay[c.Component]
	}
	e.BaselineCostUSD = e.CostUSD + fig.DeltaUSD
	if estimated {
		// The library could not rule out a break: it had no history for the thread, so it priced
		// each removal as a write the provider would have made. When the cache was in fact warm
		// (a restart that lost the state file, or a session begun before the proxy) the figure is
		// about 2x too high, and the proxy cannot say by how much: the size of the unseen warm
		// prefix is the thing the missing history held. A figure nothing measured is not booked
		// (issue #915). The request books no break-affected saving, and the figure is kept in
		// EstimatedSavedUSD, outside every total, so the Overview, the components table, the
		// keep-alive tab and the API still agree.
		//
		// What is held out: fig.DeltaUSD, which is every component's figure, the head-TTL premium and
		// rescue legs (the headttl row), the ended-change break, and the filter's part. The figure can
		// be negative when the request's net effect is a cost (a tooldefer names list, an injected
		// expand or adjudicate declaration): the cost is held out with the rest, and the readers render
		// a negative figure as a cost. What stays booked, each in its own column outside the baseline:
		// the keep-alive rescue (keepalive_saved_usd) and the split saving (cachesplit_saved_usd), which
		// do not depend on a break. The filter's dollars are owned by the measured credit
		// (declFilterInBaseline is false for a held row), not by this figure.
		e.EstimatedSavedUSD = fig.DeltaUSD
		e.EstimateHeld = true
		e.BaselineCostUSD = e.CostUSD
		for i := range e.Components {
			e.Components[i].SavedUSD, e.Components[i].SavedFirstUSD, e.Components[i].SavedReplayUSD = 0, 0, 0
		}
	}
	// The keep-alive rescue and the split saving keep their own columns, outside the baseline, as
	// the legacy rules had them: every reader of keepalive_saved_usd and cachesplit_saved_usd stays valid.
	e.KeepAliveSavedUSD, e.CachesplitSavedUSD = fig.KeepAliveSavedUSD, fig.SplitUSD
	e.PriceEstimated = estimated
	e.AccountantPriced = true
	return ""
}

func inAdded(rows []CompRow, name string) bool {
	for _, r := range rows {
		if r.Component == name {
			return true
		}
	}
	return false
}

// toolSectionOperators are the operators whose operations act on the tools section, not on messages:
// the declaration filter and the schema rewrite (proxy/accountant_envelope.go reports them from a body
// diff), and the two declarations the proxy injects. Their tokens are not in TokensBefore, which counts
// message text only.
var toolSectionOperators = map[string]bool{
	"context-guru.toolfilter": true, "context-guru.toolschema": true,
	"context-guru.expand": true, "context-guru.adjudicate": true,
}

// countedSavedTokens is the Saved tokens, all four tiers, of the operations that feed
// AccountantFigure.ByComponent: not DryRun, not a spend, and not an operation on the tools section.
// TokensBefore is the size of the messages. A tool declaration is not in it, so 2,188 removed
// declaration tokens would look larger than a 121-token conversation and drop the whole result (#907).
// The bound stays for every operation on messages.
func (e *Event) countedSavedTokens() int {
	n := 0
	for _, o := range e.accRes.Ops {
		if o.DryRun || o.Call != nil || toolSectionOperators[o.Operator] {
			continue
		}
		n += o.Saved.Input + o.Saved.CacheRead + o.Saved.CacheWrite5m + o.Saved.CacheWrite1h
	}
	return n
}
