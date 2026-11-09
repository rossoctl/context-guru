package proxy

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

func rewriteOp(change string, at, before, after int) accountant.Operation {
	return accountant.Operation{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: change,
		At:         accountant.Position{Section: accountant.SectionMessages, Index: at},
		SizeBefore: accountant.Size{Bytes: before}, SizeAfter: accountant.Size{Bytes: after}}
}

// msg builds one message element and its byte length as hashed.
func msg(role, text string) (string, int) {
	s := fmt.Sprintf(`{"role":%q,"content":%q}`, role, text)
	return s, len(s)
}

func body(msgs ...string) []byte { return []byte(`{"messages":[` + strings.Join(msgs, ",") + `]}`) }

var accFacts = accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeExact}

type accRun struct {
	s  *savingsTracker
	th accountant.Thread
}

func (c accRun) do(n int, client, fwd []byte, usage Usage, ops ...accountant.Operation) accountant.Result {
	meta := accountant.RequestMeta{RequestID: fmt.Sprint("r", n), Session: "s", At: time.Unix(int64(1000+n*10), 0), Billing: accountant.Billing{Mode: accountant.BillingAPI}}
	r := c.s.begin(meta, client)
	c.s.end(r, c.th, fwd, ops, accFacts)
	return c.s.compute(r, usage)
}

func countChange(res accountant.Result, id string) (n int, saved int) {
	for _, o := range res.Ops {
		if o.ChangeID == id {
			n++
			saved += o.Saved.Input + o.Saved.CacheWrite5m + o.Saved.CacheWrite1h + o.Saved.CacheRead
		}
	}
	return
}

func TestSavings_ReplayedChangeIsPricedOnce(t *testing.T) {
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	big, nBig := msg("user", strings.Repeat("tool result ", 400))
	small, nSmall := msg("user", "[masked]")
	m0, _ := msg("user", "first")
	m2, _ := msg("assistant", "answer")
	client, fwd := body(m0, big, m2), body(m0, small, m2)
	usage := Usage{FreshInput: 10, CacheRead: 100}
	const id = "cg:mask:a1"

	// (a) first request: reported and priced.
	res := c.do(1, client, fwd, usage, rewriteOp(id, 1, nBig, nSmall))
	if len(res.Checks) != 0 {
		t.Fatalf("checks = %+v", res.Checks)
	}
	if n, _ := countChange(res, id); n != 1 {
		t.Fatalf("first request priced %d times, want 1", n)
	}

	// (b) the change is reported again while active: it is added, and the library prices it once.
	client2 := body(m0, big, m2, mustMsg("user", "next"))
	fwd2 := body(m0, small, m2, mustMsg("user", "next"))
	req := c.s.begin(accountant.RequestMeta{RequestID: "r2", Session: "s", At: time.Unix(1020, 0), Billing: accountant.Billing{Mode: accountant.BillingAPI}}, client2)
	c.s.end(req, c.th, fwd2, []accountant.Operation{rewriteOp(id, 1, nBig, nSmall)}, accFacts)
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
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	big, nBig := msg("user", strings.Repeat("tool result ", 400))
	small, nSmall := msg("user", "[masked]")
	m0, _ := msg("user", "first")
	m2, _ := msg("assistant", "answer")
	usage := Usage{FreshInput: 10, CacheRead: 100}
	const id = "cg:mask:a1"

	c.do(1, body(m0, big, m2), body(m0, small, m2), usage, rewriteOp(id, 1, nBig, nSmall))

	// The client edits message 0 (at or before the change): the library ends the change.
	m0b, _ := msg("user", "first, edited")
	c.do(2, body(m0b, big, m2), body(m0b, big, m2), usage)

	// Request 3 reports the same ChangeID again: it is added and priced (no filter).
	req := c.s.begin(accountant.RequestMeta{RequestID: "r3", Session: "s", At: time.Unix(1030, 0), Billing: accountant.Billing{Mode: accountant.BillingAPI}}, body(m0b, big, m2))
	c.s.end(req, c.th, body(m0b, small, m2), []accountant.Operation{rewriteOp(id, 1, nBig, nSmall)}, accFacts)
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
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	big, nBig := msg("user", strings.Repeat("x", 2000))
	small, nSmall := msg("user", "y")
	usage := Usage{FreshInput: 10}
	c.do(1, body(big), body(small), usage, rewriteOp("c1", 0, nBig, nSmall))
	dry := rewriteOp("c1", 0, nBig, nSmall)
	dry.DryRun = true
	spend := accountant.Operation{Kind: accountant.KindSpend, ChangeID: "c1"}
	req := c.s.begin(accountant.RequestMeta{RequestID: "r2", Session: "s", At: time.Unix(1020, 0)}, body(big))
	c.s.end(req, c.th, body(big), []accountant.Operation{dry, spend}, accFacts)
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
			r := s.begin(accountant.RequestMeta{RequestID: fmt.Sprint("r", i), Session: "s", At: time.Now(), Billing: accountant.Billing{Mode: accountant.BillingAPI}}, b)
			s.end(r, accountant.Thread{ID: "main"}, b, nil, accFacts)
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

// M2: a ChangeID that changes between turns for the same rewrite (an earlier component started or
// stopped acting) leaves the old change active and reports the new one: the library prices both,
// twice the real saving. The sum check catches it, and PriceMode falls back to the legacy figures
// with reason check_sum, so no inflated override is stored.
func TestSavings_ChangedChangeIDDoubleCountIsGated(t *testing.T) {
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	big, nBig := msg("user", strings.Repeat("tool result ", 400))
	small, nSmall := msg("user", "[masked]")
	m0, _ := msg("user", "first")
	m2, _ := msg("assistant", "answer")
	usage := Usage{FreshInput: 10, CacheRead: 100}
	r1 := c.do(1, body(m0, big, m2), body(m0, small, m2), usage, rewriteOp("cg:mask:1:h1", 1, nBig, nSmall))
	if len(r1.Checks) != 0 {
		t.Fatalf("first request checks = %+v", r1.Checks)
	}
	next := mustMsg("user", "next")
	r2 := c.do(2, body(m0, big, m2, next), body(m0, small, m2, next), usage, rewriteOp("cg:mask:1:h2", 1, nBig, nSmall))
	n1, _ := countChange(r2, "cg:mask:1:h1")
	n2, _ := countChange(r2, "cg:mask:1:h2")
	if n1 != 1 || n2 != 1 {
		t.Fatalf("precondition (the double count): h1 priced %d, h2 priced %d", n1, n2)
	}
	sum := false
	for _, ch := range r2.Checks {
		sum = sum || ch.Check == "sum"
	}
	if !sum {
		t.Fatalf("the sum check must fire on the double count: %+v", r2.Checks)
	}
	e := &dash.Event{Model: "m", FreshInput: 100, CacheRead: 1000, OutputTokens: 1, TokensBefore: 100000,
		Components: []dash.CompRow{{Component: "mask", Acted: true, Mutated: true, SavedGross: 10, SavedUnique: 10}}}
	leg := *e
	leg.Components = append([]dash.CompRow(nil), e.Components...)
	leg.Price(accPrice, true)
	e.SetAccountant(r2)
	e.PriceMode(accPrice, true)
	if fb := e.AccountantFallback(); fb != dash.FallbackCheckSum {
		t.Fatalf("fallback = %q, want %q", fb, dash.FallbackCheckSum)
	}
	if e.Components[0].SavedUSD != leg.Components[0].SavedUSD || e.BaselineCostUSD != leg.BaselineCostUSD {
		t.Fatalf("an inflated override was stored: %v/%v vs legacy %v/%v",
			e.Components[0].SavedUSD, e.BaselineCostUSD, leg.Components[0].SavedUSD, leg.BaselineCostUSD)
	}
}

var accPrice = modelinfo.Price{Input: 3e-6, CacheWrite: 3.75e-6, CacheRead: 0.3e-6, Output: 15e-6}

// The client moves its cache mark to the newest message every turn, as Claude Code does. A change made on turn 1
// and replayed on turns 2 and 3 sits inside what the provider keeps reading, so on a hot turn it is priced at
// the READ tier. With the mark in the hash, the previous message looked edited every turn, the library found
// no common prefix, and the replay was priced at the WRITE tier: the booked saving grew each turn while the
// real one did not (context-guru#768 B1: 2.0x, 2.9x, 3.7x on turns 2 to 4).
func TestSavings_AMovingCacheMarkDoesNotRepriceAReplayAtTheWriteRate(t *testing.T) {
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	const id = "cg:mask:mm"
	big, nBig := msg("user", strings.Repeat("tool result ", 400))
	small, nSmall := msg("user", "[masked]")
	// withMark puts the mark on a message's single text block.
	withMark := func(text string) string {
		return fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}]}`, text)
	}
	plain := func(text string) string {
		return fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":%q}]}`, text)
	}
	m0 := mustMsg("user", "first")
	op := rewriteOp(id, 1, nBig, nSmall)

	// turn 1: the mark is on message 2. Nothing cached before it.
	c.do(1, body(m0, big, withMark("a")), body(m0, small, withMark("a")), Usage{CacheWrite: 100}, op)
	// turn 2 and 3: the mark moves to the newest message. The text of the earlier ones did not change.
	c.do(2, body(m0, big, plain("a"), withMark("b")), body(m0, small, plain("a"), withMark("b")),
		Usage{CacheRead: 100, CacheWrite: 10}, op)
	res := c.do(3, body(m0, big, plain("a"), plain("b"), withMark("c")), body(m0, small, plain("a"), plain("b"), withMark("c")),
		Usage{CacheRead: 110, CacheWrite: 10}, op)

	var got *accountant.PricedOp
	for i := range res.Ops {
		if res.Ops[i].ChangeID == id {
			got = &res.Ops[i]
		}
	}
	if got == nil {
		t.Fatalf("the replayed change is not priced: %+v", res.Ops)
	}
	if got.Saved.CacheWrite5m != 0 || got.Saved.Input != 0 || got.Saved.CacheRead == 0 {
		t.Fatalf("turn 3 priced the replay at %+v, want the read tier (the provider read it)", got.Saved)
	}
	// The mark moved; the text did not. A hash that includes the mark reads that as an edit and the
	// library reports a cache break that never happened.
	for _, e := range res.CacheEffects {
		if e.Kind == accountant.EffectBreak {
			t.Fatalf("turn 3 reports a cache break %+v: the moving mark was read as an edit", e)
		}
	}
}

// context-guru#768 B9: the cachesplit credit was the whole cached read. A replay of four Claude Code sessions booked
// $350.93 of it on $29.96 of cost, although the system snapshot never changed and the true saving was $0. Two
// faults made that: the hash included the moving cache mark (B1), so the library lost B*, and the library credited a
// split with no check that the system section changed (accountant#20). Both are fixed. This drives the shape of the
// replay through the real tracker: a split every turn, an unchanged system block, a mark that moves.
//
// It fails only with BOTH faults present (the hash with the mark and the library before #20/#21): either fix alone
// is enough to keep the split effect out of this scenario. So it guards the combination, not each cause. The causes
// have their own tests: TestAccBodies_* for the hash, and the accountant cases split/tail-unchanged-* for the library.
func TestSavings_ASplitOfAnUnchangedSystemBlockBooksNothingWhenTheMarkMoves(t *testing.T) {
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	const sys = "stable instructions. "
	vol := "gitStatus: clean"
	clientSys := func() string {
		return fmt.Sprintf(`"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}]`, strings.Repeat(sys, 80)+vol)
	}
	fwdSys := func() string { // cachesplit puts the stable half first, with the mark, and the volatile tail after
		return fmt.Sprintf(`"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}},{"type":"text","text":%q}]`,
			strings.Repeat(sys, 80), vol)
	}
	turn := func(n int, withMark func(i int) bool, sysJSON string) []byte {
		var msgs []string
		for i := 0; i < n; i++ {
			text := fmt.Sprintf("message %d %s", i, strings.Repeat("x", 300))
			mark := ""
			if withMark(i) {
				mark = `,"cache_control":{"type":"ephemeral"}`
			}
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":[{"type":"text","text":%q%s}]}`, role, text, mark))
		}
		return []byte(`{` + sysJSON + `,"messages":[` + strings.Join(msgs, ",") + `]}`)
	}
	split := accountant.Operation{Kind: accountant.KindRewrite, Operator: "context-guru.cachesplit", ChangeID: "cg:cachesplit:b9",
		At:         accountant.Position{Section: accountant.SectionSystem, Index: 0},
		SizeBefore: accountant.Size{Bytes: len(clientSys())}, SizeAfter: accountant.Size{Bytes: len(clientSys())}}
	var prevTokens int64
	for n := 2; n <= 6; n++ {
		last := n - 1
		client := turn(n, func(i int) bool { return i == last }, clientSys()) // the mark is on the newest message
		fwd := turn(n, func(i int) bool { return i == last }, fwdSys())
		tokens := int64((len(fwd) + 3) / 4)
		u := Usage{CacheRead: prevTokens, CacheWrite: tokens - prevTokens}
		if n == 2 {
			u = Usage{CacheWrite: tokens}
		}
		res := c.do(n, client, fwd, u, split)
		prevTokens = tokens
		for _, e := range res.CacheEffects {
			if e.Kind == accountant.EffectSplit {
				t.Fatalf("turn %d: a split effect %+v on a system block that did not change", n, e.Saved)
			}
		}
	}
}

// The library asks the host to wrap Compute, and the repo's rule is to fail open (context-guru#768, major 1).
// A panic in Compute used to unwind through finish and lose the request's log line and its dashboard row.
// computeSafe turns it into "no result": the request is recorded with no saving. A nil request is a panic the
// test can cause on purpose; the real library has none that anyone has triggered.
func TestComputeSafeTurnsAPanicIntoNoResult(t *testing.T) {
	s := newSavingsTracker()
	if _, ok := s.computeSafe(nil, Usage{CacheRead: 10}); ok {
		t.Fatal("a panic in Compute was reported as a result")
	}
	// A real request still prices, through the same wrapper.
	c := accRun{s, accountant.Thread{ID: "main"}}
	m0 := mustMsg("user", "hi")
	req := c.s.begin(accountant.RequestMeta{RequestID: "r1", Session: "s", At: time.Unix(1000, 0),
		Billing: accountant.Billing{Mode: accountant.BillingAPI}}, body(m0))
	c.s.end(req, c.th, body(m0), nil, accFacts)
	res, ok := c.s.computeSafe(req, Usage{FreshInput: 10, CacheWrite: 100})
	if !ok || res.Phase == "" {
		t.Fatalf("a normal request did not price through computeSafe: ok=%v phase=%q", ok, res.Phase)
	}
}

// A summary splices away messages and one of them carries the client's cache mark. The splice's SizeBefore is the
// wire size (marks included), and the library compares it with the client span's bytes to guess whether the splice
// saw the original (sawOriginal). If the Body's byte table leaves the marks out while Pos.Bytes keeps them, the
// byte counts disagree and the library fails a check (conservation here: its client token total is built from
// mark-free bytes, the operations' deltas from wire bytes): the request books no saving (review of #893, finding 2).
func TestSavings_ASpliceOverAMarkedMessageKeepsTheSumCheck(t *testing.T) {
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	long := strings.Repeat("tool result ", 300)
	m0, n0 := msg("user", long)
	m1, n1 := msg("assistant", long)
	masked, nMasked := msg("assistant", "[masked]")
	m2raw := fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}]}`, long)
	n2 := len(m2raw)
	m3, _ := msg("assistant", "answer")
	sum, nSum := msg("user", "[summary]")

	// Request 1: a mask reduces message 1.
	c.do(1, body(m0, m1, m2raw, m3), body(m0, masked, m2raw, m3), Usage{CacheWrite: 1800},
		rewriteOp("cg:mask:a", 1, n1, nMasked))
	// Request 2: a summary replaces messages 0 to 2 with one. Its SizeBefore counts the wire bytes of those
	// three messages as the pipeline saw them: message 1 is already masked.
	splice := accountant.Operation{Kind: accountant.KindRewrite, Operator: "context-guru.summarize", ChangeID: "cg:sum:a",
		At:         accountant.Position{Section: accountant.SectionMessages, Index: 0},
		SizeBefore: accountant.Size{Bytes: n0 + nMasked + n2}, SizeAfter: accountant.Size{Bytes: nSum},
		MessageSplice: &accountant.MessageSplice{At: 0, Removed: 3, Inserted: 1}}
	res := c.do(2, body(m0, m1, m2raw, m3), body(sum, m3), Usage{CacheWrite: 30}, splice)
	for _, ch := range res.Checks {
		t.Errorf("check %s failed: %+v", ch.Check, ch)
	}
}

// context-guru#909: the live path booked no split credit. A short conversation (a few hundred tokens of messages
// beside a 4,300-token system block) makes the stable half the provider reads fall within the library's read
// tolerance of the whole previous entry. The library then took the read as proof that nothing broke, put B* past the
// changed system block, and dropped the split effect. The earlier tests used long messages, so they missed it.
// The fix is in accountant (fix/split-short-turn); this test pins the host side: the envelope the proxy reports
// for a split, the facts it passes, and the effect the library books for a tail that moves every turn.
func TestSavings_ASplitBooksItsEffectOnAShortConversationWhenTheTailMoves(t *testing.T) {
	c := accRun{newSavingsTracker(), accountant.Thread{ID: "main"}}
	stable := strings.Repeat("Follow the rules carefully and be concise. ", 400)
	turn := func(n int, sys string) []byte {
		var msgs []string
		for i := 0; i < n; i++ {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			mark := ""
			if i == n-1 {
				mark = `,"cache_control":{"type":"ephemeral"}`
			}
			msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":[{"type":"text","text":%q%s}]}`, role, fmt.Sprintf("message %d %s", i, strings.Repeat("w ", 20)), mark))
		}
		return []byte(`{` + sys + `,"messages":[` + strings.Join(msgs, ",") + `]}`)
	}
	for n := 1; n <= 4; n++ {
		vol := fmt.Sprintf("\nCurrent branch: main\nRecent commits:\n%08d fix thing\n", n)
		client := turn(2*n-1, fmt.Sprintf(`"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}]`, stable+vol))
		fwd := turn(2*n-1, fmt.Sprintf(`"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}},{"type":"text","text":%q}]`, stable, vol))
		total, half := (len(fwd)+3)/4, len(stable)/4
		u := Usage{CacheRead: int64(half), CacheWrite: int64(total - half)}
		if n == 1 {
			u = Usage{CacheWrite: int64(total)}
		}
		ops := envelopeOps(client, fwd)
		res := c.do(n, client, fwd, u, ops...)
		got := 0
		for _, e := range res.CacheEffects {
			if e.Kind == accountant.EffectSplit {
				got += e.Saved.CacheWrite5m
			}
		}
		if n == 1 && got != 0 {
			t.Fatalf("turn 1 has no previous request, yet a split effect of %d tokens", got)
		}
		if n > 1 && got != half {
			t.Fatalf("turn %d: split effect %d tokens, want the stable half %d (checks %v)", n, got, half, res.Checks)
		}
	}
}
