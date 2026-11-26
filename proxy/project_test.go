package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

// requestInDir builds an Anthropic request whose prompt states a working directory the way Claude
// Code states it — in a later message, not the first — so the fixture exercises the scan rather
// than a lucky index.
func requestInDir(model, dir string) []byte {
	env := "# Environment\nYou have been invoked in the following environment: \n" +
		" - Primary working directory: " + dir + "\n - Is a git repository: true\n"
	body := map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "please fix the failing test"},
			map[string]any{"role": "user", "content": env},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func postIn(t *testing.T, srv *httptest.Server, session, dir string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(requestInDir("aws/claude-sonnet-5", dir))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
	req.Header.Set("x-context-guru-session", session)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d", resp.StatusCode)
	}
}

// The whole point of the dimension: a real request through the real proxy lands in the database
// attributed to the folder its prompt named.
func TestARequestIsAttributedToTheFolderItsPromptNames(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	const dir = "/Users/v/OpenSource/context-guru"
	postIn(t, srv, "sess-proj", dir)
	waitForRows(t, rec, 1)

	page, err := rec.DB().Requests(dash.Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 1 {
		t.Fatalf("got %d rows, want 1", len(page.Requests))
	}
	if got := page.Requests[0].Project; got != dir {
		t.Errorf("project = %q; want %q", got, dir)
	}

	// And it is queryable, which is what a per-project view actually does.
	scoped, err := rec.DB().Requests(dash.Filter{Project: dir}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Requests) != 1 {
		t.Errorf("filtering to %s returned %d rows; want 1", dir, len(scoped.Requests))
	}
}

// PER REQUEST, NOT PER SESSION — the test that fails if attribution is ever moved to the session.
//
// Measured on real transcripts: 40 of 48 sessions stay in one directory, but 8 move, one of them
// across 21. Attributing a project to the session would label every request of those 8 with
// whichever folder happened to be first, so a per-project cost report would charge one project for
// work done in another.
func TestOneSessionThatChangesDirectoryIsTwoProjects(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	const first = "/Users/v/proj-one"
	const second = "/Users/v/proj-two"
	postIn(t, srv, "same-session", first)
	postIn(t, srv, "same-session", second)
	waitForRows(t, rec, 2)

	for dir, want := range map[string]int{first: 1, second: 1} {
		page, err := rec.DB().Requests(dash.Filter{Project: dir}, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Requests) != want {
			t.Errorf("project %s has %d requests; want %d — one session that changed directory "+
				"must produce two projects, not one", dir, len(page.Requests), want)
		}
	}
}

// A request that names no directory is recorded as unattributed rather than guessed at.
func TestARequestWithNoWorkingDirectoryIsUnattributed(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "no-dir")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)

	page, err := rec.DB().Requests(dash.Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := page.Requests[0].Project; got != "" {
		t.Errorf("project = %q; want \"\" so the row reads as unattributed", got)
	}
}
