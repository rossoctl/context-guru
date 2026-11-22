package models_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/models"
	"github.com/rossoctl/context-guru/models/litellm"
)

// The calls a host such as Cortex makes, from outside the package (issue #1251): build a Chain of
// a LiteLLM resolver and the compiled-in table, then ask for an exact window.
func TestAHostResolvesAWindowThroughModels(t *testing.T) {
	static := models.DefaultStatic()
	tokens, exact, ok := models.ExactWindow(models.Chain{static}, context.Background(), "claude-sonnet-4-5")
	if !ok || tokens <= 0 {
		t.Errorf("ExactWindow over DefaultStatic: tokens=%d exact=%v ok=%v; want a real window",
			tokens, exact, ok)
	}

	// A LiteLLM resolver is constructible without reaching the network, and fits in a Chain.
	live := litellm.New("https://example.invalid", http.DefaultClient, time.Minute)
	if live == nil {
		t.Fatal("litellm.New returned nil")
	}
	_ = models.Chain{live, static}

	// The type aliases must be usable as types, not merely present.
	var (
		_ models.Resolver = static
		_ models.Resolver = live
		_ models.Pricer
		_ models.Price
		_ *models.Table
	)
}

// LoadTable's error path is part of the contract a host depends on.
func TestLoadTableReportsAMissingFile(t *testing.T) {
	if _, err := models.LoadTable(t.TempDir() + "/does-not-exist.json"); err == nil {
		t.Error("LoadTable on a missing path returned no error")
	}
}
