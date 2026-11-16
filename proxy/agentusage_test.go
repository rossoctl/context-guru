package proxy

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/context-guru/agent"
)

// everyFieldSet is a Usage with every field non-zero, so a mapping that drops or zeroes any field
// shows in a comparison.
func everyFieldSet(t *testing.T) Usage {
	t.Helper()
	u := Usage{
		Searches: 2, SearchLoaded: []string{"Read"}, SearchDropped: 1, Live: &LiveFacts{},
		FreshInput: 11, CacheRead: 22, CacheWrite: 33, Output: 44, CacheWrite1h: 5,
		StopReason: "stop", Backend: "A", ToolUseNames: []string{"Bash"}, ToolUseIncomplete: true,
	}
	v := reflect.ValueOf(u)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("everyFieldSet leaves Usage.%s zero; set it, or this test cannot see it dropped", v.Type().Field(i).Name)
		}
	}
	return u
}

// The neutral agent changes nothing, so on every route no agent owns, the usage is exactly the
// shared parser's: all fields, not only the token counts.
func TestReadAgentUsageWithTheNeutralAgentIsTheIdentity(t *testing.T) {
	base := everyFieldSet(t)

	got := readAgentUsage(context.Background(), agent.Base{}, "application/json", []byte(`{"usage":{}}`), base)

	if !reflect.DeepEqual(got, base) {
		t.Fatalf("readAgentUsage(Base) = %+v, want base unchanged %+v", got, base)
	}
}

// fixedTokens is an agent whose ReadUsage returns set token counts, whatever the reply.
type fixedTokens struct {
	agent.Base
	u agent.Usage
}

func (f fixedTokens) ReadUsage(string, []byte, agent.Usage) agent.Usage { return f.u }

// The agent owns the token counts and the backend label only. Every other field stays as the shared parser read it.
func TestReadAgentUsageTakesOnlyTheTokenCountsFromTheAgent(t *testing.T) {
	base := everyFieldSet(t)
	ag := fixedTokens{u: agent.Usage{FreshInput: 1, CacheRead: 2, CacheWrite: 3, CacheWrite1h: 4, Output: 5, Backend: "B"}}

	got := readAgentUsage(context.Background(), ag, "application/json", nil, base)

	want := base
	want.FreshInput, want.CacheRead, want.CacheWrite, want.CacheWrite1h, want.Output = 1, 2, 3, 4, 5
	want.Backend = "B"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readAgentUsage = %+v, want %+v", got, want)
	}
}

// usagePanicky is a test agent whose ReadUsage panics on the route it owns. It counts its calls,
// as panicky does.
type usagePanicky struct {
	agent.Base
	path  string
	calls *atomic.Int64
}

func (p usagePanicky) Paths() []string { return []string{p.path} }
func (p usagePanicky) ReadUsage(string, []byte, agent.Usage) agent.Usage {
	p.calls.Add(1)
	panic(panicCanary)
}

// An agent whose ReadUsage panics must not fail the request. The row then has the usage the route
// has with no agent, on the streamed path and on the buffered path.
func TestPanickingAgentReadUsageFailsOpenToTheSharedReading(t *testing.T) {
	const route, upPath = "/openai/v1/chat/completions", "/v1/chat/completions"
	tools := `{"model":"premium","messages":[{"role":"system","content":"You are Bob."},{"role":"user","content":"fix it"}],` +
		`"tools":[{"type":"function","function":{"name":"read_file","description":"Read a file.","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	for name, body := range map[string]string{"streamed": bobBody("fix it", ""), "buffered": tools} {
		t.Run(name, func(t *testing.T) {
			plainSrv, plainRec := agentSessionHandler(t, agent.NewRegistry())
			want := rowAt(t, plainSrv, plainRec, route, body, nil)
			var calls atomic.Int64
			reg := agent.NewRegistry()
			if err := reg.Register(usagePanicky{path: upPath, calls: &calls}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			srv, rec := agentSessionHandler(t, reg)
			logs := captureDefaultLog(t)

			// rowAt fails the test on any status but 200.
			got := rowAt(t, srv, rec, route, body, nil)

			if calls.Load() == 0 {
				t.Fatal("the agent was never asked; the fail-open path was not run")
			}
			if got.FreshInput != want.FreshInput || got.CacheRead != want.CacheRead || got.CacheWrite != want.CacheWrite ||
				got.CacheWrite1h != want.CacheWrite1h || got.OutputTokens != want.OutputTokens || want.FreshInput != 12 {
				t.Fatalf("row = fresh %d read %d write %d write1h %d out %d; want the no-agent row %d/%d/%d/%d/%d with fresh 12",
					got.FreshInput, got.CacheRead, got.CacheWrite, got.CacheWrite1h, got.OutputTokens,
					want.FreshInput, want.CacheRead, want.CacheWrite, want.CacheWrite1h, want.OutputTokens)
			}
			out := logs.String()
			if !strings.Contains(out, "recovered from panic in the agent's usage reading") {
				t.Fatalf("the recovery was not logged where this test reads:\n%s", out)
			}
			if strings.Contains(out, panicCanary) {
				t.Fatalf("the log holds the agent's panic message:\n%s", out)
			}
		})
	}
}
