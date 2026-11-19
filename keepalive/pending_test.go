package keepalive

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/rossoctl/context-guru/internal/waitcause"
)

// #1227. The prefix of kaBody's usage is 49,776 tokens: under the person floor (80k), over the tool-wait
// floor (20k).

// personFloor is the plan policy with the shipped split: 80k for a person, 20k for the rest.
func personFloor() Policy {
	p := planPolicy()
	p.MinPrefixTokens = 80000
	return p
}

// recordWide records toolCall, then a final response on key, the last at `at`, with the wide policy.
func recordWide(k *Keeper, key Key, pol Policy, wide *Policy, body string, at time.Time, usage Usage, last resp) {
	for i, r := range []resp{toolCall, last} {
		rec := Record{Key: key, StartedAt: at.Add(-time.Duration(1-i) * time.Second), Body: []byte(body),
			Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
			Usage: usage, ToolUseNames: r.names, UsageOK: true, Policy: pol, Header: kaHeader(),
			Hold: fakeOf(k).hold(key)}
		usage.StopReason = r.stop
		rec.Usage.StopReason = r.stop
		rec.WidePolicy, rec.WideTag = wide, "wide"
		k.Record(rec)
	}
}

var usage50k = Usage{CacheRead: 48576, CacheWrite: 1200}

func pendingCount(k *Keeper) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.pending)
}

func TestPendingThreadUnderThePersonFloorIsPromotedOnBackground(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordWide(k, sess1, personFloor(), nil, kaBody, clock.now(), usage50k, endTurn)
	if _, ok := k.Entry(sess1); ok {
		t.Fatal("a 50k person wait is held: the case proves nothing")
	}
	if pendingCount(k) != 1 || fd.holds(sess1) != 1 {
		t.Fatalf("pending = %d, holds = %d, want 1 and 1", pendingCount(k), fd.holds(sess1))
	}
	if n, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground); !ok || n != 5 {
		t.Fatalf("promote = %d %v, want 5 true", n, ok)
	}
	if pendingCount(k) != 0 {
		t.Fatal("promoted record is still pending")
	}
	if got := plannedPings(t, k, sess1); got != 5 {
		t.Fatalf("planned = %d, want 5", got)
	}
	for i := 0; i < 8; i++ {
		k.Sweep(clock.advance(281 * time.Second))
	}
	if fd.n() != 5 {
		t.Fatalf("pings sent = %d, want 5", fd.n())
	}
	if fd.holds(sess1) != 0 {
		t.Fatalf("credential still held after the entry ended: %d", fd.holds(sess1))
	}
}

func TestPendingNotPromotedByAnotherCause(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordWide(k, sess1, personFloor(), nil, kaBody, clock.now(), usage50k, endTurn)
	for _, c := range []string{waitcause.WaitingHuman, waitcause.WaitingOwnTool, waitcause.WaitingTeammate, waitcause.WaitingUnknown} {
		if _, ok := k.RaiseWaitPlan(sess1, c); ok {
			t.Errorf("cause %q promoted a pending record", c)
		}
	}
	if _, ok := k.Entry(sess1); ok || fd.n() != 0 {
		t.Fatal("a pending record was held or pinged")
	}
}

func TestPendingExpiresWhenNoReportComes(t *testing.T) {
	k, fd, clock := testKeeperWith(t, func(o *Options) { o.PendingTTL = 20 * time.Millisecond })
	recordWide(k, sess1, personFloor(), nil, kaBody, clock.now(), usage50k, endTurn)
	if pendingCount(k) != 1 {
		t.Fatal("no pending record")
	}
	testutil.WaitFor(t, "the pending record expires", func() bool { return pendingCount(k) == 0 })
	if fd.holds(sess1) != 0 {
		t.Fatalf("credential still held after expiry: %d", fd.holds(sess1))
	}
	if _, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground); ok {
		t.Fatal("an expired record was promoted")
	}
	if r := reasonOf(k, sess1); r != ReasonBelowLimits {
		t.Errorf("reason = %q, want %q", r, ReasonBelowLimits)
	}
}

func TestPendingDroppedByTheNextRequest(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordWide(k, sess1, personFloor(), nil, kaBody, clock.now(), usage50k, endTurn)
	k.Arrive(sess1)
	if pendingCount(k) != 0 || fd.holds(sess1) != 0 {
		t.Fatalf("pending = %d, holds = %d after the next request, want 0 and 0", pendingCount(k), fd.holds(sess1))
	}
	if _, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground); ok {
		t.Fatal("a dropped record was promoted")
	}
}

func TestPendingGuards(t *testing.T) {
	sub := Key{Tenant: "t1", Session: "sess-1", Thread: "a:abc"}
	for _, tc := range []struct {
		name  string
		key   Key
		pol   func() Policy
		body  string
		usage Usage
		last  resp
	}{
		{"subagent", sub, personFloor, kaBody, usage50k, endTurn},
		{"plan off", sess1, func() Policy { p := kaPolicy(); p.MinPrefixTokens = 80000; return p }, kaBody, usage50k, endTurn},
		{"manual hold", sess1, func() Policy { p := personFloor(); p.ManualHold = true; return p }, kaBody, usage50k, endTurn},
		{"1-hour tier", sess1, personFloor, ttlBody1h, Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200}, endTurn},
		{"under the background floor", sess1, personFloor, kaBody, Usage{CacheRead: 9000, CacheWrite: 1000}, endTurn},
		{"a tool wait (not a person)", sess1, personFloor, kaBody, usage50k, toolCall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, fd, clock := testKeeper(t)
			recordWide(k, tc.key, tc.pol(), nil, tc.body, clock.now(), tc.usage, tc.last)
			if pendingCount(k) != 0 {
				t.Fatal("a pending record was kept")
			}
			if _, ok := k.RaiseWaitPlan(tc.key, waitcause.WaitingBackground); ok && tc.name != "a tool wait (not a person)" {
				t.Fatal("RaiseWaitPlan did something")
			}
			if tc.name != "a tool wait (not a person)" && fd.holds(tc.key) > 0 {
				if _, live := k.Entry(tc.key); !live {
					t.Fatalf("credential held with no entry: %d", fd.holds(tc.key))
				}
			}
		})
	}
	t.Run("OpenAI", func(t *testing.T) {
		k, fd, _ := testKeeper(t)
		pol := personFloor()
		for i := 0; i < 2; i++ {
			k.Record(Record{Key: sess1, StartedAt: time.Now(), Provider: bschemas.OpenAI, Route: "/v1/responses",
				Body:   []byte(`{"model":"azure/gpt-5.6-luna","max_output_tokens":2048,"input":[]}`),
				Status: http.StatusOK, Usage: Usage{CacheRead: 48576, StopReason: "end_turn"}, UsageOK: true, Policy: pol,
				Header: kaHeader(), Hold: fakeOf(k).hold(sess1)})
		}
		if pendingCount(k) != 0 || fd.holds(sess1) > 0 {
			t.Fatalf("OpenAI kept pending=%d holds=%d", pendingCount(k), fd.holds(sess1))
		}
	})
	t.Run("already background: held at once, nothing pending", func(t *testing.T) {
		k, _, clock := testKeeperWith(t, func(o *Options) {
			o.OpenTasks = func(string, string, string) (int, int) { return 1, 0 }
		})
		recordWide(k, sess1, personFloor(), nil, kaBody, clock.now(), usage50k, endTurn)
		if pendingCount(k) != 0 {
			t.Fatal("pending kept for a turn already read as background")
		}
		if got := plannedPings(t, k, sess1); got != 5 {
			t.Fatalf("planned %d, want 5", got)
		}
	})
}

func TestPendingIsBounded(t *testing.T) {
	k, _, clock := testKeeper(t)
	for i := 0; i < MaxPending+5; i++ {
		key := Key{Tenant: "t1", Session: string(rune('a'+i%26)) + string(rune('A'+i/26))}
		recordWide(k, key, personFloor(), nil, kaBody, clock.now(), usage50k, endTurn)
	}
	if n := pendingCount(k); n != MaxPending {
		t.Fatalf("pending = %d, want the bound %d", n, MaxPending)
	}
	k.Shutdown()
	if pendingCount(k) != 0 {
		t.Fatal("Stop left pending records")
	}
}

// A thread that a closed time window left without a policy is kept too, and promoted with the wide policy.
func TestPendingOutsideTheWindowIsPromotedWithTheWidePolicy(t *testing.T) {
	k, fd, clock := testKeeper(t)
	wide := planPolicy() // 20k floor: the policy a background wait gets
	recordWide(k, sess1, Policy{}, &wide, kaBody, clock.now(), usage50k, endTurn)
	if _, ok := k.Entry(sess1); ok || pendingCount(k) != 1 {
		t.Fatalf("held=%v pending=%d, want a pending record only", ok, pendingCount(k))
	}
	if n, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground); !ok || n != 5 {
		t.Fatalf("promote = %d %v, want 5 true", n, ok)
	}
	if v, _ := k.Entry(sess1); v.Tag != "wide" {
		t.Errorf("tag = %q, want wide", v.Tag)
	}
	for i := 0; i < 8; i++ {
		k.Sweep(clock.advance(281 * time.Second))
	}
	if fd.n() != 5 {
		t.Fatalf("pings sent = %d, want 5", fd.n())
	}
}

func TestPendingOutsideTheWindowExpiresWithTheOffReason(t *testing.T) {
	k, fd, clock := testKeeperWith(t, func(o *Options) { o.PendingTTL = 20 * time.Millisecond })
	wide := planPolicy()
	recordWide(k, sess1, Policy{}, &wide, kaBody, clock.now(), usage50k, endTurn)
	testutil.WaitFor(t, "the pending record expires", func() bool { return pendingCount(k) == 0 })
	if fd.holds(sess1) != 0 {
		t.Fatalf("credential still held: %d", fd.holds(sess1))
	}
	if r := reasonOf(k, sess1); r != ReasonNotOptedIn {
		t.Errorf("reason = %q, want %q", r, ReasonNotOptedIn)
	}
}

// Case A: a held thread whose windows gave it the account's policy gets the wide policy and its pings.
func TestRaiseWaitPlanWidensAHeldEntry(t *testing.T) {
	t.Run("wide policy replaces the narrow one", func(t *testing.T) {
		k, fd, clock := testKeeper(t)
		narrow := planPolicy() // Idle 280, base 2, floor 20k
		wide := planPolicy()
		wide.Idle, wide.MaxPings = 250*time.Second, 4
		recordWide(k, sess1, narrow, &wide, kaBody, clock.now(), usage50k, endTurn)
		if got := plannedPings(t, k, sess1); got != 3 {
			t.Fatalf("planned %d, want 3", got)
		}
		if n, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground); !ok || n != 7 {
			t.Fatalf("raise = %d %v, want 7 true (wide base 4 + 3)", n, ok)
		}
		v, _ := k.Entry(sess1)
		if v.Policy.Idle != 250*time.Second || v.Tag != "wide" {
			t.Fatalf("policy %+v tag %q: the wide policy was not applied", v.Policy, v.Tag)
		}
		for i := 0; i < 10; i++ {
			k.Sweep(clock.advance(251 * time.Second))
		}
		if fd.n() != 7 {
			t.Fatalf("pings sent = %d, want 7", fd.n())
		}
	})
	t.Run("never fewer pings", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		narrow := planPolicy()
		narrow.MaxPings = 6 // person: 7, background: 9
		wide := planPolicy()
		wide.MaxPings = 1 // would give 4
		recordWide(k, sess1, narrow, &wide, kaBody, clock.now(), usage50k, endTurn)
		n, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground)
		if !ok || n != 9 {
			t.Fatalf("raise = %d %v, want 9 true (the entry's own plan)", n, ok)
		}
		if v, _ := k.Entry(sess1); v.Tag == "wide" {
			t.Fatal("the wide policy replaced a better plan")
		}
	})
	t.Run("a wide floor above the prefix keeps the entry", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		wide := planPolicy()
		wide.MinPrefixTokens, wide.ToolWaitMinPrefix = 90000, 90000
		recordWide(k, sess1, planPolicy(), &wide, kaBody, clock.now(), usage50k, endTurn)
		n, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground)
		if !ok || n != 5 {
			t.Fatalf("raise = %d %v, want the plain raise to 5", n, ok)
		}
		if v, _ := k.Entry(sess1); v.Tag == "wide" || v.Policy.MinPrefixTokens != 20000 {
			t.Fatalf("a wide policy that the prefix cannot pass was applied: %+v", v.Policy)
		}
	})
	t.Run("subagent and other causes ignore the wide policy", func(t *testing.T) {
		sub := Key{Tenant: "t1", Session: "sess-1", Thread: "a:abc"}
		k, _, clock := testKeeper(t)
		wide := planPolicy()
		wide.Idle, wide.MaxPings = 250*time.Second, 4
		recordWide(k, sub, planPolicy(), &wide, kaBody, clock.now(), usage50k, endTurn)
		if _, ok := k.RaiseWaitPlan(sub, waitcause.WaitingBackground); ok {
			t.Fatal("a subagent was widened")
		}
		k2, _, clock2 := testKeeper(t)
		recordWide(k2, sess1, planPolicy(), &wide, kaBody, clock2.now(), usage50k, endTurn)
		if _, ok := k2.RaiseWaitPlan(sess1, waitcause.WaitingHuman); ok {
			t.Fatal("a person cause widened an entry")
		}
	})
}

func reasonOf(k *Keeper, key Key) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reasons[key.id()].reason
}

// A late report: the wide policy's deadline ((5+1) x 60 s = 360 s) is behind us, the entry's own
// ((5+1) x 290 s) is not. The raise must fall through to the entry's own plan (the #1226 raise), not end
// the search at the wide candidate.
func TestRaiseWaitPlanLateReportFallsThroughToTheEntrysOwnPlan(t *testing.T) {
	k, _, clock := testKeeper(t)
	own := planPolicy()
	own.Idle = 290 * time.Second
	wide := planPolicy()
	wide.Idle = 60 * time.Second
	recordWide(k, sess1, own, &wide, kaBody, clock.now(), usage50k, endTurn)
	k.mu.Lock()
	armed := k.live[sess1.id()].armedAt
	k.mu.Unlock()
	clock.advance(400 * time.Second)
	n, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground)
	if !ok || n != 5 {
		t.Fatalf("raise = %d %v, want 5 true (the entry's own plan)", n, ok)
	}
	v, _ := k.Entry(sess1)
	if v.Tag == "wide" || v.Policy.Idle != 290*time.Second {
		t.Fatalf("policy %+v tag %q: the expired wide policy was applied", v.Policy, v.Tag)
	}
	k.mu.Lock()
	deadline := k.live[sess1.id()].deadline
	k.mu.Unlock()
	if want := armed.Add(6 * 290 * time.Second); !deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v (timer re-armed for 5 pings)", deadline, want)
	}
}

// The refusal reason is set when the record is made, and a promotion takes it back.
func TestPendingSetsTheReasonAtOnceAndPromotionClearsIt(t *testing.T) {
	k, _, clock := testKeeper(t)
	recordWide(k, sess1, personFloor(), nil, kaBody, clock.now(), usage50k, endTurn)
	if r := reasonOf(k, sess1); r != ReasonBelowLimits {
		t.Fatalf("reason = %q, want %q at once", r, ReasonBelowLimits)
	}
	before := k.skipped.Load()
	if _, ok := k.RaiseWaitPlan(sess1, waitcause.WaitingBackground); !ok {
		t.Fatal("not promoted")
	}
	if r := reasonOf(k, sess1); r != "" {
		t.Errorf("reason = %q after the promotion, want none", r)
	}
	if k.skipped.Load() != before-1 {
		t.Errorf("skipped = %d, want %d", k.skipped.Load(), before-1)
	}
}
