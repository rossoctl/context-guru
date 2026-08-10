package apply_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
)

func modePipe(t *testing.T, yaml string) *components.Pipeline {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func dupJSON(t *testing.T) []byte {
	t.Helper()
	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	b, err := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []map[string]any{
			{"role": "user", "content": "go"},
			{"role": "tool", "tool_call_id": "a", "content": dump},
			{"role": "tool", "tool_call_id": "b", "content": dump},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestModeDefaultsToSync: an Opts with no Mode must produce exactly what the explicit
// sync mode does, which is what the positional BodyFull has always produced.
func TestModeDefaultsToSync(t *testing.T) {
	pipe := modePipe(t, "pipeline: [dedup]\n")
	body := dupJSON(t)

	legacy, changed := apply.BodyFull(context.Background(), pipe, store.NewMemory(store.Options{}),
		bschemas.OpenAI, body, "s", false, components.ModelSpec{}, 0, "auto")
	if !changed {
		t.Fatal("the legacy entry point compacted nothing; the comparison is vacuous")
	}
	res := apply.BodyOpts(context.Background(), modePipe(t, "pipeline: [dedup]\n"),
		store.NewMemory(store.Options{}),
		apply.Opts{Provider: bschemas.OpenAI, Body: body, Session: "s", CacheMode: "auto"})
	if !bytes.Equal(legacy, res.Body) {
		t.Fatalf("BodyOpts default differs from BodyFull\n legacy: %s\n opts:   %s", legacy, res.Body)
	}
}

// TestAsyncInlinePassMakesNoModelCall: the whole point of async is that the request
// path does not wait on an LLM. The inline pass must therefore be handed no model,
// while the deferred pass is.
func TestAsyncInlinePassMakesNoModelCall(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	m := fakeModel{fn: func() string {
		mu.Lock()
		calls++
		mu.Unlock()
		return "the agent read a file twice"
	}}
	// keep_last/min_tokens/start_from_message lowered so the tiny fixture is eligible;
	// the point of the test is WHICH pass gets a model client, not the gating.
	yaml := "pipeline: [summarize]\ncomponents:\n  summarize:\n    keep_last: 1\n    min_tokens: 10\n    start_from_message: 1\n    model:\n      source: config\n"
	body := dupJSON(t)

	apply.BodyOpts(context.Background(), modePipe(t, yaml), store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, Body: body, Session: "s",
		Models: components.ModelSpec{Static: m}, Mode: components.ModeAsync,
	})
	mu.Lock()
	inline := calls
	mu.Unlock()
	if inline != 0 {
		t.Fatalf("the async inline pass made %d model call(s) — the latency it exists to remove", inline)
	}

	apply.BodyOpts(context.Background(), modePipe(t, yaml), store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, Body: body, Session: "s",
		Models: components.ModelSpec{Static: m}, Mode: components.ModeAsync, Deferred: true,
	})
	mu.Lock()
	deferred := calls
	mu.Unlock()
	if deferred == 0 {
		t.Fatal("the deferred pass made no model call either — no compaction would ever be computed")
	}
}

// TestStaleAsyncResultIsDiscardedEndToEnd wires the real pieces the proxy wires: a
// deferred run writes into a Buffer, the session advances underneath it, and the
// commit is refused — so not one byte of the stale result reaches the live store.
func TestStaleAsyncResultIsDiscardedEndToEnd(t *testing.T) {
	base := store.NewMemory(store.Options{})
	tr := modes.NewTracker(0)
	pipe := modePipe(t, "pipeline: [dedup]\n")
	body := dupJSON(t)

	// Turn 1 records the generation the deferred job will be built from.
	inline := apply.BodyOpts(context.Background(), pipe, base, apply.Opts{
		Provider: bschemas.OpenAI, Body: body, Session: "s", CacheMode: "on",
		Mode: components.ModeAsync, Tracker: tr,
	})

	// A newer TURN ships, superseding the job's snapshot. This is the realistic path and
	// the one that used to be broken: the generation advanced only on commit, so a job
	// from turn 1 read its own generation as current no matter how many turns had
	// shipped, and committed against a transcript long since replaced.
	tr.Turn("s", 99)

	// Now the older job finishes.
	buf := store.NewBuffer(base)
	prev := inline.PrevLen
	res := apply.BodyOpts(context.Background(), pipe, buf, apply.Opts{
		Provider: bschemas.OpenAI, Body: body, Session: "s", CacheMode: "on",
		Mode: components.ModeAsync, Deferred: true, PrevLen: &prev,
	})
	if !res.Changed || buf.Writes() == 0 {
		t.Fatal("the deferred run produced nothing; the discard test proves nothing")
	}
	if tr.CommitIfCurrent("s", inline.Generation, buf.Commit) {
		t.Fatal("a STALE async result was applied")
	}
	// The buffer still holds every write, which IS the proof that none reached the live
	// store: Commit is the only path there, and it never ran.
	if buf.Writes() == 0 {
		t.Fatal("the buffer drained even though the commit was refused")
	}
	// And the generation really had moved on, so the discard was not vacuous.
	if tr.Gen("s") == inline.Generation {
		t.Fatal("the generation never advanced, so nothing was ever stale")
	}
}

// TestBufferIsolatesUntilCommit: the buffer is what makes "discard a stale result"
// possible at all — without it a deferred run's writes land as it goes and cannot be
// taken back.
func TestBufferIsolatesUntilCommit(t *testing.T) {
	base := store.NewMemory(store.Options{})
	base.Put("pre", []byte("existing"))
	buf := store.NewBuffer(base)

	buf.Put("k", []byte("v"))
	buf.MarkSticky("s", "id")
	if _, ok := base.Get("k"); ok {
		t.Fatal("a buffered write reached the base store before Commit")
	}
	if v, ok := buf.Get("k"); !ok || string(v) != "v" {
		t.Fatal("the buffer cannot read its own write")
	}
	if v, ok := buf.Get("pre"); !ok || string(v) != "existing" {
		t.Fatal("the buffer does not fall through to the base store")
	}
	if _, ok := base.Sticky("s")["id"]; ok {
		t.Fatal("a buffered sticky mark reached the base store before Commit")
	}

	buf.Commit()
	if v, ok := base.Get("k"); !ok || string(v) != "v" {
		t.Fatal("Commit did not flush")
	}
	if _, ok := base.Sticky("s")["id"]; !ok {
		t.Fatal("Commit did not flush sticky marks")
	}
	if buf.Writes() != 0 {
		t.Fatal("Commit did not drain the buffer")
	}

	// Discard: never committed, never visible.
	d := store.NewBuffer(base)
	d.Put("gone", []byte("x"))
	if _, ok := base.Get("gone"); ok {
		t.Fatal("an uncommitted write is visible")
	}
}

// TestSessionOfMatchesApply: the async worker keys jobs by the session id apply
// resolves, so the two must agree — including on the content-hash fallback.
func TestSessionOfMatchesApply(t *testing.T) {
	pipe := modePipe(t, "pipeline: [dedup]\n")
	body := dupJSON(t)
	res := apply.BodyOpts(context.Background(), pipe, store.NewMemory(store.Options{}),
		apply.Opts{Provider: bschemas.OpenAI, Body: body}) // no explicit session
	if got := apply.SessionOf(bschemas.OpenAI, body, ""); got != res.Session {
		t.Fatalf("SessionOf resolved %q, apply used %q", got, res.Session)
	}
}

// --- helpers ----------------------------------------------------------------

type fakeModel struct{ fn func() string }

func (f fakeModel) Complete(context.Context, string) (string, error) { return f.fn(), nil }
