package proxyapp_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/proxyapp"
)

// THE WHOLE POINT: every symbol cmd/context-guru-proxy reaches into internal/ for must be
// reachable from OUTSIDE that package. This test lives in proxyapp_test (an external test package)
// so it can only see the exported surface — exactly what a host module sees.
//
// If a future change to the composition root needs a fifth internal symbol, that root will
// compile and a host's will not. Nothing here can catch that; the corresponding note is in the
// package comment. What this test does guarantee is that the thirteen symbols named today keep
// working, and keep being reachable.
func TestEverySymbolAHostNeedsIsReachable(t *testing.T) {
	// Build identity.
	if proxyapp.Version() == "" {
		t.Error("Version() is empty; a host has nothing to stamp its build with")
	}
	if proxyapp.Commit() == "" {
		t.Error("Commit() is empty")
	}

	// Logging setup returns its own description and must never panic.
	if got := proxyapp.SetupLogging(); got == "" {
		t.Error("SetupLogging() returned no description")
	}

	// Model resolution: the compiled-in table resolves a model this project ships.
	static := proxyapp.DefaultStatic()
	chain := proxyapp.Chain{static}
	tokens, exact, ok := proxyapp.ExactWindow(chain, context.Background(), "claude-sonnet-4-5")
	if !ok || tokens <= 0 {
		t.Errorf("ExactWindow over DefaultStatic: tokens=%d exact=%v ok=%v; want a real window",
			tokens, exact, ok)
	}

	// A LiteLLM resolver is constructible without reaching the network.
	if proxyapp.NewLiteLLM("https://example.invalid", http.DefaultClient, time.Minute) == nil {
		t.Error("NewLiteLLM returned nil")
	}

	// The type aliases must be usable as types, not merely present.
	var (
		_ proxyapp.Resolver = static
		_ proxyapp.Pricer
		_ proxyapp.Price
		_ proxyapp.AnthropicCheapModel
		_ proxyapp.OpenAICheapModel
		_ *proxyapp.Table
	)
}

// LoadTable is the operator's escape hatch — correcting the compiled-in table without a rebuild —
// so its error path is part of the contract a host depends on.
func TestLoadTableReportsAMissingFileRatherThanPanicking(t *testing.T) {
	if _, err := proxyapp.LoadTable(t.TempDir() + "/does-not-exist.json"); err == nil {
		t.Error("LoadTable on a missing path returned no error")
	}
}
