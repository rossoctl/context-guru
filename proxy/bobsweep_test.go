package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/tidwall/gjson"
)

// The sweep's warm path on Bob's route, end to end through the proxy (#1191). bob-ultrahigh turns the
// sweep on with `warm: true`, so a long Bob chat with an old, large tool output must get one sweep
// question, sent to Bob's own model path with Bob's own `Apikey` login.

// sweepQuestionMark is a line of every sweep question (internal/extract BuildPrefixAsk).
const sweepQuestionMark = "TOOL OUTPUTS UNDER CONSIDERATION"

type bobSweepCall struct {
	path, auth string
	body       []byte
}

// bobSweepUpstream answers like Bob. It bills about 1.5x the local size of the messages, and reports
// most of the prompt as a cache read, as both of Bob's backends do.
type bobSweepUpstream struct {
	mu    sync.Mutex
	calls []bobSweepCall
}

func (u *bobSweepUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.calls = append(u.calls, bobSweepCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: b})
	u.mu.Unlock()
	prompt := len(gjson.GetBytes(b, "messages").Raw) * 3 / 8
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"chatcmpl-0f3c9a1e-5b7d-4e2a-9c1f-8d6b2a4e7c10","object":"chat.completion",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":%d}}}`,
		prompt, prompt-50)
}

// questions is the calls that carry a sweep question.
func (u *bobSweepUpstream) questions() []bobSweepCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []bobSweepCall
	for _, c := range u.calls {
		if bytes.Contains(c.body, []byte(sweepQuestionMark)) {
			out = append(out, c)
		}
	}
	return out
}

// bobSweepHandler runs the plugin's bob-ultrahigh preset with the plugin's own Bob prices.
func bobSweepHandler(t *testing.T, upstream string) *Handler {
	t.Helper()
	prices, err := modelinfo.LoadTable(filepath.Join("..", "bob-plugin", "context-guru", "prices.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return bobHandlerPriced(t, bobPreset(t), upstream, prices)
}

// bobSweepGrep is a large grep result, every line different. It is fact-dense, so extract_llm leaves it
// whole (contentclass.go) and only the sweep can drop it once it is old.
func bobSweepGrep() string {
	var b strings.Builder
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, "pkg/h%03d/server.go:%d:  if err := serve(w, r, %d); err != nil {\\n", i%97, i*7919%10007, i)
	}
	return b.String()
}

// bobSweepBody is turn n of one Bob chat: Bob ran a large grep first, then made n-1 small tool calls.
func bobSweepBody(t *testing.T, n int) []byte {
	t.Helper()
	msgs := []string{
		`{"role":"system","content":"You are Bob, a coding agent."}`,
		`{"role":"user","content":"Fix the failing test in server.go."}`,
		`{"role":"assistant","content":null,"tool_calls":[` + csCall("c0", "grep", `{"pattern":"serve\\(w, r"}`) + `]}`,
		`{"role":"tool","tool_call_id":"c0","content":"` + bobSweepGrep() + `"}`,
	}
	for i := 1; i < n; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			`{"role":"assistant","content":null,"tool_calls":[`+csCall(id, "list_files", fmt.Sprintf(`{"path":"pkg%d"}`, i))+`]}`,
			`{"role":"tool","tool_call_id":"`+id+`","content":"pkg`+itoa(i)+`/a.go"}`)
	}
	return []byte(`{"model":"premium-ide","stream":false,"tools":` + bobFixtureTools(t) +
		`,"messages":[` + strings.Join(msgs, ",") + `]}`)
}

func TestBobUltrahighSweepAsksBobOnALongChat(t *testing.T) {
	// Fails on main today, on 2 blockers, each one enough alone. The fix PRs of #1191 remove this skip.
	//  1. No conversation copy: Ctx.CanKeepAlive is keeper.canPing, false on Bob (ping: not_registered).
	//  2. Every Bob request is cold, so the warm count never reaches warm_every_n (no trigger check).
	// #1191's blocker 3 (the 30-turn defaults) is not covered: the chat is long enough for the defaults.
	t.Skip("#1191: the sweep cannot ask on Bob's route yet (no copy without a ping; every request cold)")
	up := &bobSweepUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	srv := httptest.NewServer(bobSweepHandler(t, us.URL).Mux())
	defer srv.Close()
	before := offload.SweepWarmCounters()
	pingBefore := offload.SweepPingCounters()

	// 64 turns. With the defaults (warm_every_n 30, warm_min_age_turns 30) the first check comes at about
	// turn 30, when the grep is only 29 model turns old, so it finds no candidate; the count then restarts
	// and the next check, at about turn 60, finds it.
	for n := 1; n <= 64; n++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+bobModelPath, bytes.NewReader(bobSweepBody(t, n)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Apikey bob-caller-key")
		req.Header.Set("X-Task-Id", "bob-task-sweep")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("turn %d: client got %d %s", n, resp.StatusCode, b)
		}
	}

	testutil.WaitForState(t, "one sweep question sent to Bob", func() bool { return len(up.questions()) > 0 },
		func() string {
			w, p := offload.SweepWarmCounters(), offload.SweepPingCounters()
			reasons := map[string]int64{}
			for k, v := range w.TriggerReasons {
				if d := v - before.TriggerReasons[k]; d != 0 {
					reasons[k] = d
				}
			}
			return fmt.Sprintf("warm: fired %d, asked %d, not_registered %d, trigger_reasons %v; ping: not_registered %d",
				w.Fired-before.Fired, w.Asked-before.Asked, w.NotRegistered-before.NotRegistered,
				reasons, p.NotRegistered-pingBefore.NotRegistered)
		})
	// No request follows, so no second question can start: one more would mean the trigger fired twice
	// in 64 turns, while the next check is due at about turn 90.
	qs := up.questions()
	if len(qs) != 1 {
		t.Fatalf("got %d sweep questions, want exactly 1", len(qs))
	}
	q := qs[0]
	if m := gjson.GetBytes(q.body, "model").String(); m != "premium-ide" {
		t.Errorf("question model = %q, want the chat's own model premium-ide (Bob picks its model)", m)
	}
	if q.path != bobModelPath {
		t.Errorf("question path = %q, want Bob's model path %q", q.path, bobModelPath)
	}
	if q.auth != "Apikey bob-caller-key" {
		t.Errorf("question auth scheme = %q, want Bob's own Apikey login", authScheme(q.auth))
	}
}
