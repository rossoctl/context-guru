package proxy

import (
	"context"
	"math"
	"testing"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

type tieredPricer struct{ p modelinfo.Price }

func (t tieredPricer) Price(context.Context, string) (modelinfo.Price, bool) { return t.p, true }

func gpt56Price() modelinfo.Price {
	return modelinfo.Price{Input: 5e-6, CacheRead: 0.5e-6, CacheWrite: 5e-6, Output: 30e-6}.WithTiers("gpt-5.6")
}

func haiku55Price() modelinfo.Price {
	return modelinfo.Price{Input: 1e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6, Output: 5e-6}.WithTiers("claude-haiku-5-5")
}

// The dispatcher's price adapter passes the prompt size through (it ignored it before).
func TestDispatcherPriceAdapterAppliesTheTier(t *testing.T) {
	h := &Handler{opts: Options{Prices: tieredPricer{gpt56Price()}}}
	d := h.dispatcher()
	p, ok := d.Price(context.Background(), "", "gpt-5.6", 272_000)
	if !ok || p.CacheRead != 0.5e-6 || p.Output != 30e-6 {
		t.Fatalf("at the line: %+v", p)
	}
	p, ok = d.Price(context.Background(), "", "gpt-5.6", 272_001)
	if !ok || p.CacheRead != 1e-6 || p.Input != 10e-6 || p.Output != 45e-6 {
		t.Fatalf("above the line: %+v", p)
	}
	h = &Handler{opts: Options{Prices: tieredPricer{haiku55Price()}}}
	p, _ = h.dispatcher().Price(context.Background(), "", "claude-haiku-5-5", 100_001)
	if p.CacheRead != 0.5e-6 || p.Output != 25e-6 {
		t.Fatalf("haiku above the line: %+v", p)
	}
	p, _ = h.dispatcher().Price(context.Background(), "", "claude-haiku-5-5", 100_000)
	if p.CacheRead != 0.1e-6 {
		t.Fatalf("haiku at the line: %+v", p)
	}
	// 0 is unknown: the base rates. A flat price (no tiers) never changes.
	if p, _ = h.dispatcher().Price(context.Background(), "", "claude-haiku-5-5", 0); p.CacheRead != 0.1e-6 {
		t.Fatalf("unknown size: %+v", p)
	}
}

// The campaign replay prices each row at its own prompt size.
func TestAutoPriceUsesTheRowsPromptSize(t *testing.T) {
	h := &Handler{opts: Options{Prices: tieredPricer{gpt56Price()}}}
	price := h.autoPrice()
	in, out, read, write, ok := price("gpt-5.6", modelinfo.PromptTokens(1000, 271_000, 0))
	if !ok || in != 5e-6 || out != 30e-6 || read != 0.5e-6 || write != 5e-6 {
		t.Fatalf("at the line: %v %v %v %v", in, out, read, write)
	}
	in, out, read, write, _ = price("gpt-5.6", modelinfo.PromptTokens(1000, 271_001, 0))
	if in != 10e-6 || out != 45e-6 || read != 1e-6 || write != 10e-6 {
		t.Fatalf("above the line: %v %v %v %v", in, out, read, write)
	}
}

// The avoidable write premium is priced at the tier of the request's prompt size.
func TestMissCostUsesTheTierOfThePrompt(t *testing.T) {
	im := &InferenceMetrics{prices: tieredPricer{gpt56Price()}}
	above := im.missCostUSD(&dash.Event{Model: "gpt-5.6", CacheWrite: 300_000})
	if want := 300_000 * (10e-6 - 1e-6); math.Abs(above-want) > 1e-9 {
		t.Fatalf("above the line %v want %v", above, want)
	}
	below := im.missCostUSD(&dash.Event{Model: "gpt-5.6", CacheWrite: 272_000})
	if want := 272_000 * (5e-6 - 0.5e-6); math.Abs(below-want) > 1e-9 {
		t.Fatalf("at the line %v want %v", below, want)
	}
}
