package dash

// Live cold-storage test against a REAL remote (IBM Box). Opt-in, following the
// repo's zz_*_live_test.go convention, because it needs credentials and touches a
// network service:
//
//	CG_LIVE_ARCHIVE_REMOTE=box:context-guru go test ./dash/ -run TestLiveBox -v
//
// It writes only under a `.livetest/` prefix and deletes what it writes. What it
// proves that the local-remote test cannot: that Box itself accepts `rcat` streaming,
// that `lsjson --stat` returns a size Box actually agrees with, and that a real
// session survives the whole archive → verify → delete → fetch cycle over the wire.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func liveArchiveRemote(t *testing.T) *Rclone {
	t.Helper()
	base := os.Getenv("CG_LIVE_ARCHIVE_REMOTE")
	if base == "" {
		t.Skip("set CG_LIVE_ARCHIVE_REMOTE (e.g. box:context-guru) to run the live cold-storage test")
	}
	bin := os.Getenv("CG_LIVE_RCLONE")
	if bin == "" {
		bin = "rclone"
	}
	// Confine every object this test writes to one prefix, so a failure mid-run leaves
	// an obvious pile in one place rather than scattering test data through a real
	// tenant's archive tree.
	return &Rclone{
		Bin:        bin,
		Base:       strings.TrimRight(base, "/") + "/.livetest",
		ConfigPath: os.Getenv("RCLONE_CONFIG"),
		Timeout:    3 * time.Minute,
	}
}

func TestLiveBoxRoundTrip(t *testing.T) {
	r := liveArchiveRemote(t)
	ctx := context.Background()
	if err := r.Check(ctx); err != nil {
		t.Fatalf("Check against %s: %v", r.Base, err)
	}

	// Big enough to be a real multi-chunk upload, and binary so a transfer that
	// mangles bytes or re-encodes anything shows up.
	payload := make([]byte, 512<<10)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	path := "probe-" + time.Now().UTC().Format("20060102T150405Z") + ".bin"
	t.Cleanup(func() { _ = r.Delete(context.Background(), path) })

	if err := r.Put(ctx, path, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	size, err := r.Size(ctx, path)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != int64(len(payload)) {
		t.Fatalf("Box reports %d bytes, uploaded %d — putVerified would refuse to delete "+
			"the local copy, so archiving would never reclaim disk", size, len(payload))
	}
	got, err := r.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("read back %d bytes, want %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d differs after a Box round trip: got %d want %d", i, got[i], payload[i])
		}
	}
	if err := r.Delete(ctx, path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// The whole archival cycle over the wire: seed a session, archive it, confirm the
// local rows are gone and the object is retrievable with its transcripts intact.
func TestLiveBoxArchiveCycle(t *testing.T) {
	r := liveArchiveRemote(t)
	rec, err := NewRecorder(Options{
		DBPath:         filepath.Join(t.TempDir(), "d.db"),
		CaptureContent: true, ContentCap: 4096, Remote: r,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	session := "livebox-" + time.Now().UTC().Format("20060102T150405Z")
	seedSessionWithContent(t, rec.db, "t-live", session, 4, 72*time.Hour)

	cands, err := rec.db.oldestLocalSessions(1)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates = %v, %v", cands, err)
	}
	t.Cleanup(func() {
		p, err := archivePath(cands[0], ArchiveFull)
		if err != nil {
			return
		}
		_ = r.Delete(context.Background(), p)
	})

	moved, err := rec.ArchiveSessionFull(context.Background(), cands[0])
	if err != nil {
		t.Fatalf("ArchiveSessionFull to %s: %v", r.Base, err)
	}
	if moved != 4 {
		t.Errorf("archived %d requests, want 4", moved)
	}
	if got := countRows(t, rec.db, "requests"); got != 0 {
		t.Errorf("%d rows survived locally after a verified archive", got)
	}

	evs, err := rec.FetchArchived(context.Background(), session)
	if err != nil {
		t.Fatalf("FetchArchived: %v", err)
	}
	if len(evs) != 4 {
		t.Fatalf("fetched %d events from Box, want 4", len(evs))
	}
	if len(evs[0].Content) != 1 || !strings.HasPrefix(evs[0].Content[0].Before, "TRANSCRIPT-") {
		t.Errorf("transcripts did not survive the Box round trip: %+v", evs[0].Content)
	}
	if len(evs[0].Components) != 1 {
		t.Errorf("component rows did not survive: %+v", evs[0].Components)
	}
}
