package main

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
)

const docWithKey = "preset: off\ncache:\n  keepalive_terminal_tools: []\n"

// #857: a TENANT document cannot set the host-wide terminal-tool list. It is refused at save
// (Validate) and at build, not ignored.
func TestTenantDocumentRefusesTerminalTools(t *testing.T) {
	if err := validateTenantDoc([]byte(docWithKey)); err == nil || !strings.Contains(err.Error(), "keepalive_terminal_tools") {
		t.Fatalf("save-time check: got %v, want an error naming keepalive_terminal_tools", err)
	}
	if _, err := buildHostedTenantConfig([]byte(docWithKey), components.NopEmitter{}); err == nil || !strings.Contains(err.Error(), "keepalive_terminal_tools") {
		t.Fatalf("build: got %v, want an error naming keepalive_terminal_tools", err)
	}
	ok := []byte("preset: off\ncache:\n  keepalive: false\n")
	if err := validateTenantDoc(ok); err != nil {
		t.Fatalf("a document without the key must save: %v", err)
	}
	if _, err := buildHostedTenantConfig(ok, components.NopEmitter{}); err != nil {
		t.Fatalf("a document without the key must build: %v", err)
	}
}

// The host's own config may set the key, and the local adapt rebuild goes through
// buildTenantConfig, so it must accept it.
func TestHostConfigWithTerminalToolsStillBuilds(t *testing.T) {
	if _, err := buildTenantConfig([]byte(docWithKey), components.NopEmitter{}); err != nil {
		t.Fatalf("local rebuild of a host config with the key: %v", err)
	}
}

// #858: the wait-plan fields reach the policy, also for a parked account block (a manager strategy can
// switch the keep-alive on for it), and the off switch reaches it.
func TestCachePolicyCarriesTheWaitPlan(t *testing.T) {
	p := cachePolicy(config.CacheConfig{})
	if !p.WaitPlan || p.On() || p.ToolWaitMaxPings != 8 || p.BackgroundExtraPings != 3 || p.HumanExtraPings != 1 || p.ToolWaitMinPrefix != 20000 {
		t.Errorf("parked block: %+v", p)
	}
	off := false
	p = cachePolicy(config.CacheConfig{KeepAlive: true, KeepAliveWaitPlan: &off})
	if p.WaitPlan || !p.On() || p.MinPrefixTokens != 80000 {
		t.Errorf("switch off: %+v", p)
	}
}
