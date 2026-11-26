package proxy

import (
	"testing"

	"github.com/rossoctl/context-guru/thread"
)

// #947 review: a result the thread tracker could not judge (no session id, no message list, a
// recovered panic) has Source "session". Its row must store no verdict (NULL), so the replay cap keeps
// the drop detector for it. A judged result stores its verdict, "" included.
func TestNoteThreadStoresAVerdictOnlyWhenTheTrackerJudged(t *testing.T) {
	for _, c := range []struct {
		res       thread.Result
		known     bool
		wantValue string
	}{
		{thread.Result{ID: thread.Primary, Source: thread.SourceSession}, false, ""},
		{thread.Result{ID: thread.Primary, Source: thread.SourcePrefix}, true, ""},
		{thread.Result{ID: "a:x", Source: thread.SourceHeader}, true, ""},
		{thread.Result{ID: thread.Primary, Source: thread.SourcePrefix, AfterCompaction: true}, true, "unknown"},
	} {
		cp := &capture{}
		cp.noteThread(c.res)
		if cp.compKnown != c.known || cp.compVerdict != c.wantValue {
			t.Errorf("%+v: known=%v verdict=%q, want %v %q", c.res, cp.compKnown, cp.compVerdict, c.known, c.wantValue)
		}
	}
}
