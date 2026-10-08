package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/thread"
)

// threadBody is an Anthropic-dialect body whose messages are the given texts, with Claude Code's
// layout (the breakpoint on the last block). Distinct texts give distinct cache prefixes.
func threadBody(texts ...string) []byte {
	msgs := make([]any, len(texts))
	for i, t := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		block := map[string]any{"type": "text", "text": t}
		if i == len(texts)-1 {
			block["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		msgs[i] = map[string]any{"role": role, "content": []any{block}}
	}
	b, _ := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64,
		"system": []any{map[string]any{"type": "text", "text": "you are claude code",
			"cache_control": map[string]any{"type": "ephemeral"}}},
		"messages": msgs})
	return b
}

// Test 6 of the issue: after the main thread M and a subagent S each send requests, the keeper
// holds two entries, each with its own thread's body, and a request from S does not replace or
// clear M's entry. Both are pinged, each with its own body.
func TestKeeperHoldsOneEntryPerThread(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider bschemas.ModelProvider
		path     string
		body     func(first string) []byte
	}{
		{"anthropic", bschemas.Anthropic, "/v1/messages", func(first string) []byte {
			return []byte(strings.Replace(kaBody, `"text":"hi"`, `"text":"`+first+`"`, 1))
		}},
		// Test 7: the Responses route, with no Claude Code header anywhere.
		{"responses", bschemas.OpenAI, "/v1/responses", func(first string) []byte {
			return []byte(`{"model":"azure/gpt-5.6-luna","input":[{"role":"user","content":"` + first + `"}]}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t, Limits{})
			var mu sync.Mutex
			sent := map[string]string{} // thread -> the body its ping carried
			k.send = func(j pingJob, body []byte) (Usage, int, error) {
				mu.Lock()
				sent[j.thread] = string(body)
				mu.Unlock()
				return Usage{CacheRead: 48576, Output: 1}, http.StatusOK, nil
			}
			k.dispatch = k.fire
			pol := kaPolicy()
			pol.MinPrefixTokens = 1000
			pol.OpenAIIdle = 280 * time.Second
			tn := &Tenancy{ID: "t1", Cache: pol}
			r := httptest.NewRequest(http.MethodPost, "/x", nil)
			r.Header.Set("Authorization", "Bearer k")
			up := upstream{base: "http://up", path: tc.path}
			rec := func(threadID, first string, at time.Time) {
				k.record(tn, "sess", threadID, at, tc.body(first), up, r, tc.provider, tc.path,
					http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 100}, true)
			}
			start := clock.now()
			// M sends two turns (the gate skips a thread's first), then S sends two, interleaved
			// with M's own arrival on its next turn never happening: M is idle from here on.
			k.arrive("t1", "sess", "")
			rec("", "MAIN-THREAD", start)
			k.arrive("t1", "sess", "")
			rec("", "MAIN-THREAD", start.Add(time.Second))
			for i := 2; i < 4; i++ {
				if p, _, _ := k.arrive("t1", "sess", "a:S"); p != 0 {
					t.Fatal("a subagent's arrival reported the main thread's pings")
				}
				rec("a:S", "SUBAGENT", start.Add(time.Duration(i)*time.Second))
			}
			if got := k.Stats().Live; got != 2 {
				t.Fatalf("keeper holds %d entries, want 2 (one per thread)", got)
			}
			if live := k.LiveSessionKeys(); len(live) != 1 || live[0] != "sess" {
				t.Errorf("LiveSessionKeys = %v, want the session once", live)
			}
			if n := k.sweep(clock.advance(300 * time.Second)); n != 2 {
				t.Fatalf("sweep fired %d pings, want 2", n)
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(sent[""], "MAIN-THREAD") {
				t.Errorf("the main thread's ping did not carry its own body: %.80q", sent[""])
			}
			if !strings.Contains(sent["a:S"], "SUBAGENT") {
				t.Errorf("the subagent's ping did not carry its own body: %.80q", sent["a:S"])
			}
		})
	}
}

// Control for the test above: with every request keyed on the session — today's behaviour, and
// what a header-less request with no match still gets — the subagent's request replaces M's
// entry. If this stopped holding, the test above would no longer be testing anything.
func TestControlSessionKeyedEntriesReplaceEachOther(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	pol := kaPolicy()
	pol.MinPrefixTokens = 1000
	tn := &Tenancy{ID: "t1", Cache: pol}
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	up := upstream{base: "http://up", path: "/v1/messages"}
	for i := 0; i < 4; i++ {
		k.record(tn, "sess", "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
			bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
	}
	if got := k.Stats().Live; got != 1 {
		t.Fatalf("session-keyed entries: %d live, want 1", got)
	}
}

// Disarming a session's override releases every thread of that session, and no other session.
func TestDisarmReleasesEveryThreadOfTheSession(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	armOn(t, k, "sess", 280*time.Second, 2, now.Add(time.Hour))
	pol := kaPolicy()
	pol.KeepAlive = false // only the override enables it
	pol.MinPrefixTokens = 1000
	tn := &Tenancy{ID: "t1", Cache: pol}
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	up := upstream{base: "http://up", path: "/v1/messages"}
	for _, th := range []string{"", "a:S"} {
		for i := 0; i < 2; i++ {
			k.record(tn, "sess", th, now.Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
		}
	}
	// A different session whose id starts with the same bytes must survive.
	armOn(t, k, "sess2", 280*time.Second, 2, now.Add(time.Hour))
	for i := 0; i < 2; i++ {
		k.record(tn, "sess2", "", now.Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
			bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
	}
	if got := k.Stats().Live; got != 3 {
		t.Fatalf("precondition: %d live entries, want 3", got)
	}
	k.disarm("t1", "sess")
	if got := k.Stats().Live; got != 1 {
		t.Fatalf("after disarm: %d live entries, want 1 (sess2 only)", got)
	}
}

// Test 5 of the issue: a header that is present but empty or malformed falls back to prefix
// continuity — no crash, no merged thread, and never the malformed value as a key.
func TestThreadForFallsBackOnAnUnusableHeader(t *testing.T) {
	h := &Handler{threads: thread.New()}
	lg := slog.New(slog.DiscardHandler)
	req := func(agentID string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		if agentID != "\x01unset" {
			r.Header.Set(agentIDHeader, agentID)
		}
		return r
	}
	if id := h.threadFor(req("\x01unset"), "t1", "s", threadBody("main"), false, lg); id != thread.Primary {
		t.Fatalf("main thread = %q", id)
	}
	if id := h.threadFor(req("a9b99ca1f8284c823"), "t1", "s", threadBody("sub"), false, lg); id != "a:a9b99ca1f8284c823" {
		t.Fatalf("valid header thread = %q", id)
	}
	for _, bad := range []string{"", "   ", "has space", strings.Repeat("x", 500)} {
		// A continuation of the main thread with a bad header stays on the main thread...
		if id := h.threadFor(req(bad), "t1", "s", threadBody("main", "a", "u"), false, lg); id != thread.Primary {
			t.Errorf("header %.20q on a main-thread continuation gave %q", bad, id)
		}
		// ...and a new conversation with a bad header gets a prefix thread, not the bad value.
		id := h.threadFor(req(bad), "t1", "s", threadBody("other "+bad), false, lg)
		if id == thread.Primary || strings.HasPrefix(id, "a:") {
			t.Errorf("header %.20q on a new conversation gave %q", bad, id)
		}
	}
	// No session: the session itself, whatever the header says.
	if id := h.threadFor(req("x"), "t1", "", threadBody("a"), false, lg); id != thread.Primary {
		t.Errorf("no session gave %q", id)
	}
	// A nil tracker fails open to the session.
	if id := (&Handler{}).threadFor(req("x"), "t1", "s", threadBody("a"), false, lg); id != thread.Primary {
		t.Errorf("nil tracker gave %q", id)
	}
}

// End to end through the handler: the main thread and a subagent interleave on one session, with
// Claude Code's header and without it. Each dashboard row carries its thread, the two paths group
// the requests the same way, and the keeper ends with one entry per thread.
func TestHandlerKeysRowsAndKeepAliveByThread(t *testing.T) {
	for _, withHeader := range []bool{true, false} {
		t.Run(fmt.Sprintf("header=%v", withHeader), func(t *testing.T) {
			up := fakeUpstream(t)
			defer up.Close()
			h, rec := dashHandler(t, up.URL, dash.Options{})
			defer h.Close()
			pol := kaPolicy()
			pol.MinPrefixTokens = 1000 // fakeUpstream bills a 10.5k prefix
			h.static.Cache = pol
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			send := func(agentID string, body []byte) {
				req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-context-guru-session", "sess-th")
				if withHeader && agentID != "" {
					req.Header.Set(agentIDHeader, agentID)
				}
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("proxy returned %d", resp.StatusCode)
				}
			}
			send("", threadBody("fix the bug"))
			send("", threadBody("fix the bug", "spawning a subagent", "go on"))
			send("agentS", threadBody("investigate file x"))
			send("agentS", threadBody("investigate file x", "reading", "contents"))
			waitForRows(t, rec, 4)

			page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			byTS := page.Requests
			sort.Slice(byTS, func(i, j int) bool { return byTS[i].ID < byTS[j].ID })
			var ids []string
			for _, e := range byTS {
				ids = append(ids, e.ThreadID)
			}
			if len(ids) != 4 || ids[0] != "" || ids[1] != "" || ids[2] == "" || ids[2] != ids[3] {
				t.Fatalf("row threads = %q, want [\"\" \"\" X X] with X a subagent thread", ids)
			}
			if withHeader && ids[2] != "a:agentS" {
				t.Errorf("header path thread = %q, want a:agentS", ids[2])
			}
			if !withHeader && !strings.HasPrefix(ids[2], "p:") {
				t.Errorf("fallback path thread = %q, want a prefix thread", ids[2])
			}
			// The subagent's first request is a cold start of its own thread, never a prefix
			// change of the session's (fakeUpstream reads 9000 from cache, so every row is a hit;
			// the label check lives in dash.TestASubagentsFirstRequestIsAColdStart).
			if got := h.keeper.Stats().Live; got != 2 {
				t.Errorf("keeper holds %d entries after two threads of two turns each, want 2", got)
			}
		})
	}
}
