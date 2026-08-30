package dash

import "testing"

// StrategyLedger groups a strategy's own ping rows by tenant, and now attributes each
// credited request to whichever strategy's ping actually earned it (see StrategyLedger's
// own comment) rather than a tenant's whole lifetime credit.

func TestStrategyLedgerGroupsPingsByTenantAndAttributesTheirCredit(t *testing.T) {
	db := openTestDB(t)

	t1ping := kaPing(1000, "s1", 0.02, 40_000, 0)
	t1ping.TenantID, t1ping.KeepAliveStrategyID = "t1", "strat-1"
	t1credit := kaCredit(2000, "s1", 0.10)
	t1credit.TenantID, t1credit.KeepAliveStrategyID = "t1", "strat-1"

	t2ping := kaPing(1500, "s2", 0.03, 60_000, 0)
	t2ping.TenantID, t2ping.KeepAliveStrategyID = "t2", "strat-1"
	t2credit := kaCredit(2500, "s2", 0.05)
	t2credit.TenantID, t2credit.KeepAliveStrategyID = "t2", "strat-1"

	// A ping under a DIFFERENT strategy, and one that is not a ping at all: neither must
	// be counted for strat-1.
	otherStrategyPing := kaPing(1600, "s3", 0.09, 90_000, 0)
	otherStrategyPing.TenantID, otherStrategyPing.KeepAliveStrategyID = "t1", "strat-2"

	// t1's credit from an EARLIER idle span, under strat-2 (a strategy swap over time — the
	// exact case that used to inflate every strategy's ledger to the tenant's whole
	// lifetime credit): must not count toward strat-1's SavedUSD.
	t1olderCredit := kaCredit(1800, "s4", 0.30)
	t1olderCredit.TenantID, t1olderCredit.KeepAliveStrategyID = "t1", "strat-2"

	if err := db.insertBatch([]*Event{t1ping, t1credit, t2ping, t2credit, otherStrategyPing, t1olderCredit}); err != nil {
		t.Fatal(err)
	}

	led, err := db.StrategyLedger("strat-1")
	if err != nil {
		t.Fatal(err)
	}
	if led.StrategyID != "strat-1" {
		t.Errorf("StrategyID = %q", led.StrategyID)
	}
	if led.Pings != 2 {
		t.Errorf("Pings = %d, want 2 (the strat-2 ping must not be counted)", led.Pings)
	}
	if len(led.Tenants) != 2 {
		t.Fatalf("Tenants = %d rows, want 2: %+v", len(led.Tenants), led.Tenants)
	}
	byTenant := map[string]StrategyLedgerRow{}
	for _, r := range led.Tenants {
		byTenant[r.TenantID] = r
	}
	t1 := byTenant["t1"]
	if t1.Pings != 1 || t1.PingUSD < 0.019 || t1.PingUSD > 0.021 {
		t.Errorf("t1 row = %+v, want 1 ping at ~$0.02", t1)
	}
	if t1.SavedUSD < 0.099 || t1.SavedUSD > 0.101 {
		t.Errorf("t1 SavedUSD = %v, want ~0.10 (only the credit strat-1's own ping earned, "+
			"not the $0.30 credited under strat-2 in an earlier span)", t1.SavedUSD)
	}
	t2 := byTenant["t2"]
	if t2.Pings != 1 || t2.PingUSD < 0.029 || t2.PingUSD > 0.031 {
		t.Errorf("t2 row = %+v, want 1 ping at ~$0.03", t2)
	}
	// Totals are the sum of the per-tenant rows.
	if led.PingUSD < t1.PingUSD+t2.PingUSD-0.0001 || led.PingUSD > t1.PingUSD+t2.PingUSD+0.0001 {
		t.Errorf("total PingUSD %v does not sum the rows (%v + %v)", led.PingUSD, t1.PingUSD, t2.PingUSD)
	}
	if led.NetUSD != led.SavedUSD-led.PingUSD {
		t.Errorf("NetUSD %v != SavedUSD %v - PingUSD %v", led.NetUSD, led.SavedUSD, led.PingUSD)
	}
}

// A strategy with no ping rows yet answers with an empty ledger, not an error.
func TestStrategyLedgerOnAnUnusedStrategy(t *testing.T) {
	db := openTestDB(t)
	led, err := db.StrategyLedger("never-fired")
	if err != nil {
		t.Fatal(err)
	}
	if led.Pings != 0 || len(led.Tenants) != 0 {
		t.Errorf("got %+v, want an empty ledger", led)
	}
}
