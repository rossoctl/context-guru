package dash

import (
	"context"
	"log/slog"
	"time"
)

// defaultRepriceInterval is how often the pass looks for a price. The host's price map loads in
// the background and normally lands within seconds of start.
const defaultRepriceInterval = 15 * time.Second

// maxUnpriced bounds the rows waiting for a price. At the cap the oldest is dropped and stays
// partial at $0, which the accounting already reads as unknown.
const maxUnpriced = 4096

// noteUnpriced remembers a stored row that has billed tokens and no cost, because the price was
// unknown when it was written (issue #920). Called on the writer goroutine, after the insert.
//
// A copy with only the fields a price needs is kept, not the event: the event carries content.
func (r *Recorder) noteUnpriced(e *Event) {
	// A bypassed request is written "missing" and becomes "partial" when the count worker fills in
	// its token count, after this runs, so both labels are queued. A row whose compaction calls
	// could not be priced is left alone: repricing it would mark it complete with
	// cg_llm_cost_usd still an unknown 0.
	unpriced := e.TokenAccounting == AccountingPartial || e.TokenAccounting == AccountingMissing
	if r.opts.Prices == nil || e.ID == 0 || e.Model == "" || !unpriced || e.CostUSD != 0 || e.CGCostUnpriced ||
		e.FreshInput+e.CacheRead+e.CacheWrite+e.OutputTokens <= 0 {
		return
	}
	c := &Event{ID: e.ID, TS: e.TS, TenantID: e.TenantID, Model: e.Model, KeepAlive: e.KeepAlive,
		FreshInput: e.FreshInput, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite,
		CacheWrite1h: e.CacheWrite1h, OutputTokens: e.OutputTokens}
	r.unpricedMu.Lock()
	defer r.unpricedMu.Unlock()
	if len(r.unpriced) >= maxUnpriced {
		r.unpriced = r.unpriced[1:]
	}
	r.unpriced = append(r.unpriced, c)
}

func (r *Recorder) repriceLoop() {
	defer r.wg.Done()
	every := r.opts.RepriceInterval
	if every <= 0 {
		every = defaultRepriceInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.repricePass()
		}
	}
}

// repricePass prices every waiting row whose model now has a price. The cost is the row's own
// tokens at the current rate, the same arithmetic as at write time. It books no saving: the row
// had none (acc_fallback unpriced), so baseline_cost_usd equals cost_usd. A row whose model is
// still unknown waits for the next pass.
func (r *Recorder) repricePass() {
	r.unpricedMu.Lock()
	waiting := r.unpriced
	r.unpriced = nil
	r.unpricedMu.Unlock()
	if len(waiting) == 0 {
		return
	}
	var keep []*Event
	var done int
	for _, e := range waiting {
		p, ok := r.opts.Prices.Price(context.Background(), e.Model)
		if !ok || p.Zero() || !e.priceCost(p, true) {
			keep = append(keep, e)
			continue
		}
		if err := r.db.applyReprice(e); err != nil {
			slog.Warn("dash: could not price a row booked before the price map loaded", "id", e.ID, "err", err)
			keep = append(keep, e)
			continue
		}
		done++
	}
	if len(keep) > 0 {
		r.unpricedMu.Lock()
		r.unpriced = append(keep, r.unpriced...)
		if over := len(r.unpriced) - maxUnpriced; over > 0 {
			r.unpriced = r.unpriced[over:] // the oldest go first, as in noteUnpriced
		}
		r.unpricedMu.Unlock()
	}
	if done > 0 {
		slog.Info("dash: priced rows booked before the price map loaded", "rows", done, "still_waiting", len(keep))
	}
}

// applyReprice writes the cost priceCost computed on e into its stored row, and adds it to the
// tenant's monthly spend in the same transaction. It changes only a row that is still partial at
// $0 (partial, or missing for a bypassed row), so a row priced some other way is never overwritten and the rollup is never added twice.
func (d *DB) applyReprice(e *Event) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	res, err := tx.Exec(`UPDATE requests SET cost_usd = ?, baseline_cost_usd = ?, cache_saved_usd = ?,
		billed_token_factor = ?, token_accounting = ?, currency = ?
		WHERE id = ? AND token_accounting IN (?, ?) AND cost_usd = 0`,
		e.CostUSD, e.BaselineCostUSD, e.CacheSavedUSD, e.BilledTokenFactor, AccountingComplete, rowCurrency(e.Currency),
		e.ID, AccountingPartial, AccountingMissing)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO tenant_spend(tenant_id,month,usd) VALUES (?,?,?)
		ON CONFLICT(tenant_id,month) DO UPDATE SET usd = usd + excluded.usd`,
		e.TenantID, monthKey(e.TS), e.CostUSD); err != nil {
		return err
	}
	return tx.Commit()
}
