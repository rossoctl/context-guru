package components_test

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/offload" // registers summarize and cache_aware_summarizer
	"github.com/rossoctl/context-guru/store"
)

// fracProvider is a fake cut component with a fixed fraction and enabled state.
type fracProvider struct {
	name    string
	frac    float64
	enabled bool
}

func (f fracProvider) Name() string                 { return f.name }
func (f fracProvider) Enabled(*components.Ctx) bool { return f.enabled }
func (f fracProvider) CutFrac() float64             { return f.frac }
func (f fracProvider) Offload(*bschemas.BifrostChatRequest, *components.Report, *components.Ctx) ([]string, error) {
	return nil, nil
}

func runCutFrac(t *testing.T, c *components.Ctx, comps ...components.Component) float64 {
	t.Helper()
	req := &bschemas.BifrostChatRequest{}
	c.Store = store.NewMemory(store.Options{})
	components.NewPipeline(comps, nil).Run(req, c)
	return c.CutFrac
}

func TestPipelineCutFracSmallestEnabledWins(t *testing.T) {
	got := runCutFrac(t, &components.Ctx{}, fracProvider{"a", 0.9, true}, fracProvider{"b", 0.5, true})
	if got != 0.5 {
		t.Fatalf("CutFrac = %v, want the smallest, 0.5", got)
	}
}

func TestPipelineCutFracIgnoresDisabled(t *testing.T) {
	got := runCutFrac(t, &components.Ctx{}, fracProvider{"a", 0.9, true}, fracProvider{"b", 0.3, false})
	if got != 0.9 {
		t.Fatalf("CutFrac = %v, want 0.9: a disabled provider must contribute nothing", got)
	}
}

func TestPipelineCutFracKeepsAPreset(t *testing.T) {
	got := runCutFrac(t, &components.Ctx{CutFrac: 0.7}, fracProvider{"a", 0.5, true})
	if got != 0.7 {
		t.Fatalf("CutFrac = %v, want the preset 0.7", got)
	}
}

func TestPipelineCutFracNoneIsZero(t *testing.T) {
	if got := runCutFrac(t, &components.Ctx{}); got != 0 {
		t.Fatalf("empty pipeline CutFrac = %v, want 0", got)
	}
	if got := runCutFrac(t, &components.Ctx{}, fracProvider{"a", 0, true}); got != 0 {
		t.Fatalf("a provider with no fraction gave CutFrac = %v, want 0", got)
	}
}

// The REAL constructors return pointers; the type assertion in Pipeline.Run must find them.
func TestPipelineCutFracRealSummarizers(t *testing.T) {
	sum, err := components.New("summarize", []byte("trigger:\n  min_request_frac: 0.8\n"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := components.New("cache_aware_summarizer", []byte("trigger:\n  min_request_frac: 0.6\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := runCutFrac(t, &components.Ctx{}, sum, ca); got != 0.6 {
		t.Fatalf("CutFrac = %v, want 0.6 (smallest of summarize 0.8, cache_aware_summarizer 0.6)", got)
	}
	if got := runCutFrac(t, &components.Ctx{}, sum); got != 0.8 {
		t.Fatalf("summarize alone: CutFrac = %v, want 0.8", got)
	}
	// summarize is disabled when the request disallows count changes.
	if got := runCutFrac(t, &components.Ctx{DisallowCountChange: true}, sum); got != 0 {
		t.Fatalf("disabled summarize: CutFrac = %v, want 0", got)
	}
}
