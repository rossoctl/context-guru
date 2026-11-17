package proxy

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
	_ "modernc.org/sqlite"
)

// hostedProcess stands for one hosted proxy process: a tenant source over the shared session-state
// table. A restart is a new process over the same database file.
func hostedProcess(t *testing.T, f *hostedFixture, db *sql.DB, withDisk bool) (*TenantSource, *store.Disk) {
	t.Helper()
	src := NewTenantSource(f.reg, nil, func(doc []byte, e components.Emitter) (BuiltConfig, error) {
		return BuiltConfig{Pipe: components.NewPipeline(nil, e),
			Store: store.NewMemory(store.Options{}), Preset: "test"}, nil
	}, 0)
	if !withDisk {
		return src, nil
	}
	d, err := store.OpenDisk(db, store.DiskOptions{MetadataOnly: true, FlushEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	src.SetSessionState(d, store.DefaultStateTTL)
	return src, d
}

func dedupGatesFor(t *testing.T, st store.Store, session, dump string) map[string]int {
	t.Helper()
	comp, err := components.New("dedup", []byte("min_tokens: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	mk := func() bschemas.ChatMessage {
		s := dump
		return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
			Content: &bschemas.ChatMessageContent{ContentStr: &s}}
	}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{mk(), mk()}}
	var rep components.Report
	if _, err := comp.(components.Offload).Offload(req, &rep, &components.Ctx{Session: session, Store: st}); err != nil {
		t.Fatal(err)
	}
	return rep.Gates
}

// #1104: a hosted restart keeps dedup's keep-in-full mark, and one tenant's mark is not another's.
func TestAHostedRestartKeepsTheKeepMarkAndTenantsStaySeparate(t *testing.T) {
	f := newHostedFixture(t, "up", "anthropic")
	a, _ := f.register(t, "a@ibm.com")
	b, _ := f.register(t, "b@ibm.com")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dump := strings.Repeat("verbose tool output line\n", 30)
	// The same client-supplied session id for both tenants: only the tenant scope tells them apart.
	sessA := session.Scoped(a.ID, "thread-1", "", "")
	sessB := session.Scoped(b.ID, "thread-1", "", "")

	src1, d1 := hostedProcess(t, f, db, true)
	ta, err := src1.ForTenant(a)
	if err != nil {
		t.Fatal(err)
	}
	offload.MarkKeptVerbatim(ta.Store, sessA, dump)
	// Another metadata key, not a keep mark: the whole metadata set survives, as on a local install.
	ta.Store.Put(store.BilledPrefix+sessA, []byte("123"))
	d1.Flush()

	// The restart.
	src2, _ := hostedProcess(t, f, db, true)
	ta2, err := src2.ForTenant(a)
	if err != nil {
		t.Fatal(err)
	}
	if ta2.Store == ta.Store {
		t.Fatal("the restart must give a new store")
	}
	ta2.Store.(store.SessionLoader).LoadSession(sessA)
	if g := dedupGatesFor(t, ta2.Store, sessA, dump); g[offload.GateKeptVerbatim] == 0 {
		t.Fatalf("the mark must survive a hosted restart: %v", g)
	}
	if v, ok := ta2.Store.Get(store.BilledPrefix + sessA); !ok || string(v) != "123" {
		t.Fatalf("another metadata key must survive too: %q %v", v, ok)
	}

	// Tenant b: same thread id, same bytes. No mark.
	tb2, err := src2.ForTenant(b)
	if err != nil {
		t.Fatal(err)
	}
	tb2.Store.(store.SessionLoader).LoadSession(sessB)
	if g := dedupGatesFor(t, tb2.Store, sessB, dump); g[offload.GateKeptVerbatim] != 0 {
		t.Fatalf("another tenant must not see the mark: %v", g)
	}
}

// Content keys never reach the hosted table.
func TestAHostedSessionStateTableHoldsNoContentKeys(t *testing.T) {
	f := newHostedFixture(t, "up", "anthropic")
	a, _ := f.register(t, "a@ibm.com")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	src, d := hostedProcess(t, f, db, true)
	ta, err := src.ForTenant(a)
	if err != nil {
		t.Fatal(err)
	}
	s := session.Scoped(a.ID, "thread-1", "", "")
	ta.Store.Put(store.SumPrefix+s, []byte("summary text"))
	ta.Store.Put(store.BilledPrefix+s, []byte("1"))
	d.Flush()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1 (the metadata key only)", n)
	}
}
