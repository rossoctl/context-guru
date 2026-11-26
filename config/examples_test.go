package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Every config shipped under examples/ must pass the same check a settings-page save does.
// Nothing else loads them, so without this guard a field removal (model.base_url, api_key,
// provider, auth) left three of them failing at startup for anyone following the example.
func TestShippedExampleConfigsValidate(t *testing.T) {
	var paths []string
	for _, glob := range []string{"../examples/*/configs/*.yaml", "../examples/*/sample/configs/*.yaml"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, m...)
	}
	if len(paths) == 0 {
		t.Fatal("found no example configs; the globs no longer match the examples/ layout")
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(b); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
