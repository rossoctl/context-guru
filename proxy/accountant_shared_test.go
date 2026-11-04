package proxy

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

func sharedBody(msgs ...string) accountant.Body {
	var m [][]byte
	for _, s := range msgs {
		m = append(m, []byte(s))
	}
	return accountant.NewBody(nil, [][]byte{[]byte("sys")}, m)
}

func bpAt(sec accountant.Section, i int) []accountant.Breakpoint {
	return []accountant.Breakpoint{{Pos: accountant.Position{Section: sec, Index: i}, TTL: "5m"}}
}

func TestSharedWarm(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	ttl := 5 * time.Minute
	sys0 := accountant.Position{Section: accountant.SectionSystem, Index: 0}
	msg1 := accountant.Position{Section: accountant.SectionMessages, Index: 1}
	for _, tc := range []struct {
		name    string
		sibAt   time.Duration
		sibBody accountant.Body
		sibBPs  []accountant.Breakpoint
		sibID   string
		want    *accountant.Position
	}{
		{"a sibling shares the system block", time.Minute, sharedBody("x"), bpAt(accountant.SectionSystem, 0), "sub", &sys0},
		{"a sibling shares two messages", time.Minute, sharedBody("a", "b", "z"), bpAt(accountant.SectionMessages, 1), "sub", &msg1},
		{"the sibling's mark is past the shared prefix", time.Minute, sharedBody("a", "z"), bpAt(accountant.SectionMessages, 1), "sub", nil},
		{"the sibling is older than the TTL", -time.Minute, sharedBody("a", "b"), bpAt(accountant.SectionMessages, 1), "sub", nil},
		{"the same thread is not a sibling", time.Minute, sharedBody("a", "b"), bpAt(accountant.SectionMessages, 1), "main", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSiblingStore()
			sibAt := t0.Add(tc.sibAt)
			if tc.sibAt < 0 {
				sibAt = t0.Add(-ttl - time.Minute)
			}
			_ = s.sharedWarm("s1", tc.sibID, sibAt, ttl, tc.sibBody, tc.sibBPs)
			at := t0.Add(2 * time.Minute)
			if tc.sibID == "main" {
				// the thread's own earlier request is replaced, not compared
				at = t0.Add(2 * time.Minute)
			}
			got := s.sharedWarm("s1", "main", at, ttl, sharedBody("a", "b", "c"), bpAt(accountant.SectionMessages, 2))
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("SharedWarm %+v, want nil", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("SharedWarm %v, want %+v", got, *tc.want)
			}
		})
	}
}

// Another session's thread is never a sibling.
func TestSharedWarmIsPerSession(t *testing.T) {
	s := newSiblingStore()
	t0 := time.Unix(1_700_000_000, 0)
	_ = s.sharedWarm("other", "sub", t0, 5*time.Minute, sharedBody("a"), bpAt(accountant.SectionSystem, 0))
	if got := s.sharedWarm("s1", "main", t0.Add(time.Minute), 5*time.Minute, sharedBody("a"), nil); got != nil {
		t.Fatalf("SharedWarm %+v across sessions", *got)
	}
}

// A session keeps at most siblingThreadsMax threads: the oldest goes first.
func TestSharedWarmBoundsThreadsPerSession(t *testing.T) {
	s := newSiblingStore()
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < siblingThreadsMax+10; i++ {
		s.sharedWarm("s1", fmt.Sprintf("t%03d", i), t0.Add(time.Duration(i)*time.Second), time.Hour, sharedBody("a"), nil)
	}
	ss := s.session("s1")
	if n := len(ss.threads); n != siblingThreadsMax {
		t.Fatalf("threads = %d, want %d", n, siblingThreadsMax)
	}
	if _, ok := ss.threads["t000"]; ok {
		t.Fatal("the oldest thread was kept")
	}
	if _, ok := ss.threads[fmt.Sprintf("t%03d", siblingThreadsMax+9)]; !ok {
		t.Fatal("the newest thread was dropped")
	}
}

// The store keeps at most siblingsMax sessions and drops the least recently used.
func TestSharedWarmBoundsSessions(t *testing.T) {
	s := newSiblingStore()
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < siblingsMax; i++ {
		s.sharedWarm(fmt.Sprintf("s%d", i), "main", t0, time.Hour, sharedBody("a"), nil)
	}
	s.sharedWarm("s0", "main", t0, time.Hour, sharedBody("a"), nil) // s0 is now the most recent
	s.sharedWarm("new", "main", t0, time.Hour, sharedBody("a"), nil)
	if len(s.m) != siblingsMax {
		t.Fatalf("sessions = %d, want %d", len(s.m), siblingsMax)
	}
	if _, ok := s.m["s0"]; !ok {
		t.Fatal("the recently used session was dropped")
	}
	if _, ok := s.m["s1"]; ok {
		t.Fatal("the least recently used session was kept")
	}
}
