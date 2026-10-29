package apply

import (
	"context"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
)

// Issue #766: the restart grace asks the last-seen lookup for the thread's keys under BOTH
// session ids, because one conversation reaches the proxy under an explicit header on some
// turns and a content-derived id on others, and a refresh under either id kept the provider's
// single entry warm. This is the apply-level wiring of what modes' graceLocked does with the
// two keys: the header-bearing turn after a restart must be protected by the ALIAS id's
// clock alone.
//
// PROVEN TO FAIL WITHOUT THE FIX: with apply passing no alias key, the lookup is asked only
// for the explicit id, does not know it, and the turn reads as a new thread — MaxCachedIdx
// -1, the whole request editable.
func TestTheRestartGraceAsksForTheAliasSessionIdToo(t *testing.T) {
	pipe, st := components.NewPipeline(nil, nil), store.NewMemory(store.Options{})
	body := []byte(`{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":"more"}]}`)
	base := time.Unix(1_700_000_000, 0)
	// The explicit id this conversation's header-bearing turns run under. Scoped with an
	// explicit id ignores the content, which is what makes it stable across turns.
	explicitKey := session.Scoped("", "explicit-session", "", "")

	// The restart: a fresh tracker whose lookup knows ONLY the alias id — refreshed two
	// minutes ago, inside the 5-minute lifetime a plain Anthropic body carries. The explicit
	// id is brand new to it, exactly as it would be after the headerless turns before the
	// restart recorded the thread under the alias alone.
	tr := modes.NewTracker(0)
	now := base.Add(10 * time.Minute)
	tr.SetLastSeen(func(key string) (int64, bool, bool) {
		if key == explicitKey {
			return 0, false, true
		}
		return now.Add(-2 * time.Minute).UnixMilli(), true, true
	})

	res := BodyOpts(context.Background(), pipe, st, Opts{
		Provider: bschemas.Anthropic, Body: body, Session: "explicit-session",
		Tracker: tr, Now: now,
	})
	// Two messages, both cached: the grace protected the whole request through the alias
	// id's clock. Without the alias key the boundary is -1 (nothing cached, all editable).
	if res.Trace.MaxCachedIdx != 1 {
		t.Fatalf("header-bearing turn after the restart: MaxCachedIdx = %d, want 1 "+
			"(the alias id's clock protects the whole request)", res.Trace.MaxCachedIdx)
	}
}
