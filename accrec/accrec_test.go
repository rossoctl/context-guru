package accrec

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/components"
)

func TestShouldCompute(t *testing.T) {
	for _, c := range []struct {
		name           string
		status, rounds int
		noResult       string
		compute        bool
		reason         string
	}{
		{"ok", 200, 1, "", true, ""},
		{"no upstream status", 0, 1, "", true, ""},
		{"rejected", 429, 1, "", false, RejectedStatus},
		{"rejected beats an earlier reason", 500, 1, DeferralUnplaced, false, RejectedStatus},
		{"deferral unplaced", 200, 1, DeferralUnplaced, false, DeferralUnplaced},
		{"two rounds", 200, 2, "", false, ""},
		{"an earlier reason does not stop a compute", 200, 1, "x", true, "x"},
	} {
		compute, reason := ShouldCompute(c.status, c.rounds, c.noResult)
		if compute != c.compute || reason != c.reason {
			t.Errorf("%s: got %v,%q want %v,%q", c.name, compute, reason, c.compute, c.reason)
		}
	}
}

func TestNoResultWhy(t *testing.T) {
	for _, c := range []struct {
		rec, usage bool
		noResult   string
		rounds     int
		want       string
	}{
		{false, true, "", 1, "no_record"},
		{true, false, "", 1, "no_usage"},
		{true, true, "compute_panicked", 3, "compute_panicked"},
		{true, true, "", 3, "upstream_rounds=3"},
		{true, true, "", 1, ""},
	} {
		if got := NoResultWhy(c.rec, c.usage, c.noResult, c.rounds); got != c.want {
			t.Errorf("%+v: got %q want %q", c, got, c.want)
		}
	}
}

func TestBilledTTL(t *testing.T) {
	exact1h := accountant.EntryFacts{TTL: time.Hour, Lifetime: accountant.LifetimeExact}
	tiers := NewTierStore()
	if !BilledTTL(tiers, "k", exact1h, 100, 0) {
		t.Error("a write billed at 5m did not lower the TTL")
	}
	// A read-only turn keeps the last billed tier: 5m.
	if !BilledTTL(tiers, "k", exact1h, 0, 0) {
		t.Error("a read-only turn forgot the 5m tier")
	}
	if BilledTTL(tiers, "k", exact1h, 100, 100) {
		t.Error("a write billed at 1h lowered the TTL")
	}
	if BilledTTL(NewTierStore(), "new", exact1h, 0, 0) {
		t.Error("a thread with no earlier write lowered the asked tier")
	}
	if BilledTTL(tiers, "k", accountant.EntryFacts{TTL: time.Hour, Lifetime: accountant.LifetimeMinimum}, 100, 0) {
		t.Error("a minimum lifetime has no tier to lower")
	}
	if BilledTTL(tiers, "k", accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeExact}, 100, 0) {
		t.Error("a 5m entry has nothing to lower")
	}
}

type sink struct {
	put    map[string]accountant.PingFacts
	priced int
}

func (s *sink) Put(k string, f accountant.PingFacts) { s.put[k] = f }
func (s *sink) NotePriced()                          { s.priced++ }

func TestPingSavingsRules(t *testing.T) {
	r := components.KeepAliveReport{Session: "s", Thread: "", Model: "m", Status: 200, CacheRead: 100,
		Window: time.Unix(1_700_000_000, 0), TS: 1_700_000_000_000}
	s := &sink{put: map[string]accountant.PingFacts{}}
	if _, ok := PingSavings(r, s, PingCalls{}); !ok || s.priced != 1 || len(s.put) != 1 {
		t.Fatalf("a normal ping: ok=%v priced=%d stored=%d", ok, s.priced, len(s.put))
	}
	bad := r
	bad.Status = 500
	if _, ok := PingSavings(bad, s, PingCalls{}); ok {
		t.Error("a rejected ping was priced")
	}
	manual := r
	manual.Manual, manual.Window = true, time.Unix(1_700_000_100, 0)
	s2 := &sink{put: map[string]accountant.PingFacts{}}
	if _, ok := PingSavings(manual, s2, PingCalls{}); !ok || s2.priced != 1 || len(s2.put) != 0 {
		t.Errorf("a manual ping: priced=%d stored=%d, want 1 and 0", s2.priced, len(s2.put))
	}
	if _, ok := PingSavings(r, nil, PingCalls{}); !ok {
		t.Error("a nil sink must be allowed")
	}
}

func TestEnvelopeOpsForAttributesDeferralToTooldefer(t *testing.T) {
	client := []byte(`{"tools":[{"name":"big","description":"` + string(make([]byte, 0)) + `a long definition of a tool that is deferred"}],"messages":[{"role":"user","content":"hi"}]}`)
	pre := client
	own := map[string]bool{"big": true}
	fwd := []byte(`{"tools":[{"name":"big","description":"a long definition of a tool that is deferred","defer_loading":true}],"messages":[{"role":"user","content":"hi"}]}`)
	billed, ok := BilledBody(fwd, own)
	if !ok {
		t.Fatal("the stub was not placed")
	}
	ops := EnvelopeOpsFor(client, billed, pre)
	if len(ops) != 1 || ops[0].Operator != "context-guru.tooldefer" {
		t.Fatalf("ops = %+v, want one tooldefer rewrite", ops)
	}
	if ops := EnvelopeOpsFor(client, billed, nil); len(ops) != 1 || ops[0].Operator != "context-guru.toolschema" {
		t.Fatalf("without deferral the operator is toolschema: %+v", ops)
	}
}
