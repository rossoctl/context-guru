package tooldefer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The provider support check.
//
// A status code proves nothing: GPT answers 200 and ignores defer_loading, Gemini answers 200 to
// everything. So the probe checks BEHAVIOUR. It sends one loaded tool, one deferred tool with a
// unique name, and the search tool, and asks for the deferred tool. It passes only when the
// response holds a tool_search_tool_result that loads that tool, followed by a call to it.
//
// Every probe carries a unique nonce, in the tool name and in the prompt: our gateway returns a
// stored response for a byte-identical request, and a replayed answer would prove nothing.
//
// The verdict is kept per upstream URL + model + route, for ProbeTTL, and is dropped after any 4xx
// on a request we changed (Forget). Probes run in the background, never on the request path; until
// a probe passes, nothing is deferred for that key. A probe spends the caller's credential: about
// 1,000 input and 100 output tokens, under $0.01 on every Claude model.

// PurposeProbe is the dispatch purpose of a probe.
const PurposeProbe = "tooldefer.probe"

// ProbeTTL is how long a verdict holds.
const ProbeTTL = 24 * time.Hour

// probeFailTTL is how long a FAILED probe (a transport error, not a verdict) waits before a retry.
const probeFailTTL = 10 * time.Minute

// Verdict is a probe's result.
type Verdict int

const (
	Unknown Verdict = iota // no probe has finished for this key
	Pass
	Fail
)

func (v Verdict) String() string {
	switch v {
	case Pass:
		return "pass"
	case Fail:
		return "fail"
	}
	return "unknown"
}

// ProbeKey names one probe verdict.
type ProbeKey struct {
	Upstream, Model, Route string
}

type probeEntry struct {
	v       Verdict
	at      time.Time
	running bool
	retryAt time.Time
}

// Prober runs and caches probes. Safe for concurrent use.
type Prober struct {
	mu   sync.Mutex
	m    map[ProbeKey]*probeEntry
	now  func() time.Time
	runs map[Verdict]int64
	// Go starts fn in the background. Tests replace it to run inline.
	Go func(fn func())
	// Persist, when set, is told every verdict, so it survives a restart (see Mem.SaveVerdict).
	// A thread decided before a restart needs it: without a verdict a store miss mid-thread would
	// send the thread undeferred and move every deferred tool back into the cached prefix.
	Persist func(k ProbeKey, v Verdict, at time.Time)
}

// Seed loads a verdict kept from before a restart. An older verdict than ProbeTTL still seeds:
// Ensure re-probes it in the background.
func (p *Prober) Seed(k ProbeKey, v Verdict, at time.Time) {
	if v == Unknown {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.m[k]; e == nil || e.v == Unknown {
		p.m[k] = &probeEntry{v: v, at: at}
	}
}

// NewProber returns an empty Prober.
func NewProber() *Prober {
	return &Prober{m: map[ProbeKey]*probeEntry{}, now: time.Now, runs: map[Verdict]int64{},
		Go: func(fn func()) { go fn() }}
}

// Verdict returns the cached verdict for k, Unknown when no probe has finished (or Forget dropped
// it). A verdict older than ProbeTTL is still returned while Ensure re-probes: a gap in which every
// new thread read Unknown would leave those threads undeferred for their whole life.
func (p *Prober) Verdict(k ProbeKey) Verdict {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.m[k]; e != nil {
		return e.v
	}
	return Unknown
}

// Ensure starts a probe for k in the background when none is fresh and none is running. send is
// called once, off the request path.
func (p *Prober) Ensure(k ProbeKey, send func(ctx context.Context) (Verdict, error)) {
	p.mu.Lock()
	e := p.m[k]
	if e == nil {
		e = &probeEntry{}
		p.m[k] = e
	}
	now := p.now()
	fresh := e.v != Unknown && now.Sub(e.at) <= ProbeTTL
	if fresh || e.running || now.Before(e.retryAt) {
		p.mu.Unlock()
		return
	}
	e.running = true
	p.mu.Unlock()
	p.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		v, err := send(ctx)
		p.mu.Lock()
		defer p.mu.Unlock()
		e.running = false
		if err != nil {
			e.retryAt = p.now().Add(probeFailTTL)
			return
		}
		e.v, e.at = v, p.now()
		p.runs[v]++
		if p.Persist != nil {
			p.Persist(k, v, e.at)
		}
	})
}

// Forget drops k's verdict, so the next request probes again. Called after a 4xx on a changed
// request.
func (p *Prober) Forget(k ProbeKey) {
	p.mu.Lock()
	delete(p.m, k)
	p.mu.Unlock()
	if p.Persist != nil {
		p.Persist(k, Unknown, p.now())
	}
}

// Runs is how many probes finished with each verdict.
func (p *Prober) Runs() (pass, fail int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[Pass], p.runs[Fail]
}

// Nonce is a fresh random hex string.
func Nonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ProbeBody is the probe request for model, with nonce.
func ProbeBody(model, nonce string) []byte {
	tool := "cg_probe_" + nonce
	body := []byte(`{"max_tokens":200,"tools":[` +
		`{"name":"cg_probe_echo","description":"Echo a short text back.","input_schema":{"type":"object","properties":{"text":{"type":"string"}}}},` +
		`{"name":"","description":"Report the probe code. Call it with code set to the number you are given.","input_schema":{"type":"object","properties":{"code":{"type":"integer"}},"required":["code"]},"defer_loading":true},` +
		searchTool + `],"messages":[{"role":"user","content":""}]}`)
	body, _ = sjson.SetBytes(body, "model", model)
	body, _ = sjson.SetBytes(body, "tools.1.name", tool)
	body, _ = sjson.SetBytes(body, "messages.0.content", "Probe "+nonce+". Load the tool "+tool+" with "+
		SearchName+" (query: "+tool+", limit: 1), then call it with code 7. Do nothing else.")
	return body
}

// ProbeVerdict reads a probe response: Pass only when a tool_search_tool_result loads the probe
// tool and a later tool_use calls it.
func ProbeVerdict(resp []byte, nonce string) Verdict {
	tool := "cg_probe_" + nonce
	loaded := false
	pass := false
	gjson.GetBytes(resp, "content").ForEach(func(_, b gjson.Result) bool {
		switch b.Get("type").String() {
		case "tool_search_tool_result":
			b.Get("content.tool_references").ForEach(func(_, r gjson.Result) bool {
				loaded = loaded || r.Get("tool_name").String() == tool
				return true
			})
		case "tool_use":
			if loaded && b.Get("name").String() == tool {
				pass = true
			}
		}
		return !pass
	})
	if pass {
		return Pass
	}
	return Fail
}

// RunProbe sends one probe through the host's Dispatcher. A transport error or a 5xx/429 is an
// error (no verdict, retried later); any other answer is a verdict.
func RunProbe(ctx context.Context, d dispatch.Dispatcher, ref dispatch.Ref, path, model string) (Verdict, error) {
	nonce := Nonce()
	if path == "" {
		path = "/v1/messages"
	}
	resp, err := d.Do(ctx, dispatch.Request{Purpose: PurposeProbe, Ref: ref, Provider: bschemas.Anthropic,
		Method: http.MethodPost, Path: path,
		Header: http.Header{"Content-Type": {"application/json"}, "Anthropic-Version": {"2023-06-01"}},
		Body:   ProbeBody(model, nonce)})
	if err != nil {
		return Unknown, err
	}
	if resp.Status == http.StatusTooManyRequests || resp.Status >= 500 {
		return Unknown, errStatus(resp.Status)
	}
	if resp.Status != http.StatusOK {
		return Fail, nil
	}
	return ProbeVerdict(resp.Body, nonce), nil
}

type errStatus int

func (e errStatus) Error() string { return "tooldefer: probe status " + itoa(int(e)) }

// AnthropicModel reports whether model names a Claude model: v1 defers on Claude models only.
func AnthropicModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "claude")
}
