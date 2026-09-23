package dash

import (
	"database/sql"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/kvcache"
)

// What a summary was WORTH, measured over a fixed span of work after it was made.
//
// # The question
//
// summarize's shipped trigger fires near the context-window edge and near cache expiry, on the
// argument that a cold full-prefix rewrite costs ~12x a cold compacted one. That argument was made
// from a measurement of what cold rewrites cost, not from a measurement of what compacting
// actually saved. This file is the latter.
//
// An EPISODE is one summary plus the span of work that follows it: from the turn a summary was
// produced (t0) until the session has been billed 10% more of the model's context window. Inside
// that span, every turn that re-sent the compacted transcript instead of the full one is worth
// something, and the sum of those turn-by-turn amounts IS the saving. It is not a projection.
//
// # Why almost nothing is computed here
//
// The economics already exist and are already right. Event.baselineDeltaUSD prices a removal as
// `SavedUnique × cacheWriteRate + (Saved − SavedUnique) × repeatRate`, and repeatRate prices the
// re-sent remainder at the cache-READ rate on a turn whose cache hit, the cache-CREATION rate on a
// turn whose cache missed, and fresh otherwise — with three documented guards against inflation.
// request_components.saved_usd stores that per component per request, priced at write time with
// the rates that were in force.
//
// So both halves of the question — "what would the cold rewrites have cost uncompacted" and "the
// warm reads are cheaper too" — are already per-request columns, attributed to summarize by name.
// This file SCOPES them into episodes and SPLITS them by the cache state of the turn that earned
// them. It introduces no new pricing, which is the reason it can be trusted: a second pricing path
// would be a second thing to keep in agreement with Event.repeatRate.
//
// # Two rulers, and which one each half uses
//
// The episode's span is measured in the PROVIDER's billed input (fresh + cache read + cache
// write). Not tokens_before: that is schema.MessagesTokens, message text only, a median 3.38x
// below billed input (see Overview.EstimatorDivergence) — so a "10% of the window" span sized
// against it would really be ~34% of the window, and the same mistake Trigger.Fires shipped with.
//
// tokens_before is still read, for exactly one thing it is sound for: DETECTING A DROP. A turn
// arriving smaller than the one before it means the client compacted its own transcript, and that
// is a scale-free comparison of one measure against itself.
//
// # What is deliberately absent
//
// A conversation whose model window is not EXACTLY known contributes nothing — not a zero. The
// substring table of last resort answers 200,000 for every Opus against a real 1,000,000, and a
// 10% span sized against that is 20,000 tokens of work rather than 100,000. Counted as
// WindowUnknown and excluded, the same rule Trigger.FracResolvable applies for the same reason.

// compactRow is one request as the episode walk needs it: the two size measures, the cache
// verdict, and summarize's own per-request figures.
type compactRow struct {
	Tenant, Session, Model string
	TS                     int64
	// TokensBefore is our own tokenizer's count of the message text. Used ONLY to detect a
	// client-side compaction (a drop), never as a fraction of the window.
	TokensBefore int64
	// Billed is what the provider counted as input: fresh + cache read + cache write. The
	// episode's span is measured in these, because the context window is stated in them.
	Billed int64
	// MissReason is Event.AttributeCache's verdict: hit, ttl_expiry, cold_start, prefix_change,
	// unknown. It is what splits the credit.
	MissReason string
	// CacheWrite / CacheWrite1h are this turn's cache-creation tokens, and the 1h-tier subset.
	// Read to price the invalidation this component caused, and to classify a write as NEW content
	// or as re-creation of an expired prefix (see newContentBilled).
	CacheWrite, CacheWrite1h int64
	// CacheRead is what the provider served from its cache. Never new content, and its presence is
	// what distinguishes a newly-written TAIL from the re-creation of a whole expired prefix.
	CacheRead int64
	// CGLLMCostUSD is context-guru's own model spend on this request — the summarizer's call.
	CGLLMCostUSD float64
	// SavedGross is summarize's own token count of what it removed on this turn — including on a
	// replay, where the same span is removed again. It is the credit's QUANTITY; the RATE comes from
	// this row's cache verdict. See creditTurn for why the stored saved_usd is not the credit.
	//
	// Measured on our tokenizer's ruler (message text only), so it under-states the provider's count
	// of the same content by ~12% at the transcript sizes compaction happens at — issue #240.
	// Conservative for a savings figure, and stated in the panel's assumptions.
	SavedGross int64
	// SavedUSD is summarize's share of this request's baseline delta, priced at write time. Read
	// ONLY to detect that summarize did something priceable on this turn, never as a credit — see
	// creditTurn.
	SavedUSD float64
	// Events is summarize's Report.Events for this request, as stored JSON. It says whether this
	// turn PAID for a summary or replayed one, which is what identifies an episode's t0.
	Events string
	// Acted is the stored flag: summarize removed tokens on this turn. True on a replay too.
	Acted bool
}

// Episode states. A closed episode is the only one that contributes to a total.
const (
	// EpisodeClosed: the span completed — the session was billed the full 10% more.
	EpisodeClosed = "closed"
	// EpisodeOpen: the window has not closed inside the queried range. Reported so a short
	// query window is distinguishable from a session that stopped.
	EpisodeOpen = "open"
	// EpisodeVoided: the client compacted its own transcript inside the span. The client did the
	// very thing the trigger exists to get ahead of, so the remainder is not comparable to a
	// world where we had not compacted. Counted, never averaged in.
	EpisodeVoided = "voided_client_compaction"
)

// Episode provenance. Kept apart and NEVER summed, the same discipline declcredit.go keeps
// between a measured saving and a modelled one.
const (
	// EpisodeRecorded: t0 carried offload.EventSummaryStarted (it commissioned a summary) or
	// offload.EventFreshSummary (the inline path committed one). A fact about that turn.
	EpisodeRecorded = "recorded"
	// EpisodeInferred: t0 acted and carried none of summarize's replay event names, so it must
	// have been a fresh summary. Sound, but it is an inference from an absence, and it is the
	// only way to see episodes from BEFORE the event existed — which is the whole history of the
	// old size-only trigger, and therefore the only available comparison.
	EpisodeInferred = "inferred"
)

// CompactionEpisode is one summary and the span of work after it.
type CompactionEpisode struct {
	TenantID   string `json:"tenant_id"`
	SessionID  string `json:"session_id"`
	Model      string `json:"model"`
	Provenance string `json:"provenance"`
	State      string `json:"state"`
	StartTS    int64  `json:"start_ts"`
	EndTS      int64  `json:"end_ts"`
	// Window is the model's context window, always exact (see the file comment), and Span is
	// the billed input the episode covered — StartBilled to EndBilled.
	Window      int   `json:"window"`
	StartBilled int64 `json:"start_billed"`
	EndBilled   int64 `json:"end_billed"`
	// NewContentBilled is the provider-billed NEW content since t0 — fresh input plus the
	// newly-written tail on turns whose cache hit. It is the axis the span closes on: monotone by
	// construction, in the window's own units, and excluding both the re-sent prefix and the
	// re-creation of an expired one. See the derivation in conversationEpisodes for why the two
	// obvious axes (per-turn size, cumulative spend) each fail.
	NewContentBilled int64 `json:"new_content_billed"`
	Turns            int64 `json:"turns"`
	// The credit, split by the cache state of the turn that earned it. ColdCreditUSD is the
	// headline: turns whose entry had lapsed, where an uncompacted prefix would have been
	// re-written in full. ReadCreditUSD is every warm turn's re-sent remainder at 0.1x, which is
	// usually the larger of the two simply because warm turns are far more numerous.
	ColdCreditUSD float64 `json:"cold_credit_usd"`
	ReadCreditUSD float64 `json:"read_credit_usd"`
	// OtherCreditUSD is cold_start and unknown turns. Reported apart rather than folded into
	// either: a session's first turn had no entry to preserve, and "unknown" is not a verdict.
	OtherCreditUSD float64 `json:"other_credit_usd"`
	// InvalidationDebitUSD is what THIS component's own rewrite cost on t0: the cache-creation
	// tokens that turn wrote, at that model's write rates. An UPPER BOUND, deliberately — some
	// of that write was transcript growth that would have been paid anyway, and no column says
	// how much. Overstating our own cost is the conservative direction for a savings figure.
	InvalidationDebitUSD float64 `json:"invalidation_debit_usd"`
	// SummarizerCostUSD is context-guru's own model spend inside the episode: t0's summary and
	// any roll-forward that happened before the span closed.
	SummarizerCostUSD float64 `json:"summarizer_cost_usd"`
	// NetUSD is credits − debits. Negative is a real outcome and is reported as one.
	NetUSD float64 `json:"net_usd"`
	// Priced is false when this model has no rates. Then every dollar here is absent rather
	// than zero — an unpriced model is not a free one.
	Priced bool `json:"priced"`
}

// CompactionEpisodeGroup is one provenance's totals.
type CompactionEpisodeGroup struct {
	Provenance           string  `json:"provenance"`
	Episodes             int64   `json:"episodes"`
	Closed               int64   `json:"closed"`
	Open                 int64   `json:"open"`
	Voided               int64   `json:"voided"`
	Turns                int64   `json:"turns"`
	ColdCreditUSD        float64 `json:"cold_credit_usd"`
	ReadCreditUSD        float64 `json:"read_credit_usd"`
	OtherCreditUSD       float64 `json:"other_credit_usd"`
	InvalidationDebitUSD float64 `json:"invalidation_debit_usd"`
	SummarizerCostUSD    float64 `json:"summarizer_cost_usd"`
	NetUSD               float64 `json:"net_usd"`
	// UnpricedEpisodes are closed episodes on a model with no rates: counted, never valued.
	UnpricedEpisodes int64 `json:"unpriced_episodes"`
	// OpenNetUSD is the net of spans that have NOT finished — money committed whose payoff is
	// still accruing. Reported apart from NetUSD rather than excluded.
	//
	// Excluding it was survivorship one level below the coverage line: an unfinished span is
	// exactly the case where we paid for a summary and have not yet recouped it, so dropping
	// those made the panel systematically optimistic. It is kept OUT of NetUSD because a partial
	// figure grows as the time range widens, which would make the settled total depend on when
	// you looked.
	//
	// It is not automatically a loss, and that is why it is computed rather than assumed: we are
	// behind only when 1.25xC > 0.1xF, i.e. when the compacted prefix exceeds ~8% of the full
	// one. At better than roughly 12.5:1 compression the compaction turn is already cheaper on
	// its own turn, before any later read saves anything.
	OpenNetUSD   float64 `json:"open_net_usd"`
	OpenTurns    int64   `json:"open_turns"`
	VoidedNetUSD float64 `json:"voided_net_usd"`
}

// CompactionCoverage is the other half of the question, and the panel is dishonest without it.
//
// An episode only exists where a summary was made. Reporting episodes alone answers "when we
// helped, how much did we help" — which is survivorship, and always positive. The number that
// decides whether the trigger is worth its default is how many qualifying conversations produced
// NO episode at all, and what the cold rewrites cost them.
type CompactionCoverage struct {
	// NoAttributableSpan counts conversations excluded because the fill threshold sits at or above
	// the CLIENT's compaction ceiling for that model: the client compacts before we would ever fire,
	// so there is no window in which anything is attributable to us and this component cannot help
	// there. A configuration to change, not an empty dataset — and a COUNT rather than a flag,
	// because the ceiling is per model and a query can span one model where this holds and another
	// where it does not.
	NoAttributableSpan int `json:"no_attributable_span"`
	// FillFrac is the threshold a conversation had to reach to qualify, as a fraction of its
	// window, measured in billed input. Reported because it is a parameter, not a law.
	FillFrac float64 `json:"fill_frac"`
	// Conversations reached FillFrac and had an exact window.
	Conversations int64 `json:"conversations"`
	WithEpisode   int64 `json:"with_episode"`
	NoEpisode     int64 `json:"no_episode"`
	// NoEpisodeColdUSD is what the ttl_expiry turns of those conversations actually paid in
	// cache creation — the money an episode there might have reduced. Not a claim that it would
	// have: it is the size of the opportunity, which is why it is named for the population and
	// not for a saving.
	NoEpisodeColdUSD float64 `json:"no_episode_cold_usd"`
	// WindowUnknown is conversations excluded because their model's window is a guess or
	// unknown. Counted so the population is never silently narrowed.
	WindowUnknown int64 `json:"window_unknown"`
	// Unpriced is qualifying conversations on a model with no rates.
	Unpriced int64 `json:"unpriced"`
}

// CompactionAssumptions is the server stating its own arithmetic, so the page prints it rather
// than restating it in a template nothing tests — the rule the KV-cache page already keeps.
type CompactionAssumptions struct {
	// SpanFrac is the explicit `?span=` override, or 0 meaning the span was DERIVED per model from
	// that model's client ceiling. The spans actually used are in ClientCeilings, one per model,
	// because the ceiling is a per-model fact and one number here would average things that are not
	// comparable.
	SpanFrac     float64 `json:"span_frac"`
	FillFrac     float64 `json:"fill_frac"`
	SpanMeasure  string  `json:"span_measure"`
	CreditSource string  `json:"credit_source"`
	// CreditQuantityNote names the known error in the credit's QUANTITY, which is a different thing
	// from its rate: the tokens are counted on our own ruler. See issue #240.
	CreditQuantityNote string `json:"credit_quantity_note"`
	ColdLabel          string `json:"cold_label"`
	ReadLabel          string `json:"read_label"`
	DebitBound         string `json:"debit_bound"`
	VoidRule           string `json:"void_rule"`
	WindowRule         string `json:"window_rule"`
	KnownOmission      string `json:"known_omission"`
	OpenRule           string `json:"open_rule"`
	SpanMeasureNote    string `json:"span_measure_note"`
	// SpanRule says WHY the span is the width it is: it is an attribution boundary derived from the
	// fill threshold, not a tuning knob. See spanFor.
	SpanRule string `json:"span_rule"`
	// ClientCeilings is one entry per model this query actually measured, because the ceiling is a
	// fact about the CLIENT's behaviour on that model and a single number would be an average of
	// things that are not comparable. Each carries its provenance, so a reader can tell which
	// published figures rest on a measurement and which on an assumption.
	ClientCeilings []ClientCeilingUsed `json:"client_ceilings"`
}

// CompactionEpisodes is the whole view.
type CompactionEpisodes struct {
	ByProvenance []CompactionEpisodeGroup `json:"by_provenance"`
	// Episodes is the drill-down list, newest first, capped. The groups above are computed over
	// everything, not over this slice.
	Episodes    []CompactionEpisode   `json:"episodes"`
	Coverage    CompactionCoverage    `json:"coverage"`
	Assumptions CompactionAssumptions `json:"assumptions"`
	Pricing     *kvcache.PriceList    `json:"pricing"`
	FirstTS     int64                 `json:"first_ts"`
	LastTS      int64                 `json:"last_ts"`
	Scanned     int64                 `json:"scanned"`
}

// Defaults for the two fractions. Both are parameters rather than constants of nature, and both
// are reported in Assumptions.
const (
	// defaultFillFrac matches summarize's own shipped min_request_frac, so the coverage
	// population is the one the trigger is actually deciding about.
	defaultFillFrac = 0.90
	// defaultClientCeilingFrac is where the CLIENT is assumed to compact its own transcript, as a
	// fraction of the model window measured in PROVIDER-BILLED input — the same ruler the fill gate
	// uses, and deliberately not the ruler the client's own indicator uses.
	//
	// 0.996 was measured on a real Claude Code session on haiku
	// (scripts/scenarios/a-firing-rate.sh): the last turn before the client compacted billed 199,184
	// of a 200,000 window. Rounded to 1.0 rather than pinned at 0.996, because the measurement is one
	// client on one model and a spuriously precise default invites treating it as established.
	//
	// It is an assumption about the CLIENT, not a property of the model, and it is a parameter for
	// that reason. See spanFor, and #239 for learning it rather than assuming it.
	defaultClientCeilingFrac = 1.00
	// episodeListCap bounds the drill-down list. The aggregates are unbounded.
	episodeListCap = 200
)

// spanFor is the span: the distance from where WE fire to where the CLIENT would have compacted
// anyway, in the units the fill is measured in. ok=false means that distance does not exist.
//
// # It is an ATTRIBUTION boundary, and the far end of it belongs to the CLIENT, not to the model
//
// We fire at fillFrac of the window. In the counterfactual world where we did NOT compact, the
// conversation keeps growing — and not forever, because the client compacts when it reaches its own
// ceiling. Past that point both worlds are running on a summarized transcript and nothing further is
// attributable to us. So the span is `ceilingFrac - fillFrac`.
//
// THE CEILING IS THE CLIENT'S, AND IT IS NOT THE MODEL WINDOW. An earlier version of this function
// computed `1 - fillFrac`, which silently asserted that the client runs the transcript all the way to
// the model's limit. That is an assumption about the client, not arithmetic about the model, and the
// repo owner caught it being stated as the latter.
//
// # What is actually measured, and in which ruler — this is the third ruler problem in this file
//
// On the one client run available (scripts/scenarios/a-firing-rate.sh, real Claude Code on haiku) the
// last turn before the client compacted read:
//
//	provider-billed input         199,184   = 0.996 of the window   <-- the ruler THIS gate uses
//	Claude Code's own count        ~167,000  = 0.835                 <-- the ruler the CLIENT uses
//	our own message-text count     147,493   = 0.737                 <-- the ruler tokens_before uses
//
// The fill gate compares against BILLED input, because a context window is stated in billed tokens.
// So the ceiling has to be expressed in billed tokens too, and 0.996 is the measured figure — which is
// why the default is 1.0 rather than the 0.835 the client's own indicator would suggest. Quoting the
// client's number here would be the same units error Trigger.Fires shipped with.
//
// It is still a measurement of ONE client, ONE model and ONE version. A deployment whose client
// compacts earlier — a configured auto-compact threshold, a different agent, a wrapper of its own —
// needs its own value, which is why this is a parameter with an override (`?ceiling=`) rather than a
// constant. Learning it per client is #239, and this is the first place the answer changes a published
// number rather than only a firing decision.
//
// # ok=false is a real answer, not an error
//
// If the ceiling is at or below the fill threshold, the client compacts BEFORE we would ever fire, so
// there is no window in which anything is attributable to us — and on that deployment this component
// cannot help at all. That is worth reporting loudly. The previous version substituted the shipped
// 0.10 in that case, which invented a span the configuration says does not exist and would have
// published credits for turns the client had already compacted away.
//
// # What the span does NOT bound
//
// Wall-clock time. A cold event happens because of ELAPSED TIME while the span advances on NEW
// CONTENT, and those are independent — anti-correlated in the helpful direction, in fact: a session
// idle enough for its entry to lapse is by definition not accruing new content, so the span cannot be
// closing while a cold event becomes possible. A session can go cold having added 2% of the window,
// and the next turn then pays a write that uncompacted would have covered most of it. An earlier
// reading of a scenario run concluded the opposite; that run's own bridging turns had read two large
// files and consumed the whole span before the idle gaps began.
func spanFor(fillFrac, ceilingFrac float64) (float64, bool) {
	span := ceilingFrac - fillFrac
	if span <= 0 {
		return 0, false
	}
	return span, true
}

// windowFn resolves a model's context window and says whether it is exact. Injected so the walk
// is a pure function under test — no resolver, no network, no clock.
type windowFn func(model string) (tokens int, exact bool)

// priceFn resolves a model's cache-write rates. Injected for the same reason.
type priceFn func(model string) kvcache.Pricing

// walkCompactEpisodes is the whole measurement, over rows already ordered by
// (tenant, session, model, ts, id).
//
// A pure function on purpose. Every interesting case here — a span that closes exactly on the
// boundary, a client compaction mid-span, a roll-forward inside an open episode, a model whose
// window is a guess — is a sequence of rows, and a test that has to build a database to express
// one is a test nobody writes the fifth variant of.
func walkCompactEpisodes(rows []compactRow, window windowFn, price priceFn,
	spanFrac, fillFrac, ceilingFrac float64) *CompactionEpisodes {
	// fillFrac FIRST: the span derives from it. Reversing these two silently derived the span from a
	// zero fill, i.e. the whole ceiling — a span ten times too wide.
	if fillFrac <= 0 {
		fillFrac = defaultFillFrac
	}
	// THE CEILING IS PER MODEL, so it is resolved per conversation below rather than once here — it
	// is a fact about the CLIENT's behaviour on a given model, and a query spanning haiku and opus
	// must not average the two. `ceilingFrac > 0` is the explicit `?ceiling=` override, which applies
	// to every conversation because an operator using it is investigating one deployment.
	explicitCeiling := ceilingFrac > 0
	// Likewise the span: derived per conversation from that conversation's own ceiling. A non-zero
	// spanFrac here is the explicit `?span=` override.
	explicitSpan := spanFrac > 0
	out := &CompactionEpisodes{
		Coverage:    CompactionCoverage{FillFrac: fillFrac},
		Assumptions: compactAssumptions(spanFrac, fillFrac),
		Scanned:     int64(len(rows)),
	}
	groups := map[string]*CompactionEpisodeGroup{}
	group := func(p string) *CompactionEpisodeGroup {
		g, ok := groups[p]
		if !ok {
			g = &CompactionEpisodeGroup{Provenance: p}
			groups[p] = g
		}
		return g
	}

	for _, conv := range groupConversations(rows) {
		if len(conv) == 0 {
			continue
		}
		out.FirstTS, out.LastTS = spanTS(out.FirstTS, out.LastTS, conv)

		w, exact := window(conv[0].Model)
		if !exact || w <= 0 {
			// Counted only if it would otherwise have qualified, which cannot be known
			// without a window — so count the conversation itself. Excluding it silently is
			// what this counter exists to prevent.
			out.Coverage.WindowUnknown++
			continue
		}
		p := price(conv[0].Model)

		// This conversation's own span, from this MODEL's compaction point.
		ceil := compactionpoint.For(conv[0].Model)
		if explicitCeiling {
			ceil = compactionpoint.Point{Frac: ceilingFrac, Source: compactionpoint.Assumed,
				Note: "supplied explicitly on the request"}
		}
		convSpan := spanFrac
		if !explicitSpan {
			var ok bool
			convSpan, ok = spanFor(fillFrac, ceil.Frac)
			if !ok {
				// THE TRIGGER IS AT OR ABOVE THIS CLIENT'S CEILING for this model, so the client
				// compacts before we would ever fire and nothing about this conversation is
				// attributable to us. Counted as its own condition rather than folded into an empty
				// result: "no episodes" and "no episodes are POSSIBLE on this model" are different
				// facts, and only the second one names a configuration to change.
				out.Coverage.NoAttributableSpan++
				continue
			}
		}
		out.Assumptions.noteCeiling(conv[0].Model, ceil, convSpan)
		eps := conversationEpisodes(conv, w, p, convSpan)

		// Coverage is about the conversation, not the episodes: did it get big enough to be
		// this trigger's business, and did anything happen when it did?
		if maxBilled(conv) >= int64(fillFrac*float64(w)) {
			out.Coverage.Conversations++
			if !p.Known {
				out.Coverage.Unpriced++
			}
			if len(eps) > 0 {
				out.Coverage.WithEpisode++
			} else {
				out.Coverage.NoEpisode++
				if p.Known {
					out.Coverage.NoEpisodeColdUSD += coldCreationUSD(conv, p)
				}
			}
		}

		for _, e := range eps {
			g := group(e.Provenance)
			g.Episodes++
			switch e.State {
			case EpisodeClosed:
				g.Closed++
			case EpisodeOpen:
				g.Open++
			case EpisodeVoided:
				g.Voided++
			}
			// ONLY a closed episode contributes money. An open one is a partial span whose
			// total would grow if the query window moved; a voided one is not comparable at
			// all. Both are counted above so the reader can see how many were dropped.
			switch {
			case !e.Priced:
				// Unpriced: counted, never valued. An unpriced model is not a free one.
				if e.State == EpisodeClosed {
					g.UnpricedEpisodes++
				}
			case e.State == EpisodeClosed:
				g.Turns += e.Turns
				g.ColdCreditUSD += e.ColdCreditUSD
				g.ReadCreditUSD += e.ReadCreditUSD
				g.OtherCreditUSD += e.OtherCreditUSD
				g.InvalidationDebitUSD += e.InvalidationDebitUSD
				g.SummarizerCostUSD += e.SummarizerCostUSD
				g.NetUSD += e.NetUSD
			case e.State == EpisodeOpen:
				// Still accruing: its own total, so the settled figure stays comparable while the
				// outstanding exposure stays visible.
				g.OpenNetUSD += e.NetUSD
				g.OpenTurns += e.Turns
			case e.State == EpisodeVoided:
				// The client compacted mid-span, so the payoff is not comparable — but we did
				// spend, and that is reported rather than dropped.
				g.VoidedNetUSD += e.NetUSD
			}
			if len(out.Episodes) < episodeListCap {
				out.Episodes = append(out.Episodes, e)
			}
		}
	}
	for _, p := range []string{EpisodeRecorded, EpisodeInferred} {
		if g, ok := groups[p]; ok {
			out.ByProvenance = append(out.ByProvenance, *g)
		}
	}
	return out
}

// conversationEpisodes finds every episode in one conversation's ordered rows.
func conversationEpisodes(conv []compactRow, w int, p kvcache.Pricing, spanFrac float64) []CompactionEpisode {
	span := int64(spanFrac * float64(w))
	if span <= 0 {
		// A degenerate span would close every episode on its own t0, measuring nothing. Emit
		// none rather than a population of zero-width episodes.
		return nil
	}
	var out []CompactionEpisode
	var cur *CompactionEpisode
	var prevTokensBefore int64
	for _, r := range conv {
		// ONE ROW CAN BELONG TO TWO EPISODES: the turn that closes a span AND itself commissions
		// the next summary. Its SAVING was earned inside the span that is closing and is credited
		// there; its COSTS — the new summary's own call, and the cache write that rewrite caused —
		// are the next span's investment and belong to the span it OPENS.
		//
		// This used to be carried in a `credited` flag that stated the rule and was then discarded
		// as `_ = credited`, so such a row was debited in BOTH episodes and the sum of episodes
		// exceeded the money that existed — the one direction a savings figure must never go. A
		// review found it. The rule is now structural (`closing && opensNew` below) rather than
		// held in a variable, so it cannot be declared and then forgotten a second time.
		if cur != nil {
			// A DROP in our own message-token count means the client compacted its own
			// transcript. Scale-free (one measure against itself), which is why this is the
			// one thing tokens_before is used for here.
			if prevTokensBefore > 0 && r.TokensBefore > 0 && r.TokensBefore < prevTokensBefore {
				cur.State = EpisodeVoided
				cur.EndTS, cur.EndBilled = r.TS, r.Billed
				finishEpisode(cur, p)
				out = append(out, *cur)
				cur = nil
			}
		}
		if cur != nil {
			cur.Turns++
			cur.EndTS, cur.EndBilled = r.TS, r.Billed
			creditTurn(cur, r, p)
			// CUMULATIVE NEW CONTENT, which is neither of the two obvious axes.
			//
			// Per-turn SIZE does not work: compaction reduces what a turn sends, so `r.Billed`
			// falls the moment a summary lands — on the live run 154,584 -> 31,682 — and a span
			// waiting for it to exceed t0's size never closes at all.
			//
			// Cumulative SPEND does not work either, and that was the first correction's mistake:
			// billed input re-counts the whole prefix every turn, so one turn of a qualifying
			// conversation already exceeds 10% of the window. On the live run the span was 20,000
			// against a next-turn 31,682, so it closed on the turn immediately after t0 — a span
			// of seconds, in which no cold miss can occur, making the headline ColdCreditUSD
			// bucket structurally empty. The threshold kept its growth-shaped 0.10 while the axis
			// became a spend, which is the same units error one level up.
			//
			// So the axis is what NEW CONTENT entered the conversation, in the provider's own
			// units, which is comparable to the window and monotone by construction:
			//
			//   - fresh_input is new by definition.
			//   - cache_write on a turn that HIT is the newly-written tail — content that was not
			//     in the entry the turn read from, i.e. new.
			//   - cache_write on a turn that MISSED is re-creation of a prefix that already
			//     existed. Counting it would call the whole transcript "new" every time an entry
			//     expired, which is exactly the over-count that made a cold t0 close its own span.
			//   - cache_read is the re-sent prefix and is never new.
			//
			// Computed BEFORE the costs are attributed, because which episode owns this row's
			// costs depends on whether the span ends here.
			cur.NewContentBilled += newContentBilled(r)
			closing := cur.NewContentBilled >= span
			// opensNew: this row commissions a summary, so a new episode begins on it. Together
			// with `closing` it identifies the one row that belongs to two episodes, whose costs
			// are charged to the span it OPENS and not to the one it closes.
			opensNew := isFreshSummary(r) != ""
			if !(closing && opensNew) {
				chargeRowCosts(cur, r, p, opensNew)
			}
			if closing {
				cur.State = EpisodeClosed
				finishEpisode(cur, p)
				out = append(out, *cur)
				cur = nil
			}
		}
		if cur == nil {
			if prov := isFreshSummary(r); prov != "" {
				cur = &CompactionEpisode{
					TenantID: r.Tenant, SessionID: r.Session, Model: r.Model,
					Provenance: prov, State: EpisodeOpen, Window: w,
					StartTS: r.TS, EndTS: r.TS,
					StartBilled: r.Billed, EndBilled: r.Billed,
					Turns:  1,
					Priced: p.Known,
					// t0's own cache creation, but ONLY when we actually caused it — see
					// causedTheWrite. On a cold t0 the entry was already gone and that write was
					// due whatever we did, so charging it invents a cost.
					InvalidationDebitUSD: causedWriteUSD(r, p),
					SummarizerCostUSD:    r.CGLLMCostUSD,
				}
				// NO CREDIT ON THE COMPACTION TURN.
				//
				// At t0 nothing has been saved — we have only SPENT: a model call, and a cache
				// write for the new smaller prefix. The saving arrives on later turns, as reads
				// against a shorter transcript and as cold rewrites that cost 1.25xC instead of
				// 1.25xF. Crediting t0 front-loaded a saving at the cache-CREATION rate for work
				// that had not yet paid off, which is the one direction a savings figure must
				// never lean.
			}
		}
		prevTokensBefore = r.TokensBefore
	}
	if cur != nil {
		// Never closed inside the queried range.
		finishEpisode(cur, p)
		out = append(out, *cur)
	}
	return out
}

// chargeRowCosts charges one row's DEBITS to the episode whose investment they are.
//
// # Our own model spend is charged on EVERY turn in the span, not only on a summarizing one
//
// On the async path the summarizer's cost does not land on the turn that commissioned it. The
// detached goroutine finishes after t0's row has already been written, so its usage is attributed
// one turn late (cheapmodel.ReplayUsage + store.UsagePrefix, commit f61e8c0). t0+1 carries
// EventAwaitedCheckpoint, which isFreshSummary classifies as a REPLAY — so gating the charge on
// isFreshSummary wrote the cost to the database and then skipped reading it back.
//
// The repo had already said this twice: in f61e8c0's own message ("The episode panel charges that
// spend as a debit, so a missing cost inflates the reported net") and in proxy/cgllm_test.go ("TWO
// REQUESTS, and the cost lands on the SECOND"). The proxy side was fixed; this side was never
// updated to follow, and A REVIEW MEASURED THE CONSEQUENCE — the summarizer's $0.0727 missing from
// a reported net, in the direction this file repeatedly insists a savings figure must never lean.
//
// Charging every turn slightly over-attributes: cg_llm_cost_usd is the whole request's context-guru
// model spend, so another component's extraction call inside the span is charged here too. That is
// the conservative direction, and it is the only rule that cannot silently MISS our call — the
// alternative is to key on where the deferred usage was attributed, which is not a fact the row
// carries.
//
// # The cache write
//
// opensNew says this row commissioned a summary, so the write it caused is ours to answer for — but
// only where the entry was still live, which is causedWriteUSD's rule.
func chargeRowCosts(e *CompactionEpisode, r compactRow, p kvcache.Pricing, opensNew bool) {
	e.SummarizerCostUSD += r.CGLLMCostUSD
	switch {
	case opensNew:
		e.InvalidationDebitUSD += causedWriteUSD(r, p)
	case r.MissReason == CachePrefixChange:
		// A prefix_change turn inside a span we opened is a cache write charged because the prompt
		// no longer matched what was cached — and inside a summarized span the thing that changes
		// the prompt is US. It is already excluded from the credit for that reason (see creditTurn);
		// excluding it from the DEBIT as well would be having it both ways, counting neither our
		// damage nor its cost.
		//
		// Observed for real: a turn whose summarizer call hung held the request past its own cache
		// lifetime, so the entry expired in our hands and the full prefix was re-written — and it
		// was recorded as prefix_change, because idle is measured at arrival while the expiry
		// happened inside the pipeline.
		e.InvalidationDebitUSD += writeUSD(r.CacheWrite, r.CacheWrite1h, p)
	}
}

// creditTurn adds one turn's summarize saving to the bucket its cache verdict names, PRICED HERE
// rather than inherited from request_components.saved_usd.
//
// # Why this does not use saved_usd, though an earlier version did
//
// saved_usd is Event.baselineDeltaUSD: `unique × cacheWriteRate + (gross − unique) × repeatRate`.
// The repeat term is right. The UNIQUE term has no meaning inside an episode, and pricing it at the
// cache-creation rate on a turn whose cache HIT overstated that turn by the full write:read ratio —
// 12.5x on the Anthropic family.
//
// NOTHING SUMMARIZE REMOVES INSIDE A SPAN IS NEW CONTENT. That is what makes it summarizable: the
// span it drops is transcript the provider has already been sent, at least once, on an earlier turn.
// So the counterfactual for a removed span is never "this content enters the prompt for the first
// time" — it is "this content is re-sent, and billed at whatever rate this turn paid for its
// prefix". `unique` is a dedup of STASH keys, and it answers a different question.
//
// A LIVE REVIEW RUN CAUGHT THIS, and it had inverted the panel's sign. On the async path the stash
// is written by the background goroutine, which has no Report — so the checkpoint key first reaches
// rep.CacheKeys on the turn that REPLAYS it, one turn after the summary was commissioned.
// Recorder.MarkUnique sees a key it has never seen, returns the whole removal as unique, and the row
// lands with saved_usd priced at the cache-creation rate. That turn is a cache HIT, and unlike t0 it
// IS credited. So going async moved the write-rate term off the uncredited compaction turn and onto
// a credited warm one: a measured episode that LOST $0.0397 was published as +$0.376, and the
// write-rate term was $0.358 of that gap — larger than the missing summarizer debit by ~5x.
//
// The bucket labels and the arithmetic now agree, which they did not before: a figure sitting in
// ReadCreditUSD — the bucket documented as "billed at the cache-read rate" — had been priced at the
// cache-WRITE rate.
//
// # The rate each verdict earns
//
//   - ttl_expiry: the entry had lapsed, so an uncompacted prefix would have been RE-CREATED. The
//     cache-creation rate, and this is the one bucket where the write rate belongs. Its 1h-tier
//     subset is split the same way the debit's is, assuming the counterfactual write would have used
//     the tier this turn's actual write did — the only tier evidence the row carries.
//   - hit: the entry was live, so the removed span would have been served from it. The cache-READ
//     rate.
//   - cold_start / unknown: reported apart and never blended into either, at the read rate, which is
//     the conservative choice for a bucket no total picks up.
//
// prefix_change is deliberately absent from all three buckets. That verdict means the entry missed
// because the prompt had CHANGED — which on a summarized session is frequently our own doing — so
// counting it as a saving would credit this component for the misses it caused.
//
// # The quantity, and its correction
//
// saved_gross is summarize's own tokenizer over message text and the RATES here are the provider's,
// so the two had to be reconciled — issue #240. This comment used to state the gap as "~12.4%,
// which under-reports, which is the safe direction". Both halves were wrong to leave: 12.4% came
// from a single cross-turn substitution, and MEASURED against real bills the gap is larger —
// f = 1.3686 on haiku-4-5 and 1.6857 on the sonnet-5/opus-5 tokenizer (see
// tokens.BilledDeltaFactor). "Conservative" is not a reason to leave a savings figure wrong, and
// the direction was never the whole story: the same root cause OVER-reports wherever a cache-write
// premium was fabricated (modelinfo.CacheWriteFracFor).
//
// So the quantity is corrected by the same per-family factor the write path and the read-time
// estimator use. The token counts on the row are untouched — only the counterfactual dollars.
func creditTurn(e *CompactionEpisode, r compactRow, p kvcache.Pricing) {
	// saved_usd is read for exactly one thing: it is non-zero iff summarize removed something on
	// this turn that the savings pipeline was willing to price. Cheaper than re-deriving that
	// condition, and it keeps this bucket's population identical to the one the Components tab
	// reports for the same component.
	if r.SavedUSD == 0 || r.SavedGross <= 0 || !p.Known {
		return
	}
	bf, _ := tokens.BilledDeltaFactor(r.Model)
	switch r.MissReason {
	case CacheTTLExpiry:
		// The counterfactual write is the whole removed span, at the tier this turn wrote at.
		e.ColdCreditUSD += writeUSD(r.SavedGross, scaledWrite1h(r), p) * bf
	case CacheHit:
		e.ReadCreditUSD += float64(r.SavedGross) * p.CacheRead * bf
	case CacheColdStart, CacheUnknown, "":
		e.OtherCreditUSD += float64(r.SavedGross) * p.CacheRead * bf
	}
}

// scaledWrite1h is how much of a counterfactual write of SavedGross tokens would have been billed at
// the 1-hour tier, assuming the removed span would have used the same tier mix this turn's own write
// did. All of it, none of it, or a proportional share — there is no per-span tier on the row, and
// the alternative is to assume the cheaper 5m tier for everything, which would understate the one
// bucket that carries the headline.
func scaledWrite1h(r compactRow) int64 {
	if r.CacheWrite1h <= 0 || r.CacheWrite <= 0 {
		return 0
	}
	if r.CacheWrite1h >= r.CacheWrite {
		return r.SavedGross
	}
	return r.SavedGross * r.CacheWrite1h / r.CacheWrite
}

// finishEpisode computes the net once the credits and debits are all in.
func finishEpisode(e *CompactionEpisode, p kvcache.Pricing) {
	if !p.Known {
		// EVERY dollar field is CLEARED, not just NetUSD left at zero. The comment here used to say
		// "leave every dollar at zero", and that was true of the credits and the invalidation debit —
		// both are priced through guards that return 0 on an unknown rate — but NOT of
		// SummarizerCostUSD, which is a stored per-request figure that does not depend on this
		// model's rates at all. So an unpriced episode published a real summarizer cost beside four
		// zeros and a zero net.
		//
		// The rendered panel was safe: every cell goes through usdOrNA with the group's priced flag.
		// A JSON consumer was not, and this is a public API route. A review flagged the general form
		// ("an unpriced episode still publishes dollars"); this is the one field where it was true.
		e.ColdCreditUSD, e.ReadCreditUSD, e.OtherCreditUSD = 0, 0, 0
		e.InvalidationDebitUSD, e.SummarizerCostUSD, e.NetUSD = 0, 0, 0
		e.Priced = false
		return
	}
	e.NetUSD = e.ColdCreditUSD + e.ReadCreditUSD + e.OtherCreditUSD -
		e.InvalidationDebitUSD - e.SummarizerCostUSD
}

// isFreshSummary reports the provenance of a fresh summary on this row, or "" if this turn did
// not produce one.
//
// RECORDED is a positive fact: summarize files offload.EventFreshSummary on the turn it commits a
// new summary. INFERRED is the fallback for rows written before that event existed — it acted, and
// it carried none of the replay names, so it must have paid. The two are never summed downstream.
func isFreshSummary(r compactRow) string {
	if r.Events == "" {
		if r.Acted {
			return EpisodeInferred
		}
		return ""
	}
	// EITHER event marks a paid t0, and the async one is the common case.
	//
	// EventSummaryStarted is the turn that COMMISSIONED a summary: it spent the model call and it
	// invalidated the prefix, and it is the only one of the two that appears on a proxied request
	// at all — the background goroutine that finishes the work has no request row and no Report to
	// file against. EventFreshSummary now reaches a row only on the INLINE path, which is the
	// sessionless caller (the library API, /compact).
	//
	// Keying on the commission rather than on the completion is also the more correct choice for
	// what t0 means here: t0 carries the DEBITS and no credit, because at that moment nothing has
	// been saved and money has been spent. That is true of the commissioning turn whether or not
	// the summary it paid for ever landed — and a summary that was paid for and lost is exactly
	// the case this measurement must not quietly drop.
	//
	// A LIVE RUN CAUGHT THIS. With the walk keying on EventFreshSummary alone, a real Claude Code
	// session that fired the trigger, commissioned a summary and spliced it on the next turn
	// produced `episodes: 0` — the coverage half correctly reported one qualifying conversation
	// with no episode, and the episode half saw nothing at all.
	if strings.Contains(r.Events, `"`+offload.EventSummaryStarted+`"`) ||
		strings.Contains(r.Events, `"`+offload.EventFreshSummary+`"`) {
		return EpisodeRecorded
	}
	for _, replay := range []string{
		offload.EventReusedCheckpoint,
		offload.EventGatedReplayedCheckpoint,
		offload.EventReserveExhaustedReplayedCheckpoint,
		// A turn that WAITED for a summary and spliced it is a splice, not a purchase: the call
		// it waited for was paid for by the turn that commissioned it.
		offload.EventAwaitedCheckpoint,
	} {
		if strings.Contains(r.Events, `"`+replay+`"`) {
			return ""
		}
	}
	if r.Acted {
		return EpisodeInferred
	}
	return ""
}

// newContentBilled is how much of a turn's billed input was CONTENT THE CONVERSATION HAD NOT SEEN,
// in the provider's own units.
//
// It is the episode span's axis. See conversationEpisodes for why the alternatives fail; the rule
// here is just the classification:
//
//   - fresh_input: new by definition.
//   - cache_write on a turn that HIT: the newly-written tail, which was not in the entry the turn
//     read from.
//   - cache_write on a turn that MISSED: re-creation of a prefix that already existed, so not new.
//     Counting it would call a whole transcript "new" every time an entry expired.
//   - cache_read: the re-sent prefix. Never new.
//
// IT ASSUMES A BREAKPOINT KEEPS UP WITH THE TAIL, which the live run does show — every warm turn
// there wrote its new tail on arrival. Where a client's breakpoints lag, the same tokens are billed
// FRESH on the turn they arrive and written on a later one, so they count twice and the span closes
// in roughly half the new content it claims. That errs toward a shorter span, which under-reports
// this component rather than over-reporting it; a client that never writes at all degenerates to
// counting fresh input only, which is correct.
func newContentBilled(r compactRow) int64 {
	// Billed = fresh + read + write, and the row carries read and write, so fresh is the remainder.
	fresh := r.Billed - r.CacheRead - r.CacheWrite
	if fresh < 0 {
		fresh = 0
	}
	n := fresh
	if r.CacheRead > 0 {
		n += r.CacheWrite
	}
	return n
}

// causedWriteUSD prices only the cache creation WE are responsible for on a summarizing turn.
//
// THE DISTINCTION DECIDES WHETHER THIS PANEL REPORTS A GAIN OR A LOSS, and getting it wrong was
// measured. On the live validation run t0 was a genuine ttl_expiry: the entry had already lapsed,
// so that turn was going to write its whole prefix whatever we did. Charging its 154,581-token
// write to us produced a $0.193 debit against $0.136 of credit — a reported net LOSS on a turn
// that was, in fact, the cheapest possible moment to compact.
//
// The rule is the same one the trigger itself follows: only a write that would NOT have happened
// otherwise is our cost.
//
//   - ttl_expiry / cold_start: the entry was gone. The write was due regardless, so we caused
//     none of it and the debit is zero. This is exactly why firing on a cold turn is the
//     unconditionally-better case.
//   - hit: the entry was LIVE and we rewrote the prefix anyway, so the creation on that turn is
//     ours. A partial hit also reads as `hit`, which is the conservative direction here — it
//     charges us for a write we may only partly have caused.
//   - prefix_change / unknown: inside a span we opened, the thing that changed the prompt is
//     usually us, so it is charged. Conservative for a savings figure.
func causedWriteUSD(r compactRow, p kvcache.Pricing) float64 {
	switch r.MissReason {
	case CacheTTLExpiry, CacheColdStart:
		return 0
	default:
		return writeUSD(r.CacheWrite, r.CacheWrite1h, p)
	}
}

// writeUSD prices cache-creation tokens, splitting out the 1h-tier subset. A 1h write is 2.0x
// base input where a 5m write is 1.25x, so pricing one as the other understates by 0.75x of the
// written prefix — the same correction cost_usd already makes.
func writeUSD(write, write1h int64, p kvcache.Pricing) float64 {
	if !p.Known || write <= 0 {
		return 0
	}
	if write1h > write {
		write1h = write
	}
	return float64(write-write1h)*p.Write5m + float64(write1h)*p.Write1h
}

// coldCreationUSD is what a conversation's TTL-expiry turns actually paid to re-create their
// prefix. The opportunity size for a conversation that never produced a summary.
func coldCreationUSD(conv []compactRow, p kvcache.Pricing) float64 {
	total := 0.0
	for _, r := range conv {
		if r.MissReason == CacheTTLExpiry {
			total += writeUSD(r.CacheWrite, r.CacheWrite1h, p)
		}
	}
	return total
}

// groupConversations splits ordered rows on the conversation key.
//
// (tenant, session, model) — exactly kvcache.Conversation, and exactly kvCacheCTE's partition,
// for the reason stated there: a cache entry does not transfer between models, so an opus request
// cannot read a sonnet request's entry. The tenant is in the key because a session id is
// CLIENT-supplied and two accounts can present the same one.
func groupConversations(rows []compactRow) [][]compactRow {
	var out [][]compactRow
	start := 0
	for i := 1; i <= len(rows); i++ {
		if i == len(rows) || rows[i].Tenant != rows[start].Tenant ||
			rows[i].Session != rows[start].Session || rows[i].Model != rows[start].Model {
			out = append(out, rows[start:i])
			start = i
		}
	}
	return out
}

func maxBilled(conv []compactRow) int64 {
	var m int64
	for _, r := range conv {
		if r.Billed > m {
			m = r.Billed
		}
	}
	return m
}

func spanTS(first, last int64, conv []compactRow) (int64, int64) {
	for _, r := range conv {
		if first == 0 || r.TS < first {
			first = r.TS
		}
		if r.TS > last {
			last = r.TS
		}
	}
	return first, last
}

// ClientCeilingUsed is the ceiling one model's conversations were measured against.
type ClientCeilingUsed struct {
	Model string `json:"model"`
	// Frac is the fraction of the context window, in PROVIDER-BILLED input, at which the client is
	// taken to compact its own transcript. Not the figure the client's own indicator displays.
	Frac float64 `json:"frac"`
	// SpanFrac is what that ceiling produced for this model: `frac - fill_frac`.
	SpanFrac float64 `json:"span_frac"`
	// Provenance is "measured", "assumed" or "default". A measured ceiling came from watching a real
	// client compact, in billed tokens; the other two did not, and an assumed ceiling that is too high
	// makes the span too wide and OVER-reports.
	Provenance string `json:"provenance"`
	// Note is why this value, in enough detail to tell whether it still holds.
	Note string `json:"note"`
}

// noteCeiling records the ceiling one model was measured against, once per model.
func (a *CompactionAssumptions) noteCeiling(model string, c compactionpoint.Point, span float64) {
	for _, e := range a.ClientCeilings {
		if e.Model == model {
			return
		}
	}
	a.ClientCeilings = append(a.ClientCeilings, ClientCeilingUsed{
		Model: model, Frac: c.Frac, SpanFrac: span, Provenance: string(c.Source), Note: c.Note,
	})
}

func compactAssumptions(spanFrac, fillFrac float64) CompactionAssumptions {
	return CompactionAssumptions{
		SpanFrac:    spanFrac,
		FillFrac:    fillFrac,
		SpanMeasure: "provider-billed NEW content (fresh input, plus the newly-written tail on turns whose cache hit), in the units the context window is stated in",
		CreditSource: "the tokens summarize removed on each turn (request_components.saved_gross), " +
			"priced at the rate THAT turn's cache verdict earns: the cache-creation rate where the " +
			"entry had lapsed, the cache-read rate where it hit. Nothing removed inside a span is " +
			"new content, so the stored saved_usd — which prices first-time removals at the " +
			"creation rate — is deliberately not used as the credit",
		CreditQuantityNote: "the removed-token count is our own tokenizer over message text, which " +
			"under-states the provider's count of the same content by roughly 12% at the transcript " +
			"sizes compaction happens at (issue #240). This panel therefore under-reports rather " +
			"than over-reports",
		ColdLabel:       "turns whose cache_miss_reason is ttl_expiry — the entry had lapsed, so an uncompacted prefix would have been re-written in full",
		ReadLabel:       "turns whose cache_miss_reason is hit — the re-sent remainder, billed at the cache-read rate",
		SpanMeasureNote: "the span closes on cumulative NEW content since t0 — fresh input plus the newly-written tail on turns whose cache hit. It excludes the re-sent prefix (a cache read) and the re-creation of an expired one (a cache write on a miss), because counting either makes one turn exceed the whole span",
		SpanRule: "the span is the CLIENT's compaction ceiling minus the fill threshold, derived rather " +
			"than configured separately, because it is an ATTRIBUTION boundary: we fire at the threshold, " +
			"and in the world where we had not compacted the conversation keeps growing until the CLIENT " +
			"compacts at its own ceiling. Past that point both worlds are running on a summarized " +
			"transcript and nothing further is attributable to us. The ceiling is an assumption about the " +
			"client rather than a property of the model, measured at 0.996 of the window on one real " +
			"Claude Code session on haiku and stated in PROVIDER-BILLED tokens — not in the client's own " +
			"count, which read ~167,000 of 200,000 on the same turn. It bounds new content, NOT " +
			"wall-clock time: a session can go cold having added 2% of the window, and a session that " +
			"adds 10% in thirty seconds never goes cold at all",
		DebitBound: "cache-creation tokens on a summarizing turn, charged ONLY where the entry was still live: on a turn whose cache had already expired the write was due whatever we did, so none of it is our cost. Where it is charged it is an upper bound, since some of that write was transcript growth that would have been paid anyway",
		VoidRule:   "an episode whose span contains a drop in our own message-token count is voided — the client compacted, so the remainder is not comparable",
		WindowRule: "a conversation whose model window is not exactly published contributes nothing and is counted as window_unknown",
		OpenRule: "a span that has not finished is reported with its own net, apart from the settled " +
			"total: it is money committed whose payoff is still accruing, and dropping it would " +
			"make the panel optimistic. It is not automatically a loss — the compaction turn is " +
			"already cheaper on its own turn at better than ~12.5:1 compression",
		KnownOmission: "context_guru_expand calls inside a span are not charged against it; a summary the agent had to undo costs input tokens this figure does not subtract",
	}
}

// --- SQL ---------------------------------------------------------------------

// compactEpisodeQuery selects one row per request, with summarize's component row LEFT JOINed.
//
// LEFT, not INNER: every turn inside a span is part of the measurement even when summarize did
// nothing on it, because the span is a quantity of WORK and the turns that did nothing still
// consumed some. An INNER join would silently shorten every span to the turns summarize touched.
const compactEpisodeSelect = `SELECT r.tenant_id, r.session_id, r.model, r.ts, r.tokens_before,
		r.fresh_input + r.cache_read + r.cache_write AS billed,
		r.cache_miss_reason, r.cache_write, r.cache_write_1h, r.cache_read, r.cg_llm_cost_usd,
		COALESCE(c.saved_gross, 0), COALESCE(c.saved_usd, 0), COALESCE(c.events, ''),
		COALESCE(c.acted, 0)
	FROM requests r
	LEFT JOIN request_components c ON c.request_id = r.id AND c.component = 'summarize'
	WHERE %s
	ORDER BY r.tenant_id, r.session_id, r.model, r.ts, r.id`

// compactEpisodeDataset reads the ordered rows one filter selects.
func (d *DB) compactEpisodeDataset(f Filter) ([]compactRow, error) {
	cond, args := f.where()
	q := strings.Replace(compactEpisodeSelect, "%s", cond, 1)
	rows, err := d.sql.QueryContext(d.readCtx(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []compactRow
	for rows.Next() {
		var r compactRow
		var acted int
		if err := rows.Scan(&r.Tenant, &r.Session, &r.Model, &r.TS, &r.TokensBefore,
			&r.Billed, &r.MissReason, &r.CacheWrite, &r.CacheWrite1h, &r.CacheRead,
			&r.CGLLMCostUSD, &r.SavedGross, &r.SavedUSD, &r.Events, &acted); err != nil {
			return nil, err
		}
		r.Acted = acted != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// CompactionEpisodesFor measures every episode one filter selects.
//
// windows is read through modelinfo.Exact, so a resolver that cannot say whether its answer is
// published is treated as inexact and its conversations are excluded rather than measured against
// a guess.
func (d *DB) CompactionEpisodesFor(f Filter, windows modelinfo.Resolver, p modelinfo.Pricer,
	cfg KVCacheSimConfig, spanFrac, fillFrac, ceilingFrac float64) (*CompactionEpisodes, error) {
	rows, err := d.compactEpisodeDataset(f)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	ctx := d.readCtx()
	models := map[string]bool{}
	for _, r := range rows {
		models[r.Model] = true
	}
	names := make([]string, 0, len(models))
	for m := range models {
		names = append(names, m)
	}
	list := kvcache.NewPriceList(ctx, names, p, cfg.Multipliers, cfg.Overrides)
	out := walkCompactEpisodes(rows,
		func(model string) (int, bool) {
			w, exact, ok := modelinfo.Exact(windows, ctx, model)
			return w, exact && ok
		},
		list.For, spanFrac, fillFrac, ceilingFrac)
	out.Pricing = list
	return out, nil
}

// --- HTTP --------------------------------------------------------------------

// compactEpisodeRoutes is declared beside its handler and appended to API.routes, which is the
// table both scoping tests walk — a route mounted anywhere else is a route whose scope nothing
// checks.
func (a *API) compactEpisodeRoutes() []route {
	return []route{
		{"GET /api/components/compaction-episodes", scopeTenant, a.compactionEpisodes},
	}
}

func (a *API) compactionEpisodes(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	// A whole-dataset read, like the KV-cache page's two: take a slot so a refresh storm cannot
	// commit the process's memory several times over.
	if err := acquireKVCache(r.Context()); err != nil {
		return
	}
	defer releaseKVCache()
	// The fill is read FIRST, because the span derives from it.
	fill := queryFrac(r, "fill", defaultFillFrac)
	ceiling := queryFrac(r, "ceiling", defaultClientCeilingFrac)
	// span=0 means DERIVE it from the fill and the ceiling (see spanFor). `?span=` stays as an override,
	// because an operator investigating whether a span contains any cold event has a legitimate reason
	// to widen it — and because the arms in scripts/scenarios/ use it to separate "the credit is wrong"
	// from "the span was too narrow to contain the event".
	out, err := a.db(r).CompactionEpisodesFor(f, a.windows, a.pricer,
		kvCacheConfigFrom(r), queryFrac(r, "span", 0), fill, ceiling)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, out)
}

// queryFrac reads a 0<f<1 fraction from the query string, falling back to the default on anything
// it cannot use. A malformed fraction must not silently become 0, which would either close every
// episode instantly (span) or qualify every conversation (fill).
func queryFrac(r *http.Request, key string, def float64) float64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	// The upper bound is 8, not 1: `span` is a multiple of the window's worth of NEW content, and
	// an operator investigating whether a span is long enough to contain cold misses has a
	// legitimate reason to ask for several windows' worth. The `fill` fraction is a fraction of one
	// window and could not exceed 1, but one guard serves both and the looser bound cannot hurt
	// fill (a fill above 1 simply qualifies nothing, which is visible rather than silent).
	// NaN AND Inf ARE CHECKED FIRST, because a range guard cannot catch them: every comparison
	// against NaN is false, so `f <= 0 || f > 8` passed `?span=NaN` straight through. The span then
	// became NaN, which compares false against everything — so no episode ever closed and the panel
	// reported an empty measurement rather than an error. A review found it.
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 8 {
		return def
	}
	return f
}
