package dash

import (
	"context"
	"database/sql"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// ratioCounts is a request ratio of 0.5 token a byte: the provider billed 3000 tokens for 6000 bytes of text.
var ratioCounts = accrec.RatioCounts{BilledTokens: 3000, TextBytes: 6000, Known: true}

// A request that carries the accountant Result stores its request ratio, and the byte counts, as counts. A request
// with none stores NULL: not zero, so a reader keeps its fallback.
func TestTheRequestRatioIsStoredAsCountsAndNullWhenUnknown(t *testing.T) {
	db := openTestDB(t)
	known := mkEvent(1000, "s", "claude", 100, 90)
	known.Ratio, known.SweepExpandBytes, known.FilteredDeclBytes = ratioCounts, 777, 555
	known.Ratio.Capped = true
	unknown := mkEvent(1001, "s", "claude", 100, 90)
	if err := db.insertBatch([]*Event{known, unknown}); err != nil {
		t.Fatal(err)
	}
	read := func(ts int64) (by, tok, capped, sweep, decl sql.NullInt64) {
		t.Helper()
		if err := db.sql.QueryRow(`SELECT body_text_bytes, body_billed_tokens, ratio_capped, sweep_expand_bytes, filtered_decl_bytes
			FROM requests WHERE ts = ?`, ts).Scan(&by, &tok, &capped, &sweep, &decl); err != nil {
			t.Fatal(err)
		}
		return
	}
	by, tok, capped, sweep, decl := read(1000)
	if by.Int64 != 6000 || tok.Int64 != 3000 || capped.Int64 != 1 || sweep.Int64 != 777 || decl.Int64 != 555 {
		t.Fatalf("stored %v %v %v %v %v, want 6000 3000 1 777 555", by, tok, capped, sweep, decl)
	}
	by, tok, capped, sweep, decl = read(1001)
	if by.Valid || tok.Valid || capped.Valid || sweep.Valid || decl.Valid {
		t.Fatalf("an unknown ratio stored %v %v %v %v %v, want NULL in every column", by, tok, capped, sweep, decl)
	}
}

// SetAccountant keeps the ratio of the Result on the event, and nothing for a Result with none.
func TestSetAccountantKeepsTheRequestRatio(t *testing.T) {
	r := accountant.RatioFromText(3000, 6000, false)
	e := &Event{}
	e.SetAccountant(accountant.Result{Ratio: &r})
	if e.Ratio != ratioCounts {
		t.Fatalf("Ratio = %+v, want %+v", e.Ratio, ratioCounts)
	}
	e = &Event{}
	e.SetAccountant(accountant.Result{})
	if e.Ratio.Known {
		t.Fatalf("a Result with no ratio set %+v", e.Ratio)
	}
}

// The sweep's expand charge is the restored BYTES through the request's own ratio when the row has one, and the
// host's count through the legacy table when it has not (#1533).
func TestTheExpandChargeUsesTheRowsOwnRatio(t *testing.T) {
	cold := sweepEvent(1, "s", CacheTTLExpiry, 0, 90_000, 50, 10_000, 10_000)
	live := sweepEvent(2, "s", CacheHit, 80_000, 12_000, 50, 10_000, 0)
	live.SweepExpands, live.SweepExpandTokens, live.SweepExpandBytes, live.Ratio = 1, 3_000, 2_000, ratioCounts // 2000 bytes: 1000 tokens
	old := sweepEvent(3, "s", CacheHit, 80_000, 12_000, 50, 10_000, 0)
	old.SweepExpands, old.SweepExpandTokens = 1, 3_000 // a row from before the ratio: no bytes, no ratio
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{cold, live, old}); err != nil {
		t.Fatal(err)
	}
	l, err := db.SweepLedgerFor(context.Background(), Filter{TenantAll: true}, staticPricer{sweepPrice})
	if err != nil {
		t.Fatal(err)
	}
	factor, _ := tokens.LegacyFactor("claude-sonnet-5")
	want := (1_000 + 3_000*factor) * sweepPrice.CacheWrite
	if !approx(l.ExpandChargeUSD, want) {
		t.Fatalf("expand charge = %v, want 1000 live tokens + 3000 x %v legacy, at the write rate = %v", l.ExpandChargeUSD, factor, want)
	}
}

// A row with its own ratio and the removed bytes prices the declaration filter's removal in the provider's tokens
// (factor 1 today). A row with neither keeps today's answer.
func TestTheDeclarationFilterCreditUsesTheRowsOwnRatio(t *testing.T) {
	db := openTestDB(t)
	live := mkEvent(1000, "s", "claude", 100, 90)
	live.TenantID, live.Tools, live.FilteredDeclTokens, live.FilteredDeclBytes, live.Ratio = "t1", 4, 1000, 4000, ratioCounts // 4000 bytes: 2000 tokens
	live.CacheRead = 5000
	old := mkEvent(1001, "s", "claude", 100, 90)
	old.TenantID, old.Tools, old.FilteredDeclTokens = "t1", 4, 1000
	old.CacheRead = 5000
	if err := db.insertBatch([]*Event{live, old}); err != nil {
		t.Fatal(err)
	}
	got, err := db.DeclFilterSavings(Filter{Tenant: "t1"}, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	// flatPrice reads at 0.1 x $1/MTok: 2000 provider tokens and 1000 host tokens at factor 1.
	if want := (2000 + 1000) * 1e-7; got.USD < want*0.999 || got.USD > want*1.001 {
		t.Errorf("USD = %g, want %g (2000 tokens from the row's own ratio, 1000 from the host count)", got.USD, want)
	}
	byTenant, err := db.DeclFilterSavingsByTenant(0, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	if s := byTenant["t1"]; s == nil || s.USD != got.USD {
		t.Errorf("the per-tenant pass gave %+v, want the same USD as the scope pass (%g)", s, got.USD)
	}
}
