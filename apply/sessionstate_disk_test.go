package apply_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
	_ "modernc.org/sqlite"
)

// After a restart (a new store on the same file), the first request of a session restores the
// session's state before the pipeline reads it. A session that does not send a request is not
// restored.
func TestBodyOptsLoadsSessionStateFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	open := func() (*store.Memory, *store.Disk) {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		d, err := store.OpenDisk(db, store.DiskOptions{})
		if err != nil {
			t.Fatal(err)
		}
		m := store.NewMemory(store.Options{StateTTLSeconds: 86400})
		m.AttachDisk(d)
		return m, d
	}
	m1, d1 := open()
	apply.RecordBilledInput(m1, "sess-769", 946000)
	m1.Put(store.SumPrefix+"sess-769", []byte("CHECKPOINT"))
	m1.Put(store.SumPrefix+"never-returns", []byte("X"))
	d1.Close()

	m2, d2 := open()
	defer d2.Close()
	if _, ok := m2.Get(store.BilledPrefix + "sess-769"); ok {
		t.Fatal("state in memory before any request")
	}
	res := runTurn(t, apply.Opts{Session: "sess-769"}, m2, compactBody(t, "a", 6))
	if res.Trace.Session != "sess-769" {
		t.Fatalf("session = %q", res.Trace.Session)
	}
	if b, ok := m2.Get(store.BilledPrefix + "sess-769"); !ok || string(b) != "946000" {
		t.Fatalf("billed input not restored by the first request: %q %v", b, ok)
	}
	if b, ok := m2.Get(store.SumPrefix + "sess-769"); !ok || string(b) != "CHECKPOINT" {
		t.Fatalf("checkpoint not restored: %q %v", b, ok)
	}
	if _, ok := m2.Get(store.SumPrefix + "never-returns"); ok {
		t.Fatal("a session that sent no request was loaded")
	}
}

// loadSpy records the sessions apply asks the store to load.
type loadSpy struct {
	*store.Memory
	loaded []string
}

func (s *loadSpy) LoadSession(session string) { s.loaded = append(s.loaded, session) }

// A request with an explicit session id also loads the ALIAS id (the content hash that the idle
// clock cg:seen: and cg:ttl: are keyed by), on both routes. Without the alias load, a session that
// comes back after a restart reads a cold clock.
func TestBothRoutesLoadTheAliasSession(t *testing.T) {
	chat := []byte(`{"model":"claude-sonnet-5","system":"sys","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":"more"}]}`)
	resp := []byte(`{"model":"gpt-5","instructions":"sys","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"more"}]}]}`)
	cases := []struct {
		name     string
		provider bschemas.ModelProvider
		api      string
		body     []byte
	}{
		{"chat", bschemas.Anthropic, "", chat},
		{"responses", bschemas.OpenAI, "responses", resp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy := &loadSpy{Memory: store.NewMemory(store.Options{})}
			apply.BodyOpts(context.Background(), components.NewPipeline(nil, nil), spy, apply.Opts{
				Provider: c.provider, API: c.api, Body: c.body, Session: "explicit-1",
				Tracker: modes.NewTracker(0), CacheMode: "on", Now: time.Unix(1_700_000_000, 0),
			})
			var sawExplicit, sawAlias bool
			for _, s := range spy.loaded {
				if s == "explicit-1" {
					sawExplicit = true
				} else if s != "" {
					sawAlias = true
				}
			}
			if !sawExplicit || !sawAlias {
				t.Fatalf("loaded %v: want the explicit id and the alias id", spy.loaded)
			}
		})
	}
}
