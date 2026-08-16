package apply

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
)

// ccUserID is the shape Claude Code actually sends in `metadata.user_id`, taken from
// real captured proxy traffic: a JSON object STRING, ~170 bytes, whose session_id is
// the CLI's own session and the one part that survives an auto-compaction.
// (account_uuid is empty when the CLI authenticates with an API key.)
func ccUserID(sessionID string) string {
	hex := strings.Repeat("0123456789abcdef", 4) // stands in for a 64-hex device digest
	return `{"device_id":"` + hex + `","account_uuid":"","session_id":"` + sessionID + `"}`
}

// sessionOf runs a minimal request through the real entry point and returns the
// session id it resolved. metaRaw is spliced in as RAW JSON (empty => no `metadata`
// field at all) so every malformed shape is exercised as bytes on the wire, which is
// where they actually arrive.
func sessionOf(t *testing.T, tenant, header, metaRaw, firstUser string) string {
	t.Helper()
	return sessionOfHead(t, tenant, header, metaRaw, "sys", firstUser)
}

// sessionOfHead is sessionOf with the system head under test too: Bob Shell
// regenerates its environment boilerplate at compaction, so BOTH halves of the
// derived hash move for it.
func sessionOfHead(t *testing.T, tenant, header, metaRaw, sys, firstUser string) string {
	t.Helper()
	body := `{"model":"claude","messages":[{"role":"system","content":` + mustJSON(t, sys) + `},{"role":"user","content":` +
		mustJSON(t, firstUser) + `}]`
	if metaRaw != "" {
		body += `,"metadata":` + metaRaw
	}
	body += `}`
	res := BodyOpts(context.Background(), components.NewPipeline(nil, nil), store.NewMemory(store.Options{}), Opts{
		Provider: bschemas.Anthropic,
		Body:     []byte(body),
		Session:  header,
		Tenant:   tenant,
	})
	if res.Trace.Session == "" {
		t.Fatalf("no session resolved for body %.120s", body)
	}
	return res.Trace.Session
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSessionIDFromMetadataUserID is the regression test for session identity
// flipping when the agent compacts its OWN context.
//
// Claude Code auto-compaction rewrites the transcript, replacing the first user
// message with "This session is being continued from a previous conversation…". The
// derived key is sha256(system + firstUser), so it changed mid-conversation: the
// dashboard split one conversation into two sessions, every c.Session-keyed cache
// reset, and /expand 404'd for pre-compaction ids. metadata.user_id carries the
// CLI's session id, which compaction does NOT change.
func TestSessionIDFromMetadataUserID(t *testing.T) {
	const (
		firstUser = "fix the failing test in foo_test.go"
		compacted = "This session is being continued from a previous conversation that ran out of context. The conversation is summarized below: …"
		ccSession = "2e168312-56a5-423e-92d1-816863d16a7d"
		huge      = 4 << 20 // 4 MB
	)
	meta := `{"user_id":` + mustJSON(t, ccUserID(ccSession)) + `}`
	derived := sessionOf(t, "", "", "", firstUser) // what the fallback produces

	t.Run("stable across compaction", func(t *testing.T) {
		before := sessionOf(t, "", "", meta, firstUser)
		after := sessionOf(t, "", "", meta, compacted)
		if before != after {
			t.Fatalf("session id flipped across compaction: %q -> %q", before, after)
		}
		if before != ccSession {
			t.Errorf("session id = %q, want the CLI session id %q", before, ccSession)
		}
		// The bug is real: without the metadata read those same two turns DO diverge.
		if sessionOf(t, "", "", "", firstUser) == sessionOf(t, "", "", "", compacted) {
			t.Error("fixture does not reproduce the derived-key flip; this test would prove nothing")
		}
	})

	t.Run("header still wins", func(t *testing.T) {
		if got := sessionOf(t, "", "explicit-hdr", meta, firstUser); got != "explicit-hdr" {
			t.Errorf("metadata.user_id overtook the header: %q", got)
		}
	})

	// Every shape that is not a usable id must fall back to the derived hash — not
	// panic, and never yield a key like "<nil>" or "map[]" or one carrying a separator
	// (it becomes a store key, a session_id column, a cold-storage path segment and a
	// string in the dashboard UI).
	t.Run("malformed shapes fall back", func(t *testing.T) {
		for name, metaRaw := range map[string]string{
			"absent":                 "",
			"null":                   "null",
			"not an object":          `"nope"`,
			"array":                  `["u1"]`,
			"number":                 "7",
			"user_id absent":         `{"other":"x"}`,
			"user_id null":           `{"user_id":null}`,
			"user_id number":         `{"user_id":12345}`,
			"user_id bool":           `{"user_id":true}`,
			"user_id object":         `{"user_id":{"session_id":"` + ccSession + `"}}`,
			"user_id array":          `{"user_id":["` + ccSession + `"]}`,
			"user_id empty":          `{"user_id":""}`,
			"user_id blank":          `{"user_id":"   "}`,
			"truncated json":         `{"user_id":"{\"session_id\":\"` + ccSession + `\""}`,
			"object without session": `{"user_id":"{\"device_id\":\"abc\"}"}`,
			"session_id not string":  `{"user_id":"{\"session_id\":42}"}`,
			"session_id object":      `{"user_id":"{\"session_id\":{\"a\":1}}"}`,
			"session_id traversal":   `{"user_id":"{\"session_id\":\"../../../../backup/cg-control.db\"}"}`,
			"session_id newline":     `{"user_id":"{\"session_id\":\"a\\nb\"}"}`,
			"session_id nul":         `{"user_id":"{\"session_id\":\"a\\u0000b\"}"}`,
			"session_id unicode":     `{"user_id":"{\"session_id\":\"sessiön\"}"}`,
			"raw id with slash":      `{"user_id":"/etc/passwd"}`,
			"raw id too long":        `{"user_id":"` + strings.Repeat("a", session.MaxExplicitLen+1) + `"}`,
			"oversized value":        `{"user_id":"` + strings.Repeat("a", huge) + `"}`,
			"oversized json value":   `{"user_id":"{\"session_id\":\"` + strings.Repeat("a", huge) + `\"}"}`,
		} {
			got := sessionOf(t, "", "", metaRaw, firstUser)
			if got != derived {
				t.Errorf("%s: session = %q, want the derived fallback %q", name, got, derived)
			}
		}
	})

	// A bare id from a host that is not Claude Code still works: the JSON unwrap only
	// applies to the object shape.
	t.Run("plain id passes through", func(t *testing.T) {
		if got := sessionOf(t, "", "", `{"user_id":"sess-e2e"}`, firstUser); got != "sess-e2e" {
			t.Errorf("plain metadata.user_id not used: %q", got)
		}
	})
}

// TestSessionIDFromMetadataTaskID is the same regression for Bob Shell, which sends
// `metadata.taskId` — a bare randomUUID assigned once per session and re-rolled only
// by /clear, so it too survives compaction.
//
// Bob's derived key is worse off than Claude Code's: at compaction the first user
// message becomes a <state_snapshot> summary AND the system head is regenerated with
// a fresh recursive listing of the working directory, so both hash inputs move. Any
// session that has created or deleted a file flips its key — which, besides splitting
// the dashboard, resets modes.Tracker's cached-prefix boundary to 0 and lets the
// offloaders rewrite a prefix the provider has already cached: a real cache-write cost,
// not just a display bug.
func TestSessionIDFromMetadataTaskID(t *testing.T) {
	const (
		sysBefore = "You are Bob. Today is 2026-08-15. Folder structure:\n- foo.py\n- bar.py"
		sysAfter  = "You are Bob. Today is 2026-08-16. Folder structure:\n- foo.py\n- bar.py\n- baz.py"
		firstUser = "add a baz module"
		compacted = "<state_snapshot>… summary of the previous conversation …</state_snapshot>"
		bobTask   = "b2f1c0de-4a51-4d0e-9f30-77a1c9d5e412" // randomUUID()
	)
	meta := `{"taskId":"` + bobTask + `"}`

	before := sessionOfHead(t, "", "", meta, sysBefore, firstUser)
	after := sessionOfHead(t, "", "", meta, sysAfter, compacted)
	if before != after {
		t.Fatalf("session id flipped across compaction: %q -> %q", before, after)
	}
	if before != bobTask {
		t.Errorf("session id = %q, want the taskId %q (a UUID must pass the explicit-id allow-list)", before, bobTask)
	}
	// Both heads really did move, so the derived fallback flips on this pair.
	if sessionOfHead(t, "", "", "", sysBefore, firstUser) == sessionOfHead(t, "", "", "", sysAfter, compacted) {
		t.Error("fixture does not reproduce the derived-key flip; this test would prove nothing")
	}
	// Neither key present: unchanged fallback behaviour.
	if got, want := sessionOfHead(t, "", "", `{"tags":["taskId:`+bobTask+`"]}`, sysBefore, firstUser),
		sessionOfHead(t, "", "", "", sysBefore, firstUser); got != want {
		t.Errorf("metadata without a known key changed the session: %q, want %q", got, want)
	}
	// Both keys present: user_id wins, deterministically (metaSessionKeys order).
	ccSession := "2e168312-56a5-423e-92d1-816863d16a7d"
	both := `{"user_id":` + mustJSON(t, ccUserID(ccSession)) + `,"taskId":"` + bobTask + `"}`
	if got := sessionOf(t, "", "", both, firstUser); got != ccSession {
		t.Errorf("with both keys present, session = %q, want metadata.user_id %q", got, ccSession)
	}
	// The header still outranks both.
	if got := sessionOf(t, "", "explicit-hdr", both, firstUser); got != "explicit-hdr" {
		t.Errorf("metadata overtook the header: %q", got)
	}
	// Tenant scoping holds for the taskId path too.
	if a, b := sessionOf(t, "tenant-a", "", meta, firstUser), sessionOf(t, "tenant-b", "", meta, firstUser); a == b {
		t.Fatalf("two tenants sending one taskId collided on %q", a)
	}
	// And a malformed taskId falls back rather than reaching a store key.
	for name, raw := range map[string]string{
		"null":      `{"taskId":null}`,
		"number":    `{"taskId":7}`,
		"object":    `{"taskId":{"id":"` + bobTask + `"}}`,
		"traversal": `{"taskId":"../../etc/passwd"}`,
		"too long":  `{"taskId":"` + strings.Repeat("a", session.MaxExplicitLen+1) + `"}`,
	} {
		got := sessionOfHead(t, "", "", raw, sysBefore, firstUser)
		if want := sessionOfHead(t, "", "", "", sysBefore, firstUser); got != want {
			t.Errorf("%s: session = %q, want the derived fallback %q", name, got, want)
		}
	}
}
