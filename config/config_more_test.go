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
