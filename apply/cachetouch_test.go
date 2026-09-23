package apply

import (
	"strconv"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
)

// A KEEP-ALIVE PING MAKES THE CACHE WARM AGAIN, and until this the cache-liveness clock never heard
// about it.
//
// A ping's whole job is to READ the cached prefix, and a read resets the entry's TTL. So after a
// successful ping the provider is holding that prefix afresh. `prevAt` came only from real requests,
// though, so on a kept-alive session the idle time grew without bound while the entry stayed alive —
// and summarize's gate concluded the cache was long dead.
//
// THE CONSEQUENCE WAS THE EXPENSIVE ONE. A strict clock test said "certainly cold", the then-shipped
// `cache_state: pre_expiry_or_cold` permitted a rewrite, and the component compacted a LIVE prefix on
// exactly the sessions someone is paying pings to protect. The gate's own docstring existed to avoid
// that outcome via Ctx.ColdCache, and then reproduced it through the timestamp underneath. Issue #243.
//
// THE TEST OUTLIVES THAT CONSUMER DELIBERATELY. The cold-gated cache states were withdrawn, so no
// shipped default reads this clock today — but `cache_state: pre_expiry` does, asking the opposite
// question ("is this prefix still LIVE?"), and it is wrong in the same direction if a ping goes
// unrecorded. Deleting this with the states would leave the write below unasserted.
func TestAPingRefreshesTheCacheLivenessClock(t *testing.T) {
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"the first user message, which the alias is derived from"},` +
		`{"role":"assistant","content":"ok"}]}`)

	const tenant = "t1"
	base := time.Now().UnixMilli()

	// A real request at t=0 establishes the clock.
	touchedAt := recordAndRead(t, st, tenant, body, base)
	if touchedAt != 0 {
		t.Fatalf("precondition: the first touch has nothing before it, got %d", touchedAt)
	}

	// Nine minutes pass with no real request — but a ping fires at t=8m and reads the entry.
	pingAt := base + (8 * time.Minute).Milliseconds()
	RecordCacheTouch(st, tenant, body, bschemas.Anthropic, pingAt)

	// The next reader of the clock must see the PING, not the nine-minute-old request.
	got := readSeen(t, st, tenant, body)
	if got != pingAt {
		t.Errorf("the cache-liveness clock reads %d, want the ping's %d. Without this a kept-alive "+
			"session's entry is judged dead while the provider is holding it, and the compaction "+
			"gate rewrites a live prefix", got, pingAt)
	}
}

// THE CLOCK IS MONOTONE. A ping racing a real request must not make the entry look OLDER than the
// request already proved it to be — later is warmer, and moving backwards is the one direction that
// costs money.
func TestAPingNeverMovesTheClockBackwards(t *testing.T) {
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello there"}]}`)
	const tenant = "t1"
	now := time.Now().UnixMilli()

	RecordCacheTouch(st, tenant, body, bschemas.Anthropic, now)
	RecordCacheTouch(st, tenant, body, bschemas.Anthropic, now-(5*time.Minute).Milliseconds())

	if got := readSeen(t, st, tenant, body); got != now {
		t.Errorf("clock = %d after a stale ping, want the later %d — a ping arriving out of order "+
			"must not age the entry", got, now)
	}
}

// AND IT LANDS ON THE KEY THE REQUEST PATH READS. The provider's entry is keyed on CONTENT, so a
// touch recorded under a different derivation would be written and never read — the failure mode
// SessionIDFor already had once, where two reasonable derivations of one key diverged and the figure
// underneath was silently always zero.
func TestThePingTouchLandsOnTheKeyTheRequestPathReads(t *testing.T) {
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"a distinctive first user message"},` +
		`{"role":"assistant","content":"ok"}]}`)
	const tenant = "t1"
	pingAt := time.Now().UnixMilli()

	RecordCacheTouch(st, tenant, body, bschemas.Anthropic, pingAt)

	// aliasSeen is what the request path calls. It must find the ping's timestamp.
	if got := recordAndRead(t, st, tenant, body, pingAt+1000); got != pingAt {
		t.Errorf("aliasSeen read %d, want the ping's %d. A touch written under a key the request "+
			"path does not read is invisible, and the defect looks exactly like the ping never "+
			"happening", got, pingAt)
	}
}

// A DIFFERENT SESSION'S PING MUST NOT WARM THIS ONE. The alias is content-derived and tenant-scoped,
// so two conversations are two keys.
func TestAPingWarmsOnlyItsOwnPrefix(t *testing.T) {
	st := store.NewMemory(store.Options{})
	a := []byte(`{"model":"m","messages":[{"role":"user","content":"conversation A opening line"}]}`)
	b := []byte(`{"model":"m","messages":[{"role":"user","content":"conversation B opening line"}]}`)
	pingAt := time.Now().UnixMilli()

	RecordCacheTouch(st, "t1", a, bschemas.Anthropic, pingAt)

	if got := readSeen(t, st, "t1", b); got != 0 {
		t.Errorf("conversation B's clock reads %d after a ping on A; a ping must warm only the "+
			"prefix it actually read", got)
	}
	// And the same content under a different TENANT is a different key.
	if got := readSeen(t, st, "t2", a); got != 0 {
		t.Errorf("tenant t2's clock reads %d after t1's ping; the alias is tenant-scoped", got)
	}
}

// recordAndRead calls the request path's own aliasSeen, returning what it found BEFORE writing nowMs.
func recordAndRead(t *testing.T, st store.Store, tenant string, body []byte, nowMs int64) int64 {
	t.Helper()
	return aliasSeen(st, aliasFor(t, tenant, body), nowMs)
}

// readSeen reads the clock without writing it, so an assertion cannot disturb what it measures.
func readSeen(t *testing.T, st store.Store, tenant string, body []byte) int64 {
	t.Helper()
	b, ok := st.Get(store.SeenPrefix + aliasFor(t, tenant, body))
	if !ok {
		return 0
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		t.Fatalf("stored clock %q is not an integer", b)
	}
	return v
}

func aliasFor(t *testing.T, tenant string, body []byte) string {
	t.Helper()
	msgs := messagesArray(body)
	if !msgs.Exists() {
		t.Fatal("fixture body has no messages array")
	}
	norm, _ := normalize(bschemas.Anthropic, msgs.Array())
	if len(norm) == 0 {
		t.Fatal("fixture body normalized to no messages")
	}
	sys, firstUser := schema.SessionHead(norm)
	return session.Scoped(tenant, "", sys, firstUser)
}
