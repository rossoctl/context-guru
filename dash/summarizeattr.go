package dash

import (
	"context"
	"database/sql"
	"math"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/kvcache"
)

// Summarize attribution, read side.
//
// request_components.saved_usd is written one request at a time, so it cannot know where a
// summary's attributable span ends. This file reads the same rows back in order, runs them
// through the episode walk (compactepisode.go) and returns the SummarizeLedger: what counts, and
// what does not. The headline, the Components tab and the episode view all read that one ledger.
//
// The rule, from the repo owner: summarize fires at fill frac 0.90. Without it the client
// compacts natively at about 1.00 of the window. So credit is attributable only inside
// span = ceiling - fill (see spanFor). Past the client's own compaction nothing is ours.

// pricerFn adapts a Pricer to the lookup rePriceRow wants. nil when there is no Pricer.
func pricerFn(ctx context.Context, p modelinfo.Pricer) rePriceFn {
	if p == nil {
		return nil
	}
	return func(model string) (modelinfo.Price, bool) {
		if model == "" {
			return modelinfo.Price{}, false
		}
		price, ok := p.Price(ctx, model)
		return price, ok && !price.Zero()
	}
}

// rePriceRow values a stored summarize row through Event.ComponentSavedUSD, the same path
// Event.Price used to store it, so a row written before the first-removal rule changed reads
// like a new one. Only a row with a unique (first) part changes; a pure replay keeps its stored
// figure, which keeps the rates it was written with. A row with no stored figure is left at zero:
// it predates the column, and the estimate path owns it.
func rePriceRow(r *compactRow, price rePriceFn) {
	if price == nil || r.StoredUSD == 0 || r.SavedUnique <= 0 || r.SavedGross <= 0 {
		return
	}
	p, ok := price(r.Model)
	if !ok {
		return
	}
	ev := Event{
		Model: r.Model, CacheMissReason: r.MissReason,
		FreshInput: r.FreshInput, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
	}
	ev.BilledTokenFactor, _ = tokens.BilledDeltaFactor(r.Model)
	r.SavedUSD = ev.ComponentSavedUSD(p, SummarizerComponents[0], int(r.SavedGross), int(r.SavedUnique))
	r.FirstUSD = ev.ComponentSavedUSD(p, SummarizerComponents[0], int(r.SavedUnique), int(r.SavedUnique))
}

// apportionFirst gives a row that was not re-priced (no rates available) the unique share of its
// stored figure as its first removal, so the ledger can still tell a first removal from a replay.
func apportionFirst(r *compactRow) {
	if r.FirstUSD != 0 || r.SavedUnique <= 0 || r.SavedGross <= 0 {
		return
	}
	share := float64(min(int(r.SavedUnique), int(r.SavedGross))) / float64(r.SavedGross)
	r.FirstUSD = r.SavedUSD * share
}

// SummarizeLedgerFor runs the walk over the sessions in which summarize ever acted and returns
// its ledger. A conversation whose model window is not exactly known contributes nothing, so its
// credit stays as stored: the same rule the episode view uses for the same reason.
//
// It walks each summarizer component on its own and returns the sum, so the headline is the sum of
// the per-component figures SummarizeLedgersFor gives the Components tab.
func (d *DB) SummarizeLedgerFor(f Filter, windows modelinfo.Resolver, p modelinfo.Pricer) (*SummarizeLedger, error) {
	m, err := d.SummarizeLedgersFor(f, windows, p)
	if err != nil {
		return nil, err
	}
	return sumLedgers(m), nil
}

// sumLedgers adds ledgers field by field.
func sumLedgers(m map[string]*SummarizeLedger) *SummarizeLedger {
	t := &SummarizeLedger{}
	for _, name := range SummarizerComponents {
		if l := m[name]; l != nil {
			t.FirstRemovalUSD += l.FirstRemovalUSD
			t.ReplayUSD += l.ReplayUSD
			t.StoredUSD += l.StoredUSD
			t.PastSpanUSD += l.PastSpanUSD
			t.PrefixChangeUSD += l.PrefixChangeUSD
			t.Rows += l.Rows
		}
	}
	return t
}

// SummarizeLedgersFor is one ledger per summarizer component, keyed by component name.
func (d *DB) SummarizeLedgersFor(f Filter, windows modelinfo.Resolver, p modelinfo.Pricer) (map[string]*SummarizeLedger, error) {
	out := map[string]*SummarizeLedger{}
	for _, name := range SummarizerComponents {
		l, err := d.summarizerLedger(f, windows, p, name)
		if err != nil {
			return nil, err
		}
		out[name] = l
	}
	return out, nil
}

func (d *DB) summarizerLedger(f Filter, windows modelinfo.Resolver, p modelinfo.Pricer, component string) (*SummarizeLedger, error) {
	if d == nil || p == nil || windows == nil {
		return &SummarizeLedger{}, nil
	}
	ctx := d.readCtx()
	rows, err := d.compactEpisodeDataset(f, pricerFn(ctx, p), true, []string{component})
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if len(rows) == 0 {
		return &SummarizeLedger{}, nil
	}
	models := map[string]bool{}
	for _, r := range rows {
		models[r.Model] = true
	}
	names := make([]string, 0, len(models))
	for m := range models {
		names = append(names, m)
	}
	list := kvcache.NewPriceList(ctx, names, p, kvcache.Multipliers{}, nil)
	out := walkCompactEpisodes(rows,
		func(model string) (int, bool) {
			w, exact, ok := modelinfo.Exact(windows, ctx, model)
			return w, exact && ok
		},
		list.For, 0, defaultFillFrac, 0)
	return &out.Ledger, nil
}

// ApplySummarizeLedger corrects the headline for summarize's credit: the baseline carried the
// stored figure, so the difference moves the baseline and every total built on it.
func (o *Overview) ApplySummarizeLedger(l *SummarizeLedger) {
	if o == nil || l == nil {
		return
	}
	delta := l.DeltaUSD()
	o.SummarizeCorrectionUSD = delta
	o.BaselineCostUSD += delta
	o.NetSavedUSD += delta
	o.TotalSavedUSD += delta
	o.TotalReducedUSD += delta
}

// applySummarizeLedgerToRow moves summarize's component row onto the ledger: the saved figure,
// its first-removal / replay split and the verdicts that follow from them.
//
// An empty ledger (an unknown model window, a trigger above the client ceiling, no resolver) leaves
// the row as it was. A partial one covers only some conversations: the stored credit the ledger did
// not see stays in the replay column, so the split still adds up to saved_usd.
func applySummarizeLedgerToRow(c *ComponentRow, l *SummarizeLedger) {
	if c == nil || l == nil || !IsSummarizer(c.Component) || l.Rows == 0 {
		return
	}
	delta := l.DeltaUSD()
	uncovered := math.Max(c.SavedUSD-l.StoredUSD, 0)
	c.SavedUSD += delta
	c.NetUSD += delta
	c.NetUSDWithEstimate += delta
	c.SavedUSDFirstRemoval = l.FirstRemovalUSD
	c.SavedUSDReplay = l.ReplayUSD + uncovered
	c.SavedUSDDecomposed = c.SavedUSDFirstRemoval + c.SavedUSDReplay
	c.SavedUSDDecomposedStored = c.SavedUSDDecomposed
	c.NetUSDFirstRemoval = c.SavedUSDFirstRemoval - c.LLMCostUSD
	c.ReplayMultiple = 0
	if c.SavedUSDFirstRemoval > 0 {
		c.ReplayMultiple = c.SavedUSDDecomposed / c.SavedUSDFirstRemoval
	}
	c.SummarizeCreditPastSpanUSD = l.PastSpanUSD
	c.SummarizeCreditPrefixChangeUSD = l.PrefixChangeUSD
}

// summarizerOwnCost is context-guru's model spend on requests where summarize ran and no
// extraction call owns the cost. summarize records no extraction_calls rows (its call is detached
// and its usage lands on a request row, see cheapmodel.ReplayUsage), so without this the
// Components tab showed summarize at llm_calls=0, llm_cost_usd=0 while the money sat in
// requests.cg_llm_cost_usd with no owner.
//
// A request counts as one call. A request with an extraction call is left to extract_llm's share
// (the apportioning in components()), so nothing is counted twice.
func (d *DB) summarizerOwnCost(cond string, args []any, component string) (calls int64, usd float64, err error) {
	if !IsSummarizer(component) {
		return 0, 0, nil // the name is spliced into the SQL, so only a listed one gets in
	}
	var cost sql.NullFloat64
	err = d.sql.QueryRowContext(d.readCtx(), `SELECT COUNT(*), SUM(r.cg_llm_cost_usd)
		FROM requests r JOIN request_components c ON c.request_id = r.id AND c.component = '`+component+`'
		WHERE `+cond+` AND r.cg_llm_cost_usd > 0
		AND NOT EXISTS (SELECT 1 FROM extraction_calls x WHERE x.request_id = r.id
			AND x.strategy NOT GLOB '*_billed')`, args...).Scan(&calls, &cost)
	return calls, cost.Float64, err
}
