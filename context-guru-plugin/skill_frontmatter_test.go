package plugin

import (
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Claude Code and Codex both parse a skill's front matter as YAML; Codex uses serde_yaml, which is strict.
// A value with a bare `: ` in it is not valid YAML, and a skill whose front matter does not parse may not
// load at all (#851). Every SKILL.md in both plugins must parse, and carry a name and a description.
func TestEverySkillFrontMatterIsValidYAML(t *testing.T) {
	for _, dir := range []string{
		"skills",
		filepath.Join("..", "codex-marketplace", "plugins", "context-guru", "skills"),
	} {
		files, _ := filepath.Glob(filepath.Join(dir, "*", "SKILL.md"))
		if len(files) < 10 {
			t.Fatalf("found %d skills in %s", len(files), dir)
		}
		for _, f := range files {
			body := readFileString(t, f)
			fm, ok := strings.CutPrefix(body, "---\n")
			if ok {
				fm, _, ok = strings.Cut(fm, "\n---\n")
			}
			if !ok {
				t.Errorf("%s has no front matter", f)
				continue
			}
			var m map[string]any
			if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
				t.Errorf("%s: front matter is not valid YAML: %v", f, err)
				continue
			}
			for _, key := range []string{"name", "description"} {
				if s, _ := m[key].(string); s == "" {
					t.Errorf("%s: front matter has no %s string", f, key)
				}
			}
		}
	}
}
