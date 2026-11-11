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

// floorRun describes one thread for the test below.
type floorRun struct {
	last    resp
	prefix  int64 // billed prefix of every response, in tokens
	noUsage bool  // the last response reports no usage, so its wait cause is unknown
}

// recordFloorSeq records two responses of one thread. The first calls a tool, because a thread with no
// tool call yet gets no ping plan. The test raises the general floor (MinPrefixTokens) to the shipped
// 80k; Policy.WaitMinPrefix lowers it to 20k for the waits that qualify.
func recordFloorSeq(k *Keeper, key Key, at time.Time, run floorRun) {
	pol := planPolicy()
	pol.MinPrefixTokens = 80000 // kaPolicy sets 20k
	for i, r := range []resp{toolCall, run.last} {
		k.Record(Record{Key: key, StartedAt: at.Add(time.Duration(i-1) * time.Second), Body: []byte(kaToolBody),
			Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
			Usage:        Usage{CacheRead: run.prefix - 1200, CacheWrite: 1200, StopReason: r.stop},
			ToolUseNames: r.names, UsageOK: !(run.noUsage && i == 1), Policy: pol, Header: kaHeader(),
			Hold: fakeOf(k).hold(key)})
	}
}

// The sweep question at a due keep-alive ping follows the prefix floor of the wait (#1040). It tests
// fireSweepSubstitute and the held answer in the Store. It does NOT test the warm path of #491
// (extract_sweep_warm.go), which has no prefix floor: do not cite this test for #491.
//
// At a 40k prefix a subagent that waits for its main agent, and a main thread that waits for background
// work or a teammate, get a ping, and the question replaces it. A thread that waits for its own tool
// gets a plain ping. A main thread that waits for a person, or for an unknown cause, gets nothing; at
// 100k the person wait gets a ping and the question. The question has no size gate of its own here:
// min_tokens (2000) only decides which tool outputs are candidates.
func TestSweepQuestionAtThePingFollowsTheWaitFloorAt40kPrefix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		thread     string
		run        floorRun
		bg, tm     int
		wantPing   bool // the keeper fires a ping
		wantAsk    bool // the sweep question replaces it
		wantPlains int  // ordinary pings sent
	}{
		{"subagent waits for its main agent", "a:x", floorRun{last: endTurn, prefix: 40000}, 0, 0, true, true, 0},
		{"subagent waits for its own tool", "a:x", floorRun{last: toolCall, prefix: 40000}, 0, 0, true, false, 1},
		{"main waits for its own tool", "", floorRun{last: toolCall, prefix: 40000}, 0, 0, true, false, 1},
		{"main waits for a person", "", floorRun{last: endTurn, prefix: 40000}, 0, 0, false, false, 0},
		{"main waits for a person at 100k", "", floorRun{last: endTurn, prefix: 100000}, 0, 0, true, true, 0},
		{"main waits for an unknown cause", "", floorRun{last: endTurn, prefix: 40000, noUsage: true}, 0, 0, false, false, 0},
		{"main waits for background work", "", floorRun{last: endTurn, prefix: 40000}, 1, 0, true, true, 0},
		{"main waits for a teammate", "", floorRun{last: endTurn, prefix: 40000}, 0, 1, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, fs, clock := testKeeperWith(t, func(o *Options) {
				o.OpenTasks = func(string, string, string) (int, int) { return tc.bg, tc.tm }
			})
			st := store.NewMemory(store.Options{MaxEntries: 400})
			asker := &sweepAsker{cacheRead: int(tc.run.prefix) - 1200}
			registerSweepThread(t, tc.thread, st, asker, sweepConversation(3))
			key := Key{Tenant: "t1", Session: "sess-1", Thread: tc.thread}
			recordFloorSeq(k, key, clock.now(), tc.run)

			n := k.Sweep(clock.advance(281 * time.Second))
			if (n == 1) != tc.wantPing {
				t.Fatalf("pings fired = %d, want a ping: %v", n, tc.wantPing)
			}
			if tc.wantPing {
				waitSweepOutcome(t, k, fs)
			}
			wantCalls := int64(0)
			if tc.wantAsk {
				wantCalls = 1
			}
			if got := asker.calls.Load(); got != wantCalls {
				t.Errorf("sweep questions asked = %d, want %d", got, wantCalls)
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
