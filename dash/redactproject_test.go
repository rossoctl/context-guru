package dash

import (
	"strings"
	"testing"
)

// ProjectPath is the trust boundary for the project dimension.
//
// A launch directory is a value the CLIENT chose, so it is attacker-influenced in exactly the way
// Meta.redact() already documents: a request whose working directory is named after a token would
// otherwise write that token to the requests table and to the SSE feed, permanently. The existing
// metaEnum() guard cannot be reused directly — it demands a short identifier, and a filesystem
// path is neither short nor an identifier — so this is the same composition (credential scan plus
// a shape check) against a different shape.
//
// An allowlist is impossible here: nobody can enumerate the directories a user may work in. So
// the shape check is structural, and anything failing it is reported as redacted rather than
// stored, for the reason metaEnum states — a value silently dropped is indistinguishable from a
// dimension that stopped working.
func TestProjectPathRefusesWhatMustNotReachTheDatabase(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"an ordinary project", "/Users/v/OpenSource/context-guru", "/Users/v/OpenSource/context-guru"},
		{"the filesystem root", "/", "/"},
		{"empty stays empty, meaning unattributed", "", ""},
		{"a path with a dash and a dot", "/Users/v/.config/my-app", "/Users/v/.config/my-app"},

		// THE REASON THIS FUNCTION EXISTS. RedactContent catches credential shapes; a directory
		// named after one must not be stored verbatim.
		{"an OpenAI-shaped key in a directory name",
			"/tmp/sk-proj-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH", Redacted},
		{"an AWS-shaped key in a directory name",
			"/home/AKIAIOSFODNN7EXAMPLE/work", Redacted},

		// Control characters would corrupt a log line and a terminal.
		{"a newline is refused", "/Users/v/proj\nPrimary working directory: /evil", Redacted},
		{"a tab is refused", "/Users/v/\tproj", Redacted},

		// Not absolute: the extractor should never emit one, and this is the second line of
		// defence rather than a duplicate of the first.
		{"a relative path is refused", "Users/v/proj", Redacted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProjectPath(tc.in); got != tc.want {
				t.Errorf("ProjectPath(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The length cap is a storage property: this value lands in a TEXT column and on an event feed,
// and a client may send a path of any length.
func TestProjectPathCapsItsLength(t *testing.T) {
	long := "/" + strings.Repeat("deep/", 300)
	got := ProjectPath(long)
	if got == long {
		t.Errorf("a %d-character path was stored verbatim", len(long))
	}
	if got != Redacted && len(got) > 512 {
		t.Errorf("result is %d characters; want it refused or capped", len(got))
	}
}

// The canary test the project's own rules ask for: drive a credential through the function and
// assert it is absent from the result, rather than asserting on a shape that happens to differ.
func TestNoCredentialSurvivesProjectPath(t *testing.T) {
	const canary = "sk-ant-api03-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	got := ProjectPath("/Users/v/" + canary + "/work")
	if strings.Contains(got, canary) {
		t.Fatalf("the canary reached the output: %q", got)
	}
}

func TestProjectPathRefusesDotDotSegments(t *testing.T) {
	if got := ProjectPath("/a/../../etc"); got != Redacted {
		t.Errorf("ProjectPath with .. = %q; want %q", got, Redacted)
	}
}
