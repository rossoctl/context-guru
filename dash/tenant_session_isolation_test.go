package dash

import (
	"context"
	"math"
	"testing"
	"time"
)

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

// --- #1069: archive, eviction and the janitor ---

// Two tenants share a session id. Archiving one tenant's session must move only its rows, file
// them under its own tenant, and leave the other's alone.
func TestArchiveMovesOnlyTheOwningTenantsSession(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "tA", sharedSession, 3, 72*time.Hour)
	seedSessionWithContent(t, rec.db, "tB", sharedSession, 2, 48*time.Hour)

	cands, err := rec.db.oldestLocalSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %+v, want one per tenant (the shared id must not merge them)", cands)
	}
	if cands[0].TenantID != "tA" || cands[0].Requests != 3 {
		t.Fatalf("oldest candidate = %+v, want tenant tA with its own 3 requests", cands[0])
	}
	if n, err := rec.ArchiveSessionFull(context.Background(), cands[0]); err != nil || n != 3 {
		t.Fatalf("ArchiveSessionFull = %d, %v; want tA's 3 rows", n, err)
	}
	var left int
	if err := rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE tenant_id = 'tB' AND session_id = ?`,
		sharedSession).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Errorf("tenant tB has %d rows after tA's archive, want 2", left)
	}
	evs, err := rec.FetchArchived(context.Background(), "tA", sharedSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("tA's archive holds %d events, want 3", len(evs))
	}
	for _, e := range evs {
		if e.TenantID != "tA" {
			t.Errorf("tA's archive holds an event of tenant %q", e.TenantID)
		}
	}

	// tB archives after tA: its index row must not overwrite tA's.
	rest, err := rec.db.oldestLocalSessions(10)
	if err != nil || len(rest) != 1 || rest[0].TenantID != "tB" {
		t.Fatalf("remaining candidates = %+v, %v; want only tB", rest, err)
	}
	if _, err := rec.ArchiveSessionFull(context.Background(), rest[0]); err != nil {
		t.Fatal(err)
	}
	var idx int
	if err := rec.db.sql.QueryRow(`SELECT COUNT(*) FROM archived_sessions WHERE session_id = ?`, sharedSession).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 2 {
		t.Errorf("archive index holds %d rows for the shared id, want one per tenant", idx)
	}
	if evs, err := rec.FetchArchived(context.Background(), "tA", sharedSession); err != nil || len(evs) != 3 {
		t.Errorf("tA's archive after tB archived: %d events, %v; want 3", len(evs), err)
	}
}

// Disk-pressure and age eviction drop the oldest session. A shared id must drop only the
// tenant that was oldest.
func TestDropOldestSessionsKeepsTheOtherTenantsRows(t *testing.T) {
	db := openTestDB(t)
	old := mkEvent(1000, sharedSession, "m1", 100, 90)
	old.TenantID = "tA"
	recent := mkEvent(9000, sharedSession, "m1", 100, 90)
	recent.TenantID = "tB"
	if err := db.insertBatch([]*Event{old, recent}); err != nil {
		t.Fatal(err)
	}
	n, err := db.DropOldestSessions(1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("dropped %d rows, want only tenant tA's 1", n)
	}
	var b int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE tenant_id = 'tB'`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if b != 1 {
		t.Errorf("tenant tB has %d rows left, want 1", b)
	}
}

// An existing database keeps session_id as the archive index's whole key. Opening it must
// rebuild the table around (tenant_id, session_id) and keep every row.
func TestArchivedSessionsKeyMigrationKeepsRowsAndAllowsASharedId(t *testing.T) {
	db := openTestDB(t)
	for _, stmt := range []string{
		`DROP TABLE archived_sessions`,
		`CREATE TABLE archived_sessions (
			session_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT '',
			first_ts INTEGER NOT NULL DEFAULT 0, last_ts INTEGER NOT NULL DEFAULT 0,
			requests INTEGER NOT NULL DEFAULT 0, content_path TEXT NOT NULL DEFAULT '',
			content_bytes INTEGER NOT NULL DEFAULT 0, full_path TEXT NOT NULL DEFAULT '',
			full_bytes INTEGER NOT NULL DEFAULT 0, archived_at INTEGER NOT NULL DEFAULT 0,
			remote TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO archived_sessions(session_id, tenant_id, requests, full_path) VALUES ('s-old','tA',4,'a/path')`,
	} {
		if _, err := db.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ { // twice: the second run must be a no-op
		if err := migrateArchivedSessionsKey(db.sql); err != nil {
			t.Fatalf("migration run %d: %v", i+1, err)
		}
	}
	a, err := db.ArchivedSessionOf("tA", "s-old")
	if err != nil || a.Requests != 4 || a.FullPath != "a/path" {
		t.Fatalf("migrated row = %+v, %v; want the original", a, err)
	}
	if _, err := db.sql.Exec(`INSERT INTO archived_sessions(session_id, tenant_id) VALUES ('s-old','tB')`); err != nil {
		t.Errorf("a second tenant cannot hold the same session id after the migration: %v", err)
	}
}

// --- #1070: the all-tenants views ---

func TestAllTenantsSessionViewsKeepTenantsApart(t *testing.T) {
	a1, a2 := kaAgent(1_000_000, sharedSession, 0.10), kaAgent(1_001_000, sharedSession, 0.10)
	b1, b2, b3 := kaAgent(2_000_000, sharedSession, 0.20), kaAgent(2_001_000, sharedSession, 0.20), kaAgent(2_002_000, sharedSession, 0.20)
	for _, e := range []*Event{a1, a2} {
		e.TenantID = "A"
	}
	for _, e := range []*Event{b1, b2, b3} {
		e.TenantID = "B"
	}
	pingA := kaPing(1_000_500, sharedSession, 0.01, 50_000, 0)
	pingA.TenantID = "A"
	f := newKAFixture(t, a1, a2, b1, b2, b3, pingA)

	rows, total, err := f.db.Sessions(Filter{TenantAll: true}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || total != 2 {
		t.Fatalf("Sessions returned %d rows, total %d; want one row per tenant", len(rows), total)
	}
	byTenant := map[string]*SessionRow{}
	for _, r := range rows {
		byTenant[r.TenantID] = r
	}
	if r := byTenant["A"]; r == nil || r.Turns != 2 || r.KeepAlivePings != 1 {
		t.Errorf("tenant A row = %+v, want 2 turns and its 1 ping", r)
	}
	if r := byTenant["B"]; r == nil || r.Turns != 3 || r.KeepAlivePings != 0 {
		t.Errorf("tenant B row = %+v, want 3 turns and no pings", r)
	}

	// Each tenant's expiry sits in its own row of the concentration table.
	ea := kaExpiry(3_000_000, sharedSession, 600, 1.00, 50_000)
	eb := kaExpiry(4_000_000, sharedSession, 600, 2.00, 50_000)
	for _, e := range ea {
		e.TenantID = "A"
	}
	for _, e := range eb {
		e.TenantID = "B"
	}
	g := newKAFixture(t, append(ea, eb...)...)
	ks, err := g.db.KeepAliveSessions(Filter{TenantAll: true}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ks) != 2 {
		t.Fatalf("KeepAliveSessions returned %d rows, want one per tenant", len(ks))
	}
	for _, r := range ks {
		want := map[string]float64{"A": 1.00, "B": 2.00}[r.TenantID]
		if r.Expiries != 1 || math.Abs(r.ExpiryUSD-want) > 1e-9 || r.Turns != 2 {
			t.Errorf("tenant %s row = %+v, want 1 expiry costing %.2f over 2 turns", r.TenantID, r, want)
		}
	}
}

// --- #1071: the startup seeds ---

// One tenant's later 5-minute write on a shared thread and model must not hide the other
// tenant's one-hour write from the seed.
func TestSeedOneHourTiersRanksPerTenant(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{})
	now := time.Now().UnixMilli()
	mk := func(tenant string, ts int64, write, write1h int64) *Event {
		e := mkEvent(ts, sharedSession, "m1", 100, 90)
		e.TenantID, e.Status, e.CacheWrite, e.CacheWrite1h = tenant, 200, write, write1h
		return e
	}
	if err := rec.db.insertBatch([]*Event{
		mk("A", now-60_000, 1000, 1000), // A: one-hour write
		mk("B", now-10_000, 1000, 0),    // B: later, 5-minute write
	}); err != nil {
		t.Fatal(err)
	}
	got, err := rec.seedOneHourTiers(now - 3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("seed = %v, want tenant A's one-hour write to be found", got)
	}
}
