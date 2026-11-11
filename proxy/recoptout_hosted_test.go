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

	"github.com/rossoctl/context-guru/tenant"
)

// Hosted mode end to end, through the real mux and token authentication:
//   - an opted-in account's own file is deleted, and another account's file with the SAME id stays;
//   - "recording" is the account's consent, so an account that never opted in hears false;
//   - an account that is not recorded writes no marker for an id it never had;
//   - a file that exists is still deleted after the account opted out of recording;
//   - calls are rate-limited per account.
func TestRecordOffHostedFollowsTheAccountAndItsConsent(t *testing.T) {
	dir := t.TempDir()
	rec := newRec(t, dir)
	t.Cleanup(func() { rec.Close() })
	t.Setenv(envRegisterMode, "open")
	f := newHostedFixtureOpts(t, "up", "anthropic", "TEST_UPSTREAM_KEY", func(o *Options) { o.Record = rec })
	f.signUp(t, "boss@ibm.com", "l")
	_, va := f.signUp(t, "a@ibm.com", "a")
	_, vb := f.signUp(t, "b@ibm.com", "b")
	idA, tokA := va["tenant"].(map[string]any)["id"].(string), va["token"].(string)
	idB, tokB := vb["tenant"].(map[string]any)["id"].(string), vb["token"].(string)

	ta, err := f.reg.Get(idA)
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := f.reg.Update(ta, idA, tenant.Patch{RecordTraces: &yes}); err != nil {
		t.Fatal(err)
	}

	dirA, dirB := filepath.Join(dir, sanitizeTraceID(idA)), filepath.Join(dir, sanitizeTraceID(idB))
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wireOf(d, testSessionUUID), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	call := func(tok, id string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, "/api/me/record/off", strings.NewReader(`{"session":"`+id+`"}`))
		r.Header.Set("content-type", "application/json")
		r.Header.Set(TokenHeader, tok)
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		var doc map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &doc)
		return w.Code, doc
	}

	// A, opted in: its own file goes, B's file with the same id stays.
	if code, doc := call(tokA, testSessionUUID); code != http.StatusOK || doc["recording"] != true || doc["deleted"] != true {
		t.Fatalf("A: %d %v; want 200 recording and deleted", code, doc)
	}
	if exists(t, wireOf(dirA, testSessionUUID)) {
		t.Fatal("A's file is still there")
	}
	if !exists(t, wireOf(dirB, testSessionUUID)) {
		t.Fatal("A's opt-out deleted B's file with the same id")
	}

	// B, never opted in, an id it never had: "recording" is false and no marker is written.
	if code, doc := call(tokB, otherSessionUUID); code != http.StatusOK || doc["recording"] != false || doc["deleted"] != false {
		t.Fatalf("B, unknown id: %d %v; want 200, recording false", code, doc)
	}
	if exists(t, markerOf(dirB, otherSessionUUID)) {
		t.Fatal("an account that is not recorded left a marker for an id it never had")
	}

	// B's existing file (recorded before it opted out of recording) is still deleted.
	if code, doc := call(tokB, testSessionUUID); code != http.StatusOK || doc["recording"] != false || doc["deleted"] != true {
		t.Fatalf("B, existing file: %d %v; want 200, recording false, deleted", code, doc)
	}
	if exists(t, wireOf(dirB, testSessionUUID)) {
		t.Fatal("B's existing file is still there")
	}

	// Rate limit per account: A has used 1 of adaptApplyBurst.
	for i := 1; i < adaptApplyBurst; i++ {
		if code, _ := call(tokA, testSessionUUID); code != http.StatusOK {
			t.Fatalf("call %d of the burst: %d", i+1, code)
		}
	}
	if code, _ := call(tokA, testSessionUUID); code != http.StatusTooManyRequests {
		t.Fatalf("call past the burst: %d, want 429", code)
	}
	if code, _ := call(tokB, otherSessionUUID); code != http.StatusOK {
		t.Fatalf("A's limit stopped B: %d", code)
	}
}
