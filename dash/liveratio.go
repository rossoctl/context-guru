package dash

import (
	"database/sql"

	"github.com/rossoctl/context-guru/accrec"
)

// The request ratio and the byte counts that price a removal with it (#1533). All are counts: no text. A row
// written before the columns existed, and a request with no accountant Result, hold NULL, and a reader keeps
// its fallback (the legacy table, tokens.LegacyFactor).
//
//	body_text_bytes, body_billed_tokens, ratio_capped: Result.Ratio, the request's own tokens per byte of text
//	sweep_expand_bytes: the bytes the sweep's expands restored (sweep_expand_tokens is the host's count of them)
//	filtered_decl_bytes: the bytes the declaration filter removed (filtered_decl_tokens is the host's count)

// liveRatioCols are the requests columns of these counts, in insert order (see store.go).
const liveRatioCols = `body_text_bytes, body_billed_tokens, ratio_capped, sweep_expand_bytes, filtered_decl_bytes`

// liveRatioN is the number of columns in liveRatioCols.
const liveRatioN = 5

// liveRatioArgs are the insert values for liveRatioCols. An unknown count is NULL.
func liveRatioArgs(e *Event) []any {
	var by, tok, capped any
	if e.Ratio.Known {
		by, tok, capped = e.Ratio.TextBytes, e.Ratio.BilledTokens, boolInt(e.Ratio.Capped)
	}
	return []any{by, tok, capped, nullInt(e.SweepExpandBytes), nullInt(e.FilteredDeclBytes)}
}

// declRatioCols are the columns a read of the declaration filter adds to its select: the removed bytes and the
// request ratio. declRatio scans them as nullable counts.
const declRatioCols = `r.filtered_decl_bytes, r.body_text_bytes, r.body_billed_tokens, r.ratio_capped`

// declRatio scans declRatioCols. NULL scans as no count.
type declRatio struct{ bytes, textBytes, billed, capped sql.NullInt64 }

func (d *declRatio) dest() []any { return []any{&d.bytes, &d.textBytes, &d.billed, &d.capped} }

// declCounts is the removed bytes of a row and its request ratio.
type declCounts struct {
	bytes  int64
	counts accrec.RatioCounts
}

func (d *declRatio) counts() declCounts {
	return declCounts{bytes: d.bytes.Int64, counts: ratioCountsOf(d.textBytes, d.billed, d.capped)}
}

// ratioCountsOf builds the counts of a row from its three nullable columns: known only when both counts are set.
func ratioCountsOf(textBytes, billed, capped sql.NullInt64) accrec.RatioCounts {
	return accrec.RatioCounts{BilledTokens: billed.Int64, TextBytes: textBytes.Int64, Capped: capped.Int64 != 0,
		Known: billed.Valid && textBytes.Valid}
}
