package proxy

import (
	"strings"
	"testing"
)

const (
	codexRoute = "/openai/v1/responses"
	codexSID   = "019a7f2c-1d3e-7b4a-9c5d-2e8f6a0b1c34"
	codexUA    = "codex_cli_rs/0.162.1 (Mac OS 15.0; arm64) iTerm.app"
)

// codexBody is a Responses request as Codex sends it. The text before the last user turn changes
// after a compaction, so the content-hash session changes with it.
func codexBody(firstUser string) string {
	return `{"model":"gpt-x","prompt_cache_key":"` + codexSID + `","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + firstUser + `"}]}]}`
}

// A Codex request is keyed by its session-id header, so a compaction (a different first turn) keeps
// the session.
func TestCodexSessionKeyIsTheSessionIDHeader(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	hdr := map[string]string{"User-Agent": codexUA, "session-id": codexSID}
	before := sessionAt(t, srv, rec, codexRoute, codexBody("fix the bug"), hdr)
	after := sessionAt(t, srv, rec, codexRoute, codexBody("Summary of the earlier work: fixed it"), hdr)
	if !strings.HasSuffix(before, codexSID) || before != after {
		t.Fatalf("session before compaction %q, after %q; want both to end in %q", before, after, codexSID)
	}
}

// session-id has no vendor prefix. A caller that is not Codex must not move the session with it.
func TestSessionIDHeaderIgnoredForNonCodexCallers(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	body := codexBody("fix the bug")
	without := sessionAt(t, srv, rec, codexRoute, body, map[string]string{"User-Agent": "python-requests/2"})
	with := sessionAt(t, srv, rec, codexRoute, body, map[string]string{"User-Agent": "python-requests/2", "session-id": codexSID})
	if with != without || strings.Contains(with, codexSID) {
		t.Fatalf("session-id moved a non-Codex session from %q to %q", without, with)
	}
}

// Codex without a session-id header falls back to the content hash, as before.
func TestCodexWithoutSessionIDKeepsTheContentHash(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	got := sessionAt(t, srv, rec, codexRoute, codexBody("fix the bug"), map[string]string{"User-Agent": codexUA})
	if got == "" || strings.Contains(got, codexSID) {
		t.Fatalf("session = %q, want the content hash", got)
	}
}

// The x-context-guru-session header still wins over Codex's own id.
func TestCodexSessionIDLosesToTheContextGuruHeader(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	got := sessionAt(t, srv, rec, codexRoute, codexBody("x"),
		map[string]string{"User-Agent": codexUA, "session-id": codexSID, "x-context-guru-session": "plugin-1"})
	if !strings.HasSuffix(got, "plugin-1") {
		t.Fatalf("session = %q, want the x-context-guru-session id", got)
	}
}

// The caller decides, not the route: Codex headers on another path give the same key.
func TestCodexSessionKeyDoesNotDependOnTheRoute(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	hdr := map[string]string{"User-Agent": codexUA, "session-id": codexSID}
	got := sessionAt(t, srv, rec, "/openai/v1/chat/completions", bobBody("fix it", ""), hdr)
	if !strings.HasSuffix(got, codexSID) {
		t.Fatalf("session = %q, want it to end in %q", got, codexSID)
	}
}
