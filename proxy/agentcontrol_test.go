package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/agent/bob"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/store"
)

// controlUpstream answers every call with body and records the Accept-Encoding it got.
type controlUpstream struct {
	body string
	mu   sync.Mutex
	ae   []string
}

func (u *controlUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.ae = append(u.ae, r.Header.Get("Accept-Encoding"))
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(u.body))
}

func (u *controlUpstream) acceptEncodings() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.ae...)
}

// control is a test agent on Bob's model path. It is a ControlPlane: WantsReply gives wants, and
// each method panics with panicCanary when told to.
type control struct {
	agent.Base
	wants, wantsPanics, replyPanics bool
	reply                           agent.ControlReply
	calls                           *atomic.Int64
}

func (control) Paths() []string { return []string{bob.ModelPath} }

func (c control) WantsReply(agent.ControlCall) bool {
	c.calls.Add(1)
	if c.wantsPanics {
		panic(panicCanary)
	}
	return c.wants
}

func (c control) Reply(agent.ControlCall, []byte) agent.ControlReply {
	c.calls.Add(1)
	if c.replyPanics {
		panic(panicCanary)
	}
	return c.reply
}

var _ agent.ControlPlane = control{}

// controlGateway is a local Bob gateway in front of up, with the agents taken from reg.
func controlGateway(t *testing.T, up *controlUpstream, reg *agent.Registry) (*httptest.Server, *Handler) {
	t.Helper()
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	cfg, err := config.LoadBytes([]byte("pipeline: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), nil, Options{BobUpstream: us.URL})
	h.agentFor = reg.ForPath
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return srv, h
}

func registryWith(t *testing.T, a agent.Agent) *agent.Registry {
	t.Helper()
	reg := agent.NewRegistry()
	if err := reg.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// clientAE is the Accept-Encoding the Bob IDE sends. When the proxy drops it, Go's transport asks
// for "gzip" by itself, so the upstream sees "gzip" or nothing, never this value.
const clientAE = "gzip, deflate, br"

// controlGet sends a GET with clientAE. The client transport does not decompress, so the test sees
// the bytes the proxy really sent.
func controlGet(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", clientAE)
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

const modelInfo = `{"data":[{"model_name":"m1","model_info":{"max_input_tokens":200000}}]}`

// A WantsReply that panics fails open to "no": the reply streams as on any other path, with the
// client's Accept-Encoding kept, and the log holds the panic's type only.
func TestPanickingWantsReplyStreamsTheReplyUnchanged(t *testing.T) {
	var calls atomic.Int64
	up := &controlUpstream{body: modelInfo}
	srv, h := controlGateway(t, up, registryWith(t, control{wantsPanics: true, calls: &calls}))
	logs := captureDefaultLog(t)

	resp, got := controlGet(t, srv.URL+"/inference/v1/model/info")

	if calls.Load() == 0 {
		t.Fatal("the agent was never asked; the fail-open path was not run")
	}
	if resp.StatusCode != http.StatusOK || string(got) != modelInfo {
		t.Fatalf("reply = %d %s, want 200 %s", resp.StatusCode, got, modelInfo)
	}
	if ae := up.acceptEncodings(); len(ae) != 1 || ae[0] != clientAE {
		t.Errorf("upstream Accept-Encoding = %q, want [%s] (kept: the agent wants no reply)", ae, clientAE)
	}
	if w := h.bobWindow("m1"); w != 0 {
		t.Errorf("bobWindow(m1) = %d, want 0 (no reply was read)", w)
	}
	out := logs.String()
	// Seen first, or the canary check below could pass on a log it never captured.
	if !strings.Contains(out, "recovered from panic in the agent's control-plane check") {
		t.Fatalf("the recovery was not logged where this test reads:\n%s", out)
	}
	if strings.Contains(out, panicCanary) {
		t.Fatalf("the log holds the agent's panic message:\n%s", out)
	}
}

// A Reply that panics fails open to the original bytes, with their Content-Length, and stores no
// windows.
func TestPanickingReplySendsTheOriginalBytes(t *testing.T) {
	var calls atomic.Int64
	up := &controlUpstream{body: modelInfo}
	srv, h := controlGateway(t, up, registryWith(t, control{wants: true, replyPanics: true, calls: &calls}))
	logs := captureDefaultLog(t)

	resp, got := controlGet(t, srv.URL+"/inference/v1/model/info")

	if calls.Load() != 2 {
		t.Fatalf("agent calls = %d, want 2 (WantsReply, then Reply)", calls.Load())
	}
	if resp.StatusCode != http.StatusOK || string(got) != modelInfo {
		t.Fatalf("reply = %d %s, want 200 %s", resp.StatusCode, got, modelInfo)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(modelInfo)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(modelInfo))
	}
	if ae := up.acceptEncodings(); len(ae) != 1 || ae[0] == clientAE {
		t.Errorf("upstream Accept-Encoding = %q, want one request without the client's value (dropped: the agent wants the reply)", ae)
	}
	if w := h.bobWindow("m1"); w != 0 {
		t.Errorf("bobWindow(m1) = %d, want 0 (a panicking Reply stores no windows)", w)
	}
	out := logs.String()
	if !strings.Contains(out, "recovered from panic in the agent's control-plane reply") {
		t.Fatalf("the recovery was not logged where this test reads:\n%s", out)
	}
	if strings.Contains(out, panicCanary) {
		t.Fatalf("the log holds the agent's panic message:\n%s", out)
	}
}

// The host sends Reply's Body and stores its Windows: the agent, not the host, decides both.
func TestReplyBodyAndWindowsReachTheClientAndTheStore(t *testing.T) {
	var calls atomic.Int64
	const changed = `{"changed":true}`
	a := control{wants: true, calls: &calls, reply: agent.ControlReply{Body: []byte(changed), Windows: map[string]int{"m9": 9000}}}
	srv, h := controlGateway(t, &controlUpstream{body: modelInfo}, registryWith(t, a))

	resp, got := controlGet(t, srv.URL+"/any/control/path")

	if resp.StatusCode != http.StatusOK || string(got) != changed {
		t.Fatalf("reply = %d %s, want 200 %s", resp.StatusCode, got, changed)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(changed)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(changed))
	}
	if w := h.bobWindow("m9"); w != 9000 {
		t.Errorf("bobWindow(m9) = %d, want 9000", w)
	}
}

// pathOnly owns Bob's model path and is no ControlPlane.
type pathOnly struct{ agent.Base }

func (pathOnly) Paths() []string { return []string{bob.ModelPath} }

// An agent that is not a ControlPlane reads no reply: even Bob's profile streams unchanged, with
// its region, and the client's Accept-Encoding goes upstream.
func TestAnAgentThatIsNotAControlPlaneStreamsUnchanged(t *testing.T) {
	const profile = `{"instances":[{"instance_id":"i1","region":"us-east","region_domain":"us-east.example.com"}]}`
	for name, reg := range map[string]*agent.Registry{
		"not a ControlPlane": registryWith(t, pathOnly{}),
		"no agent":           agent.NewRegistry(),
	} {
		t.Run(name, func(t *testing.T) {
			up := &controlUpstream{body: profile}
			srv, _ := controlGateway(t, up, reg)

			resp, got := controlGet(t, srv.URL+bob.ProfilePath)

			if resp.StatusCode != http.StatusOK || string(got) != profile {
				t.Fatalf("reply = %d %s, want 200 %s", resp.StatusCode, got, profile)
			}
			if ae := up.acceptEncodings(); len(ae) != 1 || ae[0] != clientAE {
				t.Errorf("upstream Accept-Encoding = %q, want [%s]", ae, clientAE)
			}
		})
	}
}

// Hosted, the host sends the agent's Body but stores no Windows: the store is one per Handler, and
// a tenant's catalog must not set another tenant's window.
func TestHostedReplyStoresNoWindows(t *testing.T) {
	var calls atomic.Int64
	const changed = `{"changed":true}`
	a := control{wants: true, calls: &calls, reply: agent.ControlReply{Body: []byte(changed), Windows: map[string]int{"m9": 9000}}}
	f := newHostedFixture(t, "up", "bob")
	f.h.agentFor = registryWith(t, a).ForPath
	mux := f.h.Mux()
	_, tok := f.register(t, "a@ibm.com")
	r := httptest.NewRequest(http.MethodGet, "/any/control/path", nil)
	r.Header.Set(TokenHeader, tok)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, r)

	if calls.Load() != 2 {
		t.Fatalf("agent calls = %d, want 2 (WantsReply, then Reply)", calls.Load())
	}
	if w.Code != http.StatusOK || w.Body.String() != changed {
		t.Fatalf("reply = %d %s, want 200 %s", w.Code, w.Body, changed)
	}
	if got := f.h.bobWindow("m9"); got != 0 {
		t.Errorf("bobWindow(m9) = %d, want 0 (hosted stores no windows)", got)
	}
}

// The host checks an agent's catalog itself: a window <= 0 is dropped, and a catalog over the cap,
// or with no usable window, records nothing.
func TestNoteBobWindowsKeepsTheHostLimits(t *testing.T) {
	h := &Handler{}
	h.noteBobWindows(map[string]int{"good": 1000, "zero": 0, "neg": -5})
	if h.bobWindow("good") != 1000 || h.bobWindow("zero") != 0 || h.bobWindow("neg") != 0 || len(h.bobWin) != 1 {
		t.Fatalf("bobWin = %v, want only good:1000", h.bobWin)
	}

	h.noteBobWindows(map[string]int{"zero": 0})
	big := make(map[string]int, bobWindowMax+1)
	for i := range bobWindowMax + 1 {
		big["m"+strconv.Itoa(i)] = 1
	}
	h.noteBobWindows(big)

	if h.bobWindow("good") != 1000 || len(h.bobWin) != 1 {
		t.Fatalf("bobWin = %d entries, want the old catalog kept", len(h.bobWin))
	}
}

// The host stores a copy of the agent's windows: a change to the agent's map after the call does not
// reach the store.
func TestNoteBobWindowsStoresACopy(t *testing.T) {
	h := &Handler{}
	windows := map[string]int{"m1": 1000}

	h.noteBobWindows(windows)
	windows["m1"] = 5
	windows["m2"] = 7

	if h.bobWindow("m1") != 1000 || h.bobWindow("m2") != 0 {
		t.Fatalf("bobWindow(m1, m2) = %d, %d, want 1000, 0", h.bobWindow("m1"), h.bobWindow("m2"))
	}
}
