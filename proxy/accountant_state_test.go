package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

// twoTurns runs turn 1 (the change is made and reported) on a tracker, saves it, and returns the bodies
// of turn 2, on which the change is not reported again: the library carries it from the thread state.
func twoTurns(t *testing.T, path string) (turn2c, turn2f []byte, id string) {
	t.Helper()
	id = "cg:mask:state"
	big, nBig := msg("user", strings.Repeat("tool result ", 400))
	small, nSmall := msg("user", "[masked]")
	m0, m2 := mustMsg("user", "first"), mustMsg("assistant", "answer")
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	c.do(1, body(m0, big, m2), body(m0, small, m2), Usage{FreshInput: 10, CacheRead: 100}, rewriteOp(id, 1, nBig, nSmall))
	if err := c.s.save(path); err != nil {
		t.Fatal(err)
	}
	next := mustMsg("user", "next")
	return body(m0, big, m2, next), body(m0, small, m2, next), id
}

func turn2(s *savingsTracker, c, f []byte) accountant.Result {
	meta := accountant.RequestMeta{RequestID: "r2", Session: "s", At: time.Unix(1020, 0), Billing: accountant.Billing{Mode: accountant.BillingAPI}}
	r := s.begin(meta, c)
	s.end(r, accountant.Thread{ID: "main"}, f, nil, accFacts)
	return s.compute(r, Usage{FreshInput: 10, CacheRead: 100})
}

// A restart keeps the active change of a live session: the next request still counts it. Without the restore the
// change is gone (context-guru#768, major 2).
func TestSavingsStateSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	c, f, id := twoTurns(t, path)

	cold := turn2(newSavingsTracker(), c, f)
	if n, _ := countChange(cold, id); n != 0 {
		t.Fatalf("precondition: a fresh tracker priced the change %d times, want 0", n)
	}
	s := newSavingsTracker()
	if !s.restore(path, time.Now()) {
		t.Fatal("restore reported nothing restored")
	}
	if n, saved := countChange(turn2(s, c, f), id); n != 1 || saved == 0 {
		t.Fatalf("after the restore the change was priced %d times for %d tokens, want 1 and non-zero", n, saved)
	}
}

// A missing, stale or corrupt file starts the proxy empty. None of them may fail the start.
func TestSavingsStateFailsOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if newSavingsTracker().restore(path, time.Now()) {
		t.Error("a missing file was restored")
	}
	twoTurns(t, path)
	if newSavingsTracker().restore(path, time.Now().Add(accStateMaxAge+time.Minute)) {
		t.Error("a file older than any cache entry was restored")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if newSavingsTracker().restore(path, time.Now()) {
		t.Error("a corrupt file was restored")
	}
}

// Close saves the state, and the file is private: it holds session ids.
func TestHandlerCloseSavesTheState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	up := fakeUpstream(t)
	defer up.Close()
	h, _, _ := accHandlerOpts(t, up.URL, "pipeline: [extract]\n", func(o *Options) { o.AccountantState = path })
	h.Close()
	h.Close() // the test cleanup closes it again; that must be safe
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Close saved no state file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %v, want 0600", fi.Mode().Perm())
	}
}

// begin keeps the client body's hash for end, so a request forwarded unchanged does not hash 600 KB twice. The
// reuse must not change the answer, and the entry must not outlive the request.
func TestSavingsEndReusesTheClientHashOnlyForIdenticalBytes(t *testing.T) {
	s := newSavingsTracker()
	client := benchBody()
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s", At: time.Unix(1000, 0), Billing: accountant.Billing{Mode: accountant.BillingAPI}}
	held := func() (n int) { s.clients.Range(func(any, any) bool { n++; return true }); return }

	r := s.begin(meta, client)
	if held() != 1 {
		t.Fatalf("begin kept %d client hashes, want 1", held())
	}
	s.end(r, accountant.Thread{ID: "main"}, client, nil, accFacts)
	if held() != 0 {
		t.Fatalf("end left %d client hashes behind", held())
	}
	same := s.compute(r, Usage{FreshInput: 100, CacheWrite: 30000})

	// A changed body takes the other path, and the answer is the one a fresh hash gives.
	changed := []byte(strings.Replace(string(client), "word word", "wurd word", -1))
	r2 := s.begin(meta, client)
	s.end(r2, accountant.Thread{ID: "main"}, changed, nil, accFacts)
	if r2.Forwarded.Pos[len(r2.Forwarded.Pos)-1].Hash == r2.Client.Pos[len(r2.Client.Pos)-1].Hash {
		t.Fatal("a changed forwarded body reused the client hash")
	}
	if want := accBodies(client); fmt.Sprint(r.Forwarded) != fmt.Sprint(want) || len(same.Checks) > 1 {
		t.Fatalf("the reused hash differs from a fresh one")
	}
}
