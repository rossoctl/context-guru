package accrec

import (
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/tooldefer"
)

// pricedRatio prices one request and returns its Result and the usage it was priced with.
func pricedRatio(t *testing.T) (*Recorder, *accountant.Request, accountant.Result, Usage) {
	t.Helper()
	r := NewRecorder()
	req := recOneRequest(r, 1, ThreadOf("main", "", false))
	u := Usage{FreshInput: 10, CacheRead: 100}
	// The ratio before Compute is the ratio of the Result: the same billed size over the same forwarded body.
	return r, req, r.Compute(req, u), u
}

func TestRatioForIsTheRatioOfTheResult(t *testing.T) {
	r, req, _, u := pricedRatio(t)
	_ = req
	// Compute has run, but the body stays until Price or Drop, so RatioFor still answers.
	got := r.RatioFor(req, u)
	if got == nil {
		t.Fatal("RatioFor is nil for an ended request")
	}
	res := r.Compute(recOneRequest(r, 2, ThreadOf("main", "", false)), u)
	want := res.Ratio
	if want == nil {
		t.Fatal("Result.Ratio is nil")
	}
	gt, gb := got.Text()
	wt, wb := want.Text()
	if gt != wt || gb != wb || got.Tokens(1234) != want.Tokens(1234) {
		t.Fatalf("RatioFor = %d tokens over %d bytes, Result.Ratio = %d over %d", gt, gb, wt, wb)
	}
	r.Drop(req)
	if r.RatioFor(req, u) != nil {
		t.Fatal("Drop must forget the forwarded body")
	}
	var nilRec *Recorder
	if nilRec.RatioFor(req, u) != nil || r.RatioFor(nil, u) != nil || r.RatioFor(req, Usage{}) != nil {
		t.Fatal("a nil recorder, a nil request and an empty usage answer nil")
	}
}

func TestPriceForgetsTheForwardedBody(t *testing.T) {
	r := NewRecorder()
	req := recOneRequest(r, 1, ThreadOf("main", "", false))
	held := func() (n int) { r.Forwarded.Range(func(any, any) bool { n++; return true }); return }
	if held() != 1 {
		t.Fatalf("End keeps %d forwarded bodies, want 1", held())
	}
	r.Price(req, PriceIn{Status: 200, Rounds: 1, Usage: Usage{FreshInput: 10, CacheRead: 100}})
	if held() != 0 {
		t.Fatal("Price must forget the forwarded body: a request that is priced leaks nothing")
	}
}

func TestRatioCountsRoundTrip(t *testing.T) {
	_, _, res, _ := pricedRatio(t)
	c := RatioCountsOf(res)
	if !c.Known || c.BilledTokens <= 0 || c.TextBytes <= 0 {
		t.Fatalf("counts = %+v, want known", c)
	}
	back := c.Ratio()
	if back == nil || back.Tokens(5000) != res.Ratio.Tokens(5000) {
		t.Fatalf("rebuilt ratio converts 5000 bytes to %v, the Result's ratio to %d", back, res.Ratio.Tokens(5000))
	}
	for _, bad := range []RatioCounts{{}, {Known: true}, {BilledTokens: 5, TextBytes: 0, Known: true}, {BilledTokens: 0, TextBytes: 5, Known: true}} {
		if bad.Ratio() != nil {
			t.Errorf("%+v rebuilt a ratio: a row with no usable counts must keep its fallback", bad)
		}
	}
	if RatioCountsOf(accountant.Result{}).Known {
		t.Error("a Result with no ratio gave known counts")
	}
}

func TestProviderTokensLiveAgainstLegacy(t *testing.T) {
	c := RatioCounts{BilledTokens: 3000, TextBytes: 6000, Known: true} // 0.5 token a byte
	got, live := ProviderTokens("claude-sonnet-5", 999, 2000, c)
	if !live || got != 1000 {
		t.Fatalf("live = %v tokens = %v, want live 1000 (2000 bytes at 0.5), the host count ignored", live, got)
	}
	f, _ := tokens.LegacyFactor("claude-sonnet-5")
	for name, c := range map[string]RatioCounts{"no ratio": {}, "no bytes": c} {
		bytes := int64(2000)
		if name == "no bytes" {
			bytes = 0
		}
		got, live = ProviderTokens("claude-sonnet-5", 1000, bytes, c)
		if live || got != 1000*f {
			t.Errorf("%s: live = %v tokens = %v, want the table: %v", name, live, got, 1000*f)
		}
	}
}

func TestApplyToSetsTheRatioOnlyWithBytes(t *testing.T) {
	c := RatioCounts{BilledTokens: 3000, TextBytes: 6000, Known: true}
	in := accountant.RemovalInput{Removed: 400}
	if per := c.ApplyTo(&in, 400, 1600); in.Ratio == nil || in.RemovedBytes != 1600 || per != 2 {
		t.Fatalf("input %+v per %v, want ratio set, 1600 bytes, 2 provider tokens per host token (800/400)", in, per)
	}
	for name, bytes := range map[string]int64{"no bytes": 0} {
		in = accountant.RemovalInput{Removed: 400}
		if per := c.ApplyTo(&in, 400, bytes); in.Ratio != nil || per != 1 {
			t.Errorf("%s: input %+v per %v, want today's answer", name, in, per)
		}
	}
	in = accountant.RemovalInput{Removed: 400}
	if per := (RatioCounts{}).ApplyTo(&in, 400, 1600); in.Ratio != nil || per != 1 {
		t.Errorf("no ratio: input %+v per %v, want today's answer", in, per)
	}
}

func TestDeferSpendLoadedFromBytesWithARatio(t *testing.T) {
	r := accountant.RatioFromText(3000, 6000, false) // 0.5 token a byte
	s := tooldefer.SearchSpend{Loaded: 1500, LoadedBytes: 2000}
	loaded := func(ops []accountant.Operation) int {
		for _, o := range ops {
			if o.ID == "defer-search-second-pass" {
				return o.Call.Usage.Input
			}
		}
		return -1
	}
	if got := loaded(DeferSpendOps(s, &r)); got != 1000 {
		t.Fatalf("with a ratio the second pass reads %d tokens, want 1000 (2000 bytes at 0.5)", got)
	}
	if got := loaded(DeferSpendOps(s, nil)); got != 1500 {
		t.Fatalf("with no ratio the second pass reads %d tokens, want the table's 1500", got)
	}
	if got := loaded(DeferSpendOps(tooldefer.SearchSpend{Loaded: 1500}, &r)); got != 1500 {
		t.Fatalf("with no bytes the second pass reads %d tokens, want the table's 1500", got)
	}
}

func TestRatioTextForKeepsOnlyARatioThatBooksAFigure(t *testing.T) {
	r, req, _, u := pricedRatio(t)
	if tok, by := r.RatioTextFor(req, u, 200, 1); tok <= 0 || by <= 0 {
		t.Fatalf("one round, status 200: %d, %d, want a ratio", tok, by)
	}
	for name, c := range map[string][2]int{"several rounds": {200, 3}, "rejected": {429, 1}} {
		if tok, by := r.RatioTextFor(req, u, c[0], c[1]); tok != 0 || by != 0 {
			t.Errorf("%s: %d, %d, want none", name, tok, by)
		}
	}
	if tok, by := r.RatioTextFor(req, Usage{FreshInput: 3}, 200, 1); tok != 0 || by != 0 {
		t.Errorf("an out-of-range ratio: %d, %d, want none", tok, by)
	}
	var nilRec *Recorder
	if tok, by := nilRec.RatioTextFor(req, u, 200, 1); tok != 0 || by != 0 {
		t.Errorf("nil recorder: %d, %d", tok, by)
	}
}

func TestRequestFactorIsLiveOrNotAtAll(t *testing.T) {
	c := RatioCounts{BilledTokens: 3000, TextBytes: 6000, Known: true} // 0.5 token a byte
	if f, ok := RequestFactor(400, 1600, c); !ok || f != 2 {
		t.Fatalf("factor = %v ok=%v, want 2 (800 provider tokens over 400 host tokens)", f, ok)
	}
	for name, a := range map[string]struct {
		host, bytes int64
		c           RatioCounts
	}{"no ratio": {400, 1600, RatioCounts{}}, "no bytes": {400, 0, c}, "no removal": {0, 1600, c},
		"above the bound": {10, 1600, c}, "below one": {1000, 1600, c}} {
		if f, ok := RequestFactor(a.host, a.bytes, a.c); ok || f != 0 {
			t.Errorf("%s: %v %v, want the table (not ok)", name, f, ok)
		}
	}
}
