package dash

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercises the REAL rclone subprocess path — argument construction, stdin streaming,
// the lsjson size parse, and how a missing object is distinguished from a broken
// transfer. It runs against a `local` remote created on the fly, so it needs no
// credentials and no network, but it is the only test that proves the code actually
// speaks rclone rather than speaking to a Go fake.
//
// Skipped when rclone is absent, so CI without it stays green. The one thing it
// cannot cover is Box's own behaviour; everything up to the remote's edge is here.
func liveRclone(t *testing.T) *Rclone {
	t.Helper()
	bin, err := exec.LookPath("rclone")
	if err != nil {
		if home, herr := os.UserHomeDir(); herr == nil {
			cand := filepath.Join(home, ".local", "bin", "rclone")
			if _, serr := os.Stat(cand); serr == nil {
				bin = cand
			}
		}
	}
	if bin == "" {
		t.Skip("rclone not installed; skipping the live remote test")
	}

	// A throwaway config with one `local` remote pointed at a temp dir. Writing the
	// config rather than using an env var because that is exactly how the service is
	// configured, so the --config plumbing is under test too.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(cfg, []byte("[livetest]\ntype = local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Rclone{Bin: bin, Base: "livetest:" + filepath.Join(dir, "store"),
		ConfigPath: cfg, Timeout: 60 * time.Second}
}

func TestRcloneLiveRoundTrip(t *testing.T) {
	r := liveRclone(t)
	ctx := context.Background()

	if err := r.Check(ctx); err != nil {
		t.Fatalf("Check on a fresh (nonexistent) base should succeed: %v", err)
	}

	// Deliberately not tiny and not text-only: a size check that only ever sees short
	// ASCII would not catch a transfer that mangles bytes or truncates.
	payload := bytes.Repeat([]byte("context-guru cold storage \x00\xff binary test\n"), 500)
	const path = "archive/tenant-x/2026/08/sess-live.full.jsonl.gz"

	if err := r.Put(ctx, path, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	size, err := r.Size(ctx, path)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("Size = %d, want %d — the lsjson parse is wrong", size, len(payload))
	}
	got, err := r.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %d bytes, want %d, and they differ", len(got), len(payload))
	}

	// Overwriting must replace, not append — the archiver re-uploads on retry.
	if err := r.Put(ctx, path, []byte("short")); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	if got, err = r.Get(ctx, path); err != nil || string(got) != "short" {
		t.Errorf("overwrite left %q (%v)", got, err)
	}

	if err := r.Delete(ctx, path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Missing must be ErrRemoteMissing, NOT a generic failure: the archiver treats
	// those two completely differently, and conflating them is how a Box outage gets
	// reported to a user as "this session was never archived".
	if _, err := r.Get(ctx, path); !errors.Is(err, ErrRemoteMissing) {
		t.Errorf("Get after Delete = %v, want ErrRemoteMissing", err)
	}
	if _, err := r.Size(ctx, path); !errors.Is(err, ErrRemoteMissing) {
		t.Errorf("Size of a missing object = %v, want ErrRemoteMissing", err)
	}
	// Deleting what is already gone is a success: the caller's intent is satisfied.
	if err := r.Delete(ctx, path); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
}

// A remote that does not exist must fail Check, so a misconfiguration is caught at
// boot rather than surfacing later as "archiving failed" with no cause.
func TestRcloneLiveCheckFailsOnBadRemote(t *testing.T) {
	r := liveRclone(t)
	r.Base = "nosuchremote:whatever"
	if err := r.Check(context.Background()); err == nil {
		t.Fatal("Check succeeded against a remote that is not in the config")
	}
}

// The full archival path, driven by real rclone rather than the in-memory fake:
// archive a session, confirm the local rows are gone, and read it back.
func TestArchiveThroughRealRclone(t *testing.T) {
	r := liveRclone(t)
	rec, err := NewRecorder(Options{
		DBPath: filepath.Join(t.TempDir(), "d.db"), CaptureContent: true,
		ContentCap: 4096, Remote: r,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	seedSessionWithContent(t, rec.db, "t-live", "sess-live", 3, 72*time.Hour)
	cands, err := rec.db.oldestLocalSessions(1)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates = %v, %v", cands, err)
	}
	if _, err := rec.ArchiveSessionFull(context.Background(), cands[0]); err != nil {
		t.Fatalf("ArchiveSessionFull through real rclone: %v", err)
	}
	if got := countRows(t, rec.db, "requests"); got != 0 {
		t.Errorf("%d rows survived the archive", got)
	}
	evs, err := rec.FetchArchived(context.Background(), "sess-live")
	if err != nil {
		t.Fatalf("FetchArchived: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("read back %d events, want 3", len(evs))
	}
	if len(evs[0].Content) != 1 || !strings.HasPrefix(evs[0].Content[0].Before, "TRANSCRIPT-") {
		t.Errorf("content did not survive a real round trip: %+v", evs[0].Content)
	}
}
