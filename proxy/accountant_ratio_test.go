package proxy

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/lifecycle"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
)

// The request ratio the thread's next warm gate reads is the ratio the accountant prices the request with, built
// before the request is priced (#1533). A request with nothing to keep gives 0, 0.
func TestLiveRatioTextIsTheRatioOfThePricedRequest(t *testing.T) {
	s := newSavingsTracker()
	client := benchBody()
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s", At: time.Unix(1000, 0), Billing: accountant.Billing{Mode: accountant.BillingAPI}}
	req := s.begin(meta, client)
	s.end(req, accountant.Thread{ID: "main"}, client, nil, accFacts)
	c := &capture{savings: s, accReq: req}
	u := Usage{FreshInput: 100, CacheWrite: int64(len(client)) / 4} // a quarter token a byte: inside check 9

	tok, by := c.liveRatioText(u, true)
	if tok <= 0 || by <= 0 {
		t.Fatalf("liveRatioText = %d, %d, want both above 0", tok, by)
	}
	res := s.compute(req, u)
	if res.Ratio == nil {
		t.Fatalf("Result.Ratio is nil (checks %+v)", res.Checks)
	}
	if wt, wb := res.Ratio.Text(); wt != tok || wb != by {
		t.Fatalf("liveRatioText = %d over %d bytes, the priced Result's ratio = %d over %d", tok, by, wt, wb)
	}
	for name, c := range map[string]*capture{"nil capture": nil, "no record": {savings: s}, "no savings": {accReq: req}} {
		if tok, by := c.liveRatioText(u, true); tok != 0 || by != 0 {
			t.Errorf("%s: %d, %d, want 0, 0", name, tok, by)
		}
	}
	if tok, by := c.liveRatioText(u, false); tok != 0 || by != 0 {
		t.Errorf("no usage: %d, %d, want 0, 0", tok, by)
	}
}

// A ratio that Compute refuses (check 9) is not kept: the next turn's gate keeps the table.
func TestLiveRatioTextSkipsARatioTheAccountantRefuses(t *testing.T) {
	s := newSavingsTracker()
	client := benchBody()
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s", At: time.Unix(1000, 0), Billing: accountant.Billing{Mode: accountant.BillingAPI}}
	req := s.begin(meta, client)
	s.end(req, accountant.Thread{ID: "main"}, client, nil, accFacts)
	c := &capture{savings: s, accReq: req}
	for name, u := range map[string]Usage{"too few tokens": {FreshInput: 3}, "too many": {CacheWrite: int64(len(client)) * 3}} {
		if tok, by := c.liveRatioText(u, true); tok != 0 || by != 0 {
			t.Errorf("%s: %d, %d, want 0, 0", name, tok, by)
		}
	}
}

// The real job path (#1533, review round 1): accEnd opens the record on its own goroutine, and the host reads the
// ratio BEFORE finish joins the job. liveRatioText must join it, or it reads no record and no key is ever written.
func TestTheRatioKeyIsWrittenThroughTheRealJobPath(t *testing.T) {
	c := asyncCapture()
	body := manyToolResultsRequest("claude-sonnet-5", 24)
	c.accBegin(asyncMeta(), body)
	c.accEnd(thread.Result{ID: thread.Primary}, body, nil)
	u := Usage{FreshInput: 100, CacheWrite: int64(len(body)) / 4}

	tok, by := c.liveRatioText(u, true) // the job is not joined yet: finish does that later
	if tok <= 0 || by <= 0 {
		t.Fatalf("liveRatioText = %d, %d through the real job path, want both above 0", tok, by)
	}
	st := store.NewMemory(store.Options{})
	lifecycle.Finish(lifecycle.Fin{Store: st, Session: "sess-async", ThreadID: "", UsageOK: true,
		Billed: u.FreshInput + u.CacheWrite, RatioTokens: tok, RatioBytes: by})
	if b, ok := st.Get(store.RatioThreadPrefix + thread.Key("sess-async", "")); !ok || len(b) == 0 {
		t.Fatal("no cg:ratio: key was written for the thread")
	}
}

// A request with expand rounds has the first round's body and the last round's usage: its ratio is too high, and
// the thread keeps none. A rejected request keeps none either.
func TestNoRatioIsKeptForSeveralRoundsOrARejectedStatus(t *testing.T) {
	c := asyncCapture()
	body := manyToolResultsRequest("claude-sonnet-5", 24)
	c.accBegin(asyncMeta(), body)
	c.accEnd(thread.Result{ID: thread.Primary}, body, nil)
	u := Usage{FreshInput: 100, CacheWrite: int64(len(body)) / 4}
	c.rounds = 3
	if tok, by := c.liveRatioText(u, true); tok != 0 || by != 0 {
		t.Fatalf("3 rounds kept %d, %d, want none", tok, by)
	}
	c.rounds, c.status = 1, 429
	if tok, by := c.liveRatioText(u, true); tok != 0 || by != 0 {
		t.Fatalf("a rejected status kept %d, %d, want none", tok, by)
	}
	c.status = 200
	if tok, by := c.liveRatioText(u, true); tok <= 0 || by <= 0 {
		t.Fatalf("one round with status 200 kept %d, %d, want a ratio", tok, by)
	}
}
