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
	c, err := LoadBytes([]byte("pipeline: [collapse]\ncomponents:\n  collapse: {max_tokens: 123}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Build(nil); err != nil {
		t.Fatalf("build with a component config block failed: %v", err)
	}
}

// buildTagged names components that only register under a build tag, so a default
// pure-Go build legitimately does not have them. `skeleton` needs `cg_skeleton`
// (tree-sitter), and neither the Makefile nor CI passes that tag — so a preset naming
// it cannot build here. That is the tag, not a typo, which is why the test below skips
// buildTagged lists components that only exist in a tagged build. A preset naming one is a
// FAILURE, not an excuse (see the loop below) — this map exists so the failure can say why.
var buildTagged = map[string]bool{"skeleton": true}

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
			// NOT a skip. A preset is a promise that one word of config works, and it is
			// resolved in whatever binary the user is running — so a preset that names a
			// tag-gated component is broken FOR EVERY USER of the default build, which is
			// exactly the failure this test was written to catch. Skipping it is how
			// `preset: coding` shipped naming `skeleton` and died at boot with
			// `unknown component "skeleton"` for anyone who selected it.
			//
			// A tag-gated component is welcome to exist; a DEFAULT-BUILD preset may not
			// depend on one. If a tagged build ever needs its own preset, give it a
			// separate name declared in the tagged file.
			for _, comp := range pipeline {
				if buildTagged[comp] && !registered[comp] {
					t.Fatalf("preset %q names %q, which is behind a build tag and is not in this build: "+
						"every user selecting this preset gets a boot failure", name, comp)
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
	exempt := map[string]bool{"off": true, "summarize": true, "agentdiet": true, "cache": true}
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
func TestToonIsInNoPreset(t *testing.T) {
	byOperatorRequest := map[string]bool{"house": true, "housellm": true}
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

// linecap must sit immediately before cachesplit in every preset that runs it. The mechanism
// is asserted in components/offload (TestLinecapYieldsToAnEarlierOffloader), but the mistake
// that actually happened was an ORDERING one: every Offload leaves a marker and every Offload
// skips marker-bearing content, so a modest reducer ahead of a drastic one takes its candidate
// away. Measured on `general` over 1,795 real captured requests, linecap 7th saved 5,524,476
// tokens — worse than the 5,556,801 with no linecap at all — against 5,811,621 placed last.
func TestLinecapRunsLastAmongTheOffloaders(t *testing.T) {
	for name, pipeline := range presets {
		for i, c := range pipeline {
			if c != "linecap" {
				continue
			}
			if i != len(pipeline)-2 || pipeline[len(pipeline)-1] != "cachesplit" {
				t.Errorf("preset %q runs linecap at %d of %v; it must be immediately before "+
					"cachesplit, or it steals candidates from the heavier offloaders", name, i, pipeline)
			}
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
