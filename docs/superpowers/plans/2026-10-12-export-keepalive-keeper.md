# Export the keep-alive keeper — Implementation Plan (Phase 1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the idle keep-alive out of `proxy` into an importable `keepalive` package, behind a two-method `Dispatcher`, with no behavior change for `cmd/context-guru-proxy`.

**Architecture:** The keeper's state machine (entries, gates, sweep, ping body, cost ceiling, write-vs-read guard, summary substitute, hard deadline, eviction) moves verbatim into `keepalive.Keeper`. Everything that is about the proxy as a host — the upstream allow-list, the caller's credential, the rate limiter, manager strategies and session overrides, the audit-sink rule — stays in `proxy`, which becomes the first host: a thin `keeper` wrapper plus an `httpDispatcher`. The credential-masking helpers move to a public `credmask` package so a second host (Cortex, Plan 2) uses the same code.

**Tech Stack:** Go 1.26 (module `github.com/rossoctl/context-guru`), bifrost schemas, gjson/sjson.

**Spec:** rossoctl/context-guru PR #381, `docs/proposals/cortex-integration.md` at commit `03c5b6a` (§6, §7, §10 Phase 1, Appendix A). Read it with `git show 03c5b6a:docs/proposals/cortex-integration.md`. The companion plan for the Cortex side is `rossoctl/cortex` `docs/superpowers/plans/2026-10-12-contextguru-keepalive-adapter.md`.

## Global Constraints

- Go 1.26; module `github.com/rossoctl/context-guru`; pure-Go build (`CGO_ENABLED=0` for `make build`), cgo only for `-race`.
- **No behavior change** for `cmd/context-guru-proxy`: every existing test in `./proxy/...` passes, with only mechanical edits to how it reaches keeper internals.
- **Fail open, always.** A keep-alive error never affects a request.
- No sidecar-plugin code in this repo. `keepalive` and `credmask` must not import `proxy`, `dash`, `tenant` or anything Cortex.
- A credential is never logged, never persisted, masked while idle, zeroized on every release path, and bounded by the `(MaxPings+1) x Idle` hard deadline — Appendix A of the spec, unchanged.
- Commits: `git commit -s` (DCO), conventional-commit titles, `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`; never `Co-Authored-By`.
- Run Go commands with Homebrew Go on this laptop: `export PATH=/opt/homebrew/bin:$PATH`. (The repo's `CLAUDE.md` describes a shared eval box; it is not set up on this machine, and local Go 1.27.2 builds this module.)
- Work only in the worktree `.worktrees/keeper-export` on branch `feat/export-keepalive-keeper`.

## Deliberate deviations from the spec (review these)

The spec was written against `v0.4.2`; `main` has since moved. These choices follow the code as it is today:

1. **`Key{Tenant, Session, Thread}`, not `(tenant, session)`.** Since #423 the keeper tracks one entry per *thread*. `Arrive`, `Release` and every lookup take a `Key`.
2. **`Record.Hold func()` captures the credential.** The spec has the host capture it "at Arrive/Record time". If the host captured it before the keeper decided, every refused request (non-2xx, below the prefix floor, thinking-enabled, …) would leave a credential held with no entry and no deadline. So the keeper calls `Hold` once, under its lock, only when it tracks the entry, and calls `Dispatcher.Release(key)` exactly once when that entry leaves. Hold and Release are always paired.
3. **`Pricer.Price(ctx, endpoint, model, promptTokens)`.** Cortex prices by host and by prompt size (long-context premiums); the proxy's `modelinfo.Pricer` prices by model only. The proxy adapter ignores the two extra arguments.
4. **The summary substitute moves with the keeper** (agreed in review). It needs no host plumbing: `offload.KeepAliveSubstitute` already owns the call.
5. **Policy is resolved by the host.** The keeper takes a resolved `Policy`. The proxy keeps its strategy and override layers and resolves before calling `Record`.

## Review Focus

1. **A ping in flight while its entry is released** (a real request `Arrive`s, the hard deadline fires, or `Shutdown` runs). Expected: the ping completes or fails on its own copy of the material; nothing panics; the released credential is zeroized and never used again.
2. **The same key recorded twice in a row** (a request finishes while the previous entry for that thread is still held). Expected: the old credential is released *before* the new one is held, and the new one is never zeroized by the old entry's release.
3. **Kill switch `CONTEXT_GURU_KEEPALIVE=off`.** Expected: `Hold` is never called and nothing is retained, as today.
4. **`Shutdown` with entries held.** Expected: `Release` is called once for every held entry before `Shutdown` returns, and a second `Shutdown` is a no-op.
5. **A host whose `Dispatcher.Release` is slow or blocks.** Expected: it runs under the keeper lock (documented), so the contract test pins that `Release` must not call back into the keeper; the proxy's implementation only zeroizes and deletes.

---

## File Structure

| File | Responsibility |
|---|---|
| `credmask/credmask.go` (new) | Per-process XOR mask, `Mask`, `Zero`, `Header` — Appendix A, moved from `proxy/keepalive.go:102-140` |
| `credmask/credmask_test.go` (new) | Mask is its own inverse; Zero overwrites; masked bytes differ |
| `keepalive/types.go` (new) | `Key`, `Usage`, `Price`, `Pricer`, `Policy`, `DefaultMaxUSDPerPing`, `Disabled()`, bounds constants |
| `keepalive/dispatch.go` (new) | `Dispatcher`, `DispatchRequest`, `PingHeaders` |
| `keepalive/keeper.go` (new) | `Keeper`: entries, gates, sweep, fire, record1, eviction, stats — moved from `proxy/keepalive.go` |
| `keepalive/substitute.go` (new) | `fireSummarySubstitute`, `recordSummarySubstitute`, `cachePhaseAt`, `preExpiryFor` — moved |
| `keepalive/ping.go` (new) | `pingBody`, `pingOutputBudget`, `eligible` — moved |
| `keepalive/keepalivetest/contract.go` (new) | Exported `RunDispatcherContract(t, NewDispatcherFunc)` that any host's dispatcher must pass |
| `keepalive/*_test.go` (moved + new) | The keeper's white-box tests, moved from `proxy` |
| `proxy/keepalive.go` (rewritten) | Thin host: `keeper` wrapper (policy resolution, overrides, strategies), aliases, `PendingPings` |
| `proxy/keepalivedispatch.go` (new) | `httpDispatcher`: holds upstream + masked credential per key, sends the ping over `h.client` |
| `proxy/keepaliveoverride.go`, `proxy/keepalivestrategy.go` | Unchanged logic; `k.mu`/`k.now` now refer to the wrapper's own lock and the core clock |
| `config/markercap.go` (new) | `Config.CapMarkerMode` — a host without an expand loop caps every offloader's `marker_mode` |

---

### Task 1: `credmask` package

**Files:**
- Create: `credmask/credmask.go`, `credmask/credmask_test.go`
- Modify: `proxy/keepalive.go:102-140` (delete the moved code; use `credmask`)

**Interfaces:**
- Produces: `credmask.Mask(b []byte)`, `credmask.Zero(b []byte)`, `type credmask.Header struct{ Name string; Val []byte }`, `func (h Header) Clone() Header`.

- [ ] **Step 1: Write the failing test**

```go
// credmask/credmask_test.go
package credmask

import (
	"bytes"
	"testing"
)

func TestMaskIsItsOwnInverse(t *testing.T) {
	orig := []byte("sk-ant-api03-secret-value")
	b := append([]byte(nil), orig...)
	Mask(b)
	if bytes.Equal(b, orig) {
		t.Fatal("masked bytes equal the plaintext")
	}
	if bytes.Contains(b, []byte("secret")) {
		t.Fatal("masked bytes still contain a plaintext run")
	}
	Mask(b)
	if !bytes.Equal(b, orig) {
		t.Fatalf("unmask = %q, want %q", b, orig)
	}
}

func TestZeroOverwrites(t *testing.T) {
	b := []byte("Bearer abc")
	Zero(b)
	for i, c := range b {
		if c != 0 {
			t.Fatalf("byte %d = %d after Zero, want 0", i, c)
		}
	}
}

func TestHeaderCloneIsIndependent(t *testing.T) {
	h := Header{Name: "Authorization", Val: []byte("x")}
	c := h.Clone()
	Zero(h.Val)
	if c.Val[0] != 'x' {
		t.Fatal("Clone shares its value with the original")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./credmask/`
Expected: FAIL — `undefined: Mask` (package has no non-test files).

- [ ] **Step 3: Write the implementation**

Move `credMask`, `xorMask`, `zero` and `maskedHeader` from `proxy/keepalive.go:102-140`, keeping their doc comments word for word, renamed as below:

```go
// Package credmask holds a credential (or a transcript) masked while it is idle in memory.
//
// Be precise about what this buys, because it is easy to oversell. A heap or core dump, a crash
// report, a `/proc/<pid>/mem` read or a stray `strings` over a snapshot yields masked bytes
// instead of a working key — that is the accidental-capture class, and it is the realistic one.
// It does NOT stop an attacker with code execution in this process: the mask is right here in
// the same address space. It is obfuscation at rest, not encryption.
//
// Shared by every keep-alive host (cmd/context-guru-proxy and the Cortex plugin), so the one
// security-sensitive routine exists once.
package credmask

import "crypto/rand"

var key = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("context-guru: no entropy for the keep-alive credential mask: " + err.Error())
	}
	return b
}()

// Mask masks or unmasks in place — XOR is its own inverse, so one function does both.
func Mask(b []byte) {
	for i := range b {
		b[i] ^= key[i%len(key)]
	}
}

// Zero overwrites a buffer. Dropping the reference is not enough: the bytes sit in the heap
// until a collection that may never come, and a dump taken in between still has them.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Header is one auth header held masked. The NAME is not a secret; the value is.
type Header struct {
	Name string
	Val  []byte
}

// Clone returns a copy whose value can be zeroized without touching the original.
func (h Header) Clone() Header {
	return Header{Name: h.Name, Val: append([]byte(nil), h.Val...)}
}
```

In `proxy/keepalive.go`, delete lines 102-140 and replace every use: `xorMask(` → `credmask.Mask(`, `zero(` → `credmask.Zero(`, `maskedHeader{name: n, val: v}` → `credmask.Header{Name: n, Val: v}`, `.name` → `.Name`, `.val` → `.Val` on those values. Drop the `crypto/rand` import from `proxy/keepalive.go`.

- [ ] **Step 4: Run the tests**

Run: `go test ./credmask/ && go test -count=1 ./proxy/ -run 'KeepAlive|Keeper|Retained|Override|Thread'`
Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l credmask proxy
git add credmask proxy/keepalive.go
git commit -s -m "refactor(keepalive): move credential masking to a shared credmask package" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: `keepalive` types, with proxy aliases

**Files:**
- Create: `keepalive/types.go`, `keepalive/types_test.go`
- Modify: `proxy/keepalive.go` (delete `CachePolicy`, `on`, `Ceiling`, `DefaultMaxUSDPerPing`, `keepAliveDisabled`, the bounds `const` block; add aliases)

**Interfaces:**
- Produces:
  - `type keepalive.Key struct{ Tenant, Session, Thread string }`
  - `type keepalive.Usage struct{ FreshInput, CacheRead, CacheWrite, Output, CacheWrite1h int64; StopReason string }`
  - `type keepalive.Price = modelinfo.Price` (fields `Input, Output, CacheRead, CacheWrite float64`, USD per token)
  - `type keepalive.Pricer interface{ Price(ctx context.Context, endpoint, model string, promptTokens int64) (Price, bool) }`
  - `type keepalive.Policy struct{…}` — `CachePolicy` moved verbatim; methods `On() bool`, `Ceiling() float64`
  - `const keepalive.DefaultMaxUSDPerPing = 0.50`
  - `func keepalive.Disabled() bool` — the `CONTEXT_GURU_KEEPALIVE` kill switch

- [ ] **Step 1: Write the failing test**

```go
// keepalive/types_test.go
package keepalive

import (
	"math"
	"testing"
	"time"
)

func TestPolicyCeilingResolvesZeroToTheDefault(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{{0, DefaultMaxUSDPerPing}, {0.1, 0.1}, {-1, math.Inf(1)}}
	for _, c := range cases {
		if got := (Policy{MaxUSDPerPing: c.in}).Ceiling(); got != c.want {
			t.Errorf("Ceiling(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPolicyOnNeedsAllThree(t *testing.T) {
	on := Policy{KeepAlive: true, Idle: time.Minute, MaxPings: 2}
	if !on.On() {
		t.Fatal("complete policy reported off")
	}
	for _, p := range []Policy{
		{Idle: time.Minute, MaxPings: 2},
		{KeepAlive: true, MaxPings: 2},
		{KeepAlive: true, Idle: time.Minute},
	} {
		if p.On() {
			t.Errorf("%+v reported on", p)
		}
	}
}

func TestDisabledReadsTheKillSwitch(t *testing.T) {
	for _, v := range []string{"off", "0", "false", " OFF "} {
		t.Setenv("CONTEXT_GURU_KEEPALIVE", v)
		if !Disabled() {
			t.Errorf("CONTEXT_GURU_KEEPALIVE=%q did not disable", v)
		}
	}
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "")
	if Disabled() {
		t.Error("empty CONTEXT_GURU_KEEPALIVE disabled the keep-alive")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./keepalive/`
Expected: FAIL — `undefined: Policy`.

- [ ] **Step 3: Write the implementation**

`keepalive/types.go`: a package doc comment made of `proxy/keepalive.go:29-100` (the "# What it is for … the guards" essay, unchanged), then:

```go
package keepalive

import (
	"context"
	"math"
	"os"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/thread"
)

// Key names one tracked THREAD of one session (#423). Thread "" is the primary thread.
type Key struct{ Tenant, Session, Thread string }

// id is the live-map key. The primary thread keys as tenant NUL session, exactly as before
// threads existed; the NUL separator is what lets RetireSession and Forget match by prefix.
func (k Key) id() string { return k.Tenant + "\x00" + thread.Key(k.Session, k.Thread) }

// Usage is what one upstream response billed, in the provider's own units. A host fills it from
// whatever it already parses; the keeper never reads a response body itself.
type Usage struct {
	FreshInput, CacheRead, CacheWrite, Output int64
	// CacheWrite1h is the part of CacheWrite billed at the one-hour tier.
	CacheWrite1h int64
	// StopReason is the response's own terminal stop reason; read only by a policy predictor.
	StopReason string
}

// Price is one model's per-token rates in USD. An alias, so the proxy's modelinfo.Pricer and a
// Cortex resolver produce the same struct and the keeper prices with one formula
// (Price.CostWithCacheWrite1h).
type Price = modelinfo.Price

// Pricer resolves rates for a ping. endpoint is the upstream host the entry's request went to and
// promptTokens the billed prefix; a host that prices by model alone ignores both.
type Pricer interface {
	Price(ctx context.Context, endpoint, model string, promptTokens int64) (Price, bool)
}
```

Then move, unchanged except for the renames below: `CachePolicy` (`proxy/keepalive.go:142-184`) as `Policy`; `on()` stays and a new `func (p Policy) On() bool { return p.on() }` is added for hosts; `Ceiling` (`:431-451`); `DefaultMaxUSDPerPing` (`:453-456`; keep the comment, which says it mirrors `config.DefaultKeepAliveMaxUSDPerPing`); the bounds `const` block (`:191-222`); `keepAliveDisabled` (`:552-558`) as exported `Disabled`.

In `proxy/keepalive.go` add:

```go
// CachePolicy is keepalive.Policy, kept under its old name for the config builder and tests.
type CachePolicy = keepalive.Policy

// DefaultMaxUSDPerPing mirrors keepalive's; pinned to config's by
// TestTheProxysPingCeilingDefaultMatchesTheConfigLoaders.
const DefaultMaxUSDPerPing = keepalive.DefaultMaxUSDPerPing

func keepAliveDisabled() bool { return keepalive.Disabled() }
```

Replace `pol.on()` in `proxy` with `pol.On()`. Leave the bounds constants referenced from `proxy` (if any test names `maxKeepAliveBodyBytes`) as aliases: `const maxKeepAliveBodyBytes = keepalive.MaxBodyBytes` — export the four bounds as `MaxSessions`, `MaxBytes`, `MaxTurnKeys`, `MaxBodyBytes` and keep `keepAliveTick` unexported as `tick`.

- [ ] **Step 4: Run the tests**

Run: `go test ./keepalive/ && go vet ./proxy/ && go test -count=1 ./proxy/ -run 'KeepAlive|Keeper|Ceiling|Override'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l keepalive proxy
git add keepalive proxy
git commit -s -m "refactor(keepalive): move the cache policy and keeper bounds to package keepalive" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: `Dispatcher` and its exported contract test

**Files:**
- Create: `keepalive/dispatch.go`, `keepalive/dispatch_test.go`, `keepalive/keepalivetest/contract.go`

**Interfaces:**
- Consumes: `keepalive.Key`, `keepalive.Usage` (Task 2), `credmask` (Task 1).
- Produces:

```go
// keepalive/dispatch.go
type DispatchRequest struct {
	Key      Key
	Provider bschemas.ModelProvider
	Model    string
	// Route is the upstream path the recorded request used ("/v1/messages", "/v1/responses").
	Route string
	// Body is the exact ping bytes. Owned by the keeper and zeroized after Dispatch returns:
	// a Dispatcher must not retain it.
	Body []byte
	// Headers is non-secret only, built by PingHeaders from a fixed allow-list. No path puts a
	// credential here; the Dispatcher attaches the one it Held for Key.
	Headers http.Header
}

// Dispatcher is the host's half of a ping: transport and credential custody, never a decision.
type Dispatcher interface {
	// Dispatch sends one ping. A non-nil error is transient (the keeper retries on the next due
	// tick); a 4xx status stops the entry; 2xx usage feeds the write-vs-read guard.
	Dispatch(ctx context.Context, req DispatchRequest) (u Usage, status int, err error)
	// Release zeroizes whatever the host Held for key. Called exactly once per Hold, UNDER THE
	// KEEPER'S LOCK — it must not call back into the Keeper. A Dispatch already in flight for
	// key must keep working on its own copy.
	Release(key Key)
}

// PingHeaders copies the non-secret headers a ping needs from the recorded request.
func PingHeaders(src http.Header) http.Header
```

- `package keepalivetest`: `type Harness struct{ D keepalive.Dispatcher; Hold func(k keepalive.Key, auth http.Header); Seen func() []http.Header }`, `type NewHarness func(t *testing.T, upstreamURL string) Harness`, `func RunDispatcherContract(t *testing.T, newH NewHarness)`.

- [ ] **Step 1: Write the failing test for `PingHeaders`**

```go
// keepalive/dispatch_test.go
package keepalive

import (
	"net/http"
	"testing"
)

func TestPingHeadersCarriesNoCredential(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer secret")
	src.Set("x-api-key", "secret")
	src.Set("x-goog-api-key", "secret")
	src.Set("Cookie", "secret")
	src.Set("Anthropic-Version", "2023-06-01")
	src.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
	src.Set("OpenAI-Organization", "org-1")
	src.Set("OpenAI-Project", "proj-1")
	got := PingHeaders(src)
	for name, vs := range got {
		for _, v := range vs {
			if v == "secret" || v == "Bearer secret" {
				t.Fatalf("PingHeaders copied %s", name)
			}
		}
	}
	want := map[string]string{
		"Content-Type": "application/json", "Anthropic-Version": "2023-06-01",
		"Anthropic-Beta": "prompt-caching-2024-07-31", "Openai-Organization": "org-1",
		"Openai-Project": "proj-1",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("PingHeaders returned %d headers, want %d: %v", len(got), len(want), got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./keepalive/ -run PingHeaders`
Expected: FAIL — `undefined: PingHeaders`.

- [ ] **Step 3: Implement `dispatch.go`**

The types above, plus `PingHeaders` — the non-secret half of today's `pingHeaders` (`proxy/keepalive.go:1548-1555`), with its comment:

```go
func PingHeaders(src http.Header) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "OpenAI-Organization", "OpenAI-Project"} {
		if v := src.Get(name); v != "" {
			h.Set(name, v)
		}
	}
	return h
}
```

- [ ] **Step 4: Write the contract test harness**

`keepalive/keepalivetest/contract.go`. It starts an `httptest.Server` that records each request's headers and answers `{"usage":{"input_tokens":1,"cache_read_input_tokens":4000,"cache_creation_input_tokens":0,"output_tokens":1}}` with status 200, then runs these subtests against a fresh harness each:

```go
package keepalivetest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/keepalive"
)

// Harness is one host's dispatcher under test, plus the host-side hook the keeper's Record.Hold
// would call. Hold receives the request headers the host captures the credential from.
type Harness struct {
	D    keepalive.Dispatcher
	Hold func(k keepalive.Key, reqHeaders http.Header)
}

// NewHarness builds a dispatcher whose pings go to upstreamURL (scheme://host, no path).
type NewHarness func(t *testing.T, upstreamURL string) Harness

const usageJSON = `{"usage":{"input_tokens":1,"cache_read_input_tokens":4000,` +
	`"cache_creation_input_tokens":0,"output_tokens":1}}`

// RunDispatcherContract is what every keep-alive host's Dispatcher must satisfy. Phase 3's
// Cortex PR runs it against cortexDispatcher; a context-guru change that breaks it fails here
// first.
func RunDispatcherContract(t *testing.T, newH NewHarness) {
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageJSON)
	}))
	t.Cleanup(srv.Close)
	last := func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return nil
		}
		return seen[len(seen)-1]
	}
	key := keepalive.Key{Tenant: "t1", Session: "s1"}
	auth := http.Header{}
	auth.Set("Authorization", "Bearer caller-key")
	req := func() keepalive.DispatchRequest {
		return keepalive.DispatchRequest{Key: key, Provider: bschemas.Anthropic, Model: "m",
			Route: "/v1/messages", Body: []byte(`{"model":"m","max_tokens":1,"stream":false}`),
			Headers: keepalive.PingHeaders(http.Header{"Anthropic-Version": {"2023-06-01"}})}
	}

	t.Run("dispatch attaches the held credential and returns usage", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.Hold(key, auth)
		u, status, err := h.D.Dispatch(context.Background(), req())
		if err != nil || status != 200 {
			t.Fatalf("Dispatch = %d, %v", status, err)
		}
		if u.CacheRead != 4000 || u.CacheWrite != 0 {
			t.Fatalf("usage = %+v, want cache_read 4000", u)
		}
		if got := last().Get("Authorization"); got != "Bearer caller-key" {
			t.Fatalf("upstream saw Authorization %q", got)
		}
		if last().Get("Anthropic-Version") != "2023-06-01" {
			t.Fatal("non-secret header not forwarded")
		}
		h.D.Release(key)
	})

	t.Run("dispatch after release carries no credential", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.Hold(key, auth)
		h.D.Release(key)
		_, status, err := h.D.Dispatch(context.Background(), req())
		if err == nil && last().Get("Authorization") == "Bearer caller-key" && status == 200 {
			t.Fatal("a released credential was used")
		}
	})

	t.Run("release of an unknown key is a no-op", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.D.Release(keepalive.Key{Tenant: "nobody", Session: "x"})
	})

	t.Run("hold replaces, and keys do not cross", func(t *testing.T) {
		h := newH(t, srv.URL)
		other := keepalive.Key{Tenant: "t1", Session: "s1", Thread: "a:sub"}
		a2 := http.Header{}
		a2.Set("Authorization", "Bearer second")
		h.Hold(key, auth)
		h.Hold(other, a2)
		h.D.Release(other)
		if _, _, err := h.D.Dispatch(context.Background(), req()); err != nil {
			t.Fatal(err)
		}
		if got := last().Get("Authorization"); got != "Bearer caller-key" {
			t.Fatalf("releasing another thread affected this one: %q", got)
		}
		h.D.Release(key)
	})

	t.Run("dispatch does not retain the body", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.Hold(key, auth)
		r := req()
		if _, _, err := h.D.Dispatch(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		for i := range r.Body {
			r.Body[i] = 'x' // the keeper zeroizes it; a retained alias would now be corrupt
		}
		if _, _, err := h.D.Dispatch(context.Background(), req()); err != nil {
			t.Fatal(err)
		}
		h.D.Release(key)
	})
}
```

- [ ] **Step 5: Run the tests**

Run: `go vet ./keepalive/... && go test ./keepalive/...`
Expected: PASS (`keepalivetest` has no tests of its own yet; Task 5 runs it against the proxy dispatcher).

- [ ] **Step 6: Commit**

```bash
gofmt -l keepalive
git add keepalive
git commit -s -m "feat(keepalive): add the Dispatcher interface and an exported contract test" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: Move the keeper core into `keepalive.Keeper`

The largest task. It is a move, not a rewrite: every function below keeps its body and its comments; only the names on this list change. Do it in one commit so the tree never builds with two keepers.

**Files:**
- Create: `keepalive/keeper.go`, `keepalive/substitute.go`, `keepalive/ping.go`
- Move (git mv, then edit): `proxy/keepalive_test.go` → `keepalive/keeper_test.go`; `proxy/keepalive_snapshot_test.go` → `keepalive/snapshot_test.go`; `proxy/keepalive_summarysubstitute_test.go` → `keepalive/substitute_test.go`; `proxy/keepalive_candidate_drain_test.go` → `keepalive/candidate_drain_test.go`; `proxy/keepalive_billed_contract_test.go` → `keepalive/billed_contract_test.go`; `proxy/keepalive_openai_test.go` → `keepalive/openai_test.go`; plus any testdata they read (check with `grep -n testdata` in each file and `git mv` it too).
- Modify: `proxy/keepalive.go` (only what remains: see Task 5)

**Interfaces:**
- Consumes: Tasks 1-3.
- Produces:

```go
type Options struct {
	Dispatcher Dispatcher // required
	Prices     Pricer     // nil: every ping is unpriced, as with no price table today
	// Spare reserves the tenant's spare rate/concurrency headroom for one ping; nil = no limiter.
	Spare func(tenant string, reserveFrac float64) (release func(), err error)
	Now    func() time.Time // nil: time.Now
	Go     func(f func())   // runs one ping; nil: `go f()`. Tests pass an inline runner.
	OnTick func(now time.Time) // start of every sweep, outside the keeper lock; nil: none
}

type Record struct {
	Key       Key
	StartedAt time.Time // when the upstream request STARTED
	Body      []byte    // exact bytes sent upstream; the keeper copies and masks it
	Provider  bschemas.ModelProvider
	Endpoint  string    // upstream host, passed to Pricer
	Route     string    // upstream path
	Status    int
	Usage     Usage
	UsageOK   bool
	Policy    Policy    // resolved by the host
	Tag       string    // host label returned by Arrive (proxy: the applied strategy id)
	Agent, Preset string
	Header    http.Header // the request's headers; only PingHeaders' allow-list is read
	Store     store.Store
	Emitter   components.Emitter // nil: components.NopEmitter{}
	Hold      func()             // called once, under the keeper lock, only if the entry is tracked
}

type EntryView struct {
	Pings, Turn int
	Prefix      int64
	PingUSD, Spent float64
	Refreshed   int64
	Stopped     bool
	Policy      Policy
	Tag         string
	MaskedBody  []byte // a copy, still masked
}

func New(o Options) *Keeper
func (k *Keeper) Start()
func (k *Keeper) Shutdown()
func (k *Keeper) Now() time.Time
func (k *Keeper) Arrive(key Key) (pings int, refreshed int64, tag string)
func (k *Keeper) Record(r Record)
func (k *Keeper) Retire(key Key)
func (k *Keeper) RetireSession(tenant, session string)
func (k *Keeper) Forget(tenant string)
func (k *Keeper) Sweep(now time.Time) int
func (k *Keeper) Stats() Stats          // Stats is today's KeepAliveStats, renamed
func (k *Keeper) PendingPings() int
func (k *Keeper) LiveSessionKeys() []string
func (k *Keeper) Entry(key Key) (EntryView, bool)
```

All methods are nil-safe, as today's are.

- [ ] **Step 1: Move the tests first and make them fail to compile**

`git mv` the six test files listed above into `keepalive/`, change `package proxy` to `package keepalive`, and update their helpers to build a keeper with `New(Options{Dispatcher: fd, Now: clock.now, Go: func(f func()) { f() }})`, where `fd` is a test fake:

```go
// keepalive/fake_test.go
package keepalive

import (
	"context"
	"sync"
)

// fakeDispatcher replaces today's k.send override. resp is what each Dispatch returns.
type fakeDispatcher struct {
	mu       sync.Mutex
	resp     func(DispatchRequest) (Usage, int, error)
	sent     []DispatchRequest
	held     map[Key]int // Hold count minus Release count, per key
	released []Key
}

func newFake(resp func(DispatchRequest) (Usage, int, error)) *fakeDispatcher {
	return &fakeDispatcher{resp: resp, held: map[Key]int{}}
}

func (f *fakeDispatcher) Dispatch(_ context.Context, r DispatchRequest) (Usage, int, error) {
	f.mu.Lock()
	f.sent = append(f.sent, DispatchRequest{Key: r.Key, Route: r.Route, Model: r.Model,
		Body: append([]byte(nil), r.Body...), Headers: r.Headers.Clone()})
	resp := f.resp
	f.mu.Unlock()
	return resp(r)
}

func (f *fakeDispatcher) Release(k Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[k]--
	f.released = append(f.released, k)
}

// hold is what a test passes as Record.Hold.
func (f *fakeDispatcher) hold(k Key) func() {
	return func() { f.mu.Lock(); f.held[k]++; f.mu.Unlock() }
}
```

Mechanical edits inside the moved tests:

| Old (in `proxy` tests) | New (in `keepalive` tests) |
|---|---|
| `k.send = func(j pingJob, body []byte) (Usage, int, error) {…}` | `fd.resp = func(r DispatchRequest) (Usage, int, error) {…}`, reading `r.Body` where the old code read `body`, and `r.Key` where it read `j.tenant`/`j.session`/`j.thread` |
| `k.dispatch = k.fire` | `Options.Go: func(f func()) { f() }` |
| `k.now = clock.now` | `Options.Now: clock.now` |
| `k.live[kaKey(t, s)]` | `k.live[Key{Tenant: t, Session: s}.id()]` |
| `k.record(tn, s, th, at, body, up, r, prov, route, st, u, ok)` | `k.Record(Record{Key: Key{tn.ID, s, th}, StartedAt: at, Body: body, Provider: prov, Route: route, Status: st, Usage: u, UsageOK: ok, Policy: pol, Header: r.Header, Store: st0, Hold: fd.hold(key)})` — `pol` is what the test's tenancy's `Cache` was |
| `k.arrive(t, s, th)` | `k.Arrive(Key{t, s, th})` |
| `k.sweep(now)` | `k.Sweep(now)` |
| `xorMask` / `zero` / `maskedHeader` | `credmask.Mask` / `credmask.Zero` / `credmask.Header` |
| `e.auth[0].val` (credential held by the entry) | the credential is no longer in the entry: assert `fd.held[key] == 1` while tracked and `0` after release |
| `k.h.opts.Prices = p` | `Options.Prices: proxyPricer{p}` — a 6-line test adapter in `fake_test.go` that ignores endpoint/promptTokens |
| `k.h.rec = nil` (audit gate) | not a keeper concern any more; that test stays in `proxy` (Task 5) |
| `upstream{…}` in a test | drop it; the route goes in `Record.Route` |

Run: `go test ./keepalive/`
Expected: FAIL to compile — `undefined: New`, `Record`, `Keeper` (and so on).

- [ ] **Step 2: Move the code**

Into `keepalive/keeper.go`, from `proxy/keepalive.go`, keeping bodies and comments: `kaEntry` (`:224-325`), `due` (`:327-339`), `pingable` (`:382-429`), `keeper` (`:458-550`) as `Keeper`, `newKeeper` (`:560-571`) as `New`, `start` as `Start`, `Stop` as `Shutdown` (Task 6 adds the Release loop), `clear` (`:624-645`), `arrive` as `Arrive`, `record` (`:688-846`) as `Record`, `projectedPingUSD`/`projectedSummaryUSD`/`expectedRefreshSavingUSD` (`:848-914`), `retire` (`:916-935`), `retireSession` as `RetireSession`, `forget` as `Forget`, `evictLocked`, `sweep` as `Sweep`, `pingJob`, `fire`, `markStopped`, `record1`, `pendingPings` as `PendingPings`, `Stats`, `KeepAliveStats` as `Stats`, `LiveSessionKeys`. Into `keepalive/substitute.go`: `cachePhaseAt`, `preExpiryFor`, `fireSummarySubstitute`, `recordSummarySubstitute`. Into `keepalive/ping.go`: `pingOutputBudget`, `pingBody`, `keepAliveEligible` as `eligible`, `keepAlivePingTimeout` as `pingTimeout`, `keepAliveReserveFrac` as `reserveFrac`.

Then apply exactly these changes and no others:

1. **Fields removed from `kaEntry`:** `up upstream`, `auth []maskedHeader`. **Added:** `key Key`, `endpoint string`, `tag string` (replaces `appliedStrategy`), `held bool` (true between `Hold` and `Release`).
2. **Fields removed from `Keeper`:** `h *Handler`, `overrides`, `strategies`, `send`, `dispatch`. **Added:** `o Options` (with `Now`, `Go` defaulted in `New`), and `now` reads `k.o.Now`.
3. **`New(o Options)`:** `o.Now = time.Now` if nil; `o.Go = func(f func()) { go f() }` if nil; no `loadStrategies` call (the proxy does it, Task 5).
4. **`Record(r Record)`** keeps today's order of checks, with these substitutions: `tn.ID` → `r.Key.Tenant`; the kill switch stays; the `k.h.rec == nil` block (`:713-722`) is deleted (the host passes a zero Policy instead — Task 5); the policy block (`:723-759`) is deleted and replaced by `pol := r.Policy`; `pingHeaders(r, up)` → `hdr := PingHeaders(r.Header)`; `tn.Store` → `r.Store`; `tn.Pipe.Emitter()` → `r.Emitter` (default `components.NopEmitter{}` when nil); `tn.Preset` → `r.Preset`; `r.UserAgent()` → `r.Agent`; `applied` → `r.Tag`; `k.projectedPingUSD(model, prefix, route)` gains `r.Endpoint`. **The Hold/Release pairing** goes where the previous entry is dropped and the new one is admitted:

```go
	if prev, ok := k.live[key]; ok {
		k.bytes -= int64(len(prev.body))
		k.releaseLocked(prev) // the OLD credential goes before the new one is held
		prev.clear()
		delete(k.live, key)
	}
	if !e.pingable() {
		k.skipped.Add(1)
		e.clear() // hold no body and no credential for a session we will not ping
		return
	}
	k.live[key] = e
	k.bytes += int64(len(owned))
	if r.Hold != nil {
		r.Hold()
	}
	e.held = r.Hold != nil
```

with a new helper used by **every** path that removes a tracked entry (`Arrive`, `retire`, `RetireSession`, `Forget`, `evictLocked`, the eager drop in `Sweep`, `Shutdown`, and the replacement above):

```go
// releaseLocked tells the host to drop the credential it Held for e. Exactly once per Hold:
// e.held is cleared here, so a second removal path reaching the same entry is a no-op.
func (k *Keeper) releaseLocked(e *kaEntry) {
	if e.held {
		e.held = false
		k.o.Dispatcher.Release(e.key)
	}
}
```

5. **`Sweep`:** at the top, before taking the lock, `if k.o.OnTick != nil { k.o.OnTick(now) }` (replaces `k.expireOverridesLocked(now)`). `pingJob` drops `auth` and `up`, gains `key Key`, `route string`, `provider`, `model`, `hdr`.
6. **`fire`:** `k.h.limiter.AcquireSpare(j.tenant, keepAliveReserveFrac)` → a helper `k.spare(j.key.Tenant)` that returns a no-op release when `k.o.Spare == nil`; `pingBody(j.raw, j.up.path)` → `pingBody(j.raw, j.route)`; `k.send(j, body)` →

```go
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	u, status, err := k.o.Dispatcher.Dispatch(ctx, DispatchRequest{Key: j.key,
		Provider: j.provider, Model: j.model, Route: j.route, Body: body, Headers: j.hdr})
	cancel()
	credmask.Zero(body)
```

   The deferred wipe keeps `credmask.Zero(j.raw)` and drops the `j.auth` loop.
7. **Pricing:** every `k.h.opts.Prices` read becomes `k.o.Prices`, and `p.Price(context.Background(), model)` becomes `p.Price(context.Background(), e.endpoint, model, e.prefix)` (`projectedSummaryUSD` and `expectedRefreshSavingUSD` take the entry's endpoint and prefix; add the parameters).
8. **`Usage`** in moved code is `keepalive.Usage`. `tenantLabel` (used in log lines) moves with the code if it is defined in `proxy/keepalive*.go`; if it lives elsewhere in `proxy`, copy the function into `keepalive/keeper.go` verbatim.
9. **`Entry(key Key) (EntryView, bool)`** is new: copy the listed fields under the lock; `MaskedBody: append([]byte(nil), e.body...)`.

- [ ] **Step 3: Run the moved tests**

Run: `go vet ./keepalive/... && go test -count=1 ./keepalive/...`
Expected: PASS. `TestKeepAliveOnProductionSnapshot` must report the same net figure it reports on `main` (+$125.08 at the shipped defaults, per the comment at the top of the old file); if it differs, the move changed behavior — stop and find out why before going on.

- [ ] **Step 4: Commit** (after Task 5 compiles — Tasks 4 and 5 land as one commit)

---

### Task 5: The proxy becomes the first host

**Files:**
- Create: `proxy/keepalivedispatch.go`, `proxy/keepalivedispatch_test.go`
- Modify: `proxy/keepalive.go` (rewrite to the wrapper), `proxy/proxy.go:339,375,1248,1595,2309`, `proxy/keepaliveoverride.go`, `proxy/keepalivestrategy.go`, `proxy/savings.go:84`, `proxy/modes.go:55`, `proxy/campaign.go` (the `loadStrategies` call sites), `proxy/control.go`, `proxy/keepalivectl.go` (the `forget` call sites keep their names)
- Modify tests that stay in `proxy`: `proxy/keepalive_wire_test.go`, `proxy/keepaliveoverride_test.go`, `proxy/thread_test.go`, `proxy/campaign_test.go`, `proxy/savings_test.go`

**Interfaces:**
- Consumes: `keepalive.*` (Tasks 2-4), `credmask` (Task 1).
- Produces (proxy-internal): `type keeper struct{ *keepalive.Keeper; h *Handler; d *httpDispatcher; mu sync.Mutex; overrides map[string]sessionOverride; strategies []tenant.Strategy }`, `func newKeeper(h *Handler) *keeper`, `func newKeeperWith(h *Handler, o keepalive.Options) *keeper` (tests), and the unexported methods the rest of `proxy` already calls: `start`, `Stop`, `arrive`, `record`, `forget`, `retireSession`, `pendingPings`, `now`.

- [ ] **Step 1: Write the failing contract test for the proxy dispatcher**

```go
// proxy/keepalivedispatch_test.go
package proxy

import (
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/keepalive/keepalivetest"
)

func TestHTTPDispatcherMeetsTheContract(t *testing.T) {
	keepalivetest.RunDispatcherContract(t, func(t *testing.T, upstreamURL string) keepalivetest.Harness {
		d := newHTTPDispatcher(http.DefaultClient)
		return keepalivetest.Harness{D: d, Hold: func(k keepalive.Key, hdr http.Header) {
			r, _ := http.NewRequest(http.MethodPost, "http://proxy.local/v1/messages", nil)
			r.Header = hdr
			d.hold(k, upstream{base: upstreamURL, path: "/v1/messages"}, r)
		}}
	})
}
```

Run: `go test ./proxy/ -run TestHTTPDispatcherMeetsTheContract`
Expected: FAIL — `undefined: newHTTPDispatcher`.

- [ ] **Step 2: Implement `httpDispatcher`**

```go
// proxy/keepalivedispatch.go
package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/rossoctl/context-guru/credmask"
	"github.com/rossoctl/context-guru/keepalive"
)

// httpDispatcher is cmd/context-guru-proxy's keepalive.Dispatcher. It holds, per tracked
// thread, the upstream the request went to and — when that upstream is caller-pays — the
// caller's credential, MASKED. See credmask and keepalive.Record.Hold.
type httpDispatcher struct {
	client *http.Client
	mu     sync.Mutex
	held   map[keepalive.Key]heldUpstream
}

type heldUpstream struct {
	up   upstream
	auth []credmask.Header
}

func newHTTPDispatcher(c *http.Client) *httpDispatcher {
	return &httpDispatcher{client: c, held: map[keepalive.Key]heldUpstream{}}
}

// hold is what proxy's record passes as keepalive.Record.Hold. The credential half of the old
// pingHeaders, unchanged: only the auth slots, only after our own token has been scrubbed out of
// them, and masked. A server-held key (up.setKey) retains nothing.
func (d *httpDispatcher) hold(key keepalive.Key, up upstream, r *http.Request) {
	var auth []credmask.Header
	if up.setKey == nil {
		slots := http.Header{}
		for _, name := range authHeaders {
			for _, v := range r.Header.Values(name) {
				slots.Add(name, v)
			}
		}
		scrubToken(slots)
		for name, vs := range slots {
			for _, v := range vs {
				b := []byte(v)
				credmask.Mask(b)
				auth = append(auth, credmask.Header{Name: name, Val: b})
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if prev, ok := d.held[key]; ok {
		for _, a := range prev.auth {
			credmask.Zero(a.Val)
		}
	}
	d.held[key] = heldUpstream{up: up, auth: auth}
}

// Release zeroizes and forgets what hold kept. Called under the keeper's lock: no keeper calls.
func (d *httpDispatcher) Release(key keepalive.Key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.held[key]; ok {
		for _, a := range h.auth {
			credmask.Zero(a.Val)
		}
		delete(d.held, key)
	}
}

// Dispatch is the old sendPing. It copies the held material under its own lock, so a Release
// racing an in-flight ping zeroizes the held copy and never the one this request is using.
func (d *httpDispatcher) Dispatch(ctx context.Context, req keepalive.DispatchRequest) (keepalive.Usage, int, error) {
	d.mu.Lock()
	h, ok := d.held[req.Key]
	var auth []credmask.Header
	if ok {
		auth = make([]credmask.Header, len(h.auth))
		for i, a := range h.auth {
			auth[i] = a.Clone()
		}
	}
	d.mu.Unlock()
	defer func() {
		for _, a := range auth {
			credmask.Zero(a.Val)
		}
	}()
	if !ok || h.up.base == "" {
		return keepalive.Usage{}, 0, errNoUpstream
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.up.base+h.up.path,
		strings.NewReader(string(req.Body)))
	if err != nil {
		return keepalive.Usage{}, 0, err
	}
	for name, vs := range req.Headers {
		hreq.Header[name] = append([]string(nil), vs...)
	}
	// Unmask as late as possible and wipe immediately — the old sendPing's comment applies.
	for _, a := range auth {
		credmask.Mask(a.Val)
		hreq.Header.Add(a.Name, string(a.Val))
		credmask.Zero(a.Val)
	}
	setUpstreamAuth(hreq.Header, h.up)
	resp, err := d.client.Do(hreq)
	if err != nil {
		return keepalive.Usage{}, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil && !errors.Is(err, io.EOF) {
		return keepalive.Usage{}, resp.StatusCode, err
	}
	u, _ := responseUsage(resp.Header.Get("Content-Type"), raw)
	return keepalive.Usage{FreshInput: u.FreshInput, CacheRead: u.CacheRead,
		CacheWrite: u.CacheWrite, Output: u.Output, CacheWrite1h: u.CacheWrite1h,
		StopReason: u.StopReason}, resp.StatusCode, nil
}
```

Run: `go test ./proxy/ -run TestHTTPDispatcherMeetsTheContract`
Expected: PASS once Step 3 compiles.

- [ ] **Step 3: Rewrite `proxy/keepalive.go` as the host wrapper**

What remains in the file: the aliases from Task 2, `errNoUpstream` if defined here, and:

```go
// keeper is the proxy's keep-alive host: it resolves policy (account config, manager strategy,
// session override — keepalivestrategy.go, keepaliveoverride.go), enforces the audit-sink rule,
// and owns the httpDispatcher. Every decision about WHEN to ping lives in keepalive.Keeper.
type keeper struct {
	*keepalive.Keeper
	h *Handler
	d *httpDispatcher
	// mu guards overrides and strategies only — the core has its own lock.
	mu         sync.Mutex
	overrides  map[string]sessionOverride
	strategies []tenant.Strategy
}

func newKeeper(h *Handler) *keeper { return newKeeperWith(h, keepalive.Options{}) }

// newKeeperWith lets tests inject the clock and an inline runner. Dispatcher, Prices and Spare
// are always the proxy's own.
func newKeeperWith(h *Handler, o keepalive.Options) *keeper {
	k := &keeper{h: h, overrides: map[string]sessionOverride{}}
	var client *http.Client
	if h != nil {
		client = h.client
	}
	k.d = newHTTPDispatcher(client)
	o.Dispatcher = k.d
	if h != nil && h.opts.Prices != nil {
		o.Prices = modelPricer{h.opts.Prices}
	}
	if h != nil && h.limiter != nil {
		o.Spare = h.limiter.AcquireSpare
	}
	o.OnTick = func(now time.Time) {
		k.mu.Lock()
		k.expireOverridesLocked(now)
		k.mu.Unlock()
	}
	k.Keeper = keepalive.New(o)
	k.loadStrategies()
	return k
}

// modelPricer adapts modelinfo.Pricer, which prices by model alone.
type modelPricer struct{ p modelinfo.Pricer }

func (m modelPricer) Price(ctx context.Context, _, model string, _ int64) (keepalive.Price, bool) {
	return m.p.Price(ctx, model)
}

func (k *keeper) now() time.Time { return k.Keeper.Now() }

func (k *keeper) start() {
	if k != nil {
		k.Keeper.Start()
	}
}

// Stop ends the sweep, releases everything held, and drops the overrides with it: they are
// authorizations to spend, and the process they authorized is ending.
func (k *keeper) Stop() {
	if k == nil {
		return
	}
	k.Keeper.Shutdown()
	k.mu.Lock()
	k.overrides = map[string]sessionOverride{}
	k.mu.Unlock()
}

func (k *keeper) arrive(tenant, session, threadID string) (int, int64, string) {
	if k == nil {
		return 0, 0, ""
	}
	return k.Keeper.Arrive(keepalive.Key{Tenant: tenant, Session: session, Thread: threadID})
}

func (k *keeper) retireSession(tenant, session string) { k.Keeper.RetireSession(tenant, session) }

func (k *keeper) forget(tenant string) {
	if k == nil {
		return
	}
	k.Keeper.Forget(tenant)
	k.forgetOverrides(tenant)
}

func (k *keeper) pendingPings() int {
	if k == nil {
		return 0
	}
	return k.Keeper.PendingPings()
}
```

and `record`, which carries the deleted policy block from the old `record` (`:713-759`) word for word, then hands off:

```go
func (k *keeper) record(tn *Tenancy, session, threadID string, startedAt time.Time, body []byte,
	up upstream, r *http.Request, provider bschemas.ModelProvider, route string, status int,
	u Usage, usageOK bool) {
	if k == nil || tn == nil {
		return
	}
	// [the old comment "NO AUDIT SINK, NO RETENTION …"] — a zero policy makes the core retire the
	// key after its own status and kill-switch checks, which is exactly the old order's effect.
	pol := tn.Cache
	applied := ""
	if k.h == nil || k.h.rec == nil {
		pol = keepalive.Policy{}
	} else {
		// [old :729-759, unchanged: applyStrategy, overrideFor, the OpenAI cadence block]
	}
	key := keepalive.Key{Tenant: tn.ID, Session: session, Thread: threadID}
	k.Keeper.Record(keepalive.Record{Key: key, StartedAt: startedAt, Body: body,
		Provider: provider, Endpoint: up.base, Route: route, Status: status,
		Usage: keepalive.Usage{FreshInput: u.FreshInput, CacheRead: u.CacheRead,
			CacheWrite: u.CacheWrite, Output: u.Output, CacheWrite1h: u.CacheWrite1h,
			StopReason: u.StopReason},
		UsageOK: usageOK, Policy: pol, Tag: applied, Agent: r.UserAgent(), Preset: tn.Preset,
		Header: r.Header, Store: tn.Store, Emitter: tn.Pipe.Emitter(),
		Hold: func() { k.d.hold(key, up, r) }})
}
```

Note: the old code read `model` from the body *before* the OpenAI block (`:752`). Keep `model := gjson.GetBytes(body, "model").String()` in the wrapper only if the moved OpenAI block needs it; the core reads the model itself.

`PendingPings` on `Handler` stays as is. `proxy.go:2309` (`h.keeper.Stats()`) now gets `keepalive.Stats`; add `type KeepAliveStats = keepalive.Stats` so JSON and callers are unchanged.

In `keepaliveoverride.go` and `keepalivestrategy.go`, `k.mu` already means the wrapper's lock (it guards only `overrides`/`strategies` there) and `k.now()` resolves to the wrapper method above — no edits beyond `pol.on()` → `pol.On()` and `CachePolicy` staying as the alias. `clockNow` (`keepalivestrategy.go:64`) returns `k.Keeper.Now()`.

- [ ] **Step 4: Fix the tests that stay in `proxy`**

They build the keeper through a `Handler` or `newKeeper`. Apply:

| Old | New |
|---|---|
| `k.now = clock.now` after construction | build with `newKeeperWith(h, keepalive.Options{Now: clock.now, Go: func(f func()) { f() }})` |
| `k.dispatch = k.fire` | the `Go` option above |
| `k.send = …` | wire tests keep real HTTP (that is their point); others swap `k.d` for an `httptest.Server` upstream |
| `k.mu.Lock(); e := k.live[kaKey(t, s)]; k.mu.Unlock()` | `e, ok := k.Entry(keepalive.Key{Tenant: t, Session: s})` and read `e.Pings`, `e.Stopped`, `e.MaskedBody` |
| `k.live[...].auth[0].val` | `k.d.mu.Lock(); h := k.d.held[keepalive.Key{…}]; k.d.mu.Unlock()` then `h.auth[0].Val` |
| `k.bytes != 0` | `k.Stats().Live == 0` plus `len(e.MaskedBody)` |
| `&kaEntry{turn: 1, …}.pingable()` checks in `keepaliveoverride_test.go:471-529` | move those six cases to `keepalive/keeper_test.go` (they test the core's `pingable`) |
| `f.h.keeper.mu.Lock()` in `campaign_test.go:566` | unchanged: it guards `strategies`, which stayed on the wrapper |

- [ ] **Step 5: Run everything**

Run:
```bash
go vet ./... 
go test -count=1 ./credmask/... ./keepalive/... 
CGO_ENABLED=1 go test -race -count=1 ./keepalive/... 
CGO_ENABLED=1 go test -race -count=1 ./proxy/...      # ~8 minutes
go test -count=1 ./...                                  # everything else
gofmt -l .
```
Expected: all PASS, `gofmt -l .` prints nothing new (compare with `git stash`-free check: `git diff --name-only main | xargs gofmt -l`).

- [ ] **Step 6: Commit Tasks 4 and 5 together**

```bash
git add -A keepalive proxy
git commit -s -m "refactor(keepalive): export the keeper and make the proxy its first host" \
  -m "The keep-alive state machine moves verbatim from proxy/keepalive.go to package keepalive. The proxy keeps policy resolution, the audit-sink rule and credential custody, now behind keepalive.Dispatcher. No behavior change: the production-snapshot replay reports the same net." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 6: `Shutdown` releases every held entry

The spec's §6.3 fix. Today `Stop` already zeroizes entries; what is new is that the *host's* credential is released too, which only matters now that the host holds it.

**Files:**
- Modify: `keepalive/keeper.go` (`Shutdown`)
- Test: `keepalive/shutdown_test.go`

**Interfaces:**
- Consumes: `fakeDispatcher` (Task 4), `Keeper.Record`, `Keeper.Shutdown`.

- [ ] **Step 1: Write the failing test**

```go
// keepalive/shutdown_test.go
package keepalive

import (
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func trackedRecord(fd *fakeDispatcher, key Key, at time.Time) Record {
	return Record{Key: key, StartedAt: at, Provider: bschemas.Anthropic, Route: "/v1/messages",
		Status: 200, UsageOK: true, Usage: Usage{CacheRead: 50_000},
		Body:   []byte(`{"model":"claude-sonnet-5","max_tokens":4096,"messages":[]}`),
		Policy: Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 1},
		Hold:   fd.hold(key)}
}

func TestShutdownReleasesEveryHeldEntryOnce(t *testing.T) {
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, 200, nil })
	now := time.Now()
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }})
	keys := []Key{{Tenant: "t", Session: "a"}, {Tenant: "t", Session: "b"}, {Tenant: "t", Session: "b", Thread: "a:x"}}
	for _, key := range keys {
		k.Record(trackedRecord(fd, key, now)) // turn 0: counted, not tracked
		k.Record(trackedRecord(fd, key, now)) // turn 1: tracked, Held
	}
	for _, key := range keys {
		if fd.held[key] != 1 {
			t.Fatalf("%v held %d times before Shutdown, want 1", key, fd.held[key])
		}
	}
	k.Shutdown()
	k.Shutdown() // idempotent
	for _, key := range keys {
		if fd.held[key] != 0 {
			t.Errorf("%v: Hold-Release balance %d after Shutdown, want 0", key, fd.held[key])
		}
	}
	if len(fd.released) != len(keys) {
		t.Errorf("Release called %d times, want %d", len(fd.released), len(keys))
	}
}

func TestReRecordReleasesTheOldCredentialFirst(t *testing.T) {
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, 200, nil })
	now := time.Now()
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, now))
	k.Record(trackedRecord(fd, key, now)) // tracked
	k.Record(trackedRecord(fd, key, now)) // replaces: Release old, Hold new
	if fd.held[key] != 1 {
		t.Fatalf("balance after replacement = %d, want 1", fd.held[key])
	}
	if len(fd.released) != 1 {
		t.Fatalf("Release calls = %d, want 1", len(fd.released))
	}
}

func TestKillSwitchHoldsNothing(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, 200, nil })
	now := time.Now()
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, now))
	k.Record(trackedRecord(fd, key, now))
	if fd.held[key] != 0 {
		t.Fatalf("kill switch on, but Hold was called %d times", fd.held[key])
	}
}
```

- [ ] **Step 2: Run to verify the first test fails**

Run: `go test ./keepalive/ -run 'Shutdown|ReRecord|KillSwitch' -v`
Expected: `TestShutdownReleasesEveryHeldEntryOnce` FAILS (balance 1 after Shutdown) if Task 4 moved `Stop` without the release loop; the other two PASS (Task 4 already pairs them).

- [ ] **Step 3: Implement**

In `Shutdown`, inside the existing loop over `k.live`, call `k.releaseLocked(e)` before `e.clear()`.

- [ ] **Step 4: Run**

Run: `CGO_ENABLED=1 go test -race -count=1 ./keepalive/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add keepalive
git commit -s -m "fix(keepalive): release every held credential on Shutdown" \
  -m "A config reload in a host such as Cortex shuts the old plugin instance down while it may still hold a session's credential. Shutdown now tells the Dispatcher to release each one before it returns, instead of relying on the hard deadline." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 7: In-flight release and a finished Phase 1

**Files:**
- Test: `keepalive/inflight_test.go`
- Modify: `docs/design.md` (one paragraph: the `keepalive` and `credmask` packages and who hosts them)

- [ ] **Step 1: Write the test for Review Focus item 1**

```go
// keepalive/inflight_test.go
package keepalive

import (
	"sync"
	"testing"
	"time"
)

// A real request arriving while that thread's ping is in flight releases the held credential;
// the ping must finish on its own copy, and the entry must not be pinged again.
func TestArriveDuringAnInFlightPing(t *testing.T) {
	start := time.Now()
	now := start
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	inFlight, finish := make(chan struct{}), make(chan struct{})
	fd := newFake(func(DispatchRequest) (Usage, int, error) {
		close(inFlight)
		<-finish
		return Usage{CacheRead: 50_000}, 200, nil
	})
	k := New(Options{Dispatcher: fd, Now: clock}) // default Go: a real goroutine
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
	mu.Lock()
	now = start.Add(281 * time.Second)
	mu.Unlock()
	if n := k.Sweep(clock()); n != 1 {
		t.Fatalf("Sweep started %d pings, want 1", n)
	}
	<-inFlight
	k.Arrive(key) // the real request: releases while the ping is still on the wire
	if fd.held[key] != 0 {
		t.Fatalf("balance %d after Arrive, want 0", fd.held[key])
	}
	close(finish)
	deadline := time.Now().Add(5 * time.Second)
	for k.Stats().Pings == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, ok := k.Entry(key); ok {
		t.Fatal("the entry survived Arrive")
	}
	if len(fd.released) != 1 {
		t.Fatalf("Release calls = %d, want exactly 1", len(fd.released))
	}
}
```

- [ ] **Step 2: Run it**

Run: `CGO_ENABLED=1 go test -race -count=3 ./keepalive/ -run InFlight -v`
Expected: PASS three times, no race report. If it fails, the bug is in Task 4's `record1`/`markStopped` touching a released entry — fix there, not in the test.

- [ ] **Step 3: Document**

Add to `docs/design.md`, in the package layout section: `keepalive` — the idle keep-alive, host-agnostic; hosts implement `keepalive.Dispatcher` (cmd/context-guru-proxy's `httpDispatcher`; the Cortex plugin's `cortexDispatcher`) and must pass `keepalivetest.RunDispatcherContract`. `credmask` — the shared at-rest mask for held credentials and transcripts.

- [ ] **Step 4: Full verification**

Run the whole of Task 5 Step 5 again, plus `make build`.
Expected: all PASS; `make build` produces the proxy binary.

- [ ] **Step 5: Commit**

```bash
git add keepalive docs/design.md
git commit -s -m "test(keepalive): pin release during an in-flight ping; document the package" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 8: `Config.CapMarkerMode` — let a host refuse reversible markers

Spec §4.1: until Cortex can replace streamed frames, the plugin must force `marker_mode` away from `full` *whatever the preset says*. Every offloader defaults to `full` (`config/config.go:449`), and a preset never shows its component blocks to the host, so only the engine can do this reliably. `marker_mode` is a field on 16 components, and `components.Decode` uses `KnownFields(true)` (`components/registry.go:185`), so adding the key to a component without it would fail `Build`.

**Files:**
- Create: `config/markercap.go`, `config/markercap_test.go`

**Interfaces:**
- Produces: `func (c *Config) CapMarkerMode(max string) error` — `max` is `"summary"` or `"off"`. For each name in `c.Pipeline` that supports `marker_mode`, sets it to `max` when it is absent or `"full"`; leaves `"summary"`/`"off"` as configured, except that `max == "off"` also lowers `"summary"`. Returns an error for any other `max`.

- [ ] **Step 1: Write the failing tests**

```go
// config/markercap_test.go
package config

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/offload"
	_ "github.com/rossoctl/context-guru/components/reformat"
)

// The list must match the components that actually accept the key: a component added later
// with a marker_mode field and not listed here would keep defaulting to full in Cortex.
func TestMarkerModeComponentsMatchTheRegistry(t *testing.T) {
	for _, name := range components.Names() {
		_, err := components.New(name, []byte("marker_mode: summary\n"))
		accepts := err == nil
		if accepts != markerModeComponents[name] {
			t.Errorf("%s: accepts marker_mode = %v, listed = %v (err: %v)",
				name, accepts, markerModeComponents[name], err)
		}
	}
}

func TestCapMarkerModeOverridesThePresetDefault(t *testing.T) {
	c, err := LoadBytes([]byte("preset: general\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CapMarkerMode("summary"); err != nil {
		t.Fatal(err)
	}
	for _, name := range c.Pipeline {
		if !markerModeComponents[name] {
			continue
		}
		var m map[string]any
		node := c.Components[name]
		if err := node.Decode(&m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m["marker_mode"] != "summary" {
			t.Errorf("%s: marker_mode = %v, want summary", name, m["marker_mode"])
		}
	}
	if _, err := c.Build(components.NopEmitter{}); err != nil {
		t.Fatalf("Build after CapMarkerMode: %v", err)
	}
}

func TestCapMarkerModeKeepsAStricterChoice(t *testing.T) {
	c, err := LoadBytes([]byte("pipeline: [collapse]\ncomponents:\n  collapse: {marker_mode: \"off\", max_tokens: 300}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CapMarkerMode("summary"); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	n := c.Components["collapse"]
	_ = n.Decode(&m)
	if m["marker_mode"] != "off" || m["max_tokens"] != 300 {
		t.Fatalf("collapse = %v, want marker_mode off and max_tokens kept", m)
	}
	if err := c.CapMarkerMode("full"); err == nil {
		t.Fatal(`CapMarkerMode("full") accepted`)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./config/ -run 'MarkerMode'`
Expected: FAIL — `undefined: markerModeComponents`.

- [ ] **Step 3: Implement**

```go
// config/markercap.go
package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// markerModeComponents are the components with a `marker_mode` field. Held to the registry by
// TestMarkerModeComponentsMatchTheRegistry, so a new offloader cannot be missed silently.
var markerModeComponents = map[string]bool{
	"agentdiet": true, "cache_aware_summarizer": true, "cmdfilter": true, "collapse": true,
	"coref": true, "dedup": true, "extract": true, "extract_llm": true,
	"extract_llm_sweep": true, "failed_run": true, "linecap": true, "mask": true,
	"readlifecycle": true, "skeleton": true, "smartcrush": true, "summarize": true,
}

var markerRank = map[string]int{"off": 0, "summary": 1, "full": 2, "": 2}

// CapMarkerMode lowers every pipeline component's marker_mode to at most max. A host that cannot
// run the expand loop (it cannot replace a streamed response) calls this after LoadBytes, so no
// preset can make content "reversible" through a tool call nothing will ever answer.
func (c *Config) CapMarkerMode(max string) error {
	if max != "summary" && max != "off" {
		return fmt.Errorf("config: CapMarkerMode(%q): want summary or off", max)
	}
	if c.Components == nil {
		c.Components = map[string]yaml.Node{}
	}
	for _, name := range c.Pipeline {
		if !markerModeComponents[name] {
			continue
		}
		m := map[string]any{}
		if n, ok := c.Components[name]; ok && n.Kind != 0 {
			if err := n.Decode(&m); err != nil {
				return fmt.Errorf("config: %s: %w", name, err)
			}
		}
		cur, _ := m["marker_mode"].(string)
		if markerRank[cur] > markerRank[max] {
			m["marker_mode"] = max
		}
		var n yaml.Node
		if err := n.Encode(m); err != nil {
			return fmt.Errorf("config: %s: %w", name, err)
		}
		c.Components[name] = n
	}
	return nil
}
```

- [ ] **Step 4: Run**

Run: `go vet ./config/ && go test -count=1 ./config/`
Expected: PASS. If `TestMarkerModeComponentsMatchTheRegistry` fails for a component that needs other required keys to construct, change that probe to decode into the component's config struct via `components.Decode` instead — do not drop the component from the check.

- [ ] **Step 5: Commit**

```bash
gofmt -l config
git add config
git commit -s -m "feat(config): add CapMarkerMode for hosts without an expand loop" \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 6: Stop and report.** Do not push or open a PR without the user's go-ahead. Report: the commit list, the snapshot-replay figure before and after, and the race-test result. Opening the PR is an outward-facing action.

---

## What Plan 2 needs from this plan (and nothing else)

- `keepalive.New`, `Options`, `Record`, `Key`, `Usage`, `Price`, `Pricer`, `Policy`, `DispatchRequest`, `Dispatcher`, `PingHeaders`, `Keeper.{Start,Shutdown,Arrive,Record,Stats}`
- `credmask.{Mask,Zero,Header}`
- `keepalivetest.RunDispatcherContract`, `Harness`, `NewHarness`
- `config.(*Config).CapMarkerMode`
- A context-guru commit (later a tag) that contains all of the above.
