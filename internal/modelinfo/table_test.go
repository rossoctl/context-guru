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

// Regression test for #324. A bare id (no provider prefix) must resolve to the
// SPECIFIC qualified entry the operator wrote it under, not to a wildcard family
// that happens to also match — mirroring modelinfo.LiteLLM.fetch, which indexes the
// public map under both a model's full key and its bare tail. This fails on
// today's code: `claude-sonnet-5` falls past `aws/claude-sonnet-5` ($1.52) straight
// to `claude-sonnet*` ($2.28, "the 4-x rate"), a live 1.5x overcharge on the
// shipped list.
func TestBareIDResolvesToQualifiedEntryNotFamily324(t *testing.T) {
	tb, err := LoadTable("../../deploy/service/prices.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Price(context.Background(), "claude-sonnet-5")
	if !ok {
		t.Fatal("claude-sonnet-5 unpriced")
	}
	if math.Abs(p.Input-1.52e-6) > 1e-15 {
		t.Fatalf("bare claude-sonnet-5: in = $%.2f/MTok, want $1.52 (aws/claude-sonnet-5's own rate) — "+
			"got the claude-sonnet* family rate instead", p.Input*1e6)
	}
	// Second, smaller instance of the same root cause (#324): bare gpt-5.6-sol has no
	// wildcard family entry, so on today's code it skips Table entirely and would be
	// priced by the public LiteLLM map instead of azure/gpt-5.6-sol's explicit $5/$30.
	p, ok = tb.Price(context.Background(), "gpt-5.6-sol")
	if !ok {
		t.Fatal("gpt-5.6-sol unpriced — should resolve to azure/gpt-5.6-sol's own rate")
	}
	if math.Abs(p.Input-5.00e-6) > 1e-15 {
		t.Fatalf("bare gpt-5.6-sol: in = $%.2f/MTok, want $5.00 (azure/gpt-5.6-sol's own rate)", p.Input*1e6)
	}
}

// An operator who deliberately writes both a bare entry and a qualified one keeps
// their own distinction: the explicit bare entry is an exact match in pass one and
// must win over the tail derived from the qualified entry.
func TestExplicitBareEntryBeatsQualifiedTail(t *testing.T) {
	tb, err := ParseTable([]byte(`
models:
  - {match: "aws/claude-sonnet-5", in: 1.52, out: 7.60}
  - {match: "claude-sonnet-5",     in: 9.99, out: 40.00, note: "operator's own bare rate"}
`))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Price(context.Background(), "claude-sonnet-5")
	if !ok {
		t.Fatal("claude-sonnet-5 unpriced")
	}
	if math.Abs(p.Input-9.99e-6) > 1e-15 {
		t.Fatalf("in = $%.2f/MTok, want $9.99 (the explicit bare entry, not the qualified tail)", p.Input*1e6)
	}
}

// Two qualified entries under different providers sharing a bare tail, at DIFFERENT
// rates, is genuinely ambiguous: silently picking one is the exact mechanism that
// caused #324. The tail must not be indexed at all — it falls through to whatever
// family/containment match would otherwise apply, same as if neither existed.
func TestAmbiguousBareTailFallsThroughInsteadOfGuessing(t *testing.T) {
	tb, err := ParseTable([]byte(`
models:
  - {match: "aws/claude-sonnet-5",    in: 1.52, out: 7.60}
  - {match: "vertex/claude-sonnet-5", in: 2.00, out: 10.00}
  - {match: "claude-sonnet*",         in: 2.28, out: 11.40}
`))
	if err != nil {
		t.Fatal(err)
	}
	// The qualified ids themselves are unambiguous and still resolve exactly.
	if p, _ := tb.Price(context.Background(), "aws/claude-sonnet-5"); math.Abs(p.Input-1.52e-6) > 1e-15 {
		t.Fatalf("aws/claude-sonnet-5: in = $%.2f/MTok, want $1.52", p.Input*1e6)
	}
	// The bare id is ambiguous between the two providers' rates, so it must fall
	// through to the family entry rather than silently picking one.
	p, ok := tb.Price(context.Background(), "claude-sonnet-5")
	if !ok {
		t.Fatal("claude-sonnet-5 unpriced")
	}
	if math.Abs(p.Input-2.28e-6) > 1e-15 {
		t.Fatalf("ambiguous bare claude-sonnet-5: in = $%.2f/MTok, want $2.28 (the family fallback)", p.Input*1e6)
	}
}

// A bare id with a qualified entry under one provider, and no family wildcard at
// all, still resolves via the new tail index rather than reporting unknown.
func TestBareIDFallsToFamilyWhenNoQualifiedTailExists(t *testing.T) {
	tb, err := ParseTable([]byte(`
models:
  - {match: "aws/claude-opus-5",  in: 3.80, out: 19.00}
  - {match: "claude-opus*",       in: 3.80, out: 19.00}
`))
	if err != nil {
		t.Fatal(err)
	}
	// claude-opus-4-8 has no qualified entry of its own, only the family.
	p, ok := tb.Price(context.Background(), "claude-opus-4-8")
	if !ok {
		t.Fatal("claude-opus-4-8 unpriced")
	}
	if math.Abs(p.Input-3.80e-6) > 1e-15 {
		t.Fatalf("claude-opus-4-8: in = $%.2f/MTok, want $3.80 (the family fallback)", p.Input*1e6)
	}
}

// A bare id with neither a qualified entry nor a family wildcard is a genuine miss:
// Table must say ok=false so the Chain's next source (the public LiteLLM map) answers,
// rather than the request being silently priced at anything.
func TestBareIDWithNoTableEntryAtAllReportsUnknown(t *testing.T) {
	tb, err := ParseTable([]byte(`
models:
  - {match: "aws/claude-sonnet-5", in: 1.52, out: 7.60}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tb.Price(context.Background(), "gpt-5.6-sol"); ok {
		t.Fatal("gpt-5.6-sol matched something in a table that has no entry for it")
	}
}
