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
