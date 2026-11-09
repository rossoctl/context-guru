package plugin

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// A sub-agent launched with no type of its own is `general-purpose`, and many run at once: the band showed a row of
// identical `general-purpose` chips. Such an agent is named by its description, as Claude Code's agents list names it.
// A typed agent keeps its type, and a teammate its name.
func TestAGeneralPurposeAgentIsNamedByItsDescription(t *testing.T) {
	requireTool(t, "python3")
	cases := []struct{ meta, want string }{
		{`{"agentType":"general-purpose","description":"Strict main-track  review\nround 4"}`, "Strict main-track review round 4"},
		{`{"agentType":"general-purpose"}`, "general-purpose"},
		{`{"agentType":"general-purpose","description":"   "}`, "general-purpose"},
		{`{"agentType":"Explore","description":"find the parser"}`, "Explore"},
		{`{"agentType":"general-purpose","name":"reviewer","teamName":"t","description":"x"}`, "reviewer"},
	}
	scripts, err := filepath.Abs("scripts")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		out, err := exec.Command("python3", "-I", "-c",
			"import json,sys; sys.path.insert(0, sys.argv[1]); import statusline as s; print(json.dumps(s._meta_label(json.loads(sys.argv[2]))))",
			scripts, c.meta).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.meta, err, out)
		}
		var got string
		if err := json.Unmarshal(out, &got); err != nil || got != c.want {
			t.Errorf("_meta_label(%s) = %s, want %q", c.meta, out, c.want)
		}
	}
}
