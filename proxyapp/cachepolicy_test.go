package proxyapp

import (
	"testing"

	"github.com/rossoctl/context-guru/config"
)

// These two tests came from the IBM deployment, which used to keep its own copy of cachePolicy and
// pinned it against core's. The copy is gone; the facts they pin are about this cachePolicy. The
// wait-plan test it also had is TestCachePolicyCarriesTheWaitPlan in terminal_tools_test.go.

// TestCachePolicyCarriesToolWaitMinPrefix. A custom tool-wait floor must reach the policy, parked or
// active; dropped, WaitMinPrefix falls back to the default and the account's setting does nothing.
func TestCachePolicyCarriesToolWaitMinPrefix(t *testing.T) {
	for _, c := range []config.CacheConfig{
		{KeepAliveToolWaitMinPrefixTokens: 40000},
		{KeepAlive: true, KeepAliveToolWaitMinPrefixTokens: 40000},
	} {
		if p := cachePolicy(c); p.ToolWaitMinPrefix != 40000 {
			t.Errorf("KeepAlive=%v: ToolWaitMinPrefix = %d, want 40000", c.KeepAlive, p.ToolWaitMinPrefix)
		}
	}
}

// TestCachePolicyActivePathCarriesItsResolvedFields pins the fields only an active policy sets, so a future
// refactor of cachePolicy cannot drop them unnoticed.
func TestCachePolicyActivePathCarriesItsResolvedFields(t *testing.T) {
	p := cachePolicy(config.CacheConfig{KeepAlive: true, HeadTTL1h: true})
	if !p.KeepAlive || !p.HeadTTL1h || !p.WaitPlan {
		t.Errorf("active policy lost a switch: %+v", p)
	}
	if p.Idle <= 0 || p.OpenAIIdle <= 0 || p.Idle1h <= 0 || p.MaxPings <= 0 || p.MaxPings1h <= 0 ||
		p.MinPrefixTokens <= 0 || p.HeadTTLMinBytes <= 0 {
		t.Errorf("active policy lost a resolved default: %+v", p)
	}
}

// #1243: the 1-hour-on-wait strategy reaches the proxy's policy, with auto's keep-alive.
func TestCachePolicyCarriesOneHourOnWait(t *testing.T) {
	c, err := config.LoadBytes([]byte("cache:\n  strategy: 1-hour-on-wait\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := cachePolicy(c.Cache)
	if !p.OneHourOnWait || !p.KeepAlive || p.HeadTTL1h {
		t.Fatalf("policy wrong: onWait=%v keepalive=%v head=%v", p.OneHourOnWait, p.KeepAlive, p.HeadTTL1h)
	}
	a, _ := config.LoadBytes([]byte("cache:\n  strategy: auto\n"))
	if cachePolicy(a.Cache).OneHourOnWait {
		t.Error("auto must not switch it on")
	}
}
