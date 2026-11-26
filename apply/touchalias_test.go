package apply

import (
	"context"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
)

// ISSUE #902. The request's cold check reads cg:seen:<alias> with the alias of the body as it ARRIVED.
// Tool deferral then appends the names list to messages[1]; when that is the first system message,
// the alias of the body that goes upstream (the one the keeper holds) is another key. A ping that
// derived its key from the upstream body wrote a clock the request never read.

// deferredShape is a request shaped like Claude Code with ultrahigh deferral: messages[1] is a system
// message, so the names list lands in the head that SessionHead hashes.
const deferredShape = `{"model":"claude-sonnet-5","tools":[` +
	`{"name":"Read","description":"r","input_schema":{"type":"object"}},` +
	`{"name":"Bash","description":"b","input_schema":{"type":"object"}},` +
	`{"name":"mcp__x__one","description":"o","input_schema":{"type":"object"}},` +
	`{"name":"mcp__x__two","description":"t","input_schema":{"type":"object"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"fix the failing test",` +
	`"cache_control":{"type":"ephemeral"}}]},` +
	`{"role":"system","content":"the environment block"},` +
	`{"role":"user","content":"go on"}]}`

// insertedShape: messages[1] is NOT a system message, so placeList INSERTS a new system message at
// index 1, and the first system message (the alias input) goes from none to the names list.
const insertedShape = `{"model":"claude-sonnet-5","tools":[` +
	`{"name":"Read","description":"r","input_schema":{"type":"object"}},` +
	`{"name":"Bash","description":"b","input_schema":{"type":"object"}},` +
	`{"name":"mcp__x__one","description":"o","input_schema":{"type":"object"}},` +
	`{"name":"mcp__x__two","description":"t","input_schema":{"type":"object"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"fix the failing test",` +
	`"cache_control":{"type":"ephemeral"}}]},` +
	`{"role":"assistant","content":"ok"},{"role":"user","content":"go on"}]}`

var shapes = map[string]string{"messages[1] is system (list appended)": deferredShape,
	"messages[1] is not system (list inserted)": insertedShape}

func deferBody(t *testing.T) (pre, post []byte) { return deferBodyOf(t, deferredShape) }

func deferBodyOf(t *testing.T, shape string) (pre, post []byte) {
	t.Helper()
	pre = []byte(shape)
	post, names, ok := tooldefer.Rewrite(pre, map[string]bool{"mcp__x__one": true, "mcp__x__two": true})
	if !ok || len(names) == 0 {
		t.Fatalf("fixture: tooldefer.Rewrite deferred nothing (ok=%v names=%v)", ok, names)
	}
	return pre, post
}

func TestDeferralChangesTheAliasTheKeeperWouldDerive(t *testing.T) {
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			pre, post := deferBodyOf(t, shape)
			a, b := aliasFor(t, "t1", pre), aliasFor(t, "t1", post)
			if a == b {
				t.Fatalf("the alias is the same (%s) before and after deferral: the cause of #902 is something else", a)
			}
		})
	}
}

// coldAfterPing runs the real request path twice around a ping touch and returns the second
// request's ColdCache. The ping lands 4m after the first request, the second request 8m after it:
// without the ping that is past TTL + margin, with it the entry is 4m old. The client's own text is
// the same on every turn, so only the deferral moves the alias.
func coldAfterPing(t *testing.T, shape string, touch func(st store.Store, first Result, pre, post []byte, at int64)) bool {
	t.Helper()
	pre, post := deferBodyOf(t, shape)
	st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
	var seen []bool
	pipe := components.NewPipeline([]components.Component{coldSpy{seen: &seen}}, nil)
	base := time.Unix(1_700_000_000, 0)
	run := func(at time.Time) Result {
		return BodyOpts(context.Background(), pipe, st, Opts{
			Provider: bschemas.Anthropic, Body: pre, Tracker: tr, Now: at,
		})
	}
	first := run(base)
	touch(st, first, pre, post, base.Add(4*time.Minute).UnixMilli())
	run(base.Add(8 * time.Minute))
	if len(seen) != 2 {
		t.Fatalf("coldspy saw %d runs, want 2", len(seen))
	}
	return seen[1]
}

// The old keeper path: the key is derived from the body that went upstream.
func TestAPingTouchDerivedFromTheDeferredBodyMissesTheRequestsClock(t *testing.T) {
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			cold := coldAfterPing(t, shape, func(st store.Store, _ Result, _, post []byte, at int64) {
				RecordCacheTouch(st, "", post, bschemas.Anthropic, at)
			})
			if !cold {
				t.Fatal("expected the old derivation to read cold (this documents #902); it read warm")
			}
		})
	}
}

// The fix: the ping touches the alias apply put on the Trace of the arriving request.
func TestAPingTouchOnADeferredBodyKeepsTheNextRequestWarm(t *testing.T) {
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			cold := coldAfterPing(t, shape, func(st store.Store, first Result, pre, _ []byte, at int64) {
				key := first.Trace.TouchAlias
				if want := aliasFor(t, "", pre); key == "" || key != want {
					t.Fatalf("the ping's key %q is not the key the cold check reads %q", key, want)
				}
				RecordCacheTouchAlias(st, key, at)
			})
			if cold {
				t.Fatal("the next request read COLD 4m after a ping: the sweep would cut a live prefix")
			}
		})
	}
}

// xhigh defers nothing: the body is unchanged, so both derivations agree and RecordCacheTouch still works.
func TestAPingTouchOnAnUndeferredBodyStillKeepsTheNextRequestWarm(t *testing.T) {
	cold := coldAfterPing(t, deferredShape, func(st store.Store, _ Result, pre, _ []byte, at int64) {
		RecordCacheTouch(st, "", pre, bschemas.Anthropic, at)
	})
	if cold {
		t.Fatal("an undeferred body's ping no longer keeps the next request warm")
	}
}
