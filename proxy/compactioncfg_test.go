package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/store"
)

func cptHandler(t *testing.T) (*Handler, *httptest.Server) {
	t.Helper()
	h := tdHandler(t, "ultrahigh", "http://127.0.0.1:1")
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return h, srv
}

func cptPost(t *testing.T, srv *httptest.Server, body string) int {
	t.Helper()
	r, err := http.Post(srv.URL+"/compaction-config", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return r.StatusCode
}

func cptReq(session string) *http.Request {
	r := httptest.NewRequest("POST", "/anthropic/v1/messages", nil)
	r.Header.Set("x-context-guru-session", session)
	return r
}

// The route stores a session's settings, and compactionPointFor turns them into C.
func TestCompactionConfigRouteSetsC(t *testing.T) {
	h, srv := cptHandler(t)
	tn, _ := h.authenticate(cptReq(""))
	body := []byte(`{"model":"claude-opus-5"}`)

	// No record: the table.
	if p := h.compactionPointFor(tn, cptReq("sess-1"), body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Fatalf("no record: %+v", p)
	}
	if c := cptPost(t, srv, `{"session":"sess-1","harness":"claude-code","env_window":"200k"}`); c != http.StatusNoContent {
		t.Fatalf("status %d", c)
	}
	p := h.compactionPointFor(tn, cptReq("sess-1"), body, 1_000_000)
	if p.Source != compactionpoint.Configured || p.Tokens(1_000_000) != 167_000 {
		t.Errorf("sess-1: %+v", p)
	}
	// Another session is not touched.
	if p := h.compactionPointFor(tn, cptReq("sess-2"), body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Errorf("sess-2 inherited: %+v", p)
	}
	// A request with no id of its own cannot be matched.
	if p := h.compactionPointFor(tn, httptest.NewRequest("POST", "/", nil), body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Errorf("no session id: %+v", p)
	}
	// The setting changes: the new report replaces the old one.
	cptPost(t, srv, `{"session":"sess-1","harness":"claude-code","env_disable_auto":"1"}`)
	if p := h.compactionPointFor(tn, cptReq("sess-1"), body, 1_000_000); p.Tokens(1_000_000) != 1_000_000 {
		t.Errorf("after disable: %+v", p)
	}
	// An empty report (setting removed) goes back to the table.
	cptPost(t, srv, `{"session":"sess-1","harness":"claude-code"}`)
	if p := h.compactionPointFor(tn, cptReq("sess-1"), body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Errorf("after clear: %+v", p)
	}
}

// Fail open: an odd request or an odd stored value never changes C and never errors.
func TestCompactionConfigRouteFailsOpen(t *testing.T) {
	h, srv := cptHandler(t)
	tn, _ := h.authenticate(cptReq(""))
	for _, b := range []string{
		`not json`,
		`{"session":"s","harness":"bob","env_window":"200k"}`,                                    // not a verified harness
		`{"session":"s","harness":"claude-code","env_window":"` + strings.Repeat("9", 40) + `"}`, // too long
		`{"session":"../x","harness":"claude-code","env_window":"200k"}`,                         // unusable session id
		`{"harness":"claude-code","env_window":"200k"}`,                                          // no session id
	} {
		if c := cptPost(t, srv, b); c != http.StatusBadRequest {
			t.Errorf("%q -> %d, want 400", b, c)
		}
	}
	body := []byte(`{"model":"claude-opus-5"}`)
	for _, stored := range []string{`garbage`, `{"harness":"claude-code","env_window":"lots"}`, `{"harness":"claude-code","env_pct":"-3"}`} {
		tn.Store.Put(store.CompactionPointPrefix+"sess-x", []byte(stored))
		if p := h.compactionPointFor(tn, cptReq("sess-x"), body, 1_000_000); p.Source != compactionpoint.Assumed {
			t.Errorf("stored %q gave %+v, want the table", stored, p)
		}
	}
	// The operator's switch turns the route off.
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "off")
	if c := cptPost(t, srv, `{"session":"s","harness":"claude-code","env_window":"200k"}`); c != http.StatusNotFound {
		t.Errorf("switch off -> %d", c)
	}
}
