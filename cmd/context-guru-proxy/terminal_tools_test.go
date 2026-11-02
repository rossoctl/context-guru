package main

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
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
