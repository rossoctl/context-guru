package proxy

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

func rewriteOp(change string, at, before, after int) cf.Operation {
	return cf.Operation{Kind: cf.KindRewrite, Operator: "context-guru.mask", ChangeID: change,
		At:         cf.Position{Section: cf.SectionMessages, Index: at},
		SizeBefore: cf.Size{Bytes: before}, SizeAfter: cf.Size{Bytes: after}}
}

// msg builds one message element and its byte length as hashed.
func msg(role, text string) (string, int) {
	s := fmt.Sprintf(`{"role":%q,"content":%q}`, role, text)
	return s, len(s)
}

func body(msgs ...string) []byte { return []byte(`{"messages":[` + strings.Join(msgs, ",") + `]}`) }

var cfFacts = cf.EntryFacts{TTL: 5 * time.Minute, Lifetime: cf.LifetimeExact}

type cfRun struct {
	s  *savingsTracker
	th cf.Thread
}

func (c cfRun) do(n int, client, fwd []byte, usage Usage, ops ...cf.Operation) cf.Result {
	meta := cf.RequestMeta{RequestID: fmt.Sprint("r", n), Session: "s", At: time.Unix(int64(1000+n*10), 0), Billing: cf.BillingFrom(cf.BillingAPI)}
	r := c.s.begin(meta, client)
	c.s.end(r, c.th, fwd, ops, cfFacts)
	return c.s.compute(r, usage)
}

func countChange(res cf.Result, id string) (n int, saved int) {
	for _, o := range res.Ops {
		if o.ChangeID == id {
			n++
			saved += o.Saved.Input + o.Saved.CacheWrite5m + o.Saved.CacheWrite1h
		}
	}
	return
}

func TestSavings_ReplayedChangeIsPricedOnce(t *testing.T) {
	c := cfRun{newSavingsTracker(), cf.Thread{ID: "main"}}
	big, nBig := msg("user", strings.Repeat("tool result ", 400))
	small, nSmall := msg("user", "[masked]")
	m0, _ := msg("user", "first")
	m2, _ := msg("assistant", "answer")
	client, fwd := body(m0, big, m2), body(m0, small, m2)
	usage := Usage{FreshInput: 100, CacheRead: 1000}
	const id = "cg:mask:a1"

	// (a) first request: reported and priced.
	res := c.do(1, client, fwd, usage, rewriteOp(id, 1, nBig, nSmall))
	if len(res.Checks) != 0 {
		t.Fatalf("checks = %+v", res.Checks)
	}
	if n, _ := countChange(res, id); n != 1 {
		t.Fatalf("first request priced %d times, want 1", n)
	}
	if !c.s.trackers.IsActive("s", c.th, id) {
		t.Fatal("change must stay active after its first request")
	}

	// (b) the change is reported again while active: it is added, and the library prices it once.
	client2 := body(m0, big, m2, mustMsg("user", "next"))
	fwd2 := body(m0, small, m2, mustMsg("user", "next"))
	req := c.s.begin(cf.RequestMeta{RequestID: "r2", Session: "s", At: time.Unix(1020, 0), Billing: cf.BillingFrom(cf.BillingAPI)}, client2)
	c.s.end(req, c.th, fwd2, []cf.Operation{rewriteOp(id, 1, nBig, nSmall)}, cfFacts)
	if len(req.Ops) != 1 {
		t.Fatalf("replayed change must be added, req.Ops = %d", len(req.Ops))
	}
	res = c.s.compute(req, usage)
	if len(res.Checks) != 0 {
		t.Fatalf("checks = %+v (unique_change must not fire)", res.Checks)
	}
	if n, _ := countChange(res, id); n != 1 {
		t.Fatalf("replayed change priced %d times, want 1", n)
	}
	once := 0
	for _, o := range res.Ops {
		if o.ChangeID == id {
			once++
		}
	}
	if once != 1 {
		t.Fatalf("res.Ops holds the change %d times, want 1", once)
	}
}

func mustMsg(role, text string) string { s, _ := msg(role, text); return s }

func TestSavings_ReReportsAfterChangeEnded(t *testing.T) {
	c := cfRun{newSavingsTracker(), cf.Thread{ID: "main"}}
	big, nBig := msg("user", strings.Repeat("tool result ", 400))
	small, nSmall := msg("user", "[masked]")
	m0, _ := msg("user", "first")
	m2, _ := msg("assistant", "answer")
	usage := Usage{FreshInput: 100, CacheRead: 1000}
	const id = "cg:mask:a1"

	c.do(1, body(m0, big, m2), body(m0, small, m2), usage, rewriteOp(id, 1, nBig, nSmall))
	if !c.s.trackers.IsActive("s", c.th, id) {
		t.Fatal("expected active after request 1")
	}

	// The client edits message 0 (at or before the change): the library ends the change.
	m0b, _ := msg("user", "first, edited")
	c.do(2, body(m0b, big, m2), body(m0b, big, m2), usage)
	if c.s.trackers.IsActive("s", c.th, id) {
		t.Fatal("change must end after a client edit at or before its position")
	}

	// Request 3 reports the same ChangeID again: it is added and priced (no filter).
	req := c.s.begin(cf.RequestMeta{RequestID: "r3", Session: "s", At: time.Unix(1030, 0), Billing: cf.BillingFrom(cf.BillingAPI)}, body(m0b, big, m2))
	c.s.end(req, c.th, body(m0b, small, m2), []cf.Operation{rewriteOp(id, 1, nBig, nSmall)}, cfFacts)
	if len(req.Ops) != 1 {
		t.Fatalf("ended change must be re-added, req.Ops = %d", len(req.Ops))
	}
	res := c.s.compute(req, usage)
	if len(res.Checks) != 0 {
		t.Fatalf("checks = %+v", res.Checks)
	}
	n, saved := countChange(res, id)
	if n != 1 || saved == 0 {
		t.Fatalf("re-reported change: priced %d times, saved %d tokens; want 1 and non-zero", n, saved)
	}
}

func TestSavings_DryRunAndNonRewriteAlwaysAdded(t *testing.T) {
	c := cfRun{newSavingsTracker(), cf.Thread{ID: "main"}}
	big, nBig := msg("user", strings.Repeat("x", 2000))
	small, nSmall := msg("user", "y")
	usage := Usage{FreshInput: 10}
	c.do(1, body(big), body(small), usage, rewriteOp("c1", 0, nBig, nSmall))
	dry := rewriteOp("c1", 0, nBig, nSmall)
	dry.DryRun = true
	spend := cf.Operation{Kind: cf.KindSpend, ChangeID: "c1"}
	req := c.s.begin(cf.RequestMeta{RequestID: "r2", Session: "s", At: time.Unix(1020, 0)}, body(big))
	c.s.end(req, c.th, body(big), []cf.Operation{dry, spend}, cfFacts)
	if len(req.Ops) != 2 {
		t.Fatalf("req.Ops = %d, want 2", len(req.Ops))
	}
}

func TestSavings_ConcurrentRequests(t *testing.T) {
	s := newSavingsTracker()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fresh, recomputed int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := body(mustMsg("user", "x"))
			r := s.begin(cf.RequestMeta{RequestID: fmt.Sprint("r", i), Session: "s", At: time.Now(), Billing: cf.BillingFrom(cf.BillingAPI)}, b)
			s.end(r, cf.Thread{ID: "main"}, b, nil, cfFacts)
			res := s.compute(r, Usage{FreshInput: 5})
			mu.Lock()
			defer mu.Unlock()
			if res.Recomputed {
				recomputed++
			} else {
				fresh++
			}
		}(i)
	}
	wg.Wait()
	if fresh < 1 {
		t.Fatalf("fresh = %d, recomputed = %d; want at least one non-recomputed result", fresh, recomputed)
	}
}
