package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

// replayDigests runs one fixed request set through the tracker and returns a digest of each booked Result.
// The set has two threads, a replayed change, a client edit that ends a change, a change on a second thread,
// and a save and restore between the two halves (a restart). The digests were taken on the tracker before it
// moved into accrec, so a move that changes a booked figure changes a digest.
func replayDigests(t *testing.T, dir string) []string {
	t.Helper()
	big, nBig := msg("user", strings.Repeat("tool result ", 400))
	small, nSmall := msg("user", "[masked]")
	m0, _ := msg("user", "first")
	m2, _ := msg("assistant", "answer")
	next := mustMsg("user", "next")
	m0b, _ := msg("user", "first, edited")
	main, side := accountant.Thread{ID: "main"}, accountant.Thread{ID: "side"}
	type step struct {
		th          accountant.Thread
		client, fwd []byte
		u           Usage
		ops         []accountant.Operation
	}
	op := rewriteOp("cg:mask:a1", 1, nBig, nSmall)
	steps := []step{
		{main, body(m0, big, m2), body(m0, small, m2), Usage{FreshInput: 10, CacheWrite: 300}, []accountant.Operation{op}},
		{main, body(m0, big, m2, next), body(m0, small, m2, next), Usage{FreshInput: 10, CacheRead: 300, CacheWrite: 20}, []accountant.Operation{op}},
		{side, body(m0, big, m2), body(m0, small, m2), Usage{FreshInput: 10, CacheWrite: 300}, []accountant.Operation{rewriteOp("cg:mask:b1", 1, nBig, nSmall)}},
		{main, body(m0b, big, m2), body(m0b, big, m2), Usage{FreshInput: 400, CacheWrite: 100}, nil},
		{main, body(m0b, big, m2), body(m0b, small, m2), Usage{FreshInput: 10, CacheRead: 100, CacheWrite: 20}, []accountant.Operation{op}},
		{side, body(m0, big, m2, next), body(m0, small, m2, next), Usage{FreshInput: 10, CacheRead: 300, CacheWrite: 20, CacheWrite1h: 20}, nil},
	}
	var out []string
	s := newSavingsTracker()
	path := filepath.Join(dir, "state.json")
	for i, st := range steps {
		if i == 3 { // a restart: the state goes to the file and a new tracker reads it back
			if err := s.save(path); err != nil {
				t.Fatal(err)
			}
			s = newSavingsTracker()
			if !s.restore(path, time.Now()) {
				t.Fatal("state not restored")
			}
		}
		meta := accountant.RequestMeta{RequestID: fmt.Sprint("r", i), Session: "s", At: time.Unix(int64(1000+i*10), 0),
			Billing: accountant.Billing{Mode: accountant.BillingAPI}}
		r := s.begin(meta, st.client)
		s.end(r, st.th, st.fwd, st.ops, accFacts)
		res := s.compute(r, st.u)
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%x", sha256.Sum256(raw))[:16])
	}
	return out
}

// The booked figures of a fixed request set do not change when the tracker moves.
func TestReplayBooksTheSameFigures(t *testing.T) {
	got := strings.Join(replayDigests(t, t.TempDir()), " ")
	t.Log("digests:", got)
	// Accountant v0.10.0 (#121) adds the field Check.Claims to the marshaled Result. Cases 0, 1, 2 and 5 carry a
	// ratio_range check, so their digests changed. No booked figure changed.
	const want = "a768dfcc8394d0db ad6f6258f2136947 5a5b01339873476e a1ca4676152f8123 1cf6bfca006c491d f47c376586834631"
	if got != want {
		t.Fatalf("booked figures changed:\n got  %s\n want %s", got, want)
	}
}
