package keepalive

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/store"
)

// recordFloorSeq records two responses of one thread, with a billed prefix of 40k tokens: above the
// 20k floor of a tool wait or a subagent wait, below the 80k floor of a main thread that waits for a person.
// The first response calls a tool, because a thread with no tool call yet gets no ping plan.
func recordFloorSeq(k *Keeper, key Key, at time.Time, last resp) {
	pol := planPolicy()
	pol.MinPrefixTokens = 80000 // the shipped general floor; kaPolicy lowers it to 20k
	for i, r := range []resp{toolCall, last} {
		k.Record(Record{Key: key, StartedAt: at.Add(time.Duration(i-1) * time.Second), Body: []byte(kaToolBody),
			Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
			Usage:        Usage{CacheRead: 38800, CacheWrite: 1200, StopReason: r.stop},
			ToolUseNames: r.names, UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(key)})
	}
}

// The warm sweep path follows the keep-alive floor of the wait (#1040). At a 40k prefix a subagent that
// waits for its main agent, and a main thread that waits for background work or a teammate, get a ping,
// and the sweep question replaces it. A thread that waits for its own tool gets a plain ping. A main
// thread that waits for a person gets neither. The sweep has no size gate of its own here: its
// min_tokens (2000) only decides which tool outputs are candidates.
func TestWarmSweepFollowsTheWaitFloorAt40kPrefix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		thread     string
		r          resp
		bg, tm     int
		wantPing   bool // the keeper fires a ping
		wantAsk    bool // the sweep question replaces it
		wantPlains int  // ordinary pings sent
	}{
		{"subagent waits for its main agent", "a:x", endTurn, 0, 0, true, true, 0},
		{"subagent waits for its own tool", "a:x", toolCall, 0, 0, true, false, 1},
		{"main waits for its own tool", "", toolCall, 0, 0, true, false, 1},
		{"main waits for a person", "", endTurn, 0, 0, false, false, 0},
		{"main waits for background work", "", endTurn, 1, 0, true, true, 0},
		{"main waits for a teammate", "", endTurn, 0, 1, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, fs, clock := testKeeperWith(t, func(o *Options) {
				o.OpenTasks = func(string, string, string) (int, int) { return tc.bg, tc.tm }
			})
			st := store.NewMemory(store.Options{MaxEntries: 400})
			asker := &sweepAsker{cacheRead: 38800}
			registerSweepThread(t, tc.thread, st, asker, sweepConversation(3))
			key := Key{Tenant: "t1", Session: "sess-1", Thread: tc.thread}
			recordFloorSeq(k, key, clock.now(), tc.r)

			n := k.Sweep(clock.advance(281 * time.Second))
			if (n == 1) != tc.wantPing {
				t.Fatalf("pings fired = %d, want a ping: %v", n, tc.wantPing)
			}
			if tc.wantPing {
				waitSweepOutcome(t, k, fs)
			} else {
				time.Sleep(20 * time.Millisecond)
			}
			if got := asker.calls.Load() > 0; got != tc.wantAsk {
				t.Errorf("sweep question asked = %v, want %v", got, tc.wantAsk)
			}
			if got := k.sweepSubstituted.Load() == 1; got != tc.wantAsk {
				t.Errorf("sweepSubstituted = %d, want question: %v", k.sweepSubstituted.Load(), tc.wantAsk)
			}
			if fs.n() != tc.wantPlains {
				t.Errorf("ordinary pings = %d, want %d", fs.n(), tc.wantPlains)
			}
			if tc.wantAsk {
				_, _, _, ok := offload.HeldSweepAnswerForTest(st, thread.Key("sess-1", tc.thread))
				if !ok {
					t.Error("no held sweep answer for the thread")
				}
			}
		})
	}
}
