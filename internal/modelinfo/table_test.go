package modelinfo

import (
	"context"
	"math"
	"testing"
)

const priceTableSample = `
models:
  - {match: "aws/claude-sonnet-5", in: 1.52, out: 7.60}
  - {match: "aws/claude-opus-4-8", in: 3.80, out: 19.00}
  - {match: "premium*", in: 1.52, out: 7.60}
  - {match: "gcp/gemini-3.6-flash", in: 1.50, out: 7.50, cache_read: 0.375}
`

func table(t *testing.T) *Table {
	t.Helper()
	tb, err := ParseTable([]byte(priceTableSample))
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

// The file is written in dollars per MILLION tokens because that is the unit every
// price page uses; a per-token float in a hand-edited file is a 1000x typo waiting
// to happen. So the conversion is the thing to pin.
func TestTablePricesArePerMillionTokens(t *testing.T) {
	p, ok := table(t).Price(context.Background(), "aws/claude-sonnet-5")
	if !ok {
		t.Fatal("aws/claude-sonnet-5 not found")
	}
	if math.Abs(p.Input-1.52e-6) > 1e-15 || math.Abs(p.Output-7.60e-6) > 1e-15 {
		t.Fatalf("in/out = %g/%g, want 1.52e-6/7.6e-6", p.Input, p.Output)
	}
	// One million input tokens must cost exactly the number in the file.
	if got := p.Cost(1_000_000, 0, 0, 0); math.Abs(got-1.52) > 1e-9 {
		t.Fatalf("1M input tokens = $%.6f, want $1.52", got)
	}
	// Unstated cache tiers fall back to the Anthropic-family multiples.
	if math.Abs(p.CacheRead-p.Input*0.1) > 1e-18 || math.Abs(p.CacheWrite-p.Input*1.25) > 1e-18 {
		t.Fatalf("cache tiers = %g/%g, want 0.1x/1.25x of input", p.CacheRead, p.CacheWrite)
	}
	// An explicit cache rate is taken as written, not derived.
	g, _ := table(t).Price(context.Background(), "gcp/gemini-3.6-flash")
	if math.Abs(g.CacheRead-0.375e-6) > 1e-15 {
		t.Fatalf("explicit cache_read = %g, want 3.75e-7", g.CacheRead)
	}
}

// Bob names a server-resolved TIER, not a model. Without a family match every Bob
// request's cost read "unknown" — the symptom this table was added for.
func TestTableMatchesBobTiersAndDecoratedIDs(t *testing.T) {
	tb := table(t)
	for _, id := range []string{"premium", "premium-ide", "PREMIUM"} {
		if _, ok := tb.Price(context.Background(), id); !ok {
			t.Errorf("%q did not match premium*", id)
		}
	}
	// A gateway that decorates the id must still resolve.
	if _, ok := tb.Price(context.Background(), "bedrock/us.anthropic.aws/claude-sonnet-5"); !ok {
		t.Error("a decorated id did not match by containment")
	}
	if _, ok := tb.Price(context.Background(), "some-model-nobody-listed"); ok {
		t.Error("an unlisted model must report unknown, not a price")
	}
	if _, ok := (*Table)(nil).Price(context.Background(), "anything"); ok {
		t.Error("a nil table must price nothing")
	}
}

// Order in the file must not decide a lookup: the most specific entry wins.
func TestTableLongestMatchWins(t *testing.T) {
	tb, err := ParseTable([]byte(`
models:
  - {match: "aws/claude*", in: 99.0, out: 99.0}
  - {match: "aws/claude-sonnet-5", in: 1.52, out: 7.60}
`))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := tb.Price(context.Background(), "aws/claude-sonnet-5")
	if math.Abs(p.Input-1.52e-6) > 1e-15 {
		t.Fatalf("the family entry shadowed the specific one: in = %g", p.Input)
	}
	p, _ = tb.Price(context.Background(), "aws/claude-haiku-4-5")
	if math.Abs(p.Input-99e-6) > 1e-14 {
		t.Fatalf("the family entry did not cover an unlisted member: in = %g", p.Input)
	}
}

// A price list that loads WRONG is worse than one that fails: a mistyped key or a
// zero rate reads downstream as "this model is free".
func TestTableRejectsFilesThatWouldPriceSomethingFree(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown key":   `models: [{match: "m", input: 1.0, out: 2.0}]`,
		"no rates":      `models: [{match: "m", note: "todo"}]`,
		"no match":      `models: [{in: 1.0, out: 2.0}]`,
		"negative":      `models: [{match: "m", in: -1.0, out: 2.0}]`,
		"not a mapping": `[1,2,3]`,
	} {
		if _, err := ParseTable([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The shipped list is the one every hosted cost figure depends on, so a typo in it
// is a wrong dashboard rather than a failed build. Parse it here.
func TestShippedPriceListLoads(t *testing.T) {
	tb, err := LoadTable("../../deploy/service/prices.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if tb.Len() < 20 {
		t.Fatalf("only %d entries", tb.Len())
	}
	for id, wantIn := range map[string]float64{
		"aws/claude-sonnet-5": 1.52, "aws/claude-opus-5": 3.80,
		"premium-ide": 3.00, "gcp/gemini-3-pro-preview": 2.00,
	} {
		p, ok := tb.Price(context.Background(), id)
		if !ok {
			t.Errorf("%s: not priced", id)
			continue
		}
		if math.Abs(p.Input-wantIn/1e6) > 1e-15 {
			t.Errorf("%s: in = $%.4f/MTok, want $%.2f", id, p.Input*1e6, wantIn)
		}
	}
}

// The exact model ids ete-litellm serves, read from its /v1/models on 2026-08-19, plus
// the tier names Bob puts on the wire. Every one of them must price, because an id that
// does not is a dashboard row whose cost reads "unknown" — the Bob symptom this file
// exists to fix. Note the gateway's own casing (`Azure/gpt-4o`) and the ids it serves
// with no provider prefix at all (`claude-opus-4-8`): both used to miss.
func TestEveryGatewayModelIsPriced(t *testing.T) {
	tb, err := LoadTable("../../deploy/service/prices.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"aws/claude-opus-4-7",
		"claude-haiku-4-5-20251001",
		"aws/us.claude-opus-4-7",
		"Azure/gpt-4o",
		"gemini-2.5-pro",
		"azure/gpt-5.6-terra",
		"Azure/gpt-5-nano-2025-08-07",
		"aws/claude-sonnet-5",
		"aws/claude-sonnet-4-5",
		"Azure/gpt-5-2025-08-07",
		"aws/claude-haiku-4-5",
		"azure/gpt-5.3-chat",
		"gcp/gemini-3-pro-preview",
		"gcp/gemini-3.1-pro-preview",
		"azure/gpt-5.5",
		"aws/gpt-oss-120b",
		"azure/gpt-5.6-sol",
		"claude-opus-4-8",
		"GCP/gemini-2.0-flash",
		"claude-opus-4-6",
		"azure/gpt-5.4",
		"claude-sonnet-4-6",
		"azure/gpt-5.6-luna",
		"gcp/gemini-3-flash-preview",
		"Azure/gpt-5.1-codex-2025-11-13",
		"rits/google/gemma-4-31B",
		"claude-sonnet-4-5-20250929",
		"Azure/gpt-5-mini-2025-08-07",
		"Azure/gpt-4.1",
		"azure/gpt-5.3-codex",
		"gemini-2.5-flash",
		"aws/claude-opus-5",
		"gcp/gemini-3.6-flash",
		"gcp/gemini-3.5-flash-lite",
		"premium",
		"premium-ide",
		"standard",
		"fast",
		"openai/gpt-oss-20b",
	} {
		p, ok := tb.Price(context.Background(), id)
		if !ok || p.Zero() {
			t.Errorf("%s is unpriced: a request on it reports cost unknown", id)
		}
	}
}

// Two ways a lookup used to pick the wrong entry, both measured against the SHIPPED list
// because both produced a confidently wrong price rather than a miss.
func TestSpecificEntryBeatsAFamilyRegardlessOfMatchKind(t *testing.T) {
	tb, err := LoadTable("../../deploy/service/prices.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id     string
		wantIn float64 // $/MTok
		why    string
	}{
		// `gemini-2.5*` (flash, $0.30) used to beat `gemini-2.5-pro` ($1.25) here, because
		// the family entry was reached in an earlier PASS than the specific one.
		{"gcp/gemini-2.5-pro-preview-05-06", 1.25, "a Pro deployment priced at Flash's rate"},
		{"gemini-2.5-pro", 1.25, "the bare Pro id"},
		{"gemini-2.5-flash-8b", 0.30, "an unlisted Flash member still gets the family"},
		// Bob's tier names are ordinary English words; containment on them claimed
		// unrelated ids and reported ok=true, so the public map was never consulted.
		{"premium-ide", 3.00, "Bob's own tier still resolves"},
	} {
		p, ok := tb.Price(context.Background(), tc.id)
		if !ok {
			t.Errorf("%s: unpriced (%s)", tc.id, tc.why)
			continue
		}
		if math.Abs(p.Input-tc.wantIn/1e6) > 1e-15 {
			t.Errorf("%s: in = $%.2f/MTok, want $%.2f — %s", tc.id, p.Input*1e6, tc.wantIn, tc.why)
		}
	}
	// These must NOT match anything in the file: falling through to the public map is the
	// correct answer, and a wrong price that reads `complete` is worse than "unknown".
	for _, id := range []string{
		"azure/gpt-5.2-fast", "gpt-5-fast-preview", "standard-diffusion-xl", "my-premium-model",
	} {
		if p, ok := tb.Price(context.Background(), id); ok {
			t.Errorf("%s matched a Bob tier and was priced at $%.2f/MTok in", id, p.Input*1e6)
		}
	}
}
