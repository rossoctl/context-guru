package dash

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingRemote records how many times the transcript route reached for cold storage.
// The whole design claim of GET /api/sessions/{id}/transcript is that it does NOT — not
// on open, not for a list — until a human asks, and a claim about a network call is only
// tested by counting the network calls.
type countingRemote struct {
	*memRemote
	gets atomic.Int64
}

func (c *countingRemote) Get(ctx context.Context, path string) ([]byte, error) {
	c.gets.Add(1)
	return c.memRemote.Get(ctx, path)
}

// downRemote is reachable enough to have been written to and unreachable now.
type downRemote struct{ *memRemote }

func (downRemote) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("dial tcp: connection refused")
}

func transcriptAPI(t *testing.T, o Options) (*API, *Recorder, *countingRemote) {
	t.Helper()
	m := &countingRemote{memRemote: newMemRemote()}
	if o.DBPath == "" {
		o.DBPath = filepath.Join(t.TempDir(), "d.db")
	}
	o.Remote = m
	o.BatchSize, o.FlushInterval = 1, time.Millisecond
	rec, err := NewRecorder(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	return NewAPI(rec), rec, m
}

func contentOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	reqs, _ := body["requests"].([]any)
	var out []any
	for _, r := range reqs {
		m, _ := r.(map[string]any)
		c, _ := m["content"].([]any)
		out = append(out, c...)
	}
	return out
}

func TestTranscriptServesHotContent(t *testing.T) {
	a, rec, _ := transcriptAPI(t, Options{CaptureContent: true, ContentCap: 4096})
	e := mkEvent(time.Now().UnixMilli(), "sess-hot", "m", 1000, 400)
	e.Content = []ContentRow{{Path: "messages.1", BeforeTokens: 600, AfterTokens: 0,
		Before: "the long original tool output", After: "<<cg:ABCD>>"}}
	seed(t, rec, e)

	w, body := get(t, a, "/api/sessions/sess-hot/transcript", "127.0.0.1:1")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if body["state"] != TranscriptHot {
		t.Errorf("state = %v, want %q", body["state"], TranscriptHot)
	}
	if got := contentOf(t, body); len(got) != 1 {
		t.Fatalf("got %d content rows, want 1", len(got))
	}
	if !strings.Contains(w.Body.String(), "the long original tool output") {
		t.Error("the before-text is missing, so there is nothing to diff")
	}
	// The component rows have to come along: they are what the attribution reads.
	reqs := body["requests"].([]any)
	comps, _ := reqs[0].(map[string]any)["components"].([]any)
	if len(comps) == 0 {
		t.Error("component rows are missing; the diff view cannot attribute a change without them")
	}
}

// Capture off is its own answer, not an empty panel.
func TestTranscriptSaysWhenNothingWasCaptured(t *testing.T) {
	a, rec, _ := transcriptAPI(t, Options{CaptureContent: false})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "sess-nc", "m", 1000, 1000))

	_, body := get(t, a, "/api/sessions/sess-nc/transcript", "127.0.0.1:1")
	if body["state"] != TranscriptNotCaptured {
		t.Errorf("state = %v, want %q", body["state"], TranscriptNotCaptured)
	}
	if body["content_captured"] != false {
		t.Errorf("content_captured = %v, want false", body["content_captured"])
	}
}

// Capture on and nothing rewritten is a THIRD answer: the pipeline ran and found
// nothing, which is not the same as never having looked.
func TestTranscriptSaysWhenNothingWasRewritten(t *testing.T) {
	a, rec, _ := transcriptAPI(t, Options{CaptureContent: true})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "sess-clean", "m", 1000, 1000))

	_, body := get(t, a, "/api/sessions/sess-clean/transcript", "127.0.0.1:1")
	if body["state"] != TranscriptNothing {
		t.Errorf("state = %v, want %q", body["state"], TranscriptNothing)
	}
}

// An untrusted address gets the metrics and none of the text.
func TestTranscriptWithholdsContentFromUntrustedAddress(t *testing.T) {
	a, rec, _ := transcriptAPI(t, Options{CaptureContent: true, ContentCap: 4096})
	e := mkEvent(time.Now().UnixMilli(), "sess-priv", "m", 1000, 400)
	e.Content = []ContentRow{{Path: "messages.1", Before: "SECRET-SOURCE-CODE", After: "x"}}
	seed(t, rec, e)

	w, body := get(t, a, "/api/sessions/sess-priv/transcript", "203.0.113.9:5555")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if body["state"] != TranscriptNotPermitted {
		t.Errorf("state = %v, want %q", body["state"], TranscriptNotPermitted)
	}
	if body["content_visible"] != false {
		t.Errorf("content_visible = %v, want false", body["content_visible"])
	}
	if strings.Contains(w.Body.String(), "SECRET-SOURCE-CODE") {
		t.Fatal("transcript text was served to an untrusted address")
	}
	// The metrics must still be there — this is a content gate, not a metrics gate.
	if len(body["requests"].([]any)) != 1 {
		t.Error("metrics were withheld too; only content is gated")
	}
}

// The lazy path: an archived session reports its state and does NOT touch the remote.
func TestTranscriptDoesNotFetchColdStorageUnasked(t *testing.T) {
	a, rec, m := transcriptAPI(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "", "sess-cold", 3, 48*time.Hour)
	cands, err := rec.db.coldSessions(time.Now().UnixMilli(), ArchiveContent, 10)
	if err != nil || len(cands) == 0 {
		t.Fatalf("no cold candidates: %v", err)
	}
	if _, err := rec.ArchiveSessionContent(context.Background(), cands[0]); err != nil {
		t.Fatalf("archive: %v", err)
	}

	_, body := get(t, a, "/api/sessions/sess-cold/transcript", "127.0.0.1:1")
	if body["state"] != TranscriptCold {
		t.Fatalf("state = %v, want %q", body["state"], TranscriptCold)
	}
	if n := m.gets.Load(); n != 0 {
		t.Fatalf("cold storage was read %d times without being asked; the whole point of "+
			"this route is that opening it costs no network round trip", n)
	}
	// Metrics stay local and complete, which is what makes the lazy path acceptable.
	if len(body["requests"].([]any)) != 3 {
		t.Errorf("got %d local metric rows, want 3", len(body["requests"].([]any)))
	}
	if body["archive"] == nil {
		t.Error("the archive index row is missing; the UI needs it to say what it would fetch")
	}
}

// ?fetch=1 is the human asking. Then, and only then, the round trip happens.
func TestTranscriptFetchesColdStorageWhenAsked(t *testing.T) {
	a, rec, m := transcriptAPI(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "", "sess-cold", 3, 48*time.Hour)
	cands, _ := rec.db.coldSessions(time.Now().UnixMilli(), ArchiveContent, 10)
	if _, err := rec.ArchiveSessionContent(context.Background(), cands[0]); err != nil {
		t.Fatalf("archive: %v", err)
	}

	w, body := get(t, a, "/api/sessions/sess-cold/transcript?fetch=1", "127.0.0.1:1")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if body["state"] != TranscriptFetched {
		t.Fatalf("state = %v, want %q (body %s)", body["state"], TranscriptFetched, w.Body.String())
	}
	if n := m.gets.Load(); n != 1 {
		t.Errorf("cold storage read %d times, want exactly 1", n)
	}
	if got := contentOf(t, body); len(got) != 3 {
		t.Fatalf("got %d fetched content rows, want 3", len(got))
	}
	// The fetched text is merged onto the LOCAL metric rows, not served instead of them.
	if !strings.Contains(w.Body.String(), "TRANSCRIPT-sess-cold-a") {
		t.Error("the archived before-text did not make it into the response")
	}
	if len(body["requests"].([]any)) != 3 {
		t.Errorf("got %d rows after the merge, want the 3 local ones", len(body["requests"].([]any)))
	}
}

// A remote that is down is not a session that never existed, and the two must not
// render as the same empty panel.
func TestTranscriptReportsUnreachableColdStorage(t *testing.T) {
	a, rec, _ := transcriptAPI(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "", "sess-cold", 2, 48*time.Hour)
	cands, _ := rec.db.coldSessions(time.Now().UnixMilli(), ArchiveContent, 10)
	if _, err := rec.ArchiveSessionContent(context.Background(), cands[0]); err != nil {
		t.Fatalf("archive: %v", err)
	}
	rec.remote = downRemote{newMemRemote()}

	w, body := get(t, a, "/api/sessions/sess-cold/transcript?fetch=1", "127.0.0.1:1")
	// A 200 with a state, not a 5xx: the metrics in this response are valid and worth
	// rendering, and only the transcript is missing.
	if w.Code != 200 {
		t.Fatalf("status %d, want 200 with a state", w.Code)
	}
	if body["state"] != TranscriptUnreachable {
		t.Fatalf("state = %v, want %q", body["state"], TranscriptUnreachable)
	}
	if body["error"] == "" {
		t.Error("no error detail; the UI shows the remote's own message")
	}
}

func TestTranscriptUnknownSessionIs404(t *testing.T) {
	a, rec, _ := transcriptAPI(t, Options{CaptureContent: true})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "sess-1", "m", 10, 10))

	w, _ := get(t, a, "/api/sessions/nope/transcript", "127.0.0.1:1")
	if w.Code != 404 {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// SessionEvents is the scoped read the route depends on. If the filter did not apply,
// naming somebody else's session id would hand over their transcript.
func TestSessionEventsIsTenantScoped(t *testing.T) {
	db := openTestDB(t)
	mine := mkEvent(time.Now().UnixMilli(), "shared-id", "m", 100, 50)
	mine.TenantID = "t1"
	mine.Content = []ContentRow{{Path: "messages.0", Before: "MINE", After: "x"}}
	theirs := mkEvent(time.Now().UnixMilli()+1, "shared-id", "m", 100, 50)
	theirs.TenantID = "t2"
	theirs.Content = []ContentRow{{Path: "messages.0", Before: "THEIRS", After: "x"}}
	if err := db.insertBatch([]*Event{mine, theirs}); err != nil {
		t.Fatal(err)
	}

	got, err := db.SessionEvents(Filter{Tenant: "t1"}, "shared-id", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows for tenant t1, want 1 — the other tenant's turn leaked", len(got))
	}
	if got[0].Content[0].Before != "MINE" {
		t.Errorf("wrong tenant's transcript: %q", got[0].Content[0].Before)
	}

	// And withContent=false must not carry the text at all, so the metrics-only and
	// not-permitted paths cannot accidentally serve it.
	quiet, err := db.SessionEvents(Filter{Tenant: "t1"}, "shared-id", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(quiet) != 1 || len(quiet[0].Content) != 0 {
		t.Errorf("withContent=false still returned %d content rows", len(quiet[0].Content))
	}

	all, err := db.SessionEvents(Filter{TenantAll: true}, "shared-id", false)
	if err != nil || len(all) != 2 {
		t.Errorf("TenantAll returned %d rows, want 2 (err %v)", len(all), err)
	}
}

// Ordering is load-bearing: the diff view numbers turns from this slice, so a session
// whose rows came back newest-first would label turn 1 as the last turn.
func TestSessionEventsIsOldestFirst(t *testing.T) {
	db := openTestDB(t)
	base := time.Now().UnixMilli()
	var evs []*Event
	for i := 0; i < 4; i++ {
		evs = append(evs, mkEvent(base+int64(i)*1000, "s", "m", 100, 90))
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	got, err := db.SessionEvents(Filter{TenantAll: true}, "s", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(got); i++ {
		if got[i].TS < got[i-1].TS {
			t.Fatalf("row %d (ts %d) precedes row %d (ts %d)", i, got[i].TS, i-1, got[i-1].TS)
		}
	}
}
