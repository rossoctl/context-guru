package session

import (
	"strings"
	"testing"
)

func TestResolveExplicitWins(t *testing.T) {
	if got := Resolve("  explicit-id  ", "sys", "user"); got != "explicit-id" {
		t.Fatalf("a non-empty explicit id should win (trimmed), got %q", got)
	}
}

func TestResolveHashStableAndScoped(t *testing.T) {
	a := Resolve("", "sys", "u1")
	b := Resolve("", "sys", "u1")
	c := Resolve("", "sys", "u2")
	if a != b {
		t.Fatal("identical (system, firstUser) must hash to the same session")
	}
	if a == c {
		t.Fatal("a different first user must produce a different session")
	}
	if len(a) != 16 {
		t.Fatalf("session hash should be 16 hex chars, got %q", a)
	}
}

// Two tenants running the same agent against the same repository produce an
// identical content hash. Scoping is what stops them sharing one session — and with
// it one sticky offload set and one cached-prefix boundary.
func TestScopedSeparatesIdenticalContent(t *testing.T) {
	sys, first := "you are a coding agent", "fix the bug in foo.py"
	a := Scoped("tenant-a", "", sys, first)
	b := Scoped("tenant-b", "", sys, first)
	if a == b {
		t.Fatalf("two tenants collided on one session key: %q", a)
	}
	// Same tenant, same content, still stable across turns.
	if a != Scoped("tenant-a", "", sys, first) {
		t.Error("scoped key is not stable for one tenant")
	}
}

// A client-supplied session id must not let one tenant name another's session.
func TestScopedNamespacesExplicitIDs(t *testing.T) {
	a := Scoped("tenant-a", "shared-id", "", "")
	b := Scoped("tenant-b", "shared-id", "", "")
	if a == b {
		t.Fatalf("an explicit session id crossed tenants: %q", a)
	}
	if !strings.HasPrefix(a, "tenant-a:") {
		t.Errorf("explicit id was not namespaced: %q", a)
	}
	// Same, for the id that arrives in Anthropic's metadata.user_id rather than the
	// header. Claude Code sends its session id inside a JSON object string, so the
	// namespacing has to survive the unwrap: two tenants running two CLIs that happen
	// to report one session id must not share state.
	ccUserID := `{"device_id":"beef","account_uuid":"","session_id":"2e168312-56a5-423e-92d1-816863d16a7d"}`
	ma := Scoped("tenant-a", ExplicitID("", ccUserID), "", "")
	mb := Scoped("tenant-b", ExplicitID("", ccUserID), "", "")
	if ma == mb {
		t.Fatalf("a metadata.user_id crossed tenants: %q", ma)
	}
	if ma != "tenant-a:2e168312-56a5-423e-92d1-816863d16a7d" {
		t.Errorf("metadata session id was not unwrapped and namespaced: %q", ma)
	}
}

// A client-supplied id becomes a store key, a session_id in DELETE statements and —
// hosted — a path component of a cold-storage object. Anything outside the
// conservative charset must never reach any of them; it falls back to the derived
// hash instead (see Scoped's comment on why fall back rather than reject).
func TestScopedRejectsUnsafeExplicitIDs(t *testing.T) {
	hash := Scoped("t1", "", "sys", "user") // what the fallback must produce
	for name, bad := range map[string]string{
		"traversal":       "../../../../../backup/cg-control-20260814T031700Z.db",
		"relative":        "../x",
		"bare slash":      "/",
		"absolute path":   "/etc/passwd",
		"backslash":       `..\..\x`,
		"null byte":       "a\x00b",
		"newline":         "a\nb",
		"space":           "a b",
		"unicode":         "sessiön-ü",
		"percent encoded": "%2e%2e%2fx",
		// Inside the charset, and the two names a path interprets.
		"dot":           ".",
		"dotdot":        "..",
		"all dots":      "....",
		"empty":         "",
		"blank":         "   ",
		"too long":      strings.Repeat("a", 4096),
		"just over cap": strings.Repeat("a", MaxExplicitLen+1),
	} {
		got := Scoped("t1", bad, "sys", "user")
		if got != hash {
			t.Errorf("%s: Scoped(%.20q…) = %q, want the derived-hash fallback %q", name, bad, got, hash)
		}
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
			t.Errorf("%s: key %q can escape a path", name, got)
		}
	}
	// And the conservative charset still passes through untouched, at the cap.
	for _, ok := range []string{"sess-e2e", "a.b_c:d-1", "ABC123", strings.Repeat("a", MaxExplicitLen)} {
		if got := Scoped("t1", ok, "sys", "user"); got != "t1:"+ok {
			t.Errorf("a legitimate id was rewritten: Scoped(%q) = %q", ok, got)
		}
	}
}

// Single-tenant keys must be byte-identical to before scoping existed, or every
// deployment's frozen decisions are orphaned on upgrade.
func TestScopedEmptyTenantMatchesResolve(t *testing.T) {
	for _, c := range [][3]string{{"", "sys", "user"}, {"explicit", "sys", "user"}, {"", "", ""}} {
		if got, want := Scoped("", c[0], c[1], c[2]), Resolve(c[0], c[1], c[2]); got != want {
			t.Errorf("Scoped(\"\", %q…) = %q, want %q", c[0], got, want)
		}
	}
	if got := Scoped("  ", "x", "", ""); got != "x" {
		t.Errorf("blank tenant added a namespace: %q", got)
	}
}
