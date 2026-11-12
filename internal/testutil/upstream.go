package testutil

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Call is one request a fake upstream received.
type Call struct {
	Path   string
	Header http.Header
	Body   []byte
}

// Upstream is a fake HTTP upstream. It records each request and answers it with the handler the
// test gave. A test uses WaitForCall to wait for a request that a background goroutine sends,
// in place of a check right after the client call returns.
type Upstream struct {
	*httptest.Server

	mu    sync.Mutex
	calls []Call
}

// NewUpstream starts a fake upstream. respond writes the reply; the body is already read and
// recorded. A nil respond replies 200 with the empty JSON object {}. The server closes when the
// test ends.
func NewUpstream(t testing.TB, respond func(w http.ResponseWriter, r *http.Request)) *Upstream {
	t.Helper()
	u := &Upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.calls = append(u.calls, Call{Path: r.URL.Path, Header: r.Header.Clone(), Body: b})
		u.mu.Unlock()
		if respond == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{}"))
			return
		}
		respond(w, r)
	}))
	t.Cleanup(u.Server.Close)
	return u
}

// Calls returns a copy of the requests received so far.
func (u *Upstream) Calls() []Call {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Call(nil), u.calls...)
}

// Matching returns the received requests for which match is true.
func (u *Upstream) Matching(match func(Call) bool) []Call {
	var out []Call
	for _, c := range u.Calls() {
		if match(c) {
			out = append(out, c)
		}
	}
	return out
}

// WaitForCall waits until at least n received requests satisfy match. On timeout it fails with
// what and the number of requests seen, and returns the matching requests when it succeeds.
func (u *Upstream) WaitForCall(t testing.TB, what string, n int, match func(Call) bool) []Call {
	t.Helper()
	WaitForState(t, what, func() bool { return len(u.Matching(match)) >= n }, func() string {
		var paths []string
		for _, c := range u.Calls() {
			paths = append(paths, c.Path)
		}
		return fmt.Sprintf("%d of %d wanted calls matched; all calls: %v", len(u.Matching(match)), n, paths)
	})
	return u.Matching(match)
}
