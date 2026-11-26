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
	"sync/atomic"
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

var bobSweepRuns atomic.Int64

func TestBobUltrahighSweepAsksBobOnALongChat(t *testing.T) {
	// #1191's blocker 3 (the 30-turn defaults) is not covered: the chat is long enough for the defaults.
	up := &bobSweepUpstream{}
	us := httptest.NewServer(up)
	defer us.Close()
	srv := httptest.NewServer(bobSweepHandler(t, us.URL).Mux())
	defer srv.Close()
	// One task id per run: the sweep's warm state is process-wide and keyed by it, so a repeat run
	// (-count=N) in one process would otherwise start with the earlier run's request count.
	task := fmt.Sprintf("bob-task-sweep-%d", bobSweepRuns.Add(1))
	before := offload.SweepWarmCounters()
	pingBefore := offload.SweepPingCounters()

	// 45 turns. With the defaults (warm_every_n 30, warm_min_age_turns 30) the first check comes at turn
	// 30, and the grep from turn 1 is old enough then, so the one question goes out there. The next check
	// is due at turn 60. (A 64-turn run sees a second question at 60: the stub's 2-byte reply is unusable,
	// so no answer is held and the next check asks again.)
	for n := 1; n <= 45; n++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+bobModelPath, bytes.NewReader(bobSweepBody(t, n)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Apikey bob-caller-key")
		req.Header.Set("X-Task-Id", task)
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
	// in 45 turns, while the next check is due at turn 60.
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
