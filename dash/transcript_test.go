package dash

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/apply"
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

// The transcript route used to have no LIMIT: one session in the production corpus is
// 1,310 turns / 31,425 rewritten messages / ~78 MB of JSON, and rendering it killed the
// browser tab. Paging is the fix, so the page has to be exact — walking every page must
// visit every turn once, in order, and never twice.
func TestSessionEventsPageWalksEveryTurnExactlyOnce(t *testing.T) {
	db := openTestDB(t)
	base := time.Now().UnixMilli()
	const n = 37
	var evs []*Event
	for i := 0; i < n; i++ {
		// Two turns share a ts on purpose: a keyset on ts alone would skip or repeat here,
		// which is exactly the bug a naive cursor introduces.
		ts := base + int64(i/2)*1000
		e := mkEvent(ts, "s", "m", 100, 90)
		e.Content = []ContentRow{{Path: "messages.0", Before: "b", After: "a"}}
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}

	seen := map[int64]int{}
	var order []int64
	cursor, pages := "", 0
	for {
		p, err := db.SessionEventsPage(Filter{TenantAll: true}, "s", false, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != n {
			t.Fatalf("page %d: Total = %d, want %d (the session's turn count, not the page's)", pages, p.Total, n)
		}
		if len(p.Requests) > 10 {
			t.Fatalf("page %d returned %d rows, over the limit of 10", pages, len(p.Requests))
		}
		for _, e := range p.Requests {
			seen[e.ID]++
			order = append(order, e.TS)
		}
		pages++
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
		if pages > 10 {
			t.Fatal("cursor never terminated")
		}
	}
	if pages != 4 {
		t.Errorf("walked %d pages of 10 over %d turns, want 4", pages, n)
	}
	if len(seen) != n {
		t.Errorf("saw %d distinct turns, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("turn %d returned %d times", id, c)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Errorf("turn %d (ts %d) precedes turn %d (ts %d) across the page boundary",
				i, order[i], i-1, order[i-1])
		}
	}
}

// limit <= 0 is the archive path: an upload that carried one page would misreport what
// was captured, so the unlimited wrapper must stay unlimited.
func TestSessionEventsIsStillUnlimited(t *testing.T) {
	db := openTestDB(t)
	base := time.Now().UnixMilli()
	var evs []*Event
	for i := 0; i < 120; i++ {
		evs = append(evs, mkEvent(base+int64(i), "s", "m", 100, 90))
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	got, err := db.SessionEvents(Filter{TenantAll: true}, "s", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 120 {
		t.Fatalf("SessionEvents returned %d of 120 turns — the archive would upload a page", len(got))
	}
}

// HasContent answers the SESSION's question, not the page's. A first page of turns with
// no stored text over a session whose later turns ARE stored used to make the route say
// "transcript storage is off", i.e. tell a user to switch on a setting they already have on.
func TestSessionEventsPageHasContentIsSessionWide(t *testing.T) {
	db := openTestDB(t)
	base := time.Now().UnixMilli()
	var evs []*Event
	for i := 0; i < 12; i++ {
		e := mkEvent(base+int64(i)*1000, "s", "m", 100, 90)
		if i >= 10 { // only the LAST page carries text
			e.Content = []ContentRow{{Path: "messages.0", Before: "b", After: "a"}}
		}
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	p, err := db.SessionEventsPage(Filter{TenantAll: true}, "s", true, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 5 {
		t.Fatalf("page 1 has %d rows, want 5", len(p.Requests))
	}
	for _, e := range p.Requests {
		if len(e.Content) != 0 {
			t.Fatal("fixture assumption: page 1 was supposed to have no stored text")
		}
	}
	if !p.HasContent {
		t.Error("HasContent = false on a session with stored text on a later page")
	}

	// And it stays false when the session really has none.
	empty := openTestDB(t)
	if err := empty.insertBatch([]*Event{mkEvent(base, "s", "m", 100, 90)}); err != nil {
		t.Fatal(err)
	}
	q, err := empty.SessionEventsPage(Filter{TenantAll: true}, "s", true, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if q.HasContent {
		t.Error("HasContent = true on a session with no stored text")
	}
}

// A garbage cursor must be ignored (page 1), never turned into a SQL error or an empty
// page — a stale link is not an error the reader caused.
func TestTranscriptCursorGarbageFallsBackToPageOne(t *testing.T) {
	for _, bad := range []string{"", "abc", "abc:def", "12", ":", "1:", ":2"} {
		if _, _, ok := parseTranscriptCursor(bad); ok {
			t.Errorf("parseTranscriptCursor(%q) accepted it", bad)
		}
	}
	if ts, id, ok := parseTranscriptCursor("1700000000000:42"); !ok || ts != 1700000000000 || id != 42 {
		t.Errorf("parseTranscriptCursor round trip = %d,%d,%v", ts, id, ok)
	}
}

// The route's own contract: default page size, an explicit ?limit=, and the cap.
func TestTranscriptRouteIsCapped(t *testing.T) {
	a, rec, _ := transcriptAPI(t, Options{CaptureContent: true})
	base := time.Now().UnixMilli()
	var evs []*Event
	for i := 0; i < transcriptPageSize+7; i++ {
		e := mkEvent(base+int64(i)*1000, "s", "m", 100, 90)
		e.Content = []ContentRow{{Path: "messages.0", Before: "b", After: "a"}}
		evs = append(evs, e)
	}
	seed(t, rec, evs...)

	var page struct {
		Requests   []*Event `json:"requests"`
		NextCursor string   `json:"next_cursor"`
		Total      int64    `json:"total"`
		PageSize   int      `json:"page_size"`
	}
	w, _ := get(t, a, "/api/sessions/s/transcript", "127.0.0.1:1")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != transcriptPageSize {
		t.Errorf("default page served %d turns, want %d", len(page.Requests), transcriptPageSize)
	}
	if page.Total != int64(transcriptPageSize+7) {
		t.Errorf("total = %d, want %d", page.Total, transcriptPageSize+7)
	}
	if page.NextCursor == "" {
		t.Error("no next_cursor with 7 turns left to read")
	}

	// The last page: the cursor resolves and the walk terminates.
	w2, _ := get(t, a, "/api/sessions/s/transcript?after="+page.NextCursor, "127.0.0.1:1")
	var p2 = page
	if err := json.Unmarshal(w2.Body.Bytes(), &p2); err != nil {
		t.Fatal(err)
	}
	if len(p2.Requests) != 7 || p2.NextCursor != "" {
		t.Errorf("last page: %d turns, next_cursor %q; want 7 and \"\"", len(p2.Requests), p2.NextCursor)
	}

	// An explicit ?limit= is honoured, and cannot ask for the whole 78 MB.
	w3, _ := get(t, a, "/api/sessions/s/transcript?limit=3", "127.0.0.1:1")
	var p3 = page
	if err := json.Unmarshal(w3.Body.Bytes(), &p3); err != nil {
		t.Fatal(err)
	}
	if len(p3.Requests) != 3 || p3.PageSize != 3 {
		t.Errorf("?limit=3 served %d turns (page_size %d)", len(p3.Requests), p3.PageSize)
	}
	w4, _ := get(t, a, "/api/sessions/s/transcript?limit=100000", "127.0.0.1:1")
	var p4 = page
	if err := json.Unmarshal(w4.Body.Bytes(), &p4); err != nil {
		t.Fatal(err)
	}
	if p4.PageSize != transcriptPageMax {
		t.Errorf("?limit=100000 gave page_size %d, want the cap %d", p4.PageSize, transcriptPageMax)
	}
}

// windowEvents is the cold-storage half of the same cap: a FULL archive leaves no local
// row for SQL to page over, so without this the one path that skipped the LIMIT would
// still hand back every turn.
func TestWindowEventsPagesArchivedTurns(t *testing.T) {
	mk := func(ts, id int64) *Event { return &Event{ID: id, TS: ts} }
	all := []*Event{mk(10, 1), mk(10, 2), mk(20, 3), mk(30, 4), mk(30, 5)}

	got, next, total := windowEvents(all, "", 2)
	if total != 5 || len(got) != 2 || got[1].ID != 2 || next != "10:2" {
		t.Fatalf("page 1 = %d rows, next %q, total %d", len(got), next, total)
	}
	// Two turns share ts 10, so a cursor on ts alone would drop or repeat one.
	got, next, _ = windowEvents(all, next, 2)
	if len(got) != 2 || got[0].ID != 3 || got[1].ID != 4 || next != "30:4" {
		t.Fatalf("page 2 = %v, next %q", eventIDs(got), next)
	}
	got, next, _ = windowEvents(all, next, 2)
	if len(got) != 1 || got[0].ID != 5 || next != "" {
		t.Fatalf("page 3 = %v, next %q; want [5] and \"\"", eventIDs(got), next)
	}
	// Unlimited and a garbage cursor both mean "everything, from the start".
	if got, next, _ = windowEvents(all, "", 0); len(got) != 5 || next != "" {
		t.Errorf("limit 0 = %d rows, next %q", len(got), next)
	}
	if got, _, _ = windowEvents(all, "nonsense", 2); len(got) != 2 || got[0].ID != 1 {
		t.Errorf("garbage cursor did not fall back to page 1: %v", eventIDs(got))
	}
}

func eventIDs(evs []*Event) []int64 {
	out := make([]int64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

// content_cap_bytes used to serve the dashboard's own --dashboard-content-cap, which
// could never bind: apply clips Change.Before/After to apply.TraceTextCap (4,000 bytes)
// before a row reaches capture, and the dashboard default is 16 KiB. So the API asserted
// a number no truncation had ever used. It must report the cap that actually binds.
func TestContentCapReportsTheCapThatBinds(t *testing.T) {
	if apply.TraceTextCap >= defaultContentCap {
		t.Skipf("fixture assumption: apply's clip (%d) is the tighter of the two (dash %d)",
			apply.TraceTextCap, defaultContentCap)
	}
	for _, tc := range []struct {
		name    string
		dashCap int
		want    int
	}{
		{"default 16 KiB is not the cap", defaultContentCap, apply.TraceTextCap},
		{"a dash cap tighter than apply's does bind", 512, 512},
		{"unset means apply's", 0, apply.TraceTextCap},
	} {
		if got := effectiveContentCap(tc.dashCap); got != tc.want {
			t.Errorf("%s: effectiveContentCap(%d) = %d, want %d", tc.name, tc.dashCap, got, tc.want)
		}
	}

	// And the route serves it, on both the transcript and the single request.
	a, rec, _ := transcriptAPI(t, Options{CaptureContent: true, ContentCap: defaultContentCap})
	e := mkEvent(time.Now().UnixMilli(), "s", "m", 100, 90)
	e.Content = []ContentRow{{Path: "messages.0", Before: "b", After: "a"}}
	seed(t, rec, e)

	for _, url := range []string{"/api/sessions/s/transcript", "/api/requests/1"} {
		w, _ := get(t, a, url, "127.0.0.1:1")
		if w.Code != 200 {
			t.Fatalf("%s: status %d", url, w.Code)
		}
		var body struct {
			Cap int `json:"content_cap_bytes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Cap != apply.TraceTextCap {
			t.Errorf("%s served content_cap_bytes = %d, want the binding cap %d",
				url, body.Cap, apply.TraceTextCap)
		}
	}
}
