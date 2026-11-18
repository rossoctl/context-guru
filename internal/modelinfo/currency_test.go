package modelinfo

import (
	"context"
	"strings"
	"testing"
)

// A price file names the unit of its rates. Without the key the rates are US dollars, as before.
func TestTableCurrencyDefaultsToUSD(t *testing.T) {
	p, ok := table(t).Price(context.Background(), "aws/claude-sonnet-5")
	if !ok {
		t.Fatal("aws/claude-sonnet-5 not found")
	}
	if got := p.Unit(); got != "USD" {
		t.Fatalf("unit = %q, want USD for a file with no currency key", got)
	}
}

// Bob's file says Bobcoins, and every entry it prices carries that unit to whoever reads the cost.
func TestTableCurrencyTravelsWithEachEntry(t *testing.T) {
	tb, err := ParseTable([]byte("currency: Bobcoins\nmodels:\n  - {match: premium, in: 2.0, out: 2.0}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Price(context.Background(), "premium")
	if !ok {
		t.Fatal("premium not found")
	}
	if got := p.Unit(); got != "Bobcoins" {
		t.Fatalf("unit = %q, want Bobcoins", got)
	}
}

// A unit name reaches the dashboard. A typo, a symbol or markup is a load error, not a label.
func TestTableRefusesABadCurrency(t *testing.T) {
	for _, bad := range []string{`"$"`, `"US Dollar"`, `"<b>x</b>"`, `"Bobcoins1"`, `"abcdefghijklmnopq"`} {
		_, err := ParseTable([]byte("currency: " + bad + "\nmodels:\n  - {match: premium, in: 2.0, out: 2.0}\n"))
		if err == nil || !strings.Contains(err.Error(), "currency must be") {
			t.Errorf("currency %s: err = %v, want a currency load error", bad, err)
		}
	}
}

// "mixed" is the dashboard's word for a view in more than one unit. A price file that named it as its
// unit would hide every money figure, so it is refused, in any case.
func TestTableRefusesMixedAsACurrency(t *testing.T) {
	for _, bad := range []string{"mixed", "Mixed", "MIXED"} {
		_, err := ParseTable([]byte("currency: " + bad + "\nmodels:\n  - {match: premium, in: 2.0, out: 2.0}\n"))
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("currency %s: err = %v, want a reserved-name load error", bad, err)
		}
	}
}

// The dashboard shows dollars only for "USD". A file that writes usd in another case means dollars too,
// so the loader stores the one spelling.
func TestTableReadsUSDInAnyCase(t *testing.T) {
	for _, c := range []string{"usd", "Usd", "USD"} {
		tb, err := ParseTable([]byte("currency: " + c + "\nmodels:\n  - {match: premium, in: 2.0, out: 2.0}\n"))
		if err != nil {
			t.Fatalf("currency %s: %v", c, err)
		}
		p, _ := tb.Price(context.Background(), "premium")
		if got := p.Unit(); got != USD {
			t.Errorf("currency %s: unit = %q, want %q", c, got, USD)
		}
	}
}

// The public map and the built-in table price in dollars.
func TestPriceWithoutAUnitIsUSD(t *testing.T) {
	if got := (Price{Input: 1e-6}).Unit(); got != "USD" {
		t.Fatalf("unit = %q, want USD", got)
	}
}
