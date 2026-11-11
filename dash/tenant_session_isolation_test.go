package dash

import "testing"

// #501: the per-session queries partition by session_id alone, and session ids are
// client-supplied, so two tenants can present the same one. Tenant B's rows must never change
// tenant A's figures.
//
// Every test builds TWO databases, one holding only tenant A's rows and one holding A's plus B's
// under the SAME session id, and compares what A reads from each. Two databases rather than one
// read twice because CachesplitSizeSpread caches its answer per DB for minutes.

const sharedSession = "s-shared"

// isolationDB writes A's rows, then B's when withB is set, through the real insert path.
func isolationDB(t *testing.T, withB bool, a, b []*Event) *DB {
	t.Helper()
	db := openTestDB(t)
	for _, e := range a {
		e.TenantID = "A"
	}
	if err := db.insertBatch(a); err != nil {
		t.Fatal(err)
	}
	if withB {
		for _, e := range b {
			e.TenantID = "B"
		}
		if err := db.insertBatch(b); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// The replay ceiling counts later turns of a session; B's turns are not A's turns.
func TestReplayCeilingIgnoresOtherTenantsRowsInTheSameSession(t *testing.T) {
	mkA := func() []*Event {
		var out []*Event
		for i := 0; i < 3; i++ {
			out = append(out, mkEvent(int64(1000+i), sharedSession, "m1", 100, 90))
		}
		return out
	}
	mkB := func() []*Event {
		var out []*Event
		for i := 0; i < 10; i++ {
			out = append(out, mkEvent(int64(2000+i), sharedSession, "m1", 100, 90))
		}
		return out
	}
	alone, mixed := isolationDB(t, false, mkA(), mkB()), isolationDB(t, true, mkA(), mkB())
	for name, f := range map[string]Filter{
		"session-scoped (correlated form)": {Tenant: "A", Session: sharedSession},
		"unscoped (window form)":           {Tenant: "A"},
	} {
		a, err := alone.Overview(f)
		if err != nil {
			t.Fatal(err)
		}
		m, err := mixed.Overview(f)
		if err != nil {
			t.Fatal(err)
		}
		if a.ReplayProjectedTokens != 30 {
			t.Fatalf("%s: baseline ceiling = %d, want 10 x (2+1+0) = 30", name, a.ReplayProjectedTokens)
		}
		if m.ReplayProjectedTokens != a.ReplayProjectedTokens {
			t.Errorf("%s: tenant A's ReplayProjectedTokens went %d -> %d when tenant B wrote rows",
				name, a.ReplayProjectedTokens, m.ReplayProjectedTokens)
		}
	}
}

// A compaction reset is a drop in tokens_before against the PREVIOUS turn. B's row, landing
// between A's two turns, must not become A's "previous turn".
func TestCompactionResetsIgnoreOtherTenantsRowsInTheSameSession(t *testing.T) {
	mkA := func() []*Event {
		return []*Event{
			mkEvent(1000, sharedSession, "m1", 1000, 900),
			mkEvent(3000, sharedSession, "m1", 500, 450), // 500 < 1000: one reset
		}
	}
	mkB := func() []*Event {
		return []*Event{mkEvent(2000, sharedSession, "m1", 100, 90)} // 500 < 100 is false
	}
	alone, mixed := isolationDB(t, false, mkA(), mkB()), isolationDB(t, true, mkA(), mkB())
	for name, f := range map[string]Filter{
		"session-scoped": {Tenant: "A", Session: sharedSession},
		"unscoped":       {Tenant: "A"},
	} {
		a, err := alone.Overview(f)
		if err != nil {
			t.Fatal(err)
		}
		m, err := mixed.Overview(f)
		if err != nil {
			t.Fatal(err)
		}
		if a.CompactionResets != 1 {
			t.Fatalf("%s: baseline resets = %d, want 1", name, a.CompactionResets)
		}
		if m.CompactionResets != a.CompactionResets {
			t.Errorf("%s: tenant A's CompactionResets went %d -> %d when tenant B wrote rows",
				name, a.CompactionResets, m.CompactionResets)
		}
	}
}

// The historical cache-split credit values each session's FIRST request. If B's earlier row in
// the shared session is ranked first, A's real first request is no longer rn=1 and A loses it.
func TestHistoricalCachesplitIgnoresOtherTenantsRowsInTheSameSession(t *testing.T) {
	const stable = 5697
	mk := func(sess string, ts int64, stableTok int) *Event {
		e := &Event{TS: ts, SessionID: sess, Model: "aws/claude-sonnet-5",
			TokensBefore: 100, TokensAfter: 100, FreshInput: 10, CacheRead: 54_304, CacheWrite: 1_000,
			OutputTokens: 20, SplitStableTokens: stableTok}
		e.Price(ibmSonnet, true)
		return e
	}
	mkA := func() []*Event {
		return []*Event{
			mk("s-teach", 500, stable), // teaches A's model a constant stable half
			mk(sharedSession, 2000, 0), // A's session-first request
		}
	}
	mkB := func() []*Event { return []*Event{mk(sharedSession, 1000, 0)} } // earlier than A's
	alone, mixed := isolationDB(t, false, mkA(), mkB()), isolationDB(t, true, mkA(), mkB())
	pricer := staticPricer{ibmSonnet}

	for name, f := range map[string]Filter{
		"session-scoped": {Tenant: "A", Session: sharedSession},
		"unscoped":       {Tenant: "A"},
	} {
		a, err := alone.CachesplitHistoricalUSD(f, pricer)
		if err != nil {
			t.Fatal(err)
		}
		m, err := mixed.CachesplitHistoricalUSD(f, pricer)
		if err != nil {
			t.Fatal(err)
		}
		if a.Requests != 1 {
			t.Fatalf("%s: baseline = %+v, want A's one session-first request", name, a)
		}
		if m != a {
			t.Errorf("%s: tenant A's historical credit went %+v -> %+v when tenant B wrote rows", name, a, m)
		}
	}

	// The by-tenant pass has the same window, so it must agree.
	ga, err := alone.CachesplitHistoricalUSDByTenant(0, pricer)
	if err != nil {
		t.Fatal(err)
	}
	gm, err := mixed.CachesplitHistoricalUSDByTenant(0, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if gm["A"] != ga["A"] {
		t.Errorf("by-tenant: tenant A's credit went %+v -> %+v when tenant B wrote rows", ga["A"], gm["A"])
	}
}

// The credit refuses a model whose recorded stable half is not one constant number. That spread
// is read over the whole deployment, so B recording a different stable size for the same model
// makes A's model look non-constant and removes A's credit.
func TestCachesplitSizeSpreadIsPerTenant(t *testing.T) {
	mk := func(sess string, ts int64, stableTok int) *Event {
		e := &Event{TS: ts, SessionID: sess, Model: "aws/claude-sonnet-5",
			TokensBefore: 100, TokensAfter: 100, FreshInput: 10, CacheRead: 54_304, CacheWrite: 1_000,
			OutputTokens: 20, SplitStableTokens: stableTok}
		e.Price(ibmSonnet, true)
		return e
	}
	mkA := func() []*Event {
		return []*Event{mk("s-teach", 500, 5697), mk("s-first", 2000, 0)}
	}
	mkB := func() []*Event { return []*Event{mk("s-b", 600, 8000)} } // same model, different size
	alone, mixed := isolationDB(t, false, mkA(), mkB()), isolationDB(t, true, mkA(), mkB())
	pricer := staticPricer{ibmSonnet}

	a, err := alone.CachesplitHistoricalUSD(Filter{Tenant: "A"}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	m, err := mixed.CachesplitHistoricalUSD(Filter{Tenant: "A"}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if a.Requests != 1 {
		t.Fatalf("baseline = %+v, want A's one session-first request credited", a)
	}
	if m != a {
		t.Errorf("tenant A's credit went %+v -> %+v because tenant B recorded another stable size", a, m)
	}
}

// The inflation correction deducts, per keep-alive ping, the savings already realised earlier in
// the ping's session. A ping of tenant B must not deduct tenant A's savings from A's ceiling.
func TestInflationCorrectionIgnoresOtherTenantsPingsInTheSameSession(t *testing.T) {
	mkA := func() []*Event {
		return []*Event{
			mkEvent(1000, sharedSession, "m1", 100, 90),
			mkEvent(1100, sharedSession, "m1", 100, 90),
			kaPing(1150, sharedSession, 0.01, 50_000, 0), // between A's 2nd and 3rd turn
			mkEvent(1200, sharedSession, "m1", 100, 90),
		}
	}
	mkB := func() []*Event {
		return []*Event{
			mkEvent(2000, sharedSession, "m1", 100, 90),
			kaPing(2500, sharedSession, 0.01, 50_000, 0),
		}
	}
	alone, mixed := isolationDB(t, false, mkA(), mkB()), isolationDB(t, true, mkA(), mkB())
	f := Filter{Tenant: "A"}
	a, err := alone.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	m, err := mixed.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	// 10 x (2+1+0) = 30; A's ping sits before A's last turn only, so it deducts nothing a later
	// turn could replay (see TestTheReplayCeilingCountsAgentTurnsNotPings).
	if a.ReplayProjectedTokens != 30 {
		t.Fatalf("baseline ceiling = %d, want 30", a.ReplayProjectedTokens)
	}
	if m.ReplayProjectedTokens != a.ReplayProjectedTokens {
		t.Errorf("tenant A's ReplayProjectedTokens went %d -> %d when tenant B pinged the same session id",
			a.ReplayProjectedTokens, m.ReplayProjectedTokens)
	}
}
