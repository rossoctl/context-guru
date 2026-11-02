package apply

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// restoredSpy records Ctx.Restored as the pipeline hands it over.
type restoredSpy struct{ seen *map[string]string }

func (restoredSpy) Name() string                 { return "restoredspy" }
func (restoredSpy) Enabled(*components.Ctx) bool { return true }
func (s restoredSpy) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	*s.seen = c.Restored
	return nil
}

const restoreBody = `{"model":"claude-x","messages":[{"role":"user","content":"go"},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ORIGINAL OUTPUT"}]},` +
	`{"role":"assistant","content":[{"type":"text","text":"done"}]},{"role":"user","content":"next"}]}`

// THE TWO HALVES ARE DECIDED TOGETHER. An anchor whose original is no longer in the store can
// insert nothing, so it must not suppress kept-verbatim either — or the original stays compacted
// with no copy anywhere, which is content the model cannot read.
func TestARestoreIsPlannedOnlyWhenItCanBeInserted(t *testing.T) {
	msgs := gjson.Get(restoreBody, "messages").Array()
	for _, tc := range []struct {
		name  string
		stash bool
	}{{"stash present", true}, {"stash gone", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var seen map[string]string
			pipe := components.NewPipeline([]components.Component{restoredSpy{&seen}}, nil)
			pipe.SetFixedRestore(true)
			st := store.NewMemory(store.Options{})
			sess := SessionIDFor("", "", bschemas.Anthropic, []byte(restoreBody))
			expand.RecordAnchor(st, sess, expand.Anchor{ID: "H1", At: 2, Fp: expand.Fingerprint(msgs[1])})
			if tc.stash {
				st.Put("H1", []byte("ORIGINAL OUTPUT"))
			}
			res := BodyOpts(context.Background(), pipe, st, Opts{Provider: bschemas.Anthropic, Body: []byte(restoreBody)})
			out := gjson.GetBytes(res.Body, "messages").Array()
			if !tc.stash {
				if seen != nil || len(out) != len(msgs) || res.Restored != 0 {
					t.Fatalf("with the stash gone: Restored=%v, %d messages (want nil, %d)", seen, len(out), len(msgs))
				}
				return
			}
			if seen[extract.ContentKey("ORIGINAL OUTPUT")] != "H1" {
				t.Fatalf("Ctx.Restored = %v, want the original's content key -> H1", seen)
			}
			if len(out) != len(msgs)+1 || !strings.Contains(out[2].Raw, "ORIGINAL OUTPUT") ||
				out[3].Raw != msgs[2].Raw || res.Restored != 1 || res.RestoredTokens == 0 {
				t.Fatalf("restored copy not inserted at the anchor: %s", res.Body)
			}
		})
	}
}

// A COUNT CHANGE BEFORE THE ANCHOR (summarize replacing a span) moves every later index by the
// same amount; the copy must follow its anchor's neighbour, and be appended when that cannot be
// proven.
func TestRestoreInsertFollowsTheAnchorThroughACountChange(t *testing.T) {
	client := gjson.Parse(`[{"role":"user","content":"0"},{"role":"user","content":"1"},` +
		`{"role":"assistant","content":"2"},{"role":"user","content":"3"},` +
		`{"role":"assistant","content":"4"},{"role":"user","content":"5"}]`).Array()
	plan := restorePlan{{anchor: expand.Anchor{ID: "H", At: 4}, original: "ORIG"}}
	// msgs 1..3 summarized into one: [0, S, 4, 5]
	out := []byte(`{"messages":[{"role":"user","content":"0"},{"role":"user","content":"S"},` +
		`{"role":"assistant","content":"4"},{"role":"user","content":"5"}]}`)
	nb, _, ok := plan.insert("openai", out, client)
	got := gjson.GetBytes(nb, "messages").Array()
	if !ok || len(got) != 5 || !strings.Contains(got[2].Raw, "ORIG") || got[3].Get("content").String() != "4" {
		t.Fatalf("copy not inserted right before the anchor's neighbour: %s", nb)
	}
	// The neighbour itself was rewritten, so the mapping is unprovable: append instead.
	moved := []byte(`{"messages":[{"role":"user","content":"0"},{"role":"user","content":"S"},` +
		`{"role":"assistant","content":"4 rewritten"},{"role":"user","content":"5"}]}`)
	nb, _, ok = plan.insert("openai", moved, client)
	got = gjson.GetBytes(nb, "messages").Array()
	if !ok || len(got) != 5 || !strings.Contains(got[4].Raw, "ORIG") {
		t.Fatalf("an unprovable mapping must append the copy, not guess: %s", nb)
	}
}

// The repair path skips an anchor when the client's answer IS the original. That answer can be an
// array of text blocks; compared as raw JSON it never matched, and an anchor was recorded.
func TestPlanRestoreSkipsAnArrayAnswerThatIsTheOriginal(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tu","name":"` + expand.ToolName + `","input":{"id":"H"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu","content":[{"type":"text","text":"ORIGINAL OUTPUT"}]}]}]}`)
	msgs := gjson.GetBytes(body, "messages").Array()
	pipe := components.NewPipeline(nil, nil)
	pipe.SetFixedRestore(true)
	st := store.NewMemory(store.Options{})
	st.Put("H", []byte("ORIGINAL OUTPUT"))
	planRestore(pipe, st, "sess", "anthropic", body, msgs)
	if a := expand.Anchors(st, "sess"); len(a) != 0 {
		t.Fatalf("an anchor was recorded for an answer that is the original: %+v", a)
	}
}
