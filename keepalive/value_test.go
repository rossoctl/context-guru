package keepalive

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

func gpt55() modelinfo.Price {
	return modelinfo.Price{Input: 5e-6, CacheRead: 0.5e-6, CacheWrite: 5e-6, Output: 30e-6}.WithTiers("gpt-5.5")
}

// A GPT-5.5 ping at 600K tokens costs about $0.60 and protects a miss of about $5.40. The flat $0.50
// cap refused it. The value rule lets it through, with a planned chain of 1.
func TestGPT55PingAt600KPassesTheValueRule(t *testing.T) {
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{gpt55()}})
	ping := k.projectedPingUSD("gpt-5.5", 600_000, "/v1/responses", "")
	miss := k.projectedMissUSD("gpt-5.5", 600_000, "", false)
	if ping <= DefaultMaxUSDPerPing {
		t.Fatalf("the fixture must exceed the old flat cap: ping $%.3f", ping)
	}
	if want := 600_000 * (10e-6 - 1e-6); !near(miss, want) { // tier: 2x write (= input), 2x read
		t.Fatalf("miss $%v, want $%v", miss, want)
	}
	if v := (Policy{}).PingVerdict(ping, miss, 1); v != VerdictOK {
		t.Errorf("chain of 1: verdict %v, want OK (ping $%.3f, miss $%.3f)", v, ping, miss)
	}
	// Five pings cost more than the one miss they protect: refused, with the value-rule reason.
	v := (Policy{}).PingVerdict(ping, miss, 10)
	if v != VerdictNotWorth || v.Reason() != ReasonNotWorth {
		t.Errorf("chain of 10: verdict %v reason %q, want NotWorth", v, v.Reason())
	}
	if ReasonNotWorth == ReasonOverBudget {
		t.Error("the value rule must have its own reason")
	}
}

// The chain length counts. Two pings at 60% of a miss cost more than the miss.
func TestValueRuleCountsTheWholeChain(t *testing.T) {
	p := Policy{}
	if p.PingVerdict(0.6, 1.0, 1) != VerdictOK {
		t.Error("one ping at 0.6 of a miss should pass")
	}
	if p.PingVerdict(0.6, 1.0, 2) != VerdictNotWorth {
		t.Error("two pings at 0.6 of a miss cost more than the miss")
	}
	if p.PingVerdict(0.6, 1.0, 0) != VerdictOK {
		t.Error("no planned count counts as one ping")
	}
	if p.PingVerdict(1.0, 1.0, 1) != VerdictNotWorth {
		t.Error("a ping that costs the whole miss saves nothing")
	}
}

// An explicit operator cap applies on top of the value rule. A strategy's old default does not exist.
func TestExplicitCapStillRefusesAndNoCapIsNotACap(t *testing.T) {
	if v := (Policy{MaxUSDPerPing: 0.5}).PingVerdict(0.6, 5.4, 1); v != VerdictOverCap || v.Reason() != ReasonOverBudget {
		t.Errorf("explicit cap: verdict %v", v)
	}
	if v := (Policy{}).PingVerdict(0.6, 5.4, 1); v != VerdictOK {
		t.Errorf("no cap set: verdict %v, want OK", v)
	}
	if v := (Policy{MaxUSDPerPing: -1}).PingVerdict(0.6, 5.4, 1); v != VerdictOK {
		t.Errorf("negative cap: verdict %v, want OK", v)
	}
	// Ceiling() is the old flat default. No keeper path reads it since #1439; it stays pinned to config's default.
	if got := (Policy{}).Ceiling(); got != DefaultMaxUSDPerPing {
		t.Errorf("Ceiling() = %v, want the pinned default", got)
	}
}

// An unpriced model is allowed, and so is a price list that cannot say what a miss costs.
func TestUnpricedPingIsAllowed(t *testing.T) {
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{}}})
	if got := k.projectedPingUSD("m", 600_000, "/v1/messages", ""); got != 0 {
		t.Errorf("unpriced ping = %v, want 0", got)
	}
	if got := k.projectedMissUSD("m", 600_000, "", false); got != 0 {
		t.Errorf("unpriced miss = %v, want 0", got)
	}
	if v := (Policy{}).PingVerdict(0, 0, 3); v != VerdictOK {
		t.Errorf("verdict for unpriced = %v", v)
	}
	if v := (Policy{}).PingVerdict(0.7, 0, 3); v != VerdictOK {
		t.Errorf("unknown miss cost = %v, want OK (fail open)", v)
	}
}

// An entry billed at the 1-hour tier rewrites at the 1-hour rate, so a miss costs more there. The ping
// itself is a read on a thread already at 1 hour, so its projection stays at the read rate.
func TestMissUsesTheOneHourWriteRate(t *testing.T) {
	p := modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}
	const prefix = 100_000
	five := MissUSD(p, "claude-sonnet-5", prefix, false)
	hour := MissUSD(p, "claude-sonnet-5", prefix, true)
	if want := prefix * (3.75e-6 - 0.3e-6); !near(five, want) {
		t.Errorf("5m miss = %v, want %v", five, want)
	}
	if want := prefix * (6e-6 - 0.3e-6); !near(hour, want) { // 2.0 x input
		t.Errorf("1h miss = %v, want %v", hour, want)
	}
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{p}})
	if got, want := k.projectedMissUSD("claude-sonnet-5", prefix, "", true), hour; !near(got, want) {
		t.Errorf("keeper 1h miss = %v, want %v", got, want)
	}
	if got, want := k.projectedPingUSD("claude-sonnet-5", prefix, "/v1/messages", ""), prefix*0.3e-6+7*3e-6+15e-6; !near(got, want) {
		t.Errorf("ping on a 1-hour entry = %v, want the read rate plus the 7 fresh input tokens %v", got, want)
	}
}

// End to end through Record: a held thread on a GPT-5.5 price at 600K is kept, and a pricier chain is not.
func TestRecordHoldsTheGPT55PingThatTheFlatCapRefused(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{gpt55()}})
	rec := func(key Key, pol Policy) {
		pol.ManualHold = true
		for _, d := range []time.Duration{-time.Second, 0} {
			k.Record(Record{Key: key, StartedAt: clock.now().Add(d), Body: []byte(kaBody), Provider: bschemas.Anthropic,
				Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 600_000, CacheWrite: 1200},
				UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fd.hold(key)})
		}
	}
	rec(sess1, offPolicy)
	if _, ok := k.Entry(sess1); !ok {
		t.Fatalf("the $0.60 ping was refused: reason %q", stateOf(k, sess1).Reason)
	}
	rec(Key{Tenant: "t1", Session: "sess-2"}, Policy{MaxUSDPerPing: 0.5})
	if _, ok := k.Entry(Key{Tenant: "t1", Session: "sess-2"}); ok {
		t.Error("an explicit $0.50 cap did not refuse the $0.60 ping")
	}
	if st := stateOf(k, Key{Tenant: "t1", Session: "sess-2"}); st.Reason != ReasonOverBudget {
		t.Errorf("reason %q, want %q", st.Reason, ReasonOverBudget)
	}
}

// A policy that writes at 1 hour (HeadTTL1h, OneHourAlways) or plans the 1-hour ping would rewrite at the 1-hour
// rate on a miss, though the provider has not billed the entry at 1 hour yet. The miss is priced at that rate.
func TestMissIsPricedAtOneHourWhenThePolicyWritesAtOneHour(t *testing.T) {
	p := modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}
	missOf := func(pol Policy, thread string) float64 {
		k, fd, clock := testKeeper(t)
		fd.setPrices(proxyPricer{fixedPrice{p}})
		pol.ManualHold = true
		key := Key{Tenant: "t1", Session: "miss-1", Thread: thread}
		for _, d := range []time.Duration{-time.Second, 0} {
			k.Record(Record{Key: key, StartedAt: clock.now().Add(d), Body: []byte(kaBody), Provider: bschemas.Anthropic,
				Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
				UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fd.hold(key)})
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		for _, e := range k.live {
			return e.missUSD
		}
		t.Fatal("nothing held")
		return 0
	}
	const prefix = 48576 + 1200
	five, hour := float64(prefix)*(3.75e-6-0.3e-6), float64(prefix)*(6e-6-0.3e-6)
	if got := missOf(Policy{}, ""); !near(got, five) {
		t.Errorf("plain policy miss = %v, want the 5-minute %v", got, five)
	}
	for name, pol := range map[string]Policy{"HeadTTL1h": {HeadTTL1h: true}, "OneHourAlways": {OneHourAlways: true}} {
		if got := missOf(pol, ""); !near(got, hour) {
			t.Errorf("%s miss = %v, want the 1-hour %v", name, got, hour)
		}
	}
	// OneHourAlways is the main agent only: a subagent keeps the 5-minute cache, so its miss is the 5-minute rewrite.
	if got := missOf(Policy{OneHourAlways: true}, "a:sub-1"); !near(got, five) {
		t.Errorf("OneHourAlways on a subagent: miss = %v, want the 5-minute %v", got, five)
	}
	// HeadTTL1h applies to subagent requests too.
	if got := missOf(Policy{HeadTTL1h: true}, "a:sub-1"); !near(got, hour) {
		t.Errorf("HeadTTL1h on a subagent: miss = %v, want the 1-hour %v", got, hour)
	}
}

// A head-TTL thread (a 1-hour head, a 5-minute tail) on the 5-minute ping plan: while the head lives, a miss
// rewrites only the tail, so a ping that reads head + tail is not worth it. The same thread without a mixed
// write keeps the whole-prefix price, and the ping is held.
func TestHeadTTLMissIsThePriceOfTheTail(t *testing.T) {
	p := modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}
	const head, tail = 20_157, 150
	const model = "claude-sonnet-5"

	// The price: head alive is the tail only. No head is the old whole-prefix form. An unpriced model is 0.
	if got, want := MissUSDHead(p, model, head+tail, false, head), float64(tail)*(3.75e-6-0.3e-6); !near(got, want) {
		t.Errorf("head alive miss = %v, want the tail %v", got, want)
	}
	if got, want := MissUSDHead(p, model, head+tail, true, head), float64(tail)*(3.75e-6-0.3e-6); !near(got, want) {
		t.Errorf("head alive miss with oneHour = %v, want the tail %v (oneHour does not count)", got, want)
	}
	if got, want := MissUSDHead(p, model, head+tail, false, 0), MissUSD(p, model, head+tail, false); !near(got, want) {
		t.Errorf("no head miss = %v, want the whole prefix %v", got, want)
	}
	if got := MissUSDHead(modelinfo.Price{}, model, head+tail, false, head); got != 0 {
		t.Errorf("unpriced miss = %v, want 0 (fail open)", got)
	}

	// The verdict: one ping reads head + tail and costs far more than the tail miss.
	ping := float64(head+tail)*0.3e-6 + float64(pingOutputBudget("/v1/messages"))*15e-6
	pol := Policy{KeepAlive: true, HeadTTL1h: true, Idle: 240 * time.Second, MaxPings: 2}
	if v := pol.PingVerdict(ping, MissUSDHead(p, model, head+tail, false, head), 2); v != VerdictNotWorth {
		t.Errorf("verdict on the tail miss = %v, want NotWorth", v)
	}

	held := func(u Usage, second Usage) (kept bool, miss float64) {
		k, fd, clock := testKeeper(t)
		fd.setPrices(proxyPricer{fixedPrice{p}})
		key := Key{Tenant: "t1", Session: "head-1"}
		for i, uu := range []Usage{u, second} {
			k.Record(Record{Key: key, StartedAt: clock.now().Add(time.Duration(i-1) * time.Second), Body: []byte(kaBody), Provider: bschemas.Anthropic,
				Route: "/v1/messages", Status: http.StatusOK, Usage: uu, UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fd.hold(key)})
		}
		_, kept = k.Entry(key)
		k.mu.Lock()
		defer k.mu.Unlock()
		for _, e := range k.live {
			miss = e.missUSD
		}
		return kept, miss
	}
	// Turn 1 writes the mixed entry. Turn 2 reads the head and rewrites a tail: no 1-hour token, the head is inherited.
	mixed := Usage{CacheWrite: head + tail, CacheWrite1h: head}
	reread := Usage{CacheRead: head, CacheWrite: tail}
	if kept, miss := held(mixed, reread); kept {
		t.Errorf("a ping on a head-TTL thread was held (miss %v), want it refused while the head lives", miss)
	}
	// With no mixed write the head is unknown: the whole-prefix price at the 1-hour rate (HeadTTL1h), and the ping stays.
	plain := Usage{CacheRead: head, CacheWrite: tail}
	if kept, _ := held(plain, plain); !kept {
		t.Error("a thread with no known head lost its ping")
	}
}

func TestMixedHeadFollowsTheBilledWrite(t *testing.T) {
	for _, c := range []struct {
		name string
		prev int64
		u    Usage
		ok   bool
		want int64
	}{
		{"mixed write", 0, Usage{CacheWrite: 100, CacheWrite1h: 60}, true, 60},
		{"all 1h write", 60, Usage{CacheWrite: 100, CacheWrite1h: 100}, true, 0},
		{"5m write over a head keeps the head", 60, Usage{CacheWrite: 10}, true, 60},
		{"read only", 60, Usage{CacheRead: 100}, true, 60},
		{"no usage", 60, Usage{}, false, 60},
	} {
		if got := mixedHead(c.prev, c.u, c.ok); got != c.want {
			t.Errorf("%s: head = %d, want %d", c.name, got, c.want)
		}
	}
}
