package record

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testIdentity is the person every test in this package records as, so no result depends on
// who runs the tests or what their git config says.
var testIdentity = Identity{
	Usernames: []string{"DLEVI7"},
	Names:     []string{"Dana Levi"},
	Emails:    []string{"dana.levi@example.org"},
	Terms:     []string{"dlevi-gh"},
}

func TestMain(m *testing.M) {
	SetIdentity(testIdentity)
	os.Exit(m.Run())
}

// TestIdentityRemoved pins each identity rule, ported from claude-record's redactor.py, on
// the shape it was measured to leak in a real recording.
func TestIdentityRemoved(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"windows home path", `C:\Users\DLEVI7\src\app`, `C:\Users\` + RedactedUsername + `\src\app`},
		{"mangled project dir", "projects/C--Users-dlevi7-work", "projects/C--Users-" + RedactedUsername + "-work"},
		{"full name", "Author: Dana Levi", "Author: " + RedactedName},
		{"login form", "branch dana-levi/fix", "branch " + RedactedName + "/fix"},
		{"last name any case", "LEVI and levi", RedactedName + " and " + RedactedName},
		{"first name capitalized", "Hi Dana,", "Hi " + RedactedName + ","},
		{"after escaped newline", `asked of\nDana, who`, `asked of\n` + RedactedName + `, who`},
		{"git email", "<dana.levi@example.org>", "<" + RedactedEmail + ">"},
		{"other email", "cc bob@corp.example.com", "cc " + RedactedEmail},
		{"repo url", "https://github.ibm.com/dlevi/secret-proj/pull/3",
			"https://github.ibm.com/" + RedactedOwner + "/" + RedactedRepo + "/pull/3"},
		{"ssh remote", "git@github.com:dlevi/app.git", "git@github.com:" + RedactedOwner + "/" + RedactedRepo + ".git"},
		{"profile url", `"url":"https://github.ibm.com/DLEVI"`, `"url":"https://github.ibm.com/` + RedactedOwner + `"`},
		{"extra term", "@dlevi-gh approved", "@" + RedactedTerm + " approved"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Content(c.in); got != c.want {
				t.Errorf("\n in: %s\ngot: %s\nwant: %s", c.in, got, c.want)
			}
		})
	}
}

// TestIdentityKeeps is the negative half: what looks like identity but names no one, and
// the whole-word rule that keeps a name inside a longer word.
func TestIdentityKeeps(t *testing.T) {
	for _, in := range []string{
		"dana is lowercase, so it may be a word", // first names match only Capitalized/UPPER
		"Levity and Danache",                     // not whole words
		"noreply@github.com and git@github.com",
		"https://github.com/anthropics/claude-code/issues",
		"https://github.com/settings/tokens",
		"see https://github.com/ for more",
		"notgithub.com/x/y",
	} {
		if got := Content(in); got != in {
			t.Errorf("changed text that names no one\n in: %s\nout: %s", in, got)
		}
	}
}

// TestIdentityKeepsJSONValid: identity is removed per decoded string by ContentJSON like any
// credential, so a body stays valid JSON and an escaped Windows path is still caught.
func TestIdentityKeepsJSONValid(t *testing.T) {
	in := `[{"role":"user","parts":[{"type":"text","content":"cd C:\\Users\\DLEVI7\\src && git log --author=\"Dana Levi\""}]}]`
	got := ContentJSON(in)
	if strings.Contains(got, "DLEVI7") || strings.Contains(got, "Levi") {
		t.Errorf("identity survived: %s", got)
	}
	if ContentJSON(got) != got {
		t.Errorf("not idempotent: %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("invalid JSON after redaction: %s", got)
	}
}

// TestShortAndGenericNamesSkipped: a 2-letter login or "admin" would hit ordinary text.
func TestShortAndGenericNamesSkipped(t *testing.T) {
	r := compileIdentity(Identity{Usernames: []string{"ab", "admin"}})
	if r.usernames != nil {
		t.Errorf("short or generic usernames compiled into a pattern: %v", r.usernames)
	}
}

// A first name whose first letter is multi-byte is still matched Capitalized.
func TestIdentityMultibyteFirstName(t *testing.T) {
	r := compileIdentity(Identity{Names: []string{"Élodie Dupont"}})
	if r.casedNames == nil || !r.casedNames.MatchString("Hi Élodie,") {
		t.Errorf("Capitalized multi-byte first name not matched: %v", r.casedNames)
	}
}
