package dash

import (
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// hostPricer is the host's rate table for accountant.Figure (SizedPricer): the rate of one token at the
// prompt size of its request. The rule lives in modelinfo.Price.RateAt, the same one the metrics sink uses.
// It holds the price of ONE model. For any other model (a spend call made on another model) it refuses:
// rate 0, estimated. It never prices that call with the wrong rates.
type hostPricer struct {
	model string
	p     modelinfo.Price
}

func (h hostPricer) PriceAt(model string, _ time.Time, t accountant.Tier, promptTokens int64) (float64, bool) {
	if model != h.model {
		return 0, true
	}
	return h.p.RateAt(model, t, promptTokens)
}

// AccFigure is the request's savings in dollars, from the library's tokens. Only dollars are
// summed; token tiers are never collapsed into one total.
type AccFigure struct {
	ByComponent map[string]float64 // net of the break each component caused
	// ByComponentFirst and ByComponentReplay split each component's SAVED tokens by tier: the write
	// and input tiers (content the provider had not cached: a first removal) and the read tier (a
	// replay of content in the cached prefix). ByComponent minus both is its costs (Caused, breaks).
	ByComponentFirst, ByComponentReplay map[string]float64
	// DeltaUSD is the sum of ByComponent plus EndedBreakUSD and InteractionUSD. It excludes
	// UnattributedBreakUSD, KeepAliveSavedUSD, SplitUSD and TTLUSD.
	DeltaUSD          float64
	KeepAliveSavedUSD float64 // rescue effects
	SplitUSD          float64 // cachesplit effects (Saved - Caused): CachesplitSavedUSD
	TTLUSD            float64 // headttl effects: premium (negative) plus rescue: the headttl row
	// EndedBreakUSD is the break a change left behind when it ended (accountant design §5.2 row 1,
	// Q12): it has no ChangeID, so no component owns it, but the client body would have read that
	// prefix, so it is part of the bill difference. Negative: a cost.
	EndedBreakUSD float64
	// InteractionUSD is the value of the tokens no operation owns on a rescued request (accountant
	// design §6.2): Result.Interaction minus Result.InteractionCaused, both at the size without
	// context-guru. A ping kept the cache warm and an operation changed the body, so the client body
	// would have written these tokens cold and the request read them. Neither the keeper nor the
	// operation owns them. Counted, owned by no component: the interaction row (InteractionComponent).
	// The metrics book the same figure as llm.savings.interaction.usd. Signed: positive saves.
	InteractionUSD float64
	// UnattributedBreakUSD is the breaks the client body has too (InBaseline: client edits, expired
	// caches, entries lost early): shown, never counted, never charged to a component.
	UnattributedBreakUSD float64
	Unpriced             []string // "opID:reason"; an unpriced operation is not a zero saving
	Estimated            bool     // any counted operation or effect is an estimate, or a rate the pricer used

	// Fig is the library's own figure, before the mapping above. The held floor reads it.
	Fig accountant.USDFigure

	// The same figures as tokens by tier, before any pricing (#998). TokByComponent follows exactly
	// the attribution of ByComponent: the counted operations and the breaks and mark legs each
	// component owns. It does NOT hold the head-TTL legs or the ended-change break, which accOverride
	// adds to the dollar rows afterwards: those are TokTTL and TokEndedBreak. So the component tokens
	// and the four effect figures never overlap. For each, the dollar figure is
	// tierUSD(Saved) - tierUSD(Caused).
	TokByComponent map[string]TokenFlow
	TokKeepAlive   TokenFlow // rescue effects
	TokSplit       TokenFlow // split effects
	TokTTL         TokenFlow // ttlPremium and ttlRescue effects
	TokEndedBreak  TokenFlow // the break an ended change left behind
}

// AccountantFigure is the attached Result in dollars. The dollars come from accountant.Figure, priced with
// the host's rates at the prompt size of each side (hostPricer). This function only maps the library's
// figure to the dashboard's rows and columns, and takes the tokens of each row from the Result.
//
// It reads a DryRun operation as the library does: computed, never counted. A spend (Call != nil) is a
// separate bill. Result.Interaction is attributed to nobody: it goes to InteractionUSD, the interaction row.
//
// Where the library has a figure the dashboard had no row for:
//   - UnownedMarkUSD (a mark premium or payback no counted operation owns) goes to the tail breakpoint
//     row, the same row as before: that component adds the mark in every default preset.
//   - UnownedReRateUSD (a guard: it is 0 today) goes to EndedBreakUSD, the ended_change row, as before.
func (e *Event) AccountantFigure(p modelinfo.Price) AccFigure {
	f := AccFigure{ByComponent: map[string]float64{}, ByComponentFirst: map[string]float64{}, ByComponentReplay: map[string]float64{},
		TokByComponent: map[string]TokenFlow{}}
	if e.accRes == nil {
		return f
	}
	w := modelinfo.PromptTokens(e.FreshInput, e.CacheRead, e.CacheWrite)
	fig := accountant.Figure(accountant.FigureInput{
		Model: e.Model, Output: int(e.OutputTokens), Result: *e.accRes,
		Billed: accountant.Usage{Input: int(e.FreshInput), CacheRead: int(e.CacheRead),
			CacheWrite: int(e.CacheWrite), CacheWrite1h: int(e.CacheWrite1h)},
		Unsized: w <= 0, // no billed usage: no size, so every token prices at the base rate
	}, hostPricer{model: e.Model, p: p})
	f.Fig = fig
	// A zero-token op of a name with NO row in e.Components gets no envelope row: its key is in the library's
	// maps with 0. A name with a row keeps its key at $0: a component that acted can have counted ops of 0
	// tokens (the byte ratio truncates), and without its key accOverride would drop the whole request's saving.
	hasRow := map[string]bool{}
	for _, c := range e.Components {
		hasRow[c.Component] = true
	}
	zero := map[string]bool{}
	for _, o := range e.accRes.Ops {
		if !o.DryRun && o.Call == nil {
			if n := strings.TrimPrefix(o.Operator, accwire.OperatorPrefix); !hasRow[n] {
				zero[n] = true
			}
		}
	}
	for _, o := range e.accRes.Ops {
		if !o.DryRun && o.Call == nil && !(o.Saved.IsZero() && o.Caused.IsZero()) {
			delete(zero, strings.TrimPrefix(o.Operator, accwire.OperatorPrefix))
		}
	}
	cut := func(m map[string]float64, to map[string]float64) {
		for k, v := range m {
			if zero[strings.TrimPrefix(k, accwire.OperatorPrefix)] && v == 0 {
				continue
			}
			to[strings.TrimPrefix(k, accwire.OperatorPrefix)] += v
		}
	}
	cut(fig.ByComponent, f.ByComponent)
	cut(fig.ByComponentFirst, f.ByComponentFirst)
	cut(fig.ByComponentReplay, f.ByComponentReplay)
	f.DeltaUSD = fig.DeltaUSD
	f.KeepAliveSavedUSD, f.SplitUSD, f.TTLUSD = fig.KeepAliveSavedUSD, fig.SplitUSD, fig.TTLUSD
	f.EndedBreakUSD = fig.EndedBreakUSD + fig.UnownedReRateUSD
	f.UnattributedBreakUSD = fig.UnattributedBreakUSD
	if fig.UnownedMarkUSD != 0 {
		f.ByComponent[accwire.OpTailBreakpoint] += fig.UnownedMarkUSD
	}
	f.Unpriced, f.Estimated = fig.Unpriced, fig.Estimated

	// The tokens of each row (#998). They are the Result's own tokens, not dollars: each operation's, then
	// each counted effect's, on the row that the figure charged it to.
	for _, o := range e.accRes.Ops {
		if o.DryRun || o.Call != nil {
			continue
		}
		if o.Saved.IsZero() && o.Caused.IsZero() {
			continue // an unpriced operation has no tokens: it adds no token figure, as it adds no dollars
		}
		f.addTok(strings.TrimPrefix(o.Operator, accwire.OperatorPrefix), o.Saved, o.Caused)
	}
	for _, ue := range fig.Effects {
		if !ue.Counted {
			continue
		}
		ce := ue.Effect
		switch ce.Kind {
		case accountant.EffectBreak:
			if ue.Owner != "" {
				f.addTok(strings.TrimPrefix(ue.Owner, accwire.OperatorPrefix), ce.Saved, ce.Caused)
			} else {
				f.TokEndedBreak.addTiers(ce.Saved, ce.Caused)
			}
		case accountant.EffectRescue:
			f.TokKeepAlive.addTiers(ce.Saved, ce.Caused)
		case accountant.EffectSplit:
			f.TokSplit.addTiers(ce.Saved, ce.Caused)
		case accountant.EffectTTLPremium, accountant.EffectTTLRescue:
			f.TokTTL.addTiers(ce.Saved, ce.Caused)
		case accountant.EffectMarkPremium, accountant.EffectMarkPayback:
			c := accwire.OpTailBreakpoint
			if ue.Owner != "" {
				c = strings.TrimPrefix(ue.Owner, accwire.OperatorPrefix)
			}
			f.addTok(c, ce.Saved, ce.Caused)
		}
	}
	// The interaction: the tokens no operation owns on a rescued request. Counted in DeltaUSD (the library
	// keeps it out of Figure's DeltaUSD, in TotalUSD), owned by no component: the interaction row.
	if ia, ic := e.accRes.Interaction, e.accRes.InteractionCaused; ia != (accountant.TierTokens{}) || ic != (accountant.TierTokens{}) {
		f.InteractionUSD = fig.InteractionUSD
		f.DeltaUSD += f.InteractionUSD
		f.addTok(InteractionComponent, ia, ic)
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
	FallbackCheckOther         = "check_other"          // a fatal library check this host has no name for
	FallbackSizeUnknown        = "size_unknown"         // TokensBefore is 0: the plausibility bound cannot be checked
	FallbackImplausibleSize    = "implausible_size"     // the library saved more tokens than the request had
	FallbackFigureWithoutRow   = "figure_without_row"   // a library figure for a component with no row
	FallbackDuplicateName      = "duplicate_name"       // two rows share a component name
	FallbackActedWithoutFigure = "acted_without_figure" // a component acted and the library has no figure for it
	FallbackServerHeldHistory  = "server_held_history"  // an OpenAI Responses request with server-held history (previous_response_id, conversation, item_reference): the provider holds the history
)

// FallbackUnrecorded is requests.acc_fallback on a row written before the column existed whose components say
// it fell back (backfillAccFallback). Its real reason was not stored. PriceMode never returns it.
const FallbackUnrecorded = "unrecorded"

// RecordAccountantFallback stores AccountantFallback in AccFallback, so the Overview can count the request and
// leave its cost out of the saving share (#1004). The host calls it only for a request the accountant ran on, or
// one whose history the provider holds (server_held_history): the same requests it logs as acc_fallback. A request with no record (no
// session, or the pipeline did not run) has nothing to price, so it is not a fallback, and AccFallback stays "".
//
// A bypassed request is not a fallback either. The client asked for no context-guru, so its $0 saving is a
// measured $0, and its cost belongs in the share. It falls back only because bypass leaves TokensBefore at 0
// (FallbackSizeUnknown). Stored, it would take bypassed spend out of the share and raise it.
func (e *Event) RecordAccountantFallback() {
	if e.Bypassed {
		e.AccFallback = ""
		return
	}
	e.AccFallback = e.accFallback
}

// fatalCheckFallback is the fallback reason of a fatal library check (accountant CheckFailure.Fatal):
// one that means the operation list or its attribution is wrong. The library decides which checks are
// fatal; this only names them. A fatal check this switch does not know gates all the same, as
// FallbackCheckOther, so a check the library adds later never passes silently.
func fatalCheckFallback(check string) string {
	switch check {
	case accountant.CheckSum:
		return FallbackCheckSum
	case accountant.CheckUniqueChange:
		return FallbackCheckUniqueChange
	case accountant.CheckSpans:
		return FallbackCheckSpans
	}
	return FallbackCheckOther
}

// PricedByAccountant is CompRow.PricedBy on a row the accountant library priced.
const PricedByAccountant = "accountant"

// PricedByNone is CompRow.PricedBy on a row of a request the accountant path handled but could not
// price (a fallback): it books no saving, and the read-time passes must not value it with the
// legacy rule. Any non-empty PricedBy means the stored split stands as written; only a row written
// before the switch has it empty.
const PricedByNone = "none"

// KindEnvelope is the CompRow.Kind of a row the accountant override adds: an envelope operator, the
// ended-change break or the interaction, none of which is a pipeline component.
const KindEnvelope = "envelope"

// EndedChangeComponent is the row that carries the break an ended change left behind.
const EndedChangeComponent = "ended_change"

// InteractionComponent is the row that carries the interaction on a rescued request
// (AccFigure.InteractionUSD): counted, and owned by no component.
const InteractionComponent = "interaction"

// DeferComponent is the library's component name for tool deferral: the operator context-guru.tooldefer, which the
// proxy reports from the bodies (accountant R56). Its figure is the deferral's saving.
const DeferComponent = accwire.OpToolDefer

// envelopeComponents are the operators that change the request envelope (tools, system, cache
// marks) and may have no pipeline row: the proxy reports them from a body diff
// (accwire/envelope.go). expand, adjudicate and expand_restore (the copy fixed_restore
// inserts, #481) are never pipeline components; headttl is a host option; toolschema, toolfilter and cachesplit are components whose row can be absent when
// the rewrite ran from an equivalent gate (cacheinject runs the split too).
var envelopeComponents = map[string]bool{
	accwire.OpExpand: true, accwire.OpAdjudicate: true, accwire.OpHeadTTL: true, accwire.OpExpandRestore: true,
	"toolschema": true, "toolfilter": true, "cachesplit": true,
	accwire.OpTailBreakpoint: true, accwire.OpToolDefer: true,
}

// AccountantEstimated reports whether the applied figure is an estimate: the library could not rule
// out a break (penalty_unknown, accountant design §6.4). It is false when the override did not apply.
// It reads PriceEstimated, the stored field the dashboard marks the figure by.
func (e *Event) AccountantEstimated() bool { return e.PriceEstimated }

// AccountantFallback is the reason PriceMode booked no saving, or "" when the override
// applied. It is "" too on an event priced with Price alone, which never tries the override.
// An event with no usage or no price books no saving too, and its reason is FallbackUnpriced.
func (e *Event) AccountantFallback() string { return e.accFallback }

// Causes behind FallbackUnpriced. They are for the log line only: no stored column and no API field
// carries them, so a new cause here changes nothing a client can read.
const (
	UnpricedNoUsage = "no_usage" // the request has no billed token counts (accounting is not complete)
	UnpricedNoPrice = "no_price" // the model has no price
)

// UnpricedWhy names the cause when AccountantFallback is FallbackUnpriced, and is "" for any other
// reason. It is UnpricedNoUsage or UnpricedNoPrice when the event itself cannot be priced, or the
// library's "opID:reason" for the first operation or effect it could not price.
func (e *Event) UnpricedWhy() string {
	if e.accFallback != FallbackUnpriced {
		return ""
	}
	return e.accUnpricedWhy
}

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
	e.accFallback, e.PriceEstimated, e.AccountantPriced, e.EstimatedSavedUSD, e.EstimatedSavedLowUSD, e.EstimateHeld = "", false, false, 0, 0, false
	e.accUnpricedWhy = ""
	e.clearAccTokens()
	if !e.priceCost(p, accountingComplete) {
		e.accFallback = FallbackUnpriced
		e.accUnpricedWhy = UnpricedNoUsage
		if accountingComplete {
			e.accUnpricedWhy = UnpricedNoPrice // usage is complete, so the zero price is the cause
		}
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
	case e.accServerHeldHistory:
		return FallbackServerHeldHistory // before no_result: say the real reason, not that nothing was attached
	case e.accRes == nil:
		return FallbackNoResult
	case e.accOpsDropped != 0:
		return FallbackOpsDropped
	case e.accObserve:
		return FallbackObserve
	}
	for _, c := range e.accRes.Checks {
		if c.Fatal() {
			return fatalCheckFallback(c.Check)
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
			// "opID:reason". A request-level refusal has no operation, so its id is empty: drop the colon.
			e.accUnpricedWhy = strings.TrimPrefix(u, ":")
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
		fig.ByComponent[accwire.OpHeadTTL] += fig.TTLUSD
		fig.DeltaUSD += fig.TTLUSD
		if !rows[accwire.OpHeadTTL] && !inAdded(added, accwire.OpHeadTTL) {
			added = append(added, CompRow{Component: accwire.OpHeadTTL, Kind: KindEnvelope, Acted: true, Mutated: true})
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
	// The interaction is counted and owned by no component too: a row of its own, like the ended change.
	if fig.InteractionUSD != 0 {
		if rows[InteractionComponent] {
			return FallbackDuplicateName
		}
		added = append(added, CompRow{Component: InteractionComponent, Kind: KindEnvelope, Acted: true})
		fig.ByComponent[InteractionComponent] = fig.InteractionUSD
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
		c.Tokens = fig.TokByComponent[c.Component]
	}
	// The tokens of the effects outside the component rows. The headttl and ended_change rows carry
	// the dollars of TTL and EndedBreak; their tokens are here, never on those rows, so no token is
	// counted twice.
	e.AccTokens = AccEffectTokens{KeepAlive: fig.TokKeepAlive, Split: fig.TokSplit, TTL: fig.TokTTL, EndedBreak: fig.TokEndedBreak}
	e.BaselineCostUSD = e.CostUSD + fig.DeltaUSD
	// Tool deferral is priced here, from the bodies (accountant R56): the definitions it left out as a shrink
	// of tools positions, the search tool and the names list as growth. The library's figure for it sits on the
	// tooldefer row and in the baseline delta above, which the Overview shows as the preset's saving. It is the
	// deferral's saving, so it leaves the baseline and goes to the lever Overview adds beside it (DeferNetUSD),
	// where the deferral card reads it. The total is the same: the figure is moved, not added. What the bodies
	// cannot show is the searches and the definitions they loaded, which come back in the response: the ledger
	// still owns that cost, and the lever takes it off (#908, review of #1003).
	//
	// A request the library held (below) keeps the ledger's net on the lever as before, and the deferral is
	// left out of the held estimate, so the figure appears in one place and not in two.
	deferFig := fig.ByComponent[DeferComponent]
	if e.DeferNetUSD != 0 || e.DeferGrossUSD != 0 || deferFig != 0 {
		if !estimated {
			e.BaselineCostUSD -= deferFig
			e.DeferNetUSD = deferFig - e.DeferSearchUSD
			if e.DeferNetUSD == 0 {
				// A guard, not a known case. -0 - +0 is -0 under IEEE 754, but deferFig comes from a map entry that
				// starts at +0, and +a + -a and +0 + -0 are +0, so deferFig is never -0 and this branch is not
				// reached today. If it ever were, a plain zero is what the field should serialize.
				e.DeferNetUSD = 0
			}
		}
	}
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
		// What is held out: fig.DeltaUSD less the deferral's own figure, which is every other component's
		// figure, the head-TTL premium and rescue legs (the headttl row), the ended-change break, the
		// interaction (the interaction row), and the filter's part. The deferral's figure is not held with them: a held request keeps the ledger's net
		// on the lever, so it appears once (review of #1003). The figure can
		// be negative when the request's net effect is a cost (a tooldefer names list, an injected
		// expand or adjudicate declaration): the cost is held out with the rest, and the readers render
		// a negative figure as a cost. What stays booked, each in its own column outside the baseline:
		// the keep-alive rescue (keepalive_saved_usd) and the split saving (cachesplit_saved_usd), which
		// do not depend on a break. The filter's dollars are owned by the measured credit
		// (declFilterInBaseline is false for a held row), not by this figure.
		e.EstimatedSavedUSD = fig.DeltaUSD - deferFig
		e.EstimatedSavedLowUSD = accrec.HeldFloorUSD(fig.Fig, fig.DeltaUSD-deferFig, accwire.Operator(DeferComponent))
		e.EstimateHeld = true
		e.BaselineCostUSD = e.CostUSD
		for i := range e.Components {
			e.Components[i].SavedUSD, e.Components[i].SavedFirstUSD, e.Components[i].SavedReplayUSD = 0, 0, 0
			e.Components[i].Tokens = TokenFlow{}
		}
		// The tokens of what is held out are held out too: the head-TTL legs and the ended-change
		// break are part of fig.DeltaUSD. The rescue and the split stay, as their dollars do.
		e.AccTokens.TTL, e.AccTokens.EndedBreak = TokenFlow{}, TokenFlow{}
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
// the declaration filter and the schema rewrite (accwire/envelope.go reports them from a body
// diff), the two declarations the proxy injects, and tool deferral, whose deferred definitions are a
// shrink of tools positions (accountant R56, #908). Their tokens are not in TokensBefore, which counts
// message text only. Deferral's names list is a growth in messages and saves nothing, so it adds
// no saved tokens to the bound.
var toolSectionOperators = map[string]bool{
	accwire.Operator("toolfilter"): true, accwire.Operator("toolschema"): true,
	accwire.Operator(accwire.OpExpand): true, accwire.Operator(accwire.OpAdjudicate): true,
	accwire.Operator(accwire.OpToolDefer): true,
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
