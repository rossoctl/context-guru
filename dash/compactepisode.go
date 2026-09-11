package dash

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/modelinfo"
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
	// Read only on t0, to price the invalidation this component itself caused.
	CacheWrite, CacheWrite1h int64
	// CGLLMCostUSD is context-guru's own model spend on this request — the summarizer's call.
	CGLLMCostUSD float64
	// SavedUSD is summarize's share of this request's baseline delta, priced at write time.
	// Zero on a turn where summarize did nothing.
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
	// EpisodeRecorded: t0 carried offload.EventFreshSummary, which summarize files on the turn
	// it commits a new summary. A fact about that turn.
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
	Turns       int64 `json:"turns"`
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
	SpanFrac      float64 `json:"span_frac"`
	FillFrac      float64 `json:"fill_frac"`
	SpanMeasure   string  `json:"span_measure"`
	CreditSource  string  `json:"credit_source"`
	ColdLabel     string  `json:"cold_label"`
	ReadLabel     string  `json:"read_label"`
	DebitBound    string  `json:"debit_bound"`
	VoidRule      string  `json:"void_rule"`
	WindowRule    string  `json:"window_rule"`
	KnownOmission string  `json:"known_omission"`
	OpenRule      string  `json:"open_rule"`
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
	// defaultSpanFrac is the 10%-more-of-the-window span: long enough to contain several cold
	// misses on real traffic, short enough that a session's later behaviour does not dominate.
	defaultSpanFrac = 0.10
	// defaultFillFrac matches summarize's own shipped min_request_frac, so the coverage
	// population is the one the trigger is actually deciding about.
	defaultFillFrac = 0.90
	// episodeListCap bounds the drill-down list. The aggregates are unbounded.
	episodeListCap = 200
)

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
	spanFrac, fillFrac float64) *CompactionEpisodes {
	if spanFrac <= 0 {
		spanFrac = defaultSpanFrac
	}
	if fillFrac <= 0 {
		fillFrac = defaultFillFrac
	}
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
		eps := conversationEpisodes(conv, w, p, spanFrac)

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
		// credited guards the one row that can belong to two episodes: the turn that closes a
		// span AND itself produces a fresh summary. Its SAVING was earned inside the span that
		// is closing and is credited there; its COSTS (the new summary's own call, and the cache
		// write that rewrite caused) belong to the span it opens. Crediting it twice would make
		// the sum of episodes exceed the money that existed, which is the one direction a
		// savings figure must never go.
		credited := false

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
			creditTurn(cur, r)
			credited = true
			// A roll-forward inside the span is not a new episode — it is this episode still
			// being maintained, and its cost belongs to the span it happened in.
			if isFreshSummary(r) != "" {
				cur.SummarizerCostUSD += r.CGLLMCostUSD
				cur.InvalidationDebitUSD += writeUSD(r.CacheWrite, r.CacheWrite1h, p)
			} else if r.MissReason == CachePrefixChange {
				// A prefix_change turn inside a span we opened is a cache write charged because
				// the prompt no longer matched what was cached — and inside a summarized span the
				// thing that changes the prompt is US. It is already excluded from the credit for
				// that reason (see creditTurn); excluding it from the DEBIT as well would be
				// having it both ways, counting neither our damage nor its cost.
				//
				// Observed for real: a turn whose summarizer call hung held the request past its
				// own cache lifetime, so the entry expired in our hands and the full prefix was
				// re-written — and it was recorded as prefix_change, because idle is measured at
				// arrival while the expiry happened inside the pipeline.
				cur.InvalidationDebitUSD += writeUSD(r.CacheWrite, r.CacheWrite1h, p)
			}
			if r.Billed >= cur.StartBilled+span {
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
					// t0's own cache creation, at this model's write rates. See the field
					// comment: an upper bound, on purpose.
					InvalidationDebitUSD: writeUSD(r.CacheWrite, r.CacheWrite1h, p),
					SummarizerCostUSD:    r.CGLLMCostUSD,
				}
				// NO CREDIT ON THE COMPACTION TURN, and `credited` is now irrelevant to that.
				//
				// At t0 nothing has been saved — we have only SPENT: a model call, and a cache
				// write for the new smaller prefix. The saving arrives on later turns, as reads
				// against a shorter transcript and as cold rewrites that cost 1.25xC instead of
				// 1.25xF. Crediting t0 front-loaded a saving at the cache-CREATION rate for work
				// that had not yet paid off, which is the one direction a savings figure must
				// never lean.
				_ = credited
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

// creditTurn adds one turn's summarize saving to the bucket its cache verdict names.
//
// prefix_change is deliberately absent from all three buckets. That verdict means the entry
// missed because the prompt had CHANGED — which on a summarized session is frequently our own
// doing — so counting it as a saving would credit this component for the misses it caused.
func creditTurn(e *CompactionEpisode, r compactRow) {
	if r.SavedUSD == 0 {
		return
	}
	switch r.MissReason {
	case CacheTTLExpiry:
		e.ColdCreditUSD += r.SavedUSD
	case CacheHit:
		e.ReadCreditUSD += r.SavedUSD
	case CacheColdStart, CacheUnknown, "":
		e.OtherCreditUSD += r.SavedUSD
	}
}

// finishEpisode computes the net once the credits and debits are all in.
func finishEpisode(e *CompactionEpisode, p kvcache.Pricing) {
	if !p.Known {
		// Leave every dollar at zero AND say the episode is unpriced, so no total can pick
		// these up as real figures.
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
	if strings.Contains(r.Events, `"`+offload.EventFreshSummary+`"`) {
		return EpisodeRecorded
	}
	for _, replay := range []string{
		offload.EventReusedCheckpoint,
		offload.EventGatedReplayedCheckpoint,
		offload.EventReserveExhaustedReplayedCheckpoint,
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

func compactAssumptions(spanFrac, fillFrac float64) CompactionAssumptions {
	return CompactionAssumptions{
		SpanFrac:    spanFrac,
		FillFrac:    fillFrac,
		SpanMeasure: "provider-billed input (fresh + cache read + cache write), the units the context window is stated in",
		CreditSource: "request_components.saved_usd for the summarize component, priced at write time " +
			"(Event.baselineDeltaUSD: unique removals at the cache-creation rate, re-sent " +
			"removals at the rate that turn's cache actually paid)",
		ColdLabel:  "turns whose cache_miss_reason is ttl_expiry — the entry had lapsed, so an uncompacted prefix would have been re-written in full",
		ReadLabel:  "turns whose cache_miss_reason is hit — the re-sent remainder, billed at the cache-read rate",
		DebitBound: "t0's own cache-creation tokens at this model's write rates: an UPPER bound, since some of that write was transcript growth that would have been paid anyway",
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
		r.cache_miss_reason, r.cache_write, r.cache_write_1h, r.cg_llm_cost_usd,
		COALESCE(c.saved_usd, 0), COALESCE(c.events, ''), COALESCE(c.acted, 0)
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
			&r.Billed, &r.MissReason, &r.CacheWrite, &r.CacheWrite1h, &r.CGLLMCostUSD,
			&r.SavedUSD, &r.Events, &acted); err != nil {
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
	cfg KVCacheSimConfig, spanFrac, fillFrac float64) (*CompactionEpisodes, error) {
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
		list.For, spanFrac, fillFrac)
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
		unauthorized(w)
		return
	}
	// A whole-dataset read, like the KV-cache page's two: take a slot so a refresh storm cannot
	// commit the process's memory several times over.
	if err := acquireKVCache(r.Context()); err != nil {
		return
	}
	defer releaseKVCache()
	out, err := a.db(r).CompactionEpisodesFor(f, a.windows, a.pricer,
		kvCacheConfigFrom(r), queryFrac(r, "span", defaultSpanFrac),
		queryFrac(r, "fill", defaultFillFrac))
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
	if err != nil || f <= 0 || f >= 1 {
		return def
	}
	return f
}
