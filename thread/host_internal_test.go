package thread

import "testing"

func TestHeaderAgentID(t *testing.T) {
	for _, tc := range []struct {
		in     string
		wantID string
		wantOK bool
	}{
		{"", "", true},
		{"   ", "", true},
		{" agentS ", "agentS", true},
		{"worker@team", "worker@team", true},
		{"bad id!", "", false},
		{"\x01unset", "", false},
	} {
		if id, ok := HeaderAgentID(tc.in); id != tc.wantID || ok != tc.wantOK {
			t.Errorf("HeaderAgentID(%q) = %q, %v; want %q, %v", tc.in, id, ok, tc.wantID, tc.wantOK)
		}
	}
}

// ResolveSession is the proxy's rule: an empty session gives the session itself even with a
// header, a nil tracker gives it too, and a panic fails open to it with an error.
func TestResolveSessionFailsOpenToTheSession(t *testing.T) {
	session := Result{ID: Primary, Source: SourceSession}
	if res, err := testTracker().ResolveSession("t", "", "agentS", conv("sub"), false); res != session || err != nil {
		t.Fatalf("empty session: %+v, %v", res, err)
	}
	var nilTracker *Tracker
	if res, err := nilTracker.ResolveSession("t", "s", "agentS", conv("sub"), false); res != session || err != nil {
		t.Fatalf("nil tracker: %+v, %v", res, err)
	}
	// The zero Tracker has no clock, so Resolve panics on it.
	if res, err := (&Tracker{}).ResolveSession("t", "s", "", conv("a"), false); res != session || err == nil {
		t.Fatalf("panic: %+v, %v; want the session and an error", res, err)
	}
	// The scope is tenant + "\x00" + session, the same key Resolve is called with directly.
	tr := testTracker()
	tr.Resolve("t\x00s", "", conv("task", "a1", "u2"), false)
	if res, err := tr.ResolveSession("t", "s", "agentS", conv("sub"), false); res.ID != AgentThread("agentS") || err != nil {
		t.Fatalf("header thread: %+v, %v", res, err)
	}
	if _, ok := tr.sessions["t\x00s"]; !ok || len(tr.sessions) != 1 {
		t.Fatalf("ResolveSession did not use the scope tenant+NUL+session: %d sessions", len(tr.sessions))
	}
}
