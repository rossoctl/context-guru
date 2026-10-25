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
	Applied             int64 `json:"applied"`
	AppliedCold         int64 `json:"applied_cold"`
	AppliedChangedPrefx int64 `json:"applied_changed_prefix"`
	// Referenced: drops a later turn used something from, without expanding.
	Referenced int64 `json:"referenced_later"`
	// Expands and ExpandTokens: every expand of a sweep marker, and what it returned.
	Expands      int64 `json:"expands"`
	ExpandTokens int64 `json:"expand_tokens"`
	// FalseExpands: drops expanded back for the first time and not already counted as referenced.
	FalseExpands  int64   `json:"false_expands"`
	FalseDrops    int64   `json:"false_drops"`
	FalseDropRate float64 `json:"false_drop_rate"`
	// ExpandChargeUSD is what the sweep's expands cost, taken off its credit.
	ExpandChargeUSD float64 `json:"expand_charge_usd"`
	// AnswerUSD is what the answers cost beyond the pings' own reads: its own column, booked once on
	// the ping row the question replaced.
	AnswerUSD float64 `json:"answer_usd"`
	// Questions is how many ping rows carry an answer cost.
	Questions int64 `json:"questions"`
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
		SUM(` + sprintfEv(ev, "sweep_false_ref") + `)
		FROM requests r JOIN request_components c ON c.request_id = r.id AND c.component = '` + SweepComponent + `'
		WHERE ` + cond + ` GROUP BY 1, 2`
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s, th string
		var cold, chg, ref int64
		if err := rows.Scan(&s, &th, &cold, &chg, &ref); err != nil {
			rows.Close()
			return nil, err
		}
		out.AppliedCold += cold
		out.AppliedChangedPrefx += chg
		out.Referenced += ref
		t := get(s, th)
		t.Applied += cold + chg
		t.FalseDrops += ref
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Applied = out.AppliedCold + out.AppliedChangedPrefx

	// 2. The expands, from the request rows (not the component rows: an expand lands on whichever
	// request carried the response).
	xq := `SELECT r.session_id, r.thread_id, r.model, SUM(r.sweep_expands), SUM(r.sweep_expand_tokens),
		SUM(r.sweep_false_expands) FROM requests r WHERE ` + cond + ` AND r.sweep_expands > 0 GROUP BY 1, 2, 3`
	xrows, err := d.sql.QueryContext(ctx, xq, args...)
	if err != nil {
		return nil, err
	}
	byModel := map[string]int64{}
	for xrows.Next() {
		var s, th, model string
		var n, tok, fe int64
		if err := xrows.Scan(&s, &th, &model, &n, &tok, &fe); err != nil {
			xrows.Close()
			return nil, err
		}
		out.Expands += n
		out.ExpandTokens += tok
		out.FalseExpands += fe
		byModel[model] += tok
		get(s, th).FalseDrops += fe
	}
	xrows.Close()
	if err := xrows.Err(); err != nil {
		return nil, err
	}
	out.FalseDrops = out.Referenced + out.FalseExpands
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
	c.SavedUSD -= l.ExpandChargeUSD
	c.NetUSD -= l.ExpandChargeUSD
	c.NetUSDWithEstimate -= l.ExpandChargeUSD
}
