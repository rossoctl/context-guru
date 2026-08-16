package dash

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func archiveRecorder(t *testing.T, o Options) (*Recorder, *memRemote) {
	t.Helper()
	m := newMemRemote()
	if o.DBPath == "" {
		o.DBPath = filepath.Join(t.TempDir(), "d.db")
	}
	o.Remote = m
	rec, err := NewRecorder(o)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() })
	return rec, m
}

// seedSessionWithContent writes one session whose requests carry transcript text.
func seedSessionWithContent(t *testing.T, db *DB, tenant, session string, turns int, ageAgo time.Duration) {
	t.Helper()
	base := time.Now().Add(-ageAgo).UnixMilli()
	evs := make([]*Event, 0, turns)
	for i := 0; i < turns; i++ {
		evs = append(evs, &Event{
			TS: base + int64(i)*1000, TenantID: tenant, SessionID: session,
			Model: "m", Provider: "openai", Status: 200,
			TokensBefore: 1000, TokensAfter: 900, SavedUnique: 100,
			Components: []CompRow{{Component: "extract", Acted: true, SavedGross: 100}},
			Content: []ContentRow{{Path: "messages.0",
				Before: "TRANSCRIPT-" + session + "-" + string(rune('a'+i)),
				After:  "short"}},
		})
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatalf("insertBatch: %v", err)
	}
}

// A full archive round-trips: what comes back out equals what went in, including
// components and content.
func TestArchiveRoundTrip(t *testing.T) {
	rec, m := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "t1", "sess-1", 3, 48*time.Hour)

	cands, err := rec.db.oldestLocalSessions(10)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates = %v, %v", cands, err)
	}
	n, err := rec.ArchiveSessionFull(context.Background(), cands[0])
	if err != nil {
		t.Fatalf("ArchiveSessionFull: %v", err)
	}
	if n != 3 {
		t.Errorf("archived %d requests, want 3", n)
	}
	// Local rows are gone...
	if got := countRows(t, rec.db, "requests"); got != 0 {
		t.Errorf("%d request rows survived a full archive", got)
	}
	// ...and the object is there and readable.
	if m.puts != 1 {
		t.Errorf("uploads = %d, want 1 object for the session", m.puts)
	}
	evs, err := rec.FetchArchived(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("FetchArchived: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("fetched %d events, want 3", len(evs))
	}
	if evs[0].SessionID != "sess-1" || evs[0].TenantID != "t1" {
		t.Errorf("identity lost in the archive: %+v", evs[0])
	}
	if len(evs[0].Components) != 1 || evs[0].Components[0].Component != "extract" {
		t.Errorf("components lost in the archive: %+v", evs[0].Components)
	}
	if len(evs[0].Content) != 1 || !strings.HasPrefix(evs[0].Content[0].Before, "TRANSCRIPT-") {
		t.Errorf("content lost in the archive: %+v", evs[0].Content)
	}
	// The index is queryable locally, without touching the remote.
	list, err := rec.db.ArchivedSessions(Filter{Tenant: "t1"}, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("ArchivedSessions = %v, %v", list, err)
	}
	if !list[0].Archived() || list[0].Requests != 3 || list[0].FullBytes == 0 {
		t.Errorf("index row is wrong: %+v", list[0])
	}
}

// THE invariant: a failed upload must leave the local copy alone.
func TestFailedUploadNeverDeletesLocalData(t *testing.T) {
	rec, m := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "t1", "sess-1", 4, 48*time.Hour)
	before := countRows(t, rec.db, "requests")

	m.failPut = errors.New("box is down")
	cands, _ := rec.db.oldestLocalSessions(10)
	if _, err := rec.ArchiveSessionFull(context.Background(), cands[0]); err == nil {
		t.Fatal("ArchiveSessionFull reported success with a failing remote")
	}
	if got := countRows(t, rec.db, "requests"); got != before {
		t.Fatalf("rows were deleted despite a failed upload (%d -> %d)", before, got)
	}
	if got := countRows(t, rec.db, "archived_sessions"); got != 0 {
		t.Errorf("an index row was written for an archive that never uploaded (%d)", got)
	}
	// And a content archive must behave the same way.
	if _, err := rec.ArchiveSessionContent(context.Background(), cands[0]); err == nil {
		t.Fatal("ArchiveSessionContent reported success with a failing remote")
	}
	if got := countRows(t, rec.db, "request_content"); got != before {
		t.Errorf("content rows were deleted despite a failed upload (%d)", got)
	}
}

// A truncated upload must be caught by the size check, not trusted because Put
// returned nil.
func TestTruncatedUploadIsRefused(t *testing.T) {
	rec, m := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "t1", "sess-1", 3, 48*time.Hour)
	before := countRows(t, rec.db, "requests")

	// A remote that accepts the write but stores a short object — a proxy that
	// swallowed the body, a partial PUT. Put succeeds; only the stat catches it.
	rec.remote = truncatingRemote{m}
	cands, _ := rec.db.oldestLocalSessions(10)
	_, err := rec.ArchiveSessionFull(context.Background(), cands[0])
	if err == nil {
		t.Fatal("a truncated upload was accepted")
	}
	if !strings.Contains(err.Error(), "refusing to delete") {
		t.Errorf("error should say the local copy was kept: %v", err)
	}
	if got := countRows(t, rec.db, "requests"); got != before {
		t.Fatalf("rows deleted after a truncated upload (%d -> %d)", before, got)
	}
}

type truncatingRemote struct{ *memRemote }

func (t truncatingRemote) Size(ctx context.Context, path string) (int64, error) {
	n, err := t.memRemote.Size(ctx, path)
	if err != nil {
		return 0, err
	}
	return n - 1, nil // one byte short of the truth
}

// Content archiving frees the bulk of the bytes while metrics stay queryable — the
// property that keeps the local database small enough that the disk rule never fires.
func TestContentArchiveKeepsMetricsLocal(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "t1", "sess-1", 5, 48*time.Hour)

	cands, _ := rec.db.oldestLocalSessions(10)
	n, err := rec.ArchiveSessionContent(context.Background(), cands[0])
	if err != nil {
		t.Fatalf("ArchiveSessionContent: %v", err)
	}
	if n != 5 {
		t.Errorf("moved %d content rows, want 5", n)
	}
	if got := countRows(t, rec.db, "request_content"); got != 0 {
		t.Errorf("%d content rows survived", got)
	}
	// Metrics are untouched and still queryable.
	if got := countRows(t, rec.db, "requests"); got != 5 {
		t.Fatalf("content archiving removed metric rows (%d of 5 left)", got)
	}
	p, err := rec.db.Requests(Filter{Tenant: "t1"}, 0, 10)
	if err != nil || p.Total != 5 {
		t.Fatalf("metrics not queryable after content archiving: %v, %v", p, err)
	}
	// The transcripts are retrievable from cold storage.
	rows, err := rec.FetchArchivedContent(context.Background(), "sess-1", p.Requests[0].ID)
	if err != nil {
		t.Fatalf("FetchArchivedContent: %v", err)
	}
	if len(rows) != 1 || !strings.HasPrefix(rows[0].Before, "TRANSCRIPT-") {
		t.Errorf("archived content came back wrong: %+v", rows)
	}
	// The index records the content path but NOT a full archive.
	a, err := rec.db.ArchivedSessionByID("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentPath == "" || a.FullPath != "" {
		t.Errorf("index says the wrong thing: %+v", a)
	}
	if a.Archived() {
		t.Error("a content-only archive reports the whole session as archived")
	}
}

// The age rule picks only idle sessions, and content goes before whole sessions.
func TestArchiveIdleRespectsAges(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{
		CaptureContent: true, ContentCap: 4096,
		ArchiveContentAfter: 24 * time.Hour,
		ArchiveSessionAfter: 30 * 24 * time.Hour,
	})
	seedSessionWithContent(t, rec.db, "t1", "hot", 2, time.Minute)         // active
	seedSessionWithContent(t, rec.db, "t1", "cool", 2, 48*time.Hour)       // content only
	seedSessionWithContent(t, rec.db, "t1", "ancient", 2, 60*24*time.Hour) // whole session

	rec.archiveIdle(context.Background())

	// The active session is untouched, content and all.
	var hotContent int64
	if err := rec.db.sql.QueryRow(`SELECT COUNT(*) FROM request_content c
	  JOIN requests r ON r.id = c.request_id WHERE r.session_id = 'hot'`).Scan(&hotContent); err != nil {
		t.Fatal(err)
	}
	if hotContent != 2 {
		t.Errorf("an active session's content was archived (%d of 2 left)", hotContent)
	}
	// The cool one lost its content but kept its metrics.
	var coolRows, coolContent int64
	rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id='cool'`).Scan(&coolRows)
	rec.db.sql.QueryRow(`SELECT COUNT(*) FROM request_content c JOIN requests r ON r.id=c.request_id
	  WHERE r.session_id='cool'`).Scan(&coolContent)
	if coolRows != 2 || coolContent != 0 {
		t.Errorf("cool session: %d rows / %d content, want 2 / 0", coolRows, coolContent)
	}
	// The ancient one is gone locally and present in the index.
	var ancientRows int64
	rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id='ancient'`).Scan(&ancientRows)
	if ancientRows != 0 {
		t.Errorf("an ancient session was not fully archived (%d rows left)", ancientRows)
	}
	if a, err := rec.db.ArchivedSessionByID("ancient"); err != nil || !a.Archived() {
		t.Errorf("ancient session missing from the index: %+v, %v", a, err)
	}
}

// A second pass must not re-upload what it already archived, or every cycle costs
// the same API calls again.
func TestArchiveIdleIsIdempotent(t *testing.T) {
	rec, m := archiveRecorder(t, Options{
		CaptureContent: true, ContentCap: 4096,
		ArchiveContentAfter: time.Hour,
	})
	seedSessionWithContent(t, rec.db, "t1", "s", 3, 48*time.Hour)
	rec.archiveIdle(context.Background())
	first := m.puts
	if first == 0 {
		t.Fatal("nothing was archived")
	}
	rec.archiveIdle(context.Background())
	if m.puts != first {
		t.Errorf("a second pass re-uploaded (%d -> %d)", first, m.puts)
	}
}

// Under disk pressure, eviction MIGRATES rather than deletes: the rows leave local
// disk but the history survives in cold storage.
func TestDiskPressureArchivesInsteadOfDeleting(t *testing.T) {
	rec, m := archiveRecorder(t, Options{
		CaptureContent: true, ContentCap: 4096, MinKeepBytes: 1,
	})
	for i := 0; i < 12; i++ {
		seedSessionWithContent(t, rec.db, "t1",
			"s-"+string(rune('a'+i)), 2, time.Duration(100-i)*time.Hour)
	}
	before := countRows(t, rec.db, "requests")

	probes := 0
	rec.diskProbe = func(string) (float64, bool) {
		probes++
		if probes > 2 {
			return 0.80, true
		}
		return 0.95, true
	}
	rec.relieveDiskPressure()

	after := countRows(t, rec.db, "requests")
	if after >= before {
		t.Fatalf("nothing was evicted (%d -> %d)", before, after)
	}
	if m.puts == 0 {
		t.Fatal("sessions were evicted WITHOUT being archived — this is data loss")
	}
	// Everything evicted is retrievable.
	list, err := rec.db.ArchivedSessions(Filter{Tenant: "t1"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("no archive index rows after eviction")
	}
	for _, a := range list {
		evs, err := rec.FetchArchived(context.Background(), a.SessionID)
		if err != nil || len(evs) == 0 {
			t.Errorf("evicted session %s is not retrievable: %v", a.SessionID, err)
		}
	}
	// The oldest session must be the one that went.
	if _, err := rec.db.ArchivedSessionByID("s-a"); err != nil {
		t.Errorf("the oldest session was not the one evicted: %v", err)
	}
}

// ArchiveRequired keeps data when the remote is down, accepting a full disk instead.
func TestArchiveRequiredKeepsDataWhenRemoteIsDown(t *testing.T) {
	rec, m := archiveRecorder(t, Options{MinKeepBytes: 1, ArchiveRequired: true})
	for i := 0; i < 5; i++ {
		seedSessionWithContent(t, rec.db, "t1", "s-"+string(rune('a'+i)), 2, time.Duration(50-i)*time.Hour)
	}
	before := countRows(t, rec.db, "requests")
	m.failPut = errors.New("box is down")
	rec.diskProbe = func(string) (float64, bool) { return 0.99, true }
	rec.relieveDiskPressure()
	if got := countRows(t, rec.db, "requests"); got != before {
		t.Errorf("data was deleted with --archive-required and a dead remote (%d -> %d)", before, got)
	}
}

// Without ArchiveRequired, a dead remote means the disk still gets reclaimed — the
// deliberate tradeoff, because a full filesystem takes down every user's agent.
func TestWithoutArchiveRequiredDiskIsStillReclaimed(t *testing.T) {
	rec, m := archiveRecorder(t, Options{MinKeepBytes: 1})
	for i := 0; i < 5; i++ {
		seedSessionWithContent(t, rec.db, "t1", "s-"+string(rune('a'+i)), 2, time.Duration(50-i)*time.Hour)
	}
	before := countRows(t, rec.db, "requests")
	m.failPut = errors.New("box is down")
	probes := 0
	rec.diskProbe = func(string) (float64, bool) {
		probes++
		if probes > 2 {
			return 0.80, true
		}
		return 0.99, true
	}
	rec.relieveDiskPressure()
	if got := countRows(t, rec.db, "requests"); got >= before {
		t.Errorf("the disk was not reclaimed when archiving failed (%d -> %d)", before, got)
	}
}

// Fetching a session that was never archived reports missing, not empty — the caller
// has to be able to tell "no such archive" from "the remote is unreachable".
func TestFetchArchivedDistinguishesMissingFromBroken(t *testing.T) {
	rec, m := archiveRecorder(t, Options{})
	if _, err := rec.FetchArchived(context.Background(), "never-existed"); err == nil {
		t.Error("fetching an unknown session succeeded")
	}
	seedSessionWithContent(t, rec.db, "t1", "s", 2, 48*time.Hour)
	cands, _ := rec.db.oldestLocalSessions(1)
	if _, err := rec.ArchiveSessionFull(context.Background(), cands[0]); err != nil {
		t.Fatal(err)
	}
	m.failGet = errors.New("box is down")
	_, err := rec.FetchArchived(context.Background(), "s")
	if err == nil {
		t.Fatal("fetch succeeded with a failing remote")
	}
	if errors.Is(err, ErrRemoteMissing) {
		t.Error("a transport failure was reported as a missing archive")
	}
}

// Object paths are tenant-first and time-bucketed, so one folder never accumulates
// an unlistable number of children and a tenant's data is contiguous.
func TestArchivePathLayout(t *testing.T) {
	c := coldCandidate{SessionID: "abc123", TenantID: "tenant-x",
		LastTS: time.Date(2026, 8, 14, 3, 4, 5, 0, time.UTC).UnixMilli()}
	got, err := archivePath(c, ArchiveFull)
	if err != nil {
		t.Fatalf("archivePath: %v", err)
	}
	want := "archive/tenant-x/2026/08/abc123.full.jsonl.gz"
	if got != want {
		t.Errorf("archivePath = %q, want %q", got, want)
	}
	// A single-tenant deployment still needs one stable folder.
	c.TenantID = ""
	if got, err := archivePath(c, ArchiveContent); err != nil || !strings.HasPrefix(got, "archive/_single/") {
		t.Errorf("single-tenant path = %q, %v", got, err)
	}
}

// Defence in depth: the path builder refuses anything that could name a segment other
// than the one intended, whatever its caller believes about the input.
func TestArchivePathRejectsUnsafeSegments(t *testing.T) {
	ok := coldCandidate{SessionID: "abc123", TenantID: "tenant-x", LastTS: 1}
	for name, c := range map[string]coldCandidate{
		"tenant traversal":  {SessionID: "abc", TenantID: "../../etc", LastTS: 1},
		"tenant separator":  {SessionID: "abc", TenantID: "a/b", LastTS: 1},
		"tenant dots":       {SessionID: "abc", TenantID: "..", LastTS: 1},
		"session traversal": {SessionID: "../../../backup/x", TenantID: "t", LastTS: 1},
		"session newline":   {SessionID: "a\nb", TenantID: "t", LastTS: 1},
		"empty session":     {SessionID: "", TenantID: "t", LastTS: 1},
	} {
		if p, err := archivePath(c, ArchiveFull); err == nil {
			t.Errorf("%s: archivePath built %q, want an error", name, p)
		}
	}
	if _, err := archivePath(ok, "sneaky/kind"); err == nil {
		t.Error("archivePath accepted an unknown kind")
	}
}

// The archive format must be readable without this program's help.
func TestArchiveFormatIsPlainGzippedJSONL(t *testing.T) {
	blob, err := encodeArchive([]*Event{
		{ID: 1, TS: 100, SessionID: "s", Model: "m"},
		{ID: 2, TS: 200, SessionID: "s", Model: "m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeArchive(blob)
	if err != nil {
		t.Fatalf("DecodeArchive: %v", err)
	}
	if len(back) != 2 || back[1].ID != 2 {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if _, err := DecodeArchive([]byte("not gzip")); err == nil {
		t.Error("DecodeArchive accepted non-gzip input")
	}
}
