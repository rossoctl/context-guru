package config

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
)

func TestBuildUnknownComponentErrors(t *testing.T) {
	c, err := LoadBytes([]byte("pipeline: [nonesuch]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Build(nil); err == nil {
		t.Fatal("Build must error on an unregistered component")
	}
}

func TestBuildEmptyPipeline(t *testing.T) {
	c, _ := LoadBytes([]byte("preset: off\n"))
	p, err := c.Build(nil)
	if err != nil || p == nil {
		t.Fatalf("the off preset should build a no-op pipeline: %v", err)
	}
}

// TestBuildMarshalsComponentBlock exercises the components:<name> config-block
// marshal path in Build (the raw block is handed to the constructor).
func TestBuildMarshalsComponentBlock(t *testing.T) {
	c, err := LoadBytes([]byte("pipeline: [extract]\ncomponents:\n  extract: {min_tokens: 123}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Build(nil); err != nil {
		t.Fatalf("build with a component config block failed: %v", err)
	}
}

// Every preset must actually BUILD. A name-list is only a promise until the registry
// resolves it, and a rich preset's embedded YAML doc is only valid until something
// decodes it — both fail at BOOT in production, so a typo in either is otherwise found
// by a deployment that dies on startup rather than by CI.
func TestEveryPresetBuilds(t *testing.T) {
	registered := map[string]bool{}
	for _, n := range components.Names() {
		registered[n] = true
	}
	for name := range presets {
		t.Run(name, func(t *testing.T) {
			pipeline, _ := PresetPipeline(name)
			// Build ignores a removed name, so a preset naming one would pass the build below.
			for _, comp := range pipeline {
				if removedComponents[comp] || !registered[comp] {
					t.Fatalf("preset %q names %q, which is removed or not registered", name, comp)
				}
			}
			c, err := LoadBytes([]byte("preset: " + name + "\n"))
			if err != nil {
				t.Fatalf("LoadBytes(preset: %s): %v", name, err)
			}
			if _, err := c.Build(nil); err != nil {
				t.Fatalf("Build(preset: %s): %v", name, err)
			}
		})
	}
}

// A rich preset carries tuned per-component settings, but /compact?preset= resolves
// names through PresetPipeline, which reads the plain `presets` map. So every rich
// preset needs an entry there too, and the two pipelines must agree — otherwise the
// same preset name means one thing in a config file and another over HTTP.
func TestRichPresetsAgreeWithTheNameList(t *testing.T) {
	for name, doc := range presetConfigs {
		listed, ok := PresetPipeline(name)
		if !ok {
			t.Errorf("rich preset %q has no `presets` entry, so ?preset=%s will not resolve", name, name)
			continue
		}
		c, err := LoadBytes([]byte(doc))
		if err != nil {
			t.Errorf("rich preset %q does not parse: %v", name, err)
			continue
		}
		if strings.Join(c.Pipeline, ",") != strings.Join(listed, ",") {
			t.Errorf("preset %q disagrees with itself: rich doc %v vs name-list %v",
				name, c.Pipeline, listed)
		}
	}
}

func TestNewStoreNonNil(t *testing.T) {
	c, _ := LoadBytes([]byte("store: {ttl_seconds: 5}\n"))
	if c.NewStore() == nil {
		t.Fatal("NewStore returned nil")
	}
}

// The lossless trio leads every preset that does deterministic work. All three
// verify-then-adopt, so there is no risk argument for leaving one out — and both
// omissions this asserts against were real: textclean shipped in `general` alone while
// half of all corpus messages carried ANSI, and searchfold shipped in NO preset at all,
// fully written and round-trip verified, folding nothing.
func TestLosslessFoldsAreInEveryWorkingPreset(t *testing.T) {
	// off/summarize/agentdiet are excluded BY DESIGN: off is the A-B control, summarize
	// restructures the transcript alone, and agentdiet reproduces a published baseline
	// whose whole claim is what ONE reflection achieves — stacking folds beside it would
	// reduce the same outputs first and there would be nothing left to attribute.
	// `cache` is exempt for a reason the other three do not share: its whole product claim
	// is that it is ONE component, verifiable by reading one line of the presets map. Adding
	// format/textclean/searchfold to it would each be lossless in meaning and would still
	// cost the claim — a stranger deciding whether to route their agent through us can check
	// "nothing but a cache breakpoint moves" in a second, and cannot check four rewriters as
	// fast. See TestCachePresetIsCachesplitAlone, which holds the other side of that trade.
	// summarizer1509 is exempt for exactly summarize's reason — it IS summarize, configured as the
	// cache-unaware no-reuse reference arm, and it restructures the transcript alone.
	exempt := map[string]bool{"off": true, "summarize": true, "summarizer1509": true, "cache": true}
	for name, pipeline := range presets {
		if exempt[name] {
			continue
		}
		has := map[string]bool{}
		for _, c := range pipeline {
			has[c] = true
		}
		for _, want := range []string{"format", "textclean"} {
			if !has[want] {
				t.Errorf("preset %q is missing the lossless fold %q: %v", name, want, pipeline)
			}
		}
		// searchfold needs tool-output traffic to fold; `mcp` carries none worth folding.
		if name != "mcp" && !has["searchfold"] {
			t.Errorf("preset %q is missing searchfold: %v", name, pipeline)
		}
	}
}

// toon acted 0 times on 5,752 production requests and converted 0 candidates in 11.67M
// measured tokens, at 1.53 ms + one TextTokens call per tool message. It stays REGISTERED
// (tabular traffic can opt in) but no preset may pay for it by default.
//
// house/housellm are exempt because an operator asked for toon in them explicitly, knowing
// the number above. The measurement is not retracted and the rule still holds everywhere
// else: what an exemption buys is that adding toon to a THIRD preset still has to be a
// decision somebody makes here, rather than something that quietly spreads.
//
// conservative/medium/high/xhigh are exempt for the same reason as house/housellm: they carry
// house's or housellm's pipeline verbatim, toon included, rather than re-deriving it.
func TestToonIsInNoPreset(t *testing.T) {
	byOperatorRequest := map[string]bool{
		"house": true, "housellm": true,
		"conservative": true, "medium": true, "high": true, "xhigh": true,
	}
	for name, pipeline := range presets {
		if byOperatorRequest[name] {
			continue
		}
		for _, c := range pipeline {
			if c == "toon" {
				t.Errorf("preset %q still ships toon: %v", name, pipeline)
			}
		}
	}
	for name, doc := range presetConfigs {
		if byOperatorRequest[name] {
			continue
		}
		if strings.Contains(doc, "toon") {
			t.Errorf("rich preset %q still ships toon", name)
		}
	}
	// The exemption list may not name a preset that does not exist, and may not name one
	// that no longer ships toon. The first would silently widen the rule the next time
	// somebody adds a preset with that name; the second leaves a standing permission for a
	// component nobody is running any more, which is how an exemption outlives its reason.
	// Asserted because the first version of this check only covered the first case.
	for name := range byOperatorRequest {
		p, ok := presets[name]
		if !ok {
			t.Errorf("toon exemption names %q, which is not a preset", name)
			continue
		}
		has := false
		for _, c := range p {
			if c == "toon" {
				has = true
			}
		}
		if !has {
			t.Errorf("preset %q is exempted from the toon rule but no longer runs toon; "+
				"drop the exemption rather than leaving a standing permission", name)
		}
	}
}

// TestCachePresetIsCachesplitAlone guards the one preset whose CONTENT is its promise.
//
// `cache` is what the local-distribution funnel points a stranger at, and the pitch is
// exact: no content dropped, no `<<cg:HASH>>` marker written, no expand tool injected, no
// model called. That is not a property of cachesplit that survives company — every other
// component in the repo either rewrites JSON, offloads content, or calls a model, so ANY
// addition here converts a checkable claim into a trust-me claim, and the docs that make the
// claim (docs/how-to/choose-a-preset.md, the plugin's install skill) do not get to notice.
//
// It is also why `cache` is exempt from TestLosslessFoldsAreInEveryWorkingPreset. That
// exemption is only defensible while this test exists: without it, "cache is exempt from the
// folds rule" would read as permission to put anything at all in it.
func TestCachePresetIsCachesplitAlone(t *testing.T) {
	p, ok := presets["cache"]
	if !ok {
		t.Fatal("preset `cache` is gone; the local-distribution funnel and the install skill both name it")
	}
	if len(p) != 1 || p[0] != "cachesplit" {
		t.Fatalf("preset `cache` = %v, want exactly [cachesplit]: it is the only preset whose "+
			"losslessness is verifiable by reading one line, and every other component either "+
			"rewrites JSON, offloads content, or calls a model", p)
	}
	// The pipeline the proxy actually builds, not just the map literal: applyPreset and the
	// rich-preset path both sit between this map and the wire.
	built, ok := PresetPipeline("cache")
	if !ok {
		t.Fatal(`PresetPipeline("cache") did not resolve, so ?preset=cache would 400`)
	}
	if len(built) != 1 || built[0] != "cachesplit" {
		t.Fatalf(`PresetPipeline("cache") = %v, want [cachesplit]`, built)
	}
}

// The housellm per-output floor is a MEASUREMENT, so this guard carries the measurement rather than
// the number, and any future edit has to argue with the data instead of with a literal.
//
// Own cost per million tokens saved, by candidate size, over 873 production extraction calls:
//
//	<1k $79.75 · 1-2k $32.41 · 2-3k $14.27 · 3-5k $15.76 · 5-8k $10.70 · 8-15k $4.05 · >=15k $11.56
//
// A removed token is worth one cache_write NOW plus one cache_read on each of the k-1 LATER turns
// that replay it, and k is not a constant: per session it ran p10 3.80 / median 27.11 / p90 70.46
// over the 70 sessions that saved anything. At write $4.755 and read $0.3814 per MTok that is
// $5.82 (p10) / $14.71 (median) / $31.25 (p90).
//
// USE THE P90 AS THE CEILING, and this is the correction the first version of this guard needed: it
// named a constant `intervalTop` and gave it $14.71, which is the MEDIAN. With the median standing in
// for the top the rule below selects 5000; with the real p90 it selects 2000. The number moved three
// times on the same 873 calls -- an imported k=12 from another corpus, then k=12 again through a
// mislabelled band, then the median through this constant's NAME -- so treat the endpoint as the part
// most likely to be wrong.
//
// A LOWER BOUND, not an equality. A higher floor only ever DECLINES, so it cannot hurt the agent, and
// the bands are non-monotonic in cost (2-3k at $14.27 is cheaper than 3-5k at $15.76) which means the
// ordering is inside the noise of 25-359 calls per band. Asserting equality would pin the literal to
// a rule fitting that noise; asserting the bound pins only what the measurement actually supports.
func TestHousellmFloorClearsTheBandsThatLoseAtEveryK(t *testing.T) {
	const p90TokenValue = 31.25 // $/MTok on a TOP-DECILE session: the ceiling, not the median
	bands := []struct {
		from, to int // to == 0 marks the open-ended top band
		perMTok  float64
	}{
		{0, 1000, 79.75}, {1000, 2000, 32.41}, {2000, 3000, 14.27}, {3000, 5000, 15.76},
		{5000, 8000, 10.70}, {8000, 15000, 4.05}, {15000, 0, 11.56},
	}
	floor := 0
	for _, b := range bands {
		// The open-ended band has no upper edge to raise a floor to, so it can never set one --
		// and treating its sentinel as a token count would demand a floor of that size and
		// silently disable the component.
		if b.to == 0 || b.perMTok <= p90TokenValue {
			continue
		}
		if b.to > floor {
			floor = b.to // loses even on a top-decile session
		}
	}
	if floor <= 0 || floor >= 15000 {
		t.Fatalf("derived floor %d is not a real band edge; the table or the ceiling is wrong", floor)
	}
	cfg, err := LoadBytes([]byte("preset: housellm\n"))
	if err != nil {
		t.Fatalf("load housellm preset: %v", err)
	}
	node, ok := cfg.Components["extract_llm"]
	if !ok {
		t.Fatal("housellm no longer configures extract_llm; this guard is testing nothing")
	}
	var got struct {
		MinTokens int `yaml:"min_tokens"`
	}
	if err := node.Decode(&got); err != nil {
		t.Fatalf("decode extract_llm block: %v", err)
	}
	if got.MinTokens < floor {
		t.Errorf("housellm ships min_tokens: %d, below the measured floor of %d. Bands under %d cost "+
			"$32.41-$79.75 per MTok saved against $%.2f on even a top-decile session, so a call there "+
			"loses money whatever k turns out to be. Move the table with a new measurement, not the "+
			"literal alone.", got.MinTokens, floor, floor, p90TokenValue)
	}
}

// `expand.fixed_restore` reaches the pipeline every host holds per tenant, and is off unless set.
func TestExpandFixedRestoreReachesThePipeline(t *testing.T) {
	for doc, want := range map[string]bool{
		"preset: off\n": false,
		"preset: off\nexpand:\n  fixed_restore: true\n": true,
	} {
		c, err := LoadBytes([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		pipe, err := c.Build(nil)
		if err != nil {
			t.Fatal(err)
		}
		if pipe.FixedRestore() != want {
			t.Errorf("%q: FixedRestore() = %v, want %v", doc, pipe.FixedRestore(), want)
		}
	}
	if _, err := LoadBytes([]byte("expand:\n  fixed_restor: true\n")); err == nil {
		t.Error("a typo'd expand key was accepted")
	}
}

// A stored config that still names a removed component (or the removed agentdiet preset) must
// load, build, and run without it: warn and ignore, never fail the proxy at boot.
func TestRemovedComponentsAreIgnoredNotFatal(t *testing.T) {
	for _, doc := range []string{
		"pipeline: [format, failed_run, collapse, mask, linecap, smartcrush, readlifecycle, coref, agentdiet, skeleton, extract]\n" +
			"components:\n  collapse: {max_tokens: 3000}\n  mask: {keep_recent: 3}\n  extract: {min_tokens: 400}\n",
		"preset: agentdiet\n",
	} {
		c, err := LoadBytes([]byte(doc))
		if err != nil {
			t.Fatalf("load: %v\n%s", err, doc)
		}
		for _, n := range c.Pipeline {
			if removedComponents[n] {
				t.Errorf("removed component %q left in the pipeline %v", n, c.Pipeline)
			}
		}
		if _, ok := c.Components["collapse"]; ok {
			t.Error("a removed component's config block was kept")
		}
		if _, err := c.Build(nil); err != nil {
			t.Fatalf("build: %v\n%s", err, doc)
		}
	}
	// Built straight from a struct (no LoadBytes), as library callers do.
	c := &Config{Pipeline: []string{"format", "failed_run", "extract"}}
	if _, err := c.Build(nil); err != nil {
		t.Fatalf("struct build: %v", err)
	}
	if got := strings.Join(c.Pipeline, ","); got != "format,extract" {
		t.Errorf("pipeline = %s, want format,extract", got)
	}
}

// /compact?preset=agentdiet must behave like `preset: agentdiet` in a config: empty pipeline, not an error.
func TestRemovedPresetResolvesToEmptyPipeline(t *testing.T) {
	p, ok := PresetPipeline("agentdiet")
	if !ok || len(p) != 0 {
		t.Fatalf("PresetPipeline(agentdiet) = %v, %v; want empty, true", p, ok)
	}
}
