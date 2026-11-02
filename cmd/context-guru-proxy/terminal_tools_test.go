package main

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// #857: a tenant document cannot set the host-wide terminal-tool list. It is refused, not ignored.
func TestTenantDocumentRefusesTerminalTools(t *testing.T) {
	_, err := buildTenantConfig([]byte("preset: off\ncache:\n  keepalive_terminal_tools: []\n"), components.NopEmitter{})
	if err == nil || !strings.Contains(err.Error(), "keepalive_terminal_tools") {
		t.Fatalf("got %v, want an error naming keepalive_terminal_tools", err)
	}
	if _, err := buildTenantConfig([]byte("preset: off\ncache:\n  keepalive: false\n"), components.NopEmitter{}); err != nil {
		t.Fatalf("a document without the key must still load: %v", err)
	}
}
