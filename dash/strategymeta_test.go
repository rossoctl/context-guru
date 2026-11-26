package dash

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/testutil"
)

// newMetaRecorder opens a recorder on a fresh file in dir. BatchSize 1 writes each event at once.
func newMetaRecorder(t *testing.T, dir string) *Recorder {
	t.Helper()
	rec, err := NewRecorder(Options{DBPath: filepath.Join(dir, "d.db"), BatchSize: 1, FlushInterval: time.Millisecond,
		PromptGrace: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// recordAndWait sends the events and waits until the table holds want rows.
func recordAndWait(t *testing.T, rec *Recorder, want int64, evs ...*Event) {
	t.Helper()
	for _, e := range evs {
		rec.Record(e)
	}
	waitFor(t, func() bool {
		var n int64
		_ = rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
		return n >= want
	})
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	testutil.WaitFor(t, "the writer", ok)
}

func i64(v int64) *int64 { return &v }
func bp(v bool) *bool    { return &v }

// The fields a request decides alone round-trip, and a bare row keeps them NULL.
func TestStrategyMetaFieldsRoundTrip(t *testing.T) {
	rec := newMetaRecorder(t, t.TempDir())
	defer rec.Close()
	full := &Event{TS: 1000, SessionID: "s", Model: "m", TZOffsetMin: ip(-330),
		KAPlannedPings: ip(8), KAMinPrefixTokens: ip(20000), KAIdleS: ip(240),
		WaitCause: func() WaitCauseResult {
			return WaitCauseResult{WaitingFor: "own_tool", WaitTool: "Bash",
				WaitToolTimeoutMs: i64(600000), WaitToolBackground: bp(true), WaitPrompt: "none"}
		}}
	bare := &Event{TS: 2000, SessionID: "other", Model: "m", KeepAlive: true}
	recordAndWait(t, rec, 2, full, bare)

	got, err := rec.DB().Request(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.WaitToolTimeoutMs == nil || *got.WaitToolTimeoutMs != 600000 || got.WaitToolBackground == nil || !*got.WaitToolBackground {
		t.Fatalf("wait tool parameters = %v %v, want 600000 true", got.WaitToolTimeoutMs, got.WaitToolBackground)
	}
	if got.WaitPrompt != "none" || got.TZOffsetMin == nil || *got.TZOffsetMin != -330 {
		t.Fatalf("prompt %q tz %v, want none -330", got.WaitPrompt, got.TZOffsetMin)
	}
	if got.KAPlannedPings == nil || *got.KAPlannedPings != 8 || *got.KAMinPrefixTokens != 20000 || *got.KAIdleS != 240 {
		t.Fatalf("plan = %v %v %v, want 8 20000 240", got.KAPlannedPings, got.KAMinPrefixTokens, got.KAIdleS)
	}
	if got.Turn != 1 || got.ActiveThreads != 1 {
		t.Fatalf("turn %d active %d, want 1 1", got.Turn, got.ActiveThreads)
	}

	// A ping row computes nothing: every column is NULL, not a guess.
	var nulls int
	if err := rec.DB().sql.QueryRow(`SELECT (wait_tool_timeout_ms IS NULL) + (wait_tool_background IS NULL) + (wait_prompt IS NULL)
		+ (tz_offset_min IS NULL) + (turn IS NULL) + (active_threads IS NULL) + (ka_planned_pings IS NULL)
		+ (ka_min_prefix_tokens IS NULL) + (ka_idle_s IS NULL) + (user_hash IS NULL) FROM requests WHERE session_id = 'other'`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 10 {
		t.Fatalf("%d of 10 columns are NULL on a ping row, want all", nulls)
	}
	// Zero is a value, not NULL: UTC reads 0.
	utc := &Event{TS: 3000, SessionID: "z", Model: "m", TZOffsetMin: ip(0)}
	recordAndWait(t, rec, 3, utc)
	if z, _ := rec.DB().Request(3, false); z.TZOffsetMin == nil || *z.TZOffsetMin != 0 {
		t.Fatalf("UTC offset = %v, want 0", z.TZOffsetMin)
	}
}

// The turn counts a thread's real requests from 1, per thread, and a restart keeps counting.
func TestTurnSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	rec := newMetaRecorder(t, dir)
	mk := func(ts int64, thread string) *Event {
		return &Event{TS: ts, TenantID: "t", SessionID: "t:s", ThreadID: thread, Model: "m"}
	}
	recordAndWait(t, rec, 4, mk(1000, ""), mk(2000, ""), mk(3000, "a:1"), mk(4000, ""))
	rec.Close()
	var turns []int
	rec2 := newMetaRecorder(t, dir)
	defer rec2.Close()
	recordAndWait(t, rec2, 6, mk(5000, ""), mk(6000, "a:1"))
	rows, err := rec2.DB().sql.Query(`SELECT turn FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		turns = append(turns, n)
	}
	want := []int{1, 2, 1, 3, 4, 2}
	if len(turns) != len(want) {
		t.Fatalf("turns = %v, want %v", turns, want)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Fatalf("turns = %v, want %v", turns, want)
		}
	}
}

// Three parallel subagents plus the main thread give a concurrency of 4, and the count falls once
// the others are older than the window. A ping and an imported row do not count.
func TestActiveThreadsCountsTheSessionsRecentThreads(t *testing.T) {
	rec := newMetaRecorder(t, t.TempDir())
	defer rec.Close()
	base := int64(10_000_000)
	mk := func(ts int64, thread string) *Event {
		return &Event{TS: ts, SessionID: "s", ThreadID: thread, Model: "m"}
	}
	ping := mk(base+500, "a:9")
	ping.KeepAlive = true
	recordAndWait(t, rec, 5, mk(base, ""), mk(base+1000, "a:1"), ping, mk(base+2000, "a:2"), mk(base+3000, "a:3"))
	late := mk(base+ActiveThreadsWindow.Milliseconds()+60_000, "")
	recordAndWait(t, rec, 6, late)
	var got []int
	rows, err := rec.DB().sql.Query(`SELECT COALESCE(active_threads, 0) FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	want := []int{1, 2, 0, 3, 4, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("active_threads = %v, want %v", got, want)
		}
	}
}

// After a restart the concurrency is rebuilt from the stored rows, so it does not drop to 1.
func TestActiveThreadsIsRebuiltAfterARestart(t *testing.T) {
	dir := t.TempDir()
	rec := newMetaRecorder(t, dir)
	base := int64(20_000_000)
	recordAndWait(t, rec, 2, &Event{TS: base, SessionID: "s", ThreadID: "a:1"}, &Event{TS: base + 1000, SessionID: "s", ThreadID: "a:2"})
	rec.Close()
	rec2 := newMetaRecorder(t, dir)
	defer rec2.Close()
	recordAndWait(t, rec2, 3, &Event{TS: base + 2000, SessionID: "s", ThreadID: ""})
	last, err := rec2.DB().Request(3, false)
	if err != nil {
		t.Fatal(err)
	}
	if last.ActiveThreads != 3 {
		t.Fatalf("active_threads = %d after a restart, want 3", last.ActiveThreads)
	}
}

// The user hash is keyed, stable, per tenant, and the raw id is stored nowhere. The key file is private.
func TestUserHashIsKeyedAndStable(t *testing.T) {
	dir := t.TempDir()
	rec := newMetaRecorder(t, dir)
	raw := "a:11111111-2222-3333-4444-555555555555"
	mk := func(ts int64, tenant, key string) *Event {
		return &Event{TS: ts, TenantID: tenant, SessionID: "s" + tenant, Model: "m",
			WaitCause: func() WaitCauseResult { return WaitCauseResult{UserKey: key} }}
	}
	recordAndWait(t, rec, 4, mk(1, "t1", raw), mk(2, "t1", raw), mk(3, "t2", raw), mk(4, "t1", "a:other"))
	rec.Close()
	rec2 := newMetaRecorder(t, dir)
	defer rec2.Close()
	rows, err := rec2.DB().sql.Query(`SELECT user_hash FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var h []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		h = append(h, s.String)
	}
	rows.Close()
	if len(h[0]) != 16 || h[0] != h[1] || h[0] == h[2] || h[0] == h[3] {
		t.Fatalf("hashes = %v: want 16 hex, same for one person, different across tenants and people", h)
	}
	if strings.Contains(h[0], "1111") {
		t.Fatal("the hash shows the raw id")
	}
	fi, err := os.Stat(filepath.Join(dir, userKeyFile))
	if err != nil {
		t.Fatalf("the key file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	// The same key after a restart: the same hash.
	recordAndWait(t, rec2, 5, mk(5, "t1", raw))
	var again string
	if err := rec2.DB().sql.QueryRow(`SELECT user_hash FROM requests WHERE id = 5`).Scan(&again); err != nil || again != h[0] {
		t.Fatalf("hash after a restart = %q (%v), want %q", again, err, h[0])
	}
	// The raw id is in no column of the file.
	var n int
	if err := rec2.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE user_hash LIKE '%11111111%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw id found in %d rows (%v)", n, err)
	}
	// No key from the person's body: NULL.
	recordAndWait(t, rec2, 6, mk(6, "t1", ""))
	var null bool
	if err := rec2.DB().sql.QueryRow(`SELECT user_hash IS NULL FROM requests WHERE id = 6`).Scan(&null); err != nil || !null {
		t.Fatalf("empty key must leave user_hash NULL (%v)", err)
	}
}

// A Notification of the permission type marks the request it interrupted, SessionEnd is stored with
// its reason, and an idle prompt marks nothing.
func TestClientSignalsAreStoredAndMarkThePrompt(t *testing.T) {
	rec := newMetaRecorder(t, t.TempDir())
	defer rec.Close()
	now := time.Now().UnixMilli()
	waiting := func(ts int64) *Event {
		return &Event{TS: ts, TenantID: "t", SessionID: "t:s", Model: "m",
			WaitCause: func() WaitCauseResult { return WaitCauseResult{WaitingFor: "own_tool", WaitPrompt: "none"} }}
	}
	recordAndWait(t, rec, 2, waiting(now-60_000), waiting(now-1000))
	rec.RecordSignal(ClientSignal{TS: now, TenantID: "t", Session: "t:s", Event: "Notification", Reason: "idle_prompt"})
	rec.RecordSignal(ClientSignal{TS: now + 1, TenantID: "t", Session: "t:s", Event: "Notification", Reason: "permission_prompt"})
	rec.RecordSignal(ClientSignal{TS: now + 2, TenantID: "t", Session: "t:s", Event: "SessionEnd", Reason: "clear"})
	waitFor(t, func() bool { return countRows(t, rec.DB(), "client_signals") == 3 })
	waitFor(t, func() bool {
		var p string
		return rec.DB().sql.QueryRow(`SELECT wait_prompt FROM requests WHERE id = 2`).Scan(&p) == nil && p == "permission"
	})
	prompts := map[int64]string{}
	rows, err := rec.DB().sql.Query(`SELECT id, wait_prompt FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			t.Fatal(err)
		}
		prompts[id] = p
	}
	rows.Close()
	if prompts[1] != "none" || prompts[2] != "permission" {
		t.Fatalf("prompts = %v, want the newest row marked permission and the older left none", prompts)
	}
	var ev, reason string
	if err := rec.DB().sql.QueryRow(`SELECT event, reason FROM client_signals WHERE event = 'SessionEnd'`).Scan(&ev, &reason); err != nil || reason != "clear" {
		t.Fatalf("SessionEnd row = %q %q (%v)", ev, reason, err)
	}
	// A different tenant's signal for the same session id marks nothing of this tenant.
	rec.RecordSignal(ClientSignal{TS: now + 3, TenantID: "other", Session: "t:s", Event: "Notification", Reason: "elicitation_dialog"})
	waitFor(t, func() bool { return countRows(t, rec.DB(), "client_signals") == 4 })
	var p string
	_ = rec.DB().sql.QueryRow(`SELECT wait_prompt FROM requests WHERE id = 2`).Scan(&p)
	if p != "permission" {
		t.Fatalf("a foreign tenant's signal changed the row to %q", p)
	}
}

// A permission prompt that arrives before its row is written waits in memory, and marks the LAST row
// of the session that started before it. A question the response carried is not overwritten.
func TestAPromptThatArrivesFirstMarksTheLastRowBeforeIt(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"), PromptGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	now := time.Now().UnixMilli()
	rec.writeSignal(ClientSignal{TS: now, TenantID: "t", Session: "t:s", Event: "Notification", Reason: "permission_prompt"})
	mk := func(ts int64, prompt string) *Event {
		return &Event{TS: ts, TenantID: "t", SessionID: "t:s", WaitPrompt: prompt}
	}
	older, newer, after := mk(now-5000, "none"), mk(now-500, "none"), mk(now+500, "none")
	batch := []*Event{older, newer, after}
	rec.markPending(batch)
	for _, e := range batch {
		rec.fillStrategyMeta(e, "")
	}
	if older.WaitPrompt != "none" || newer.WaitPrompt != "permission" || after.WaitPrompt != "none" {
		t.Fatalf("prompts = %q %q %q, want none permission none", older.WaitPrompt, newer.WaitPrompt, after.WaitPrompt)
	}
	rec.writeSignal(ClientSignal{TS: now + 10_000, TenantID: "t", Session: "t:s", Event: "Notification", Reason: "permission_prompt"})
	q := mk(now+9000, "question")
	rec.markPending([]*Event{q})
	rec.fillStrategyMeta(q, "")
	if q.WaitPrompt != "question" {
		t.Fatalf("wait_prompt = %q, want question kept", q.WaitPrompt)
	}
}

// A signal that arrives BEFORE its row is recorded marks that row, and leaves the older stored row alone.
func TestASignalMarksTheRowThatArrivesAfterIt(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond,
		PromptGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	now := time.Now().UnixMilli()
	mk := func(ts int64) *Event {
		return &Event{TS: ts, TenantID: "t", SessionID: "t:s", Model: "m",
			WaitCause: func() WaitCauseResult { return WaitCauseResult{WaitingFor: "own_tool", WaitPrompt: "none"} }}
	}
	recordAndWait(t, rec, 1, mk(now-60_000))
	rec.RecordSignal(ClientSignal{TS: now, TenantID: "t", Session: "t:s", Event: "Notification", Reason: "permission_prompt"})
	waitFor(t, func() bool { return countRows(t, rec.DB(), "client_signals") == 1 })
	recordAndWait(t, rec, 2, mk(now-1000))
	prompt := func(id int) string {
		var p string
		_ = rec.DB().sql.QueryRow(`SELECT wait_prompt FROM requests WHERE id = ?`, id).Scan(&p)
		return p
	}
	waitFor(t, func() bool { return prompt(2) == "permission" })
	if prompt(1) != "none" {
		t.Fatalf("the older row = %q, want none", prompt(1))
	}
}

// Signals are pruned by age and by a row cap, and leave with their tenant.
func TestClientSignalsArePrunedAndPurged(t *testing.T) {
	rec := newMetaRecorder(t, t.TempDir())
	defer rec.Close()
	now := time.Now()
	for i, ts := range []int64{now.Add(-48 * time.Hour).UnixMilli(), now.UnixMilli()} {
		rec.writeSignal(ClientSignal{TS: ts, TenantID: "t", Session: "t:s" + string(rune('a'+i)), Event: "SessionEnd", Reason: "other"})
	}
	if _, err := rec.DB().Prune(now, 24*time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, rec.DB(), "client_signals"); n != 1 {
		t.Fatalf("%d signals after the age prune, want 1", n)
	}
	if has, err := rec.DB().TenantHasRows("t"); err != nil || !has {
		t.Fatalf("TenantHasRows = %v (%v), want true while a signal stays", has, err)
	}
	if _, err := rec.PurgeTenant(context.Background(), "t"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := countRows(t, rec.DB(), "client_signals"); n != 0 {
		t.Fatalf("%d signals after a purge, want 0", n)
	}
}

// A database from before the columns opens, gains them, and its old row reads NULL.
func TestAnExistingDatabaseGainsTheStrategyColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{mkEvent(1, "s", "m", 10, 10)}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"wait_tool_timeout_ms", "wait_tool_background", "wait_prompt", "tz_offset_min", "turn",
		"active_threads", "ka_planned_pings", "ka_min_prefix_tokens", "ka_idle_s", "user_hash"} {
		if _, err := raw.Exec(`ALTER TABLE requests DROP COLUMN ` + c); err != nil {
			t.Fatalf("simulate an old file (%s): %v", c, err)
		}
	}
	if _, err := raw.Exec(`DROP TABLE client_signals`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a database without the columns: %v", err)
	}
	defer db2.Close()
	old, err := db2.Request(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if old.WaitToolTimeoutMs != nil || old.WaitPrompt != "" || old.TZOffsetMin != nil || old.Turn != 0 || old.KAPlannedPings != nil {
		t.Fatalf("old row = %+v, want every strategy field unset", old)
	}
	e := mkEvent(2, "s", "m", 10, 10)
	e.Turn, e.TZOffsetMin, e.WaitPrompt = 7, ip(60), "question"
	if err := db2.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	if got, err := db2.Request(2, false); err != nil || got.Turn != 7 || *got.TZOffsetMin != 60 || got.WaitPrompt != "question" {
		t.Fatalf("new row = %+v, err %v", got, err)
	}
	if countRows(t, db2, "client_signals") != 0 {
		t.Fatal("client_signals must exist and be empty")
	}
}

// A read-only handle on an old file reads the new columns as NULL, with no write.
func TestReadOnlyHandleReadsTheStrategyColumnsOfAnOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{mkEvent(1, "s", "m", 10, 10)}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"turn", "user_hash", "wait_prompt"} {
		if _, err := raw.Exec(`ALTER TABLE requests DROP COLUMN ` + c); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got, err := ro.Request(1, false)
	if err != nil || got.Turn != 0 || got.WaitPrompt != "" {
		t.Fatalf("read-only row = %+v, err %v", got, err)
	}
}

// A built-in subagent type is stored as is, a custom one as a keyed hash; the permission mode of the
// latest hook signal is carried onto later rows.
func TestSpawnTypeAndPermissionMode(t *testing.T) {
	rec := newMetaRecorder(t, t.TempDir())
	defer rec.Close()
	rec.writeSignal(ClientSignal{TS: 1, TenantID: "", Session: "s", Event: "SessionStart", Reason: "resume", PermissionMode: "bypassPermissions"})
	mk := func(ts int64, typ string) *Event {
		return &Event{TS: ts, SessionID: "s", Model: "m", ClientEntrypoint: "headless",
			WaitCause: func() WaitCauseResult { return WaitCauseResult{SpawnType: typ, Fanout: 3} }}
	}
	recordAndWait(t, rec, 2, mk(10, "Explore"), mk(20, "my-private-agent"))
	a, _ := rec.DB().Request(1, false)
	b, _ := rec.DB().Request(2, false)
	if a.SubagentType != "Explore" || a.ParentFanout != 3 || a.PermissionMode != "bypassPermissions" || a.ClientEntrypoint != "headless" {
		t.Fatalf("row 1 = %+v", a)
	}
	if !strings.HasPrefix(b.SubagentType, "h:") || strings.Contains(b.SubagentType, "private") || len(b.SubagentType) != 18 {
		t.Fatalf("custom type stored as %q, want h: + 16 hex", b.SubagentType)
	}
}
