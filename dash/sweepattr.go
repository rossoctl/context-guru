package dash

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// THE SWEEP'S LEDGER (#404 PR C, points 7 and 9).
//
// WHAT THE CREDIT IS. extract_llm_sweep's stored saved_usd already follows point 9, because it is
// priced by the same rule as every component (Event.ComponentSavedUSD): the tokens a cold apply
// removes are NEW content keys, so they are priced at the cache-write rate (1.25 x the input price),
// and every later request replays the same bytes, so they are priced at the tier that request paid.
// This file does not re-price the credit. It reads what sits beside it and takes from it:
//
//   - THE EXPANDS. A sweep drop the agent asked back for is content that came back into the prompt.
//     The totals already subtract all expands (SavedAdjusted = SavedUnique - ExpandTokens), with no
//     split by component. Here the expands of SWEEP markers are charged to the sweep, in dollars, at
//     the write rate (the restored text is new input on that request).
//   - THE ANSWER COST. Booked once, on the keep-alive ping row the question replaced (as #458 books
//     the summary call). It sits in its own column. It is NOT subtracted from the credit here: the
//     component's net already carries it as its model spend (billed-elsewhere row), and subtracting
//     it twice is the error this separation exists to prevent.
//
// THE FALSE-DROP RATE is (drops a later turn referred to, without expanding) + (drops the agent
// expanded back), over the drops applied. Metadata only: every count comes from the integers the
// proxy stored on the row, never from content.

// SweepComponent is the pipeline name of the component this ledger is about.
const SweepComponent = "extract_llm_sweep"

// SweepThread is one thread's quality under a label that is not the session id.
type SweepThread struct {
	Thread        string  `json:"thread"`
	Applied       int64   `json:"applied"`
	FalseDrops    int64   `json:"false_drops"`
	FalseDropRate float64 `json:"false_drop_rate"`
}

// SweepLedger is the sweep's quality and money over a filtered window.
type SweepLedger struct {
	Applied              int64 `json:"applied"`
	AppliedCold          int64 `json:"applied_cold"`
	AppliedChangedPrefix int64 `json:"applied_changed_prefix"`
	// AppliedWarm and ReferencedWarm are the warm path's share (#491), part of Applied and Referenced.
	// Warm expands are in FalseExpandsWarm below (a column on the request row), so the warm false-drop
	// rate here counts references and expands, as /stats does.
	AppliedWarm    int64 `json:"applied_warm"`
	ReferencedWarm int64 `json:"referenced_later_warm"`
	// Referenced: drops a later turn used something from, without expanding.
	Referenced int64 `json:"referenced_later"`
	// Expands and ExpandTokens: every expand of a sweep marker, and what it returned.
	Expands      int64 `json:"expands"`
	ExpandTokens int64 `json:"expand_tokens"`
	// FalseExpands: drops expanded back for the first time and not already counted as referenced.
	FalseExpands int64 `json:"false_expands"`
	// FalseExpandsWarm is the part of FalseExpands whose drop was applied warm (#491). FalseDropsWarm
	// and FalseDropRateWarm count references and expands, the same as /stats.
	FalseExpandsWarm  int64   `json:"false_expands_warm"`
	FalseDropsWarm    int64   `json:"false_drops_warm"`
	FalseDropRateWarm float64 `json:"false_drop_rate_warm"`
	FalseDrops        int64   `json:"false_drops"`
	FalseDropRate     float64 `json:"false_drop_rate"`
	// ExpandChargeUSD is what the sweep's expands cost, taken off its credit.
	ExpandChargeUSD float64 `json:"expand_charge_usd"`
	// AnswerUSD is what the answers cost beyond the pings' own reads: its own column, booked once on
	// the ping row the question replaced.
	AnswerUSD float64 `json:"answer_usd"`
	// Questions is how many ping rows carry an answer cost.
	Questions int64 `json:"questions"`
	// CreditUSD is the sweep's stored saving over the window, before the expand charge. NetUSD is what L1
	// asks for: that credit, minus the expand charge, minus the answer cost, in dollars from the
	// provider's own usage (the credit is priced from each row's billed tiers, the answer from the ping
	// row's billed output). Both are filled by SweepLedgerFor so /stats-style readers need no second query.
	CreditUSD float64 `json:"credit_usd"`
	NetUSD    float64 `json:"net_usd"`
	// Cold-apply request cache usage (C2): over requests where the sweep applied on a cold request, the
	// cache read, cache write and fresh input the provider billed. A cold request should read the same
	// cached part as with the sweep off (the system prompt and tools) and write the smaller prefix; the
	// NEXT warm request should read the whole new prefix. ColdReadTokens/ColdWriteTokens are the first,
	// NextWarmReadTokens the second (the first warm request after each such cold request, same thread).
	ColdApplyRequests   int64 `json:"cold_apply_requests"`
	ColdReadTokens      int64 `json:"cold_apply_cache_read"`
	ColdWriteTokens     int64 `json:"cold_apply_cache_write"`
	NextWarmRequests    int64 `json:"next_warm_requests"`
	NextWarmReadTokens  int64 `json:"next_warm_cache_read"`
	NextWarmWriteTokens int64 `json:"next_warm_cache_write"`
	// Threads: the busiest threads by drops applied, at most 20. Labels are hashes.
	Threads []SweepThread `json:"threads,omitempty"`
}

// SweepLedgerFor reads the ledger for the filtered window. p prices the expand charge; with nil p the
// charge is 0 and everything else is exact.
func (d *DB) SweepLedgerFor(ctx context.Context, f Filter, p modelinfo.Pricer) (*SweepLedger, error) {
	if d == nil {
		return &SweepLedger{}, nil
	}
	out := &SweepLedger{}
	cond, args := f.where()

	type tk struct{ session, thread string }
	threads := map[tk]*SweepThread{}
	get := func(s, th string) *SweepThread {
		k := tk{s, th}
		t := threads[k]
		if t == nil {
			t = &SweepThread{}
			threads[k] = t
		}
		return t
	}

	// 1. What the component did, from its events (integers in a JSON object; '' on old rows).
	const ev = `CASE WHEN c.events LIKE '{%' THEN COALESCE(json_extract(c.events, '$.%s'), 0) ELSE 0 END`
	q := `SELECT r.session_id, r.thread_id,
		SUM(` + sprintfEv(ev, "sweep_applied_cold") + `),
		SUM(` + sprintfEv(ev, "sweep_applied_changed_prefix") + `),
		SUM(` + sprintfEv(ev, "sweep_false_ref") + `),
		SUM(` + sprintfEv(ev, "sweep_applied_warm") + `),
		SUM(` + sprintfEv(ev, "sweep_false_ref_warm") + `)
		FROM requests r JOIN request_components c ON c.request_id = r.id AND c.component = '` + SweepComponent + `'
		WHERE ` + cond + ` GROUP BY 1, 2`
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s, th string
		var cold, chg, ref, warm, refWarm int64
		if err := rows.Scan(&s, &th, &cold, &chg, &ref, &warm, &refWarm); err != nil {
			rows.Close()
			return nil, err
		}
		out.AppliedCold += cold
		out.AppliedChangedPrefix += chg
		out.Referenced += ref
		out.AppliedWarm += warm
		out.ReferencedWarm += refWarm
		t := get(s, th)
		t.Applied += cold + chg + warm
		t.FalseDrops += ref
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Applied = out.AppliedCold + out.AppliedChangedPrefix + out.AppliedWarm

	// 2. The expands, from the request rows (not the component rows: an expand lands on whichever
	// request carried the response).
	xq := `SELECT r.session_id, r.thread_id, r.model, SUM(r.sweep_expands), SUM(r.sweep_expand_tokens),
		SUM(r.sweep_false_expands), SUM(r.sweep_false_expands_warm) FROM requests r WHERE ` + cond + ` AND r.sweep_expands > 0 GROUP BY 1, 2, 3`
	xrows, err := d.sql.QueryContext(ctx, xq, args...)
	if err != nil {
		return nil, err
	}
	byModel := map[string]int64{}
	for xrows.Next() {
		var s, th, model string
		var n, tok, fe, few int64
		if err := xrows.Scan(&s, &th, &model, &n, &tok, &fe, &few); err != nil {
			xrows.Close()
			return nil, err
		}
		out.Expands += n
		out.ExpandTokens += tok
		out.FalseExpands += fe
		out.FalseExpandsWarm += few
		byModel[model] += tok
		get(s, th).FalseDrops += fe
	}
	xrows.Close()
	if err := xrows.Err(); err != nil {
		return nil, err
	}
	out.FalseDrops = out.Referenced + out.FalseExpands
	out.FalseDropsWarm = out.ReferencedWarm + out.FalseExpandsWarm
	if out.AppliedWarm > 0 {
		out.FalseDropRateWarm = float64(out.FalseDropsWarm) / float64(out.AppliedWarm)
	}
	if out.Applied > 0 {
		out.FalseDropRate = float64(out.FalseDrops) / float64(out.Applied)
	}
	// The charge: the restored text is new input on the request that carried it, so the write rate,
	// with the same tokenizer correction every other counterfactual token count gets.
	if p != nil {
		for model, tok := range byModel {
			price, ok := p.Price(ctx, model)
			if !ok || price.Zero() {
				continue
			}
			factor, _ := tokens.BilledDeltaFactor(model)
			out.ExpandChargeUSD += float64(tok) * price.CacheWrite * factor
		}
	}

	// 3. The answer cost, on the ping rows. A ping is excluded from every other query by the
	// filter, so this one asks for them.
	pf := f
	pf.WithKeepAlive = true
	pcond, pargs := pf.where()
	var usd float64
	var n int64
	if err := d.sql.QueryRowContext(ctx, `SELECT COALESCE(SUM(r.sweep_answer_usd), 0), COUNT(*)
		FROM requests r WHERE `+pcond+` AND r.keepalive = 1 AND r.sweep_answer_usd > 0`, pargs...).Scan(&usd, &n); err != nil {
		return nil, err
	}
	out.AnswerUSD, out.Questions = usd, n

	// The credit, from the component rows, and the cold-apply cache numbers (C2).
	cq := `SELECT COALESCE(SUM(c.saved_usd), 0) FROM requests r JOIN request_components c
		ON c.request_id = r.id AND c.component = '` + SweepComponent + `' WHERE ` + cond
	if err := d.sql.QueryRowContext(ctx, cq, args...).Scan(&out.CreditUSD); err != nil {
		return nil, err
	}
	out.NetUSD = out.CreditUSD - out.ExpandChargeUSD - out.AnswerUSD
	if err := d.coldApplyCache(ctx, cond, args, out); err != nil {
		return nil, err
	}

	// Threads, busiest first, under a hash label.
	for k, t := range threads {
		if t.Applied == 0 && t.FalseDrops == 0 {
			continue
		}
		if t.Applied > 0 {
			t.FalseDropRate = float64(t.FalseDrops) / float64(t.Applied)
		}
		h := sha256.Sum256([]byte("cg-swl|" + k.session + "\x00" + k.thread))
		t.Thread = hex.EncodeToString(h[:5])
		out.Threads = append(out.Threads, *t)
	}
	sort.Slice(out.Threads, func(i, j int) bool {
		if out.Threads[i].Applied != out.Threads[j].Applied {
			return out.Threads[i].Applied > out.Threads[j].Applied
		}
		return out.Threads[i].Thread < out.Threads[j].Thread
	})
	if len(out.Threads) > 20 {
		out.Threads = out.Threads[:20]
	}
	return out, nil
}

// sprintfEv fills the one %s of an event-extraction template. Names are package constants, never
// input, which is why they may be spliced into SQL.
func sprintfEv(tmpl, name string) string {
	for i := 0; i+1 < len(tmpl); i++ {
		if tmpl[i] == '%' && tmpl[i+1] == 's' {
			return tmpl[:i] + name + tmpl[i+2:]
		}
	}
	return tmpl
}

// ApplySweepLedgerToRow puts the ledger on the sweep's component row: the expands charged to it come
// off its credit and its net, the answer cost sits beside them, and the quality counts are carried.
// A row that is not the sweep's, or a nil ledger, is left as it was.
func ApplySweepLedgerToRow(c *ComponentRow, l *SweepLedger) {
	if c == nil || l == nil || c.Component != SweepComponent {
		return
	}
	c.SweepApplied, c.SweepFalseDrops, c.SweepFalseDropRate = l.Applied, l.FalseDrops, l.FalseDropRate
	c.SweepExpands, c.SweepExpandTokens = l.Expands, l.ExpandTokens
	c.SweepExpandChargeUSD, c.SweepAnswerUSD = l.ExpandChargeUSD, l.AnswerUSD
	c.SweepNetUSD = l.NetUSD
	c.SweepColdApplyRequests, c.SweepColdApplyCacheRead, c.SweepColdApplyCacheWrite =
		l.ColdApplyRequests, l.ColdReadTokens, l.ColdWriteTokens
	c.SweepNextWarmRequests, c.SweepNextWarmCacheRead = l.NextWarmRequests, l.NextWarmReadTokens
	c.SavedUSD -= l.ExpandChargeUSD
	c.NetUSD -= l.ExpandChargeUSD
	c.NetUSDWithEstimate -= l.ExpandChargeUSD
}

// KNOWN IMPRECISION (a dashboard measurement, not a decision): the "next warm" request is the first later
// request of the same thread that is not a keep-alive ping and read from the cache. It can be a LATER cold
// request that happened to read the system prefix, so NextWarmReadTokens is an upper-bound read of the
// first warm turn. It runs one query per cold-apply request, bounded by the filter and used only here.
// coldApplyCache fills the C2 numbers: for each request on which the sweep applied on a cold request, the
// provider's cache tiers; and, for the first later request of the same thread that was not itself a cold
// apply, its tiers too. Read from the stored integers only.
func (d *DB) coldApplyCache(ctx context.Context, cond string, args []any, out *SweepLedger) error {
	const ev = `CASE WHEN c.events LIKE '{%' THEN COALESCE(json_extract(c.events, '$.sweep_applied_cold'), 0) ELSE 0 END`
	rows, err := d.sql.QueryContext(ctx, `SELECT r.id, r.tenant_id, r.session_id, r.thread_id, r.ts, r.cache_read, r.cache_write
		FROM requests r JOIN request_components c ON c.request_id = r.id AND c.component = '`+SweepComponent+`'
		WHERE `+cond+` AND `+ev+` > 0 ORDER BY r.ts`, args...)
	if err != nil {
		return err
	}
	type cold struct {
		id, ts, read, write int64
		tenant, sess, th    string
	}
	var colds []cold
	for rows.Next() {
		var c cold
		if err := rows.Scan(&c.id, &c.tenant, &c.sess, &c.th, &c.ts, &c.read, &c.write); err != nil {
			rows.Close()
			return err
		}
		colds = append(colds, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range colds {
		out.ColdApplyRequests++
		out.ColdReadTokens += c.read
		out.ColdWriteTokens += c.write
		var read, write int64
		err := d.sql.QueryRowContext(ctx, `SELECT cache_read, cache_write FROM requests
			WHERE tenant_id = ? AND session_id = ? AND thread_id = ? AND keepalive = 0 AND id > ? AND cache_read > 0
			ORDER BY id LIMIT 1`, c.tenant, c.sess, c.th, c.id).Scan(&read, &write)
		if err == nil {
			out.NextWarmRequests++
			out.NextWarmReadTokens += read
			out.NextWarmWriteTokens += write
		}
	}
	return nil
}
