package keepalive

import "testing"

// #1243. A main-thread request relabelled to 1h on EVERY breakpoint (tools, system, history) is a
// one-hour session, so `auto` uses the 59-minute schedule. A subagent request keeps its 5-minute
// labels and the 5-minute schedule. A mix (the head-only 1h over a 5-minute history) stays 5 minutes.
func TestResolveTierForTheMainAgentStrategy(t *testing.T) {
	const all1h = `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	const subagent = `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}`
	const headOnly = `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}`
	billed1h := Usage{CacheWrite: 1000, CacheWrite1h: 1000}
	if !ResolveTier([]byte(all1h), billed1h, true, false) {
		t.Error("a main-thread request with every breakpoint at 1h and billed 1h is not a one-hour session")
	}
	if ResolveTier([]byte(subagent), Usage{CacheWrite: 1000}, true, false) {
		t.Error("a subagent request at 5 minutes resolved to one hour")
	}
	if ResolveTier([]byte(headOnly), billed1h, true, false) {
		t.Error("a head-only 1h over a 5-minute history resolved to one hour")
	}
	// The schedule that follows: one ping at 59 minutes for the 1-hour session.
	pol := Policy{KeepAlive: true, Idle: 240e9, MaxPings: 2, Idle1h: 3540e9, MaxPings1h: 1}.ForOneHour()
	if pol.Idle != 3540e9 || pol.MaxPings != 1 {
		t.Errorf("the one-hour schedule is idle %v, %d pings; want 59 minutes and 1 ping", pol.Idle, pol.MaxPings)
	}
}

// #1243, owner decision: with 1-hour-main-agent the MAIN thread gets no ping, the 59-minute one
// included. A subagent thread keeps the policy's pings.
func TestMainAgentStrategySendsNoPingOnTheMainThread(t *testing.T) {
	pol := Policy{KeepAlive: true, Idle: 240e9, MaxPings: 2, Idle1h: 3540e9, MaxPings1h: 1, MaxUSDPerPing: 0.5,
		MinPrefixTokens: 1000, MainAgentTTL1h: true}
	for _, tc := range []struct {
		name   string
		thread string
		held   bool
	}{{"main thread", "", false}, {"subagent thread", "a:agent1", true}} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t)
			key := Key{Tenant: "t1", Session: "sess-1", Thread: tc.thread}
			recordHold(k, key, clock.now(), kaBody, pol, false)
			_, held := k.Entry(key)
			if held != tc.held {
				t.Fatalf("held = %v, want %v", held, tc.held)
			}
		})
	}
	// The same policy without the strategy holds the main thread: the gate is the strategy's.
	pol.MainAgentTTL1h = false
	k, _, clock := testKeeper(t)
	recordHold(k, sess1, clock.now(), kaBody, pol, false)
	if _, held := k.Entry(sess1); !held {
		t.Fatal("control: the main thread was not held without the strategy; the test proves nothing")
	}
}
