package dash

import (
	"context"
	"database/sql"
	"math"
	"strings"

	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// Replay credit, capped at the counterfactual client compaction (issue #550).
//
// A removal made by an offloader (extract_llm, searchfold, dedup, ...) is replayed: every later
// turn that re-sends the shortened transcript earns the credit again. Stored one request at a
// time, that credit has no end. This file gives it one, by the rule #546 gave summarize, without
// summarize's trigger fraction (an offloader has none).
//
// # The rule
//
// The counterfactual is the world without context-guru. On every turn its transcript is the one
// the provider billed PLUS what we removed from it:
//
//	cf = billed + f * (tokens_before - tokens_after)
//
// f is the measured tokenizer factor (tokens.BilledDeltaFactor, issue #240): the removed count is
// ours, the ceiling is in provider-billed tokens. When cf reaches the client's compaction ceiling
// (compactionpoint.For, the same ceiling the summarize span uses) the client of that world
// compacts, and from there both worlds carry a summarized transcript: nothing later is ours.
// A real client compaction (a drop in tokens_before, the same detector the episode walk uses)
// ends it too, for the same reason.
//
// The cap does not read the real compaction to decide the counterfactual one. A removal can only
// make the real transcript smaller, so the real client compacts later than the counterfactual
// one, and a rule that waited for it would credit exactly the turns the removals pushed past the
// ceiling.
//
// # What it deliberately does not do
//
//   - It does not reopen after a close. A later epoch could earn again; the cap is conservative.
//   - It keeps the stored figure for turns it counts. The first-removal-as-read rule of summarize
//     (Event.uniqueRate) does not apply here: extract_llm fires on cold turns, where the
//     uncompacted prefix would have been re-written, so the write rate is the right one.
//   - It sees only the rows the filter selects. A window that starts mid-session cannot know of
//     a close before it.

// ComponentLedger is one component's credit, sorted into what counts and what does not.
type ComponentLedger struct {
	// StoredUSD is request_components.saved_usd on the rows the walk saw.
	StoredUSD float64 `json:"stored_usd"`
	// CountedUSD is the part inside the cap; FirstRemovalUSD + ReplayUSD add up to it.
	CountedUSD      float64 `json:"counted_usd"`
	FirstRemovalUSD float64 `json:"first_removal_usd"`
	ReplayUSD       float64 `json:"replay_usd"`
	// PastCapUSD is the stored credit after the cap closed, with the reason that closed it.
	PastCapUSD         float64 `json:"past_cap_usd"`
	PastCapFirstUSD    float64 `json:"past_cap_first_usd"`
	PastCounterfactual float64 `json:"past_counterfactual_usd"`
	PastClientCompact  float64 `json:"past_client_compaction_usd"`
	Rows               int64   `json:"rows"`
	PastRows           int64   `json:"past_rows"`
	// WindowAssumedUSD is the stored credit of conversations whose model window was not exactly
	// known, capped against assumedWindow instead.
	WindowAssumedUSD float64 `json:"window_assumed_usd"`
}

// assumedWindow stands in for a model whose context window is not exactly known: 200k, the smallest
// window of the Claude families seen here. A family with smaller windows has its own floor
// (familyWindowFloor), and a known but unpublished window below both wins. It is an assumption, not a
// bound: a model of an unlisted family with a smaller window still over-credits, and is labelled.
const assumedWindow = 200_000

var familyWindowFloor = []struct {
	substr string
	window int
}{{"gpt-4", 128_000}, {"llama", 128_000}, {"mistral", 32_000}}

// assumedWindowFor is the smallest of the resolver's (inexact) window, the model family's floor and
// assumedWindow.
func assumedWindowFor(model string, known int) int {
	w := assumedWindow
	if known > 0 && known < w {
		w = known
	}
	m := strings.ToLower(model)
	for _, f := range familyWindowFloor {
		if strings.Contains(m, f.substr) && f.window < w {
			w = f.window
		}
	}
	return w
}

// DeltaUSD is the correction to apply to a figure built from the stored credit.
func (l *ComponentLedger) DeltaUSD() float64 { return -l.PastCapUSD }

// ReplayLedger is the ledger of every capped component, keyed by component name.
type ReplayLedger struct {
	ByComponent map[string]*ComponentLedger `json:"by_component"`
	// Conversations walked, and how many closed by each cause.
	Conversations          int64              `json:"conversations"`
	ClosedCounterfactual   int64              `json:"closed_counterfactual"`
	ClosedClientCompaction int64              `json:"closed_client_compaction"`
	WindowUnknownCovered   int64              `json:"window_unknown_conversations"`
	CeilingFrac            map[string]float64 `json:"ceiling_frac,omitempty"`
}

// DeltaUSD is the total correction across components.
func (l *ReplayLedger) DeltaUSD() float64 {
	if l == nil {
		return 0
	}
	var d float64
	for _, c := range l.ByComponent {
		d += c.DeltaUSD()
	}
	return d
}

func (l *ReplayLedger) comp(name string) *ComponentLedger {
	c := l.ByComponent[name]
	if c == nil {
		c = &ComponentLedger{}
		l.ByComponent[name] = c
	}
	return c
}

// compCredit is one component's stored credit on one request.
type compCredit struct {
	Component string
	Gross     int64
	Unique    int64
	USD       float64
	FirstUSD  float64
}

// capRow is one request as the cap walk needs it.
type capRow struct {
	Tenant, Session, Model    string
	TS                        int64
	TokensBefore, TokensAfter int64
	Billed                    int64
	// Bypassed marks a request the proxy forwarded untouched. Rows written before the agent's own
	// compaction request stopped being bypassed carry it for that request.
	Bypassed bool
	// AgentCompaction marks the agent's own compaction request (isAgentCompaction). Either flag is
	// the evidence that a drop in tokens_before is the client compacting.
	AgentCompaction bool
	Credits         []compCredit
	// What Event.ComponentSavedUSD needs to price the first-removal split.
	reason             string
	fresh, read, write int64
}

func sameConv(a, b *capRow) bool {
	return a.Tenant == b.Tenant && a.Session == b.Session && a.Model == b.Model
}

// walkReplayCap runs the rule over rows ordered by (tenant, session, model, ts, id).
//
// A drop in tokens_before is a client compaction only when the agent's compaction request went
// by since the previous turn of that conversation and the transcript does not come back within a
// few turns. A drop alone is not enough: a subagent shares
// the session and the model, its transcript is smaller, and the turns interleave. On the
// large real dataset most drops were that, not a compaction.
func walkReplayCap(rows []capRow, window windowFn) *ReplayLedger {
	out := newReplayLedger()
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].Tenant == rows[start].Tenant && rows[end].Session == rows[start].Session {
			end++
		}
		walkCapSession(rows[start:end], window, out)
		start = end
	}
	return out
}

func newReplayLedger() *ReplayLedger {
	return &ReplayLedger{ByComponent: map[string]*ComponentLedger{}, CeilingFrac: map[string]float64{}}
}

// walkCapSession runs the rule over one session's rows, ordered by (model, ts, id). The session is
// the unit because a compaction request and the turn it shrinks need not share a model.
func walkCapSession(rows []capRow, window windowFn, out *ReplayLedger) {
	var compaction []int64
	for _, r := range rows {
		if r.Bypassed || r.AgentCompaction {
			compaction = append(compaction, r.TS)
		}
	}
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && sameConv(&rows[start], &rows[end]) {
			end++
		}
		walkCapConversation(rows[start:end], window, compaction, out)
		start = end
	}
}

// compactionBetween reports whether any of the (unsorted) timestamps lies in [from, to].
func compactionBetween(ts []int64, from, to int64) bool {
	for _, t := range ts {
		if t >= from && t <= to {
			return true
		}
	}
	return false
}

// recoveryLookahead is how many turns after a drop may bring the transcript back before the drop
// stops being a compaction. A subagent or auxiliary turn is followed by the main thread's, which
// is as large as before; a compaction is not. A compaction request in between does not rule an
// interleaved turn out on its own.
const recoveryLookahead = 3

func recovers(next []capRow, prev int64) bool {
	for i := 0; i < len(next) && i < recoveryLookahead; i++ {
		if next[i].TokensBefore >= prev {
			return true
		}
	}
	return false
}

const (
	capOpen = iota
	capCounterfactual
	capClientCompaction
)

func walkCapConversation(conv []capRow, window windowFn, compactionTS []int64, out *ReplayLedger) {
	model := conv[0].Model
	out.Conversations++
	w, exact := window(model)
	assumed := false
	if !exact || w <= 0 {
		// No exact window. Leaving the credit uncapped is the one path that over-credits, so a
		// conservative window stands in: the smallest of what is known, the floor of the model's
		// provider family, and assumedWindow.
		assumed = true
		out.WindowUnknownCovered++
		w = assumedWindowFor(model, w)
	}
	ceil := compactionpoint.For(model).Frac
	out.CeilingFrac[model] = ceil
	limit := ceil * float64(w)
	f, _ := tokens.BilledDeltaFactor(model)
	state := capOpen
	var prevBefore, prevTS int64
	for i, r := range conv {
		if state == capOpen {
			if prevBefore > 0 && r.TokensBefore > 0 && r.TokensBefore < prevBefore &&
				compactionBetween(compactionTS, prevTS, r.TS) && !recovers(conv[i+1:], prevBefore) {
				state = capClientCompaction
				out.ClosedClientCompaction++
			} else if r.Billed > 0 {
				removed := math.Max(float64(r.TokensBefore-r.TokensAfter), 0)
				if float64(r.Billed)+f*removed >= limit {
					state = capCounterfactual
					out.ClosedCounterfactual++
				}
			}
		}
		prevBefore, prevTS = r.TokensBefore, r.TS
		for _, c := range r.Credits {
			l := out.comp(c.Component)
			l.Rows++
			l.StoredUSD += c.USD
			if assumed {
				l.WindowAssumedUSD += c.USD
			}
			if state == capOpen {
				l.CountedUSD += c.USD
				l.FirstRemovalUSD += c.FirstUSD
				l.ReplayUSD += c.USD - c.FirstUSD
				continue
			}
			l.PastRows++
			l.PastCapUSD += c.USD
			l.PastCapFirstUSD += c.FirstUSD
			if state == capCounterfactual {
				l.PastCounterfactual += c.USD
			} else {
				l.PastClientCompact += c.USD
			}
		}
	}
}

// --- SQL ---------------------------------------------------------------------

// capSelect streams every request of the scope with its credit rows (one output row per credit,
// or one with NULLs). summarize has its own ledger. The join is by request id, so a session-scoped
// call touches only that session's component rows.
const capSelect = `SELECT r.id, r.tenant_id, r.session_id, r.model, r.ts, r.bypassed, r.agent_compaction, r.tokens_before,
		r.tokens_after, r.fresh_input + r.cache_read + r.cache_write,
		r.cache_miss_reason, r.fresh_input, r.cache_read, r.cache_write,
		c.component, c.saved_gross, c.saved_unique, c.saved_usd
	FROM requests r
	LEFT JOIN request_components c ON c.request_id = r.id AND %c

		AND c.saved_usd <> 0 AND c.saved_gross > 0
	WHERE %s
	ORDER BY r.tenant_id, r.session_id, r.model, r.ts, r.id`

// firstShare is the fraction of a row's credit that is a first removal: the unique part priced
// through Event.ComponentSavedUSD against the whole, so the split follows the same pricing path
// as the stored figure. Without a price it falls back to the token share.
func firstShare(ev *Event, p modelinfo.Price, priced bool, comp string, gross, unique int64) float64 {
	if unique <= 0 {
		return 0
	}
	if unique > gross {
		unique = gross // same clamp as ComponentSavedUSD
	}
	if priced {
		whole := ev.ComponentSavedUSD(p, comp, int(gross), int(unique))
		if whole > 0 {
			return math.Min(ev.ComponentSavedUSD(p, comp, int(unique), int(unique))/whole, 1)
		}
	}
	return float64(unique) / float64(gross)
}

// notSummarizerSQL excludes the summarizers (SummarizerComponents): they have their own ledger.
func notSummarizerSQL() string {
	q := make([]string, len(SummarizerComponents))
	for i, n := range SummarizerComponents {
		q[i] = "'" + strings.ReplaceAll(n, "'", "") + "'"
	}
	return "c.component NOT IN (" + strings.Join(q, ", ") + ")"
}

// ReplayLedgerFor streams the rows one filter selects and runs the cap session by session. A model
// whose window is not exactly known is capped against an assumed window.
//
// Memory is bounded by the largest single session in the scope (about 120 bytes per request plus
// its credit rows), not by the scope: the ledger itself is a few counters per component.
func (d *DB) ReplayLedgerFor(f Filter, windows modelinfo.Resolver, p modelinfo.Pricer) (*ReplayLedger, error) {
	return d.ReplayLedgerForCtx(d.readCtx(), f, windows, p)
}

// ReplayLedgerForCtx is ReplayLedgerFor under a caller's context, so a caller that no longer needs
// the answer can stop the scan.
func (d *DB) ReplayLedgerForCtx(ctx context.Context, f Filter, windows modelinfo.Resolver, p modelinfo.Pricer) (*ReplayLedger, error) {
	if d == nil || windows == nil {
		return newReplayLedger(), nil
	}
	cond, args := f.where()
	rs, err := d.sql.QueryContext(ctx, strings.Replace(strings.Replace(capSelect, "%c", notSummarizerSQL(), 1), "%s", cond, 1), args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	window := func(model string) (int, bool) {
		w, exact, ok := modelinfo.Exact(windows, ctx, model)
		return w, exact && ok
	}
	price := pricerFn(ctx, p)
	out := newReplayLedger()
	var buf []capRow
	var lastID int64 = -1
	flush := func() {
		for i := range buf {
			r := &buf[i]
			if len(r.Credits) == 0 {
				continue
			}
			ev := Event{Model: r.Model, CacheMissReason: r.reason, FreshInput: r.fresh, CacheRead: r.read, CacheWrite: r.write}
			ev.BilledTokenFactor, _ = tokens.BilledDeltaFactor(r.Model)
			var pr modelinfo.Price
			priced := false
			if price != nil {
				pr, priced = price(r.Model)
			}
			for j := range r.Credits {
				c := &r.Credits[j]
				c.FirstUSD = c.USD * firstShare(&ev, pr, priced, c.Component, c.Gross, c.Unique)
			}
		}
		walkCapSession(buf, window, out)
		buf = buf[:0]
	}
	for rs.Next() {
		var id int64
		var r capRow
		var bypassed, agentComp int
		var comp sql.NullString
		var gross, unique sql.NullInt64
		var usd sql.NullFloat64
		if err := rs.Scan(&id, &r.Tenant, &r.Session, &r.Model, &r.TS, &bypassed, &agentComp, &r.TokensBefore,
			&r.TokensAfter, &r.Billed, &r.reason, &r.fresh, &r.read, &r.write,
			&comp, &gross, &unique, &usd); err != nil {
			return nil, err
		}
		if id != lastID {
			if n := len(buf); n > 0 && (buf[n-1].Tenant != r.Tenant || buf[n-1].Session != r.Session) {
				flush()
			}
			if n := len(buf); n > 0 { // share the strings of the previous row
				if buf[n-1].Tenant == r.Tenant {
					r.Tenant = buf[n-1].Tenant
				}
				if buf[n-1].Session == r.Session {
					r.Session = buf[n-1].Session
				}
				if buf[n-1].Model == r.Model {
					r.Model = buf[n-1].Model
				}
			}
			r.Bypassed = bypassed != 0
			r.AgentCompaction = agentComp != 0
			buf = append(buf, r)
			lastID = id
		}
		if !comp.Valid {
			continue
		}
		cur := &buf[len(buf)-1]
		c := compCredit{Component: comp.String, Gross: gross.Int64, Unique: unique.Int64, USD: usd.Float64}
		dup := false
		for i := range cur.Credits { // request_components has no UNIQUE: keep the larger of duplicates
			if cur.Credits[i].Component == c.Component {
				dup = true
				if c.USD > cur.Credits[i].USD {
					cur.Credits[i] = c
				}
			}
		}
		if !dup {
			cur.Credits = append(cur.Credits, c)
		}
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

// ApplyReplayLedger corrects the headline for the capped components' credit.
func (o *Overview) ApplyReplayLedger(l *ReplayLedger) {
	if o == nil || l == nil {
		return
	}
	delta := l.DeltaUSD()
	o.ReplayCapCorrectionUSD = delta
	o.BaselineCostUSD += delta
	o.NetSavedUSD += delta
	o.TotalSavedUSD += delta
	o.TotalReducedUSD += delta
}

// applyReplayLedgerToRow moves one component's row onto the ledger. Credit the walk did not see
// (an unknown window, pre-column history valued by the estimate) stays as it was.
func applyReplayLedgerToRow(c *ComponentRow, l *ReplayLedger) {
	if c == nil || l == nil {
		return
	}
	cl := l.ByComponent[c.Component]
	if cl == nil || IsSummarizer(c.Component) {
		return
	}
	c.ReplayWindowAssumed = cl.WindowAssumedUSD > 0
	if cl.PastCapUSD == 0 {
		return
	}
	delta := cl.DeltaUSD()
	c.SavedUSD += delta
	c.NetUSD += delta
	c.NetUSDWithEstimate += delta
	pastReplay := cl.PastCapUSD - cl.PastCapFirstUSD
	c.SavedUSDFirstRemoval = math.Max(c.SavedUSDFirstRemoval-cl.PastCapFirstUSD, 0)
	c.SavedUSDReplay = math.Max(c.SavedUSDReplay-pastReplay, 0)
	c.SavedUSDDecomposed = c.SavedUSDFirstRemoval + c.SavedUSDReplay
	c.SavedUSDDecomposedStored = math.Max(c.SavedUSDDecomposedStored+delta, 0)
	c.NetUSDFirstRemoval = c.SavedUSDFirstRemoval - c.LLMCostUSD
	c.ReplayMultiple = 0
	if c.SavedUSDFirstRemoval > 0 {
		c.ReplayMultiple = c.SavedUSDDecomposed / c.SavedUSDFirstRemoval
	}
	c.ReplayCreditPastCapUSD = cl.PastCapUSD
}
