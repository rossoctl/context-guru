package tooldefer

import "testing"

type fixedHist []Session

func (h fixedHist) UserSessions(_, _ string, n int) []Session {
	if len(h) < n {
		return h
	}
	return h[:n]
}
func (fixedHist) ProjectUsed(_, _, _, _ string, _ int) bool { return false }

func TestExplain(t *testing.T) {
	sess := func(decl, used []string) Session {
		s := Session{Declared: map[string]bool{}, Used: map[string]bool{}}
		for _, n := range decl {
			s.Declared[n] = true
		}
		for _, n := range used {
			s.Used[n] = true
		}
		return s
	}
	all := []string{"Monitor", "WebFetch", "mcp__pw__click", "mcp__pw__type"}
	h := fixedHist{sess(all, nil), sess(all, []string{"WebFetch"}), sess(all, []string{"mcp__pw__type"})}
	for _, c := range []struct {
		name string
		keep []string
		want bool
	}{
		{"Monitor", nil, true},
		{"WebFetch", nil, false},
		{"mcp__pw", nil, false},        // a tool of the server was used
		{"mcp__pw__click", nil, false}, // a server is one unit
		{"Monitor", []string{"Monitor"}, false},
		{"Bash", nil, false},
		{"Gone", nil, false}, // not declared
	} {
		got, why := Explain(h, "", c.name, c.keep)
		if got != c.want || why == "" {
			t.Errorf("Explain(%s, keep %v) = %v %q, want %v", c.name, c.keep, got, why, c.want)
		}
	}
	if got, _ := Explain(h[:2], "", "Monitor", nil); got {
		t.Error("with 2 sessions of history Explain defers; the rule needs 3")
	}
	if got, _ := Explain(nil, "", "Monitor", nil); got {
		t.Error("with no history Explain defers")
	}
}
