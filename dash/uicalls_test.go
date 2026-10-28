package dash

import (
	"regexp"
	"strings"
	"testing"
)

// The dashboard must use the server's own test of "never used" (SessionsUsed == 0 && Calls == 0).
// It had its own: `!t.sessions_used`. Once Calls counts every call in scope, a tool called in a
// session that did not declare it has sessions_used 0 and calls > 0, and that rule still showed it
// as removable: a "remove" card, a command to paste, a "never" pill, and an Overview top-5 entry.
//
// So the rule lives in one helper, neverCalled, and nothing in the UI tests sessions_used alone.
func TestTheDashboardUsesOneNeverCalledRule(t *testing.T) {
	app := stripComments(readUI(t, "ui/app.js"))
	def := regexp.MustCompile(`function neverCalled\(t\)\s*\{\s*return\s+([^;]+);`).FindStringSubmatch(app)
	if def == nil {
		t.Fatal("app.js defines no neverCalled(t)")
	}
	if !strings.Contains(def[1], "sessions_used") || !strings.Contains(def[1], "calls") {
		t.Errorf("neverCalled(t) is %q; it must test both sessions_used and calls", def[1])
	}
	// The JS falsy tests of sessions_used that decide "unused": filters, the row class and the pill.
	bare := regexp.MustCompile(`!t\.sessions_used\b|t\.sessions_used === 0|t\.sessions_used \?`)
	for _, f := range []string{"ui/app.js", "ui/tools.js"} {
		for _, line := range strings.Split(stripComments(readUI(t, f)), "\n") {
			if bare.MatchString(line) && !strings.Contains(line, "function neverCalled(") {
				t.Errorf("%s decides \"unused\" from sessions_used alone: %s", f, strings.TrimSpace(line))
			}
		}
	}
}
