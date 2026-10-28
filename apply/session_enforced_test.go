package apply_test

import (
	"context"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/skills"
	"github.com/rossoctl/context-guru/store"
)

// The session id is hashed from the head system message. toolfilter's skills half rewrites that
// message before the id is derived, so an id derived from the raw body is not the pipeline's id.
// A mark written under the raw id would never be read (#175).
func TestEnforcedSessionIDForMatchesThePipelineWithTheSkillsHalfActive(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5",` +
		`"tools":[{"name":"Skill","description":"Invoke a skill.","input_schema":{"type":"object"}}],` +
		`"messages":[` +
		`{"role":"system","content":"<system-reminder>\nThe following skills are available for use with the Skill tool:\n\n` +
		`- dataviz: Use for charts and plots.\n- deep-research: A research harness.\n</system-reminder>"},` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":"again"}]}`)
	p, err := pipe(t, "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: ["+skills.RemovePrefix+"deep-research]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}),
		apply.Opts{Provider: bschemas.Anthropic, Body: body, Tenant: "t"})
	if res.Session == "" {
		t.Fatal("the pipeline derived no session")
	}
	raw := apply.SessionIDFor("t", "", bschemas.Anthropic, body)
	if raw == res.Session {
		t.Fatal("fixture does not exercise the bug: the raw id equals the pipeline's id")
	}
	if got := apply.EnforcedSessionIDFor(p, "t", "", "", bschemas.Anthropic, false, body); got != res.Session {
		t.Fatalf("EnforcedSessionIDFor = %q, pipeline = %q", got, res.Session)
	}
}
