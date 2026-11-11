//go:build cg_record

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const otherSessionUUID = "0b7e9c11-2f4d-4e8a-9c3b-5a6d7e8f9a0b"

func wireOf(dir, id string) string   { return filepath.Join(dir, id+".wire.jsonl") }
func markerOf(dir, id string) string { return filepath.Join(dir, id+noRecordSuffix) }

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func newRec(t *testing.T, dir string) *sessionRecorder {
	t.Helper()
	r, err := newSessionRecorder(dir, "", 1<<20, 8<<20, 16, quietRecLog())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Decline deletes what was written, leaves the marker, and stops every later turn of that
// session, in this process and after a restart. Another session is not touched.
func TestDeclineDeletesTheSessionAndKeepsItOff(t *testing.T) {
	dir := t.TempDir()
	recordAndClose(t, newRec(t, dir),
		baseEvent(testSessionUUID, []byte(testSSE), true), baseEvent(otherSessionUUID, []byte(testSSE), true))
	if !exists(t, wireOf(dir, testSessionUUID)) {
		t.Fatal("setup: the session's file was not written")
	}

	r := newRec(t, dir)
	_, deleted, err := r.Decline("", testSessionUUID)
	if err != nil || !deleted {
		t.Fatalf("Decline = %v, %v; want the file deleted", deleted, err)
	}
	if exists(t, wireOf(dir, testSessionUUID)) {
		t.Fatal("the declined session's file is still there")
	}
	if !exists(t, markerOf(dir, testSessionUUID)) {
		t.Fatal("no .no-record marker: the opt-out would not survive a restart")
	}
	recordAndClose(t, r, baseEvent(testSessionUUID, []byte(testSSE), true))
	if exists(t, wireOf(dir, testSessionUUID)) {
		t.Fatal("a turn after Decline re-created the file")
	}
	if r.skipped.Load() != 1 {
		t.Errorf("skipped = %d, want 1", r.skipped.Load())
	}

	// A new process knows nothing in memory: the marker alone must keep the session off.
	recordAndClose(t, newRec(t, dir), baseEvent(testSessionUUID, []byte(testSSE), true))
	if exists(t, wireOf(dir, testSessionUUID)) {
		t.Fatal("after a restart the declined session was recorded again")
	}
	if !exists(t, wireOf(dir, otherSessionUUID)) {
		t.Fatal("Decline of one session deleted another")
	}
}

// Declining a session that has no file yet (the hook can arrive before the first turn) is fine,
// and stops the first turn.
func TestDeclineBeforeTheFirstTurn(t *testing.T) {
	dir := t.TempDir()
	r := newRec(t, dir)
	if _, deleted, err := r.Decline("", testSessionUUID); err != nil || deleted {
		t.Fatalf("Decline = %v, %v; want nothing deleted and no error", deleted, err)
	}
	recordAndClose(t, r, baseEvent(testSessionUUID, []byte(testSSE), true))
	if exists(t, wireOf(dir, testSessionUUID)) {
		t.Fatal("the first turn of a declined session was written")
	}
}

// In hosted mode the files are in the tenant's subdirectory: Decline must look there, and only
// there, so one tenant cannot stop another's session with the same id.
func TestDeclineIsPerTenant(t *testing.T) {
	dir := t.TempDir()
	evA, evB := baseEvent(testSessionUUID, []byte(testSSE), true), baseEvent(testSessionUUID, []byte(testSSE), true)
	evA.tenantID, evB.tenantID = "tA", "tB"
	recordAndClose(t, newRec(t, dir), evA, evB)

	r := newRec(t, dir)
	if _, deleted, err := r.Decline("tA", testSessionUUID); err != nil || !deleted {
		t.Fatalf("Decline = %v, %v", deleted, err)
	}
	r.Close()
	if exists(t, wireOf(filepath.Join(dir, "tA"), testSessionUUID)) {
		t.Fatal("tenant A's session is still recorded")
	}
	if !exists(t, wireOf(filepath.Join(dir, "tB"), testSessionUUID)) {
		t.Fatal("tenant A's opt-out deleted tenant B's session with the same id")
	}
}

func postRecordOff(h *Handler, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:40000" // a single-user proxy: the principal is direct loopback
	w := httptest.NewRecorder()
	h.recordOff(w, req)
	return w
}

func TestRecordOffRoute(t *testing.T) {
	dir := t.TempDir()
	recordAndClose(t, newRec(t, dir), baseEvent(testSessionUUID, []byte(testSSE), true))
	r := newRec(t, dir)
	defer r.Close()
	h := &Handler{recorder: r}

	for _, c := range []struct{ target, body string }{
		{"/api/me/record/off", `{"session":""}`},
		{"/api/me/record/off", `{"session":".."}`},
		{"/api/me/record/off", `{"session":"../x"}`},
		{"/api/me/record/off?tenant=someone", `{"session":"` + testSessionUUID + `"}`},
	} {
		if w := postRecordOff(h, c.target, c.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status %d, want 400", c.target, c.body, w.Code)
		}
	}
	if !exists(t, wireOf(dir, testSessionUUID)) {
		t.Fatal("a refused request deleted the file")
	}

	w := postRecordOff(h, "/api/me/record/off", `{"session":"`+testSessionUUID+`"}`)
	var doc map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &doc) != nil || doc["deleted"] != true || doc["recording"] != true {
		t.Fatalf("status %d body %s; want 200 with recording and deleted true", w.Code, w.Body)
	}
	if exists(t, wireOf(dir, testSessionUUID)) {
		t.Fatal("the route answered 200 but the file is still there")
	}

	// A request from elsewhere, with no account behind it, is refused.
	req := httptest.NewRequest(http.MethodPost, "/api/me/record/off", strings.NewReader(`{"session":"`+otherSessionUUID+`"}`))
	req.RemoteAddr = "192.0.2.10:5000"
	rw := httptest.NewRecorder()
	h.recordOff(rw, req)
	if rw.Code == http.StatusOK {
		t.Fatal("a non-loopback caller with no token could stop a recording")
	}
}

// A proxy without a recorder answers plainly that nothing is recorded.
func TestRecordOffWithoutARecorder(t *testing.T) {
	w := postRecordOff(&Handler{}, "/api/me/record/off", `{"session":"`+testSessionUUID+`"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"recording":false`) {
		t.Fatalf("status %d body %s", w.Code, w.Body)
	}
}
