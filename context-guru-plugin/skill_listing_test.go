package plugin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every skill the model may offer on its own costs its description on every request of every session, in the
// main thread and in each subagent (#823). A skill the user runs only to see something is kept out of the
// model's listing: `disable-model-invocation: true` in Claude Code, `policy.allow_implicit_invocation: false`
// in a Codex skill's agents/openai.yaml. The user still runs it by name.
var displayOnlySkills = []string{"status", "dashboard", "chart", "fill", "heatmap", "timeline", "session",
	"watch", "doctor", "statusline", "uninstall", "update"}

// maxListedDescription is the longest description a skill in the model's listing may carry, in characters.
const maxListedDescription = 220

type skillFrontMatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

// readSkillFrontMatter parses the block between the first two `---` lines as YAML, the way Claude Code does.
// A value with a bare `: ` in it is not valid YAML, and a skill whose front matter does not parse is dropped.
func readSkillFrontMatter(t *testing.T, path string) skillFrontMatter {
	t.Helper()
	body := readFileString(t, path)
	fm, ok := strings.CutPrefix(body, "---\n")
	if ok {
		fm, _, ok = strings.Cut(fm, "\n---\n")
	}
	if !ok {
		t.Fatalf("%s has no front matter", path)
	}
	var m skillFrontMatter
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		t.Fatalf("%s: front matter is not valid YAML: %v", path, err)
	}
	return m
}

func TestDisplayOnlySkillsStayOutOfTheModelListing(t *testing.T) {
	hidden := map[string]bool{}
	for _, s := range displayOnlySkills {
		hidden[s] = true
	}
	skills, _ := filepath.Glob(filepath.Join("skills", "*", "SKILL.md"))
	if len(skills) < 10 {
		t.Fatalf("found %d skills", len(skills))
	}
	for _, f := range skills {
		name := filepath.Base(filepath.Dir(f))
		m := readSkillFrontMatter(t, f)
		if m.DisableModelInvocation != hidden[name] {
			t.Errorf("%s: disable-model-invocation=%v, want %v", f, m.DisableModelInvocation, hidden[name])
		}
		if !hidden[name] && len([]rune(m.Description)) > maxListedDescription {
			t.Errorf("%s: the model reads this %d-character description on every request; keep it under %d",
				f, len([]rune(m.Description)), maxListedDescription)
		}
		if !hidden[name] && !strings.Contains(m.Description, "Use when") {
			t.Errorf("%s: a listed skill needs a \"Use when\" clause; the model picks skills from it", f)
		}
	}
	codex := filepath.Join("..", "codex-marketplace", "plugins", "context-guru", "skills")
	dirs, _ := filepath.Glob(filepath.Join(codex, "*", "SKILL.md"))
	if len(dirs) < 10 {
		t.Fatalf("found %d Codex skills", len(dirs))
	}
	for _, f := range dirs {
		name := filepath.Base(filepath.Dir(f))
		var meta struct {
			Policy struct {
				AllowImplicitInvocation *bool `yaml:"allow_implicit_invocation"`
			} `yaml:"policy"`
		}
		b, err := os.ReadFile(filepath.Join(filepath.Dir(f), "agents", "openai.yaml"))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err == nil {
			if err := yaml.Unmarshal(b, &meta); err != nil {
				t.Fatalf("%s agents/openai.yaml: %v", name, err)
			}
		}
		listed := meta.Policy.AllowImplicitInvocation == nil || *meta.Policy.AllowImplicitInvocation
		if listed == hidden[name] {
			t.Errorf("Codex %s: in the model listing=%v, want %v", name, listed, !hidden[name])
		}
		if !listed {
			continue
		}
		// The description line is read as it is, not as YAML: strict parsing of the Codex tree is #851's test.
		m := regexp.MustCompile(`(?m)^description: (.*)$`).FindStringSubmatch(readFileString(t, f))
		if m == nil {
			t.Errorf("%s: no one-line description", f)
			continue
		}
		if n := len([]rune(m[1])); n > maxListedDescription {
			t.Errorf("%s: the model reads this %d-character description on every request; keep it under %d",
				f, n, maxListedDescription)
		}
		if !strings.Contains(m[1], "Use when") {
			t.Errorf("%s: a listed skill needs a \"Use when\" clause; the model picks skills from it", f)
		}
	}
}
