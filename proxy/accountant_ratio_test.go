package proxy

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
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
