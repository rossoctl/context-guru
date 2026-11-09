# Port main's keep-alive work into the exported keeper — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Merge `fix/tool-output-system-reminder` (which carries `main` at `67c5f758`) into `feat/export-keepalive-keeper`, and port every keep-alive behaviour `main` added to the old in-proxy keeper into the exported `keepalive` package, so that both the proxy and Cortex get it.

**Architecture:** First add each behaviour to `keepalive/` as an additive API, with its own package tests, on a clean branch. Then do the merge, keeping this branch's structure for the three conflicted files, and rewire `main`'s new proxy files (`clientsignal.go`, `predictive.go`, `campaign*.go`) to the package API. Last, rebuild Cortex against the result and run its plugin tests.

**Tech Stack:** Go 1.26, `github.com/rossoctl/context-guru` (`keepalive`, `dispatch`, `proxy`, `predictive`), Cortex `core/plugins/contextguru`.

**Spec:** This document, section "Design". There is no separate spec. The behaviour is already defined by `main`'s commits (listed per task), and this plan only decides where each behaviour lives.

## Global Constraints

- Work in a NEW worktree `context-guru/.worktrees/keeper-port`, on branch `port/keepalive-main`, from `feat/export-keepalive-keeper` (`742ea693`). Do NOT edit `.worktrees/keeper-export` until the last task: Cortex's `go.mod` `replace` points at it.
- The `keepalive` package API used by Cortex must not break: `Keeper`, `New`, `Options{Dispatcher}`, `Policy`, `Record`, `Key`, `Usage`, `Keeper.{Arrive, Record, Start, Shutdown}`. Every new field and method is ADDITIVE, and a zero value means today's behaviour.
- `keepalive` must not import `proxy`, `dash`, `predictive` or `config` (it is the host-free core).
- Commits: `git commit -s`, conventional-commit title, `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.
- Run tests with `GOWORK=off go test ./keepalive/ ./proxy/ ./dash/ ./cmd/...` locally. `dash`'s `TestCalculatorPricesFromTheRowsModelNotABlend` fails on `main` too: ignore only that test.
- Run `gofmt -l keepalive proxy` and `go vet ./keepalive/ ./proxy/` before each commit.

## Design

Where each of `main`'s behaviours goes:

| Id | Behaviour (main commits) | Lives in | New package API |
|---|---|---|---|
| B4 | Ping outcome: 429/5xx is transient; other 4xx and an all-zero 2xx are failures; first `CacheRead==0` is a "miss" that keeps pinging; second miss in a row or `CacheWrite>CacheRead` stops (105f78e5 78534e5b 62954fed ba46c777 f2ba1aba) | `keepalive.fire` | `Stats.Missed` |
| B3 | Route breaker: 3 failures in a row on (tenant, endpoint, model) trip the route for 1h (105f78e5 2032d5fc 62954fed) | `keepalive` | `Stats.RouteTrips` |
| B6 | Credential dead: 401 kills the credential on all models, 403 on that model, for 1h; server-held key falls to the breaker (468a6ebb ac45348b aa79460c) | `keepalive` (marks) + host (computes the id) | `Record.Cred string` |
| B2 | No ping while the same thread has a real request open (105f78e5 43d0fe9e 693192b4) | `keepalive` | `Keeper.Depart(Key)`; `Arrive` counts, `Record` departs |
| B5 | A relay (Anthropic provider, host not `anthropic.com`) is held only for a Claude model name or a `CONTEXT_GURU_KEEPALIVE_MODELS` word (105f78e5 78534e5b 62954fed ba46c777) | `keepalive.Record` gate | none |
| B7 | Plans for the status line; `refreshedAt` moves only on a real start or a ping/substitute that READ cache (9e79dfe5 ddd44ffa 30b99939 5429fe0f) | `keepalive` + proxy adapter to `dash.KeepAlivePlan` | `Keeper.Plans(...) map[string]Plan` |
| B1 | Client lifecycle signals: `SessionEnd` retires and marks ended; `compact` retires the main thread's entries that started >2s before (ae84df7c d221800f 26b21e34 4578f01c ba8cd685) | `keepalive` (retire, ended marks) + proxy (`/signal` route, `clientsignal.go`) | `Record.Client`, `Keeper.RetireSessionStartedBefore`, `Keeper.MarkEnded`, `Keeper.EndedAfter` |
| B8 | Predictive campaigns pick the policy (47ea6476 d737bc41) | proxy only (policy is resolved host-side on this branch) | none (`Record.Policy`, `Record.Tag = "campaign:<id>"`) |

Why this split: everything driven by a ping's outcome or by the keeper's own map lives in the package, so Cortex gets it too. Everything that needs `dash`, `predictive`, tenant config or HTTP routes stays in `proxy`.

**One contract `main` did not need:** an in-flight count that never goes back to zero stops that thread's pings until the entry's hard deadline. On `main`, the proxy records every request it arrives. Cortex calls `Arrive` on every request but `Record` only on some (it skips subagents). So the package adds `Depart(Key)` for a request that ends without a `Record`, and Task 8 makes Cortex call it. No age bound is added: `Record` departs one open arrival, so a stale one is used up by the next request and the leak only moves. An entry's hard deadline ((K+1) x Idle, 14 minutes at the defaults) also comes before any useful bound. Calling `Depart` is the fix.

## Review Focus

1. **A request that calls `Arrive` but never `Record`** (an error, a subagent dropped by Cortex, a client hang-up) must not pause its thread's pings. Tests: `TestAnOpenRequestOnTheSameThreadPausesItsPing` (Task 3, `Depart` releases it) and `TestASkippedSubagentRequestDoesNotPauseTheMainThread` (Task 8, Cortex departs).
2. **Two tenants with the same session id**: a breaker trip, a dead credential or an ended mark in tenant A must not affect tenant B. Tests: `TestARouteTripIsPerTenant` (Task 2), `TestADeadCredentialIsPerTenant` (Task 2), `TestEndedMarksArePerTenant` (Task 5).
3. **A ping that fails with a transport error** (no status) must not trip the breaker and must not stop the entry, as on `main`. Test: `TestATransportErrorIsNotAFailure` (Task 1).
4. **Cortex's server-side records with `Tenant: ""`, `Cred: ""`**: every new gate must work with the empty tenant and fall back to the route breaker for the credential. Test: `TestEmptyTenantAndCredUseTheRouteBreaker` (Task 2).
5. **`Forget(tenant)`** must clear that tenant's breaker, dead credentials, ended marks and in-flight counts, and nothing of another tenant. Test: `TestForgetClearsEveryNewMapForThatTenantOnly` (Task 5).

---

### Task 0: Worktree and baseline

**Files:** none changed.

- [ ] **Step 1: Create the worktree**

```sh
CGM=/Users/davidboaz/Documents/GitHub/context-guru
git -C $CGM worktree add .worktrees/keeper-port -b port/keepalive-main feat/export-keepalive-keeper
cd $CGM/.worktrees/keeper-port && git log --oneline -1   # expect 742ea693
```

- [ ] **Step 2: Move this plan into the port branch**

```sh
P=docs/superpowers/plans/2026-10-15-port-main-keepalive-into-exported-keeper.md
mv $CGM/.worktrees/keeper-export/$P $P
git add $P && git commit -s -m "docs: Plan the port of main's keep-alive work into the exported keeper" -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 3: Baseline tests**

Run: `GOWORK=off go test ./keepalive/ ./proxy/`
Expected: `ok` for both. Record the run time of `./proxy/` for later comparison.

---

### Task 1: B4 — classify each ping's outcome as `main` does

**Files:**
- Modify: `keepalive/keeper.go` (`kaEntry`: add `missed int`; `Keeper`: add `missed atomic.Int64`; `Stats`: add `Missed int64 \`json:"missed"\``; rewrite the outcome part of `fire`, currently `keeper.go:941-979`)
- Test: `keepalive/outcome_test.go` (new)

**Interfaces:**
- Produces: `func (k *Keeper) noteOutcome(j pingJob, status int, u Usage, err error) outcome` where `type outcome int` with `outcomeTransient`, `outcomeFailed`, `outcomeMissed`, `outcomeRead`, `outcomeStopped`. Tasks 2 and 6 call `noteOutcome`'s result.
- Produces: `Stats.Missed`.

- [ ] **Step 1: Write the failing tests**

```go
package keepalive

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// pingOnce records a tracked entry and sweeps once at Idle+1s, with resp answering the ping.
func pingOnce(t *testing.T, resp func(DispatchRequest) (Usage, int, error)) (*Keeper, *fakeDispatcher, Key, time.Time) {
	t.Helper()
	start := time.Now()
	now := start
	fd := newFake(resp)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	return k, fd, key, now
}

func TestARealMissIsNotAFailedPing(t *testing.T) {
	k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) {
		return Usage{FreshInput: 40_000, Output: 1}, http.StatusOK, nil
	})
	if s := k.Stats(); s.Missed != 1 || s.Failed != 0 || s.Wrote != 0 {
		t.Fatalf("stats = %+v, want Missed=1 Failed=0 Wrote=0", s)
	}
	if e, ok := k.Entry(key); !ok || e.Stopped {
		t.Fatalf("a first miss must keep pinging: entry=%+v ok=%v", e, ok)
	}
}

func TestASecondMissInARowStops(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{FreshInput: 40_000, Output: 1}, 200, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	r := trackedRecord(fd, key, start)
	r.Policy.MaxPings = 3
	k.Record(r)
	k.Record(r)
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	if e, ok := k.Entry(key); ok && !e.Stopped {
		t.Fatal("a second miss in a row must stop the entry")
	}
	if s := k.Stats(); s.Missed != 1 || s.Wrote != 1 {
		t.Fatalf("stats = %+v, want Missed=1 Wrote=1", s)
	}
}

func TestPingFailureNeedsNoUsageOrAnError(t *testing.T) {
	k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil })
	if s := k.Stats(); s.Failed != 1 {
		t.Fatalf("an all-zero 2xx is a failure: stats = %+v", s)
	}
	if e, ok := k.Entry(key); ok && !e.Stopped {
		t.Fatal("an all-zero 2xx must stop the entry")
	}
}

func TestOverloadedPingsAreTransient(t *testing.T) {
	for _, st := range []int{429, 500, 529} {
		k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) { return Usage{}, st, nil })
		if s := k.Stats(); s.Skipped != 1 || s.Failed != 0 {
			t.Fatalf("status %d: stats = %+v, want Skipped=1 Failed=0", st, s)
		}
		if e, ok := k.Entry(key); !ok || e.Stopped {
			t.Fatalf("status %d must not stop the entry", st)
		}
	}
}

func TestAnyOther4xxStops(t *testing.T) {
	k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusBadRequest, nil })
	if s := k.Stats(); s.Failed != 1 {
		t.Fatalf("stats = %+v, want Failed=1", s)
	}
	if e, ok := k.Entry(key); ok && !e.Stopped {
		t.Fatal("a 400 must stop the entry")
	}
}

func TestATransportErrorIsNotAFailure(t *testing.T) {
	k, _, key, _ := pingOnce(t, func(DispatchRequest) (Usage, int, error) { return Usage{}, 0, errors.New("dial tcp: refused") })
	if e, ok := k.Entry(key); !ok || e.Stopped {
		t.Fatal("a transport error must not stop the entry")
	}
	if s := k.Stats(); s.RouteTrips != 0 {
		t.Fatalf("a transport error must not count toward the breaker: %+v", s)
	}
}
```

`TestATransportErrorIsNotAFailure` uses `Stats.RouteTrips`, added in Task 2. Until then, comment out its last check, and restore it in Task 2 Step 1.

Also change `TestPingThatWritesInsteadOfReadingStopsTheSession` in `keepalive/keeper_test.go` (`main`'s 78534e5b change): give its ping response `CacheRead: 1024` instead of `0`, so it tests the write guard and not a first miss. Add the comment `// A zero read is a first miss now; see TestARealMissIsNotAFailedPing.`

- [ ] **Step 2: Run the tests to see them fail**

Run: `GOWORK=off go test ./keepalive/ -run 'Miss|Failure|Overloaded|4xx|Transport'`
Expected: FAIL. `s.Missed undefined` (compile error) first.

- [ ] **Step 3: Implement**

In `fire`, replace the block from `if err != nil {` to the `cost := k.record1(...)` line with:

```go
	if err != nil {
		k.failed.Add(1) // a transport error: counted, not stopped, not a breaker failure
		slog.Debug("context-guru: cache keep-alive ping failed",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "err", err)
		return
	}
	switch k.noteOutcome(j, status, u) {
	case outcomeTransient:
		k.record1(j, u, status, ms, start) // the audit row; the next ping may work
		return
	case outcomeFailed:
		return
	case outcomeFailedRecorded:
		k.record1(j, u, status, ms, start)
		return
	}
	cost := k.record1(j, u, status, ms, start)
```

and add, after `markStopped`:

```go
// outcome is what one ping's answer means for its entry. The order of checks is main's (#527).
type outcome int

const (
	outcomeRead outcome = iota
	outcomeMissed
	outcomeTransient
	outcomeFailed         // stopped, no audit row
	outcomeFailedRecorded // stopped, with an audit row
	outcomeStopped        // the guard: wrote more than it read, or missed twice running
)

// noteOutcome classifies a ping's answer and applies it to the entry and the counters.
func (k *Keeper) noteOutcome(j pingJob, status int, u Usage) outcome {
	switch {
	case status == http.StatusTooManyRequests || status >= 500:
		k.skipped.Add(1)
		return outcomeTransient
	case status >= 400:
		k.failed.Add(1)
		k.markStopped(j.e)
		return outcomeFailed
	case u.FreshInput == 0 && u.CacheRead == 0 && u.CacheWrite == 0:
		k.failed.Add(1)
		k.markStopped(j.e)
		return outcomeFailedRecorded
	}
	if u.CacheRead == 0 && k.notePingMiss(j.e) {
		k.missed.Add(1)
		return outcomeMissed
	}
	if u.CacheWrite > u.CacheRead || u.CacheRead == 0 {
		k.wrote.Add(1)
		k.markStopped(j.e)
		slog.Error("context-guru: cache keep-alive ping did not read the held prefix (it wrote more than it read, "+
			"or missed twice running); not pinging this session again",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread,
			"cache_write", u.CacheWrite, "cache_read", u.CacheRead)
		return outcomeStopped
	}
	k.notePingRead(j.e)
	return outcomeRead
}

// notePingMiss counts a miss on e and reports whether it is the FIRST in a row.
func (k *Keeper) notePingMiss(e *kaEntry) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.missed++
	return e.missed == 1
}

// notePingRead resets e's run of misses.
func (k *Keeper) notePingRead(e *kaEntry) {
	k.mu.Lock()
	e.missed = 0
	k.mu.Unlock()
}
```

Add `Missed: k.missed.Load()` where `Stats()` fills the struct, and add `"net/http"` to the imports if it is missing.

- [ ] **Step 4: Run the tests**

Run: `GOWORK=off go test ./keepalive/`
Expected: PASS (all old and new tests).

- [ ] **Step 5: Commit**

```sh
gofmt -l keepalive; go vet ./keepalive/
git add keepalive/
git commit -s -m "feat(keepalive): Classify each ping's outcome as the in-proxy keeper does" -m "Port of main 105f78e5 78534e5b 62954fed ba46c777 f2ba1aba into the exported keeper." -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: B3 + B6 — route breaker and dead credentials

**Files:**
- Modify: `keepalive/keeper.go` (`Keeper`: `routeFails map[string]int`, `routeTripped map[string]time.Time`, `routeTrips atomic.Int64`, `credDead map[string]time.Time`; `kaEntry`: `cred string`; `Record`: `Cred string`; `Stats`: `RouteTrips int64 \`json:"route_trips"\``; `Record()` gate; `Sweep` stop; `noteOutcome` hooks; `Forget` clears)
- Create: `keepalive/breaker.go` (the helpers below)
- Test: `keepalive/breaker_test.go` (new)

**Interfaces:**
- Consumes: `noteOutcome` from Task 1.
- Produces: `Record.Cred string` — an opaque, tenant-free credential id the host computes; `""` means a server-held key. The proxy fills it in Task 7 with `main`'s HMAC `credKey`.

- [ ] **Step 1: Write the failing tests**

```go
package keepalive

import (
	"net/http"
	"testing"
	"time"
)

// failing pings on one route, each on its own fresh session, so each failure is a separate entry.
func failRoute(t *testing.T, k *Keeper, fd *fakeDispatcher, tenant string, n int, now *time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := Key{Tenant: tenant, Session: "s" + string(rune('a'+i))}
		k.Record(trackedRecord(fd, key, *now))
		k.Record(trackedRecord(fd, key, *now))
		*now = now.Add(281 * time.Second)
		k.Sweep(*now)
	}
}

func TestPingsThatFailTripTheRoute(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil }) // all-zero: failure
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "t", 3, &now)
	if s := k.Stats(); s.RouteTrips != 1 {
		t.Fatalf("RouteTrips = %d after 3 failures, want 1", s.RouteTrips)
	}
	key := Key{Tenant: "t", Session: "new"}
	k.Record(trackedRecord(fd, key, now))
	k.Record(trackedRecord(fd, key, now))
	if _, ok := k.Entry(key); ok {
		t.Fatal("a tripped route must refuse a new hold")
	}
	now = now.Add(61 * time.Minute)
	k.Record(trackedRecord(fd, key, now)) // twice: a refused record may not count as a turn
	k.Record(trackedRecord(fd, key, now))
	if _, ok := k.Entry(key); !ok {
		t.Fatal("the trip must expire after an hour")
	}
}

func TestARouteTripIsPerTenant(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "a", 3, &now)
	key := Key{Tenant: "b", Session: "s"}
	k.Record(trackedRecord(fd, key, now))
	k.Record(trackedRecord(fd, key, now))
	if _, ok := k.Entry(key); !ok {
		t.Fatal("tenant a's trip must not refuse tenant b")
	}
}

func TestAGoodPingResetsTheCount(t *testing.T) {
	now := time.Now()
	status := http.StatusBadRequest
	fd := newFake(func(DispatchRequest) (Usage, int, error) {
		if status == http.StatusOK {
			return Usage{CacheRead: 50_000, Output: 1}, status, nil
		}
		return Usage{}, status, nil
	})
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "t", 2, &now)
	status = http.StatusOK
	failRoute(t, k, fd, "t", 1, &now)
	status = http.StatusBadRequest
	failRoute(t, k, fd, "t", 2, &now)
	if s := k.Stats(); s.RouteTrips != 0 {
		t.Fatalf("a good ping must reset the run: RouteTrips = %d", s.RouteTrips)
	}
}

func credRecord(fd *fakeDispatcher, key Key, at time.Time, cred, model string) Record {
	r := trackedRecord(fd, key, at)
	r.Cred = cred
	r.Body = []byte(`{"model":"` + model + `","max_tokens":4096,"messages":[]}`)
	return r
}

func TestA401StopsThatCredentialOnEveryModel(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusUnauthorized, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	a := Key{Tenant: "t", Session: "a"}
	k.Record(credRecord(fd, a, now, "c1", "claude-sonnet-5"))
	k.Record(credRecord(fd, a, now, "c1", "claude-sonnet-5"))
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	b := Key{Tenant: "t", Session: "b"}
	k.Record(credRecord(fd, b, now, "c1", "claude-haiku-4-5"))
	k.Record(credRecord(fd, b, now, "c1", "claude-haiku-4-5"))
	if _, ok := k.Entry(b); ok {
		t.Fatal("a 401 must refuse the same credential on another model")
	}
	c := Key{Tenant: "t", Session: "c"}
	k.Record(credRecord(fd, c, now, "c2", "claude-haiku-4-5"))
	k.Record(credRecord(fd, c, now, "c2", "claude-haiku-4-5"))
	if _, ok := k.Entry(c); !ok {
		t.Fatal("another credential must still be held")
	}
	if s := k.Stats(); s.RouteTrips != 0 {
		t.Fatal("an auth refusal with a credential id must not count toward the route breaker")
	}
}

func TestA403BlocksTheKeyOnThatModelOnly(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusForbidden, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	a := Key{Tenant: "t", Session: "a"}
	k.Record(credRecord(fd, a, now, "c1", "claude-opus-5"))
	k.Record(credRecord(fd, a, now, "c1", "claude-opus-5"))
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	same := Key{Tenant: "t", Session: "same"}
	k.Record(credRecord(fd, same, now, "c1", "claude-opus-5"))
	k.Record(credRecord(fd, same, now, "c1", "claude-opus-5"))
	if _, ok := k.Entry(same); ok {
		t.Fatal("a 403 must refuse the same credential on the same model")
	}
	other := Key{Tenant: "t", Session: "other"}
	k.Record(credRecord(fd, other, now, "c1", "claude-haiku-4-5"))
	k.Record(credRecord(fd, other, now, "c1", "claude-haiku-4-5"))
	if _, ok := k.Entry(other); !ok {
		t.Fatal("a 403 must not refuse the credential on another model")
	}
}

func TestADeadCredentialIsPerTenant(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusUnauthorized, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	a := Key{Tenant: "a", Session: "s"}
	k.Record(credRecord(fd, a, now, "c1", "claude-sonnet-5"))
	k.Record(credRecord(fd, a, now, "c1", "claude-sonnet-5"))
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	b := Key{Tenant: "b", Session: "s"}
	k.Record(credRecord(fd, b, now, "c1", "claude-sonnet-5"))
	k.Record(credRecord(fd, b, now, "c1", "claude-sonnet-5"))
	if _, ok := k.Entry(b); !ok {
		t.Fatal("tenant a's dead credential must not refuse tenant b")
	}
}

func TestEmptyTenantAndCredUseTheRouteBreaker(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusUnauthorized, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "", 3, &now) // Cortex's shape: no tenant, no credential id
	if s := k.Stats(); s.RouteTrips != 1 {
		t.Fatalf("a 401 with no credential id must count toward the breaker: RouteTrips = %d", s.RouteTrips)
	}
}
```

Restore the last check of `TestATransportErrorIsNotAFailure` (Task 1).

- [ ] **Step 2: Run the tests to see them fail**

Run: `GOWORK=off go test ./keepalive/ -run 'Route|Credential|401|403|Breaker|Transport'`
Expected: FAIL to compile on `r.Cred` and `s.RouteTrips`.

- [ ] **Step 3: Implement**

Create `keepalive/breaker.go`:

```go
package keepalive

import "time"

// The route breaker and the dead-credential marks (main #527). Both are keyed with the tenant
// first, so one tenant can never switch off another's pings.
const (
	routeFailLimit = 3         // failures in a row that trip a route
	routeTripFor   = time.Hour // how long a trip, and a dead credential, lasts
)

// routeKey names one (tenant, endpoint, model) route.
func routeKey(tenant, endpoint, model string) string {
	return tenant + "\x00" + endpoint + "\x00" + model
}

// credModelKey names one credential on one model, for a 403.
func credModelKey(tenant, cred, model string) string {
	return tenant + "\x00" + cred + "\x00" + model
}

// credAllKey names one credential on every model, for a 401.
func credAllKey(tenant, cred string) string { return tenant + "\x00" + cred }

// routeFailedLocked counts a failure on route and trips it at the limit. It also prunes trips
// that have expired, so the map cannot grow without bound. k.mu must be held.
func (k *Keeper) routeFailedLocked(route string, now time.Time) {
	for r, until := range k.routeTripped {
		if !now.Before(until) {
			delete(k.routeTripped, r)
		}
	}
	k.routeFails[route]++
	if k.routeFails[route] >= routeFailLimit {
		delete(k.routeFails, route)
		k.routeTripped[route] = now.Add(routeTripFor)
		k.routeTrips.Add(1)
	}
}

// routeOKLocked resets route's run of failures. k.mu must be held.
func (k *Keeper) routeOKLocked(route string) { delete(k.routeFails, route) }

// routeIsTrippedLocked reports whether route is tripped at now. k.mu must be held.
func (k *Keeper) routeIsTrippedLocked(route string, now time.Time) bool {
	until, ok := k.routeTripped[route]
	return ok && now.Before(until)
}

// markCredDeadLocked marks cred dead for an hour: on every model for a 401, on model only for a
// 403. It prunes expired marks first. k.mu must be held.
func (k *Keeper) markCredDeadLocked(tenant, cred, model string, status int, now time.Time) {
	for c, until := range k.credDead {
		if !now.Before(until) {
			delete(k.credDead, c)
		}
	}
	key := credModelKey(tenant, cred, model)
	if status == 401 {
		key = credAllKey(tenant, cred)
	}
	k.credDead[key] = now.Add(routeTripFor)
}

// credIsDeadLocked reports whether cred is dead on model. "" (a server-held key) never is.
// k.mu must be held.
func (k *Keeper) credIsDeadLocked(tenant, cred, model string, now time.Time) bool {
	if cred == "" {
		return false
	}
	for _, key := range []string{credAllKey(tenant, cred), credModelKey(tenant, cred, model)} {
		if until, ok := k.credDead[key]; ok && now.Before(until) {
			return true
		}
	}
	return false
}
```

Then wire it into `keeper.go`:

1. In `New`, initialise `routeFails`, `routeTripped` and `credDead` as empty maps.
2. In `Record`, after the model is read from the body and before the entry is created, under `k.mu`:

```go
	now := k.now()
	if k.routeIsTrippedLocked(routeKey(r.Key.Tenant, r.Endpoint, model), now) ||
		k.credIsDeadLocked(r.Key.Tenant, r.Cred, model, now) {
		k.retireLocked(id) // drops any older entry for this thread, as main's retire(key)
		k.skipped.Add(1)
		return
	}
```

   If `Record` has no `retireLocked`, extract one from `retire(id)` (keeper.go:683): `retire` takes the lock and calls `retireLocked`. Set `e.cred = r.Cred` where the entry is built.
3. In `Sweep`, in the loop over due entries, before a ping job is created:

```go
		if k.routeIsTrippedLocked(routeKey(e.tenant, e.endpoint, e.model), now) ||
			k.credIsDeadLocked(e.tenant, e.cred, e.model, now) {
			e.stopped = true
			continue
		}
```

4. In `noteOutcome`:
   - At the top, add: `if (status == 401 || status == 403) && j.e.cred != "" { k.failed.Add(1); k.markStopped(j.e); k.mu.Lock(); k.markCredDeadLocked(j.tenant, j.e.cred, j.e.model, status, k.now()); k.mu.Unlock(); return outcomeFailed }`.
   - In the `status >= 400` and all-zero branches, after `markStopped`: `k.mu.Lock(); k.routeFailedLocked(routeKey(j.tenant, j.e.endpoint, j.e.model), k.now()); k.mu.Unlock()`.
   - Before the `CacheRead == 0` miss check (main L1341): `k.mu.Lock(); k.routeOKLocked(routeKey(j.tenant, j.e.endpoint, j.e.model)); k.mu.Unlock()`.
5. In `Forget(tenant)`, delete every key of `routeFails`, `routeTripped` and `credDead` with the prefix `tenant + "\x00"`.
6. In `Stats()`, add `RouteTrips: k.routeTrips.Load()`.

If `kaEntry` has no `model` or `endpoint` field, read them from where `pingJob` already gets them (keeper.go:850). Do not add a second copy.

- [ ] **Step 4: Run the tests**

Run: `GOWORK=off go test ./keepalive/`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
gofmt -l keepalive; go vet ./keepalive/
git add keepalive/
git commit -s -m "feat(keepalive): Trip a failing route and stop a refused credential, per tenant" -m "Port of main 105f78e5 2032d5fc 62954fed 468a6ebb ac45348b aa79460c. The host passes an opaque Record.Cred; empty means a server-held key, which falls to the route breaker." -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: B2 — no ping while the same thread has a request open

**Files:**
- Modify: `keepalive/keeper.go` (`Keeper`: `inflight map[string]int`; `Arrive` increments; `Record` departs (deferred); new `Depart`; `Sweep` skip; `Forget` clears the tenant's ids)
- Test: `keepalive/inflight_test.go` (add tests)

**Interfaces:**
- Produces: `func (k *Keeper) Depart(key Key)`. It ends one open request on that thread, and does nothing when there is none.

- [ ] **Step 1: Write the failing tests**

Append to `keepalive/inflight_test.go`:

```go
func TestAnOpenRequestOnTheSameThreadPausesItsPing(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
	k.Arrive(key)                           // request A opens
	k.Arrive(key)                           // request B opens, in parallel on the same thread
	k.Record(trackedRecord(fd, key, start)) // A finishes: tracked, and departs one; B is still open
	now = start.Add(281 * time.Second)
	if n := k.Sweep(now); n != 0 {
		t.Fatalf("Sweep started %d pings while B is open, want 0", n)
	}
	k.Depart(key) // B ends without a Record (an error)
	if n := k.Sweep(now); n != 1 {
		t.Fatalf("Sweep started %d pings after B departed, want 1", n)
	}
}

func TestASlowSubagentRequestDoesNotDelayTheMainThreadsPing(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	main := Key{Tenant: "t", Session: "s"}
	sub := Key{Tenant: "t", Session: "s", Thread: "a:x"}
	k.Record(trackedRecord(fd, main, start))
	k.Record(trackedRecord(fd, main, start))
	k.Arrive(sub) // a subagent request stays open
	now = start.Add(281 * time.Second)
	if n := k.Sweep(now); n != 1 {
		t.Fatalf("Sweep started %d pings, want 1: another thread's request must not pause the main thread", n)
	}
}

func TestDepartWithNoArrivalIsANoOp(t *testing.T) {
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd})
	k.Depart(Key{Tenant: "t", Session: "s"}) // must not panic
	var nilK *Keeper
	nilK.Depart(Key{Session: "s"}) // nil-safe, like Arrive
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `GOWORK=off go test ./keepalive/ -run 'Open|Subagent|Depart'`
Expected: FAIL to compile: `k.Depart undefined`.

- [ ] **Step 3: Implement**

In `keeper.go`:

```go
// Depart ends one open request on key's thread. Record calls it for its own key, so a host calls
// it only for a request that ends without a Record. On a thread with nothing open it does nothing.
func (k *Keeper) Depart(key Key) {
	if k == nil || key.Session == "" {
		return
	}
	k.mu.Lock()
	k.departLocked(key.id())
	k.mu.Unlock()
}

func (k *Keeper) departLocked(id string) {
	if k.inflight[id] <= 1 {
		delete(k.inflight, id)
		return
	}
	k.inflight[id]--
}
```

- In `New`: `inflight: map[string]int{}`.
- In `Arrive`, after `id := key.id()` and the lock, before the `live` lookup: `k.inflight[id]++`.
- At the top of `Record`, after the nil and empty-session guard: `defer k.Depart(r.Key)`. It must run on every return path, as `main`'s deferred `departed`. Departing after the hold is stored is `main`'s 43d0fe9e rule, and the defer gives it.
- In `Sweep`'s loop: `if !e.due(now) || k.inflight[id] > 0 { continue }`.
- In `Forget(tenant)`: delete every `inflight` id with the prefix `tenant + "\x00"`.

- [ ] **Step 4: Run the tests**

Run: `GOWORK=off go test ./keepalive/`
Expected: PASS. `TestArriveDuringAnInFlightPing` must still pass, because its `Arrive` releases the entry as before.

- [ ] **Step 5: Commit**

```sh
gofmt -l keepalive; go vet ./keepalive/
git add keepalive/
git commit -s -m "feat(keepalive): Pause a thread's ping while it has a request open" -m "Port of main 105f78e5 43d0fe9e 693192b4. Adds Keeper.Depart for a host request that ends without a Record." -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: B5 + B7 — relay name check, and plans with `refreshedAt`

**Files:**
- Create: `keepalive/relay.go` (`relayServesClaude`, `claudeModel`, from `main`'s `proxy/keepalive.go` L1956-1990 at `67c5f758`, unchanged except for names)
- Modify: `keepalive/keeper.go` (`Record` gate; `kaEntry.refreshedAt`; `record1` moves it on `CacheRead > 0`; new `Plans`)
- Modify: `keepalive/substitute.go` (`recordSummarySubstitute` moves `refreshedAt` on `CacheRead > 0`)
- Test: `keepalive/relay_test.go`, `keepalive/plans_test.go` (new)

**Interfaces:**
- Produces:

```go
// Plan is one thread's keep-alive plan, for a status line. Times are absolute; the host converts.
type Plan struct {
	RefreshedAt time.Time     // the last moment the provider's entry is known refreshed
	NextPing    time.Time     // startedAt + Idle
	PingsLeft   int
	Idle        time.Duration
}

// Plans returns the plan of every thread still to be pinged, keyed by thread ("" = main).
// session == "" with all == true lists every session, keyed by Key.id().
func (k *Keeper) Plans(tenant string, all bool, session string) map[string]Plan
```

- [ ] **Step 1: Copy `main`'s code for B5**

```sh
git show 67c5f758:proxy/keepalive.go | sed -n '1950,1995p'
```

Copy `relayServesClaude(provider bschemas.ModelProvider, base, model string) bool` and `claudeModel(model string) bool` into `keepalive/relay.go`, with their comments and the `ponytail:` note. They use `net/url`, `os` and `strings`.

- [ ] **Step 2: Write the failing tests**

`keepalive/relay_test.go`:

```go
package keepalive

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func TestKeepAliveNameCheckIsForRelaysOnly(t *testing.T) {
	cases := []struct {
		base, model string
		want        bool
	}{
		{"https://api.anthropic.com", "anything", true},
		{"https://eu.anthropic.com", "anything", true},
		{"https://gw.example.com", "aws/claude-haiku-4-5", true},
		{"https://gw.example.com", "us.anthropic.claude-sonnet", true},
		{"https://gw.example.com", "gpt-5-mini", false},
		{"https://anthropic.com.evil.example", "gpt-5-mini", false},
	}
	for _, c := range cases {
		if got := relayServesClaude(bschemas.Anthropic, c.base, c.model); got != c.want {
			t.Errorf("relayServesClaude(%q, %q) = %v, want %v", c.base, c.model, got, c.want)
		}
	}
	if !relayServesClaude(bschemas.OpenAI, "https://gw.example.com", "gpt-5-mini") {
		t.Error("the check is for the Anthropic route only")
	}
}

func TestKeepAliveModelAliasesFromTheEnvironment(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE_MODELS", "house-model,other")
	if !relayServesClaude(bschemas.Anthropic, "https://gw.example.com", "team/house-model-v2") {
		t.Fatal("a word from CONTEXT_GURU_KEEPALIVE_MODELS must count as a Claude model")
	}
}
```

If `main`'s test of the same names (`git show 67c5f758:proxy/keepalive_test.go`, search `NameCheck`) has more cases, copy them in too.

`keepalive/plans_test.go`:

```go
package keepalive

import (
	"testing"
	"time"
)

func TestPlansReportOnlyThreadsStillToBePinged(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	main := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, main, start))
	k.Record(trackedRecord(fd, main, start))
	p := k.Plans("t", false, "s")
	pl, ok := p[""]
	if !ok || pl.PingsLeft != 2 || !pl.NextPing.Equal(start.Add(280*time.Second)) || !pl.RefreshedAt.Equal(start) {
		t.Fatalf("plan = %+v ok=%v", pl, ok)
	}
	if len(k.Plans("other", false, "s")) != 0 {
		t.Fatal("another tenant's plans must be empty")
	}
}

func TestAPingThatReadNothingIsNotARefresh(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{FreshInput: 40_000, Output: 1}, 200, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
	now = start.Add(281 * time.Second)
	k.Sweep(now) // a first miss: still pinging, but nothing was refreshed
	if pl := k.Plans("t", false, "s")[""]; !pl.RefreshedAt.Equal(start) {
		t.Fatalf("RefreshedAt = %v, want the real request's start %v", pl.RefreshedAt, start)
	}
}

func TestAPingThatReadMovesTheRefresh(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	if pl := k.Plans("t", false, "s")[""]; !pl.RefreshedAt.Equal(now) || pl.PingsLeft != 1 {
		t.Fatalf("plan = %+v, want RefreshedAt=%v PingsLeft=1", pl, now)
	}
}
```

Port `main`'s `TestASummarySubstituteThatReadNothingIsNotARefresh` (`git show 67c5f758:proxy/keepaliveplan_test.go`) into `plans_test.go`. Build the substitute candidate the way `keepalive/substitute_test.go` already does.

- [ ] **Step 3: Run the tests to see them fail**

Run: `GOWORK=off go test ./keepalive/ -run 'NameCheck|Aliases|Plans|Refresh'`
Expected: FAIL to compile: `k.Plans undefined`. Once `relay.go` exists, the relay tests compile.

- [ ] **Step 4: Implement**

- In `Record`, next to the breaker gate from Task 2: `|| !relayServesClaude(r.Provider, endpointBase(r.Endpoint), model)` leads to the same `retireLocked` + `skipped` path. `endpointBase` is the scheme and host of `r.Endpoint`. If `Record.Endpoint` already is the base, pass it directly. Check `keepalive/ping.go`'s `eligible` for how it reads it.
- In `kaEntry`, add `refreshedAt time.Time`, and set `e.refreshedAt = r.StartedAt` after the entry literal in `Record`.
- In `record1` and in `recordSummarySubstitute`: `if u.CacheRead > 0 { j.e.refreshedAt = start }`, under `k.mu`, in the same place as `refreshed`.
- `Plans`:

```go
func (k *Keeper) Plans(tenant string, all bool, session string) map[string]Plan {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]Plan{}
	for id, e := range k.live {
		if (!all && (e.tenant != tenant || e.session != session)) ||
			e.stopped || !e.pol.On() || e.pings >= e.pol.MaxPings || !e.pingable() {
			continue
		}
		key := e.thread
		if all {
			key = id
		}
		out[key] = Plan{RefreshedAt: e.refreshedAt, NextPing: e.startedAt.Add(e.pol.Idle),
			PingsLeft: e.pol.MaxPings - e.pings, Idle: e.pol.Idle}
	}
	return out
}
```

Check `main`'s `Handler.KeepAlivePlans` (`git show 67c5f758:proxy/keepalive.go | sed -n '2095,2140p'`) for the `all` keying. Match it exactly, because the proxy adapter in Task 7 keeps `main`'s signature.

- [ ] **Step 5: Run the tests**

Run: `GOWORK=off go test ./keepalive/`
Expected: PASS.

- [ ] **Step 6: Commit**

```sh
gofmt -l keepalive; go vet ./keepalive/
git add keepalive/
git commit -s -m "feat(keepalive): Check a relay's model name, and report each thread's plan" -m "Port of main 105f78e5 78534e5b 62954fed ba46c777 (relay check) and 9e79dfe5 ddd44ffa 30b99939 5429fe0f (plans, refreshedAt)." -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 5: B1 — what client lifecycle signals need from the keeper

**Files:**
- Modify: `keepalive/keeper.go` (`Record.Client`; `kaEntry.client`; `ended` map; new methods; `Record` gate; `Forget` clears)
- Create: `keepalive/lifecycle.go`
- Test: `keepalive/lifecycle_test.go` (new)

**Interfaces:**
- Produces:

```go
// Record.Client is the id the client itself names the session by, when the host keys it
// differently (Codex's session-id header). "" when there is none.
Client string

// MarkEnded records that session ended at `at`. Record then refuses any entry of that session
// (matched on Key.Session or Record.Client) whose request started before `at`.
func (k *Keeper) MarkEnded(tenant, session string, at time.Time)

// EndedAfter reports whether session (or a client alias) was marked ended after startedAt.
func (k *Keeper) EndedAfter(tenant, session string, startedAt time.Time) bool

// RetireSessionStartedBefore retires the session's entries (matched on Key.Session or
// Record.Client). A zero cutoff retires every thread, as RetireSession. A non-zero cutoff
// retires only the MAIN thread's entries ("" thread) that started before it. Returns how many.
func (k *Keeper) RetireSessionStartedBefore(tenant, session string, cutoff time.Time) int
```

- `const maxEndedPerTenant = 4096` and `const lifecycleTTL = 6 * time.Hour`: copy the values from `main`'s `proxy/clientsignal.go` (`git show 67c5f758:proxy/clientsignal.go`). If they differ from these, use `main`'s.

- [ ] **Step 1: Write the failing tests**

```go
package keepalive

import (
	"testing"
	"time"
)

func TestARequestThatStartedBeforeTheEndIsNotHeld(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.MarkEnded("t", "s", start.Add(time.Second))
	k.Record(trackedRecord(fd, key, start)) // in flight when the session ended
	if _, ok := k.Entry(key); ok {
		t.Fatal("a request that started before SessionEnd must not re-arm the keep-alive")
	}
	k.Record(trackedRecord(fd, key, start.Add(2*time.Second))) // the session resumed
	if _, ok := k.Entry(key); !ok {
		t.Fatal("a request that started after the end mark is a resume and must be held")
	}
}

func TestCompactRetiresOnlyTheMainThreadStartedBefore(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	main := Key{Tenant: "t", Session: "s"}
	sub := Key{Tenant: "t", Session: "s", Thread: "a:x"}
	for _, key := range []Key{main, sub} {
		k.Record(trackedRecord(fd, key, start))
		k.Record(trackedRecord(fd, key, start))
	}
	if n := k.RetireSessionStartedBefore("t", "s", start.Add(time.Second)); n != 1 {
		t.Fatalf("retired %d, want 1 (the main thread only)", n)
	}
	if _, ok := k.Entry(main); ok {
		t.Fatal("the main thread must be retired")
	}
	if _, ok := k.Entry(sub); !ok {
		t.Fatal("a subagent thread must survive a compact")
	}
}

func TestALateCompactKeepsThePostCompactionEntry(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start.Add(5*time.Second))) // the post-compaction request
	if n := k.RetireSessionStartedBefore("t", "s", start.Add(time.Second)); n != 0 {
		t.Fatalf("retired %d, want 0: the entry started after the cutoff", n)
	}
}

func TestTheClientAliasMatches(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	key := Key{Tenant: "t", Session: "content-hash"}
	r := trackedRecord(fd, key, start)
	r.Client = "codex-session-id"
	k.Record(r)
	k.Record(r)
	if n := k.RetireSessionStartedBefore("t", "codex-session-id", time.Time{}); n != 1 {
		t.Fatalf("retired %d by the client alias, want 1", n)
	}
}

func TestEndedMarksArePerTenant(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	k.MarkEnded("a", "s", start.Add(time.Second))
	if k.EndedAfter("b", "s", start) {
		t.Fatal("tenant a's end mark must not end tenant b's session")
	}
}

func TestForgetClearsEveryNewMapForThatTenantOnly(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	for _, ten := range []string{"a", "b"} {
		k.MarkEnded(ten, "s", start.Add(time.Second))
		k.Arrive(Key{Tenant: ten, Session: "s"})
		k.mu.Lock()
		k.routeFailedLocked(routeKey(ten, "e", "m"), start)
		k.markCredDeadLocked(ten, "c", "m", 401, start)
		k.mu.Unlock()
	}
	k.Forget("a")
	k.mu.Lock()
	defer k.mu.Unlock()
	for name, n := range map[string]int{"ended": len(k.ended), "inflight": len(k.inflight),
		"routeFails": len(k.routeFails), "credDead": len(k.credDead)} {
		if n != 1 {
			t.Errorf("%s has %d entries after Forget(a), want 1 (tenant b's)", name, n)
		}
	}
}
```

`TestForgetClearsEveryNewMapForThatTenantOnly` assumes `ended` is `map[string]map[string]time.Time` keyed by tenant, as on `main`. Then `len(k.ended)` counts tenants.

- [ ] **Step 2: Run the tests to see them fail**

Run: `GOWORK=off go test ./keepalive/ -run 'Ended|Compact|Alias|Forget'`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

Port `main`'s `keeper.endedAfter` and `keeper.markEndedLocked` (`git show 67c5f758:proxy/clientsignal.go`) and `retireSessionStartedBefore` (`git show 67c5f758:proxy/keepalive.go | sed -n '1025,1075p'`) into `keepalive/lifecycle.go` as `EndedAfter`, `markEndedLocked` + `MarkEnded`, and `RetireSessionStartedBefore`. Keep `main`'s cap (`maxEndedPerTenant`) and expiry (`lifecycleTTL`) logic exactly. Make `RetireSession(tenant, session)` call `RetireSessionStartedBefore(tenant, session, time.Time{})`.

In `Record`, before the breaker gate: `if k.endedAfterLocked(r.Key.Tenant, r.Key.Session, r.StartedAt) || (r.Client != "" && k.endedAfterLocked(r.Key.Tenant, r.Client, r.StartedAt)) { k.retireLocked(id); return }`. Set `e.client = r.Client`. In `Forget(tenant)`: `delete(k.ended, tenant)`.

This does NOT port `clientSignalEnabled` (`CONTEXT_GURU_CLIENT_SIGNALS`). The switch belongs to the host route, which only calls these methods when it is on. Task 7 keeps it in `proxy/clientsignal.go`.

- [ ] **Step 4: Run the tests**

Run: `GOWORK=off go test ./keepalive/`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
gofmt -l keepalive; go vet ./keepalive/
git add keepalive/
git commit -s -m "feat(keepalive): End and compact a session's entries for client lifecycle signals" -m "Port of the keeper half of main ae84df7c d221800f 26b21e34. The /signal route stays in the host." -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 6: Package review gate

**Files:** none changed unless the review finds something.

- [ ] **Step 1: Run the package tests three times with `-race`**

Run: `CGO_ENABLED=1 GOWORK=off go test -race -count=3 ./keepalive/`
Expected: PASS, no races.

- [ ] **Step 2: Check that the API is additive**

Run: `git diff 742ea693 -- keepalive/ | grep -E '^-func|^-\s+[A-Z][A-Za-z]+ '`
Expected: no output. No exported func or field was removed or renamed.

- [ ] **Step 3: Build Cortex against the port branch, without editing Cortex**

```sh
CX=/Users/davidboaz/Documents/GitHub/cortex/.worktrees/contextguru-keepalive
cd $CX/core && GOWORK=off go build -modfile=<(sed 's#.worktrees/keeper-export#.worktrees/keeper-port#' go.mod) ./plugins/contextguru/
```

If process substitution is not accepted for `-modfile`, copy `go.mod` and `go.sum` to `/tmp/cx-mod/`, edit the copy, and pass `-modfile=/tmp/cx-mod/go.mod`.
Expected: it builds. Cortex's code is unchanged at this point.

---

### Task 7: The merge, and rewiring main's proxy files to the package

**Files:**
- Merge: `fix/tool-output-system-reminder` into `port/keepalive-main`
- Resolve: `proxy/keepalive.go` (take OURS, then add the host glue below), `proxy/keepalive_test.go` (keep it DELETED; its behaviour is in `keepalive/keeper_test.go`, with `main`'s one change done in Task 1), `proxy/thread_test.go` (take both; see Step 2)
- Modify: `proxy/clientsignal.go`, `proxy/predictive.go`, `proxy/campaignbuild.go`, `proxy/control.go`, `proxy/proxy.go`, `proxy/credmask.go`, `cmd/context-guru-proxy/main.go` only where they reach keeper internals
- Port tests: `proxy/keepalive_inflight_test.go`, `proxy/keepaliveplan_test.go`, `proxy/clientsignal_test.go`, `proxy/predictive_inert_test.go`, `proxy/campaignbuild_test.go`, `proxy/campaignauto_test.go`

**Interfaces:**
- Consumes: `Keeper.{Depart, Plans, MarkEnded, EndedAfter, RetireSessionStartedBefore}`, `Record.{Cred, Client}`, `Stats.{Missed, RouteTrips}`, `Plan`.
- Produces: `main`'s host API, unchanged: `Handler.KeepAlivePlans(tenantID string, allTenants bool, session string) map[string]dash.KeepAlivePlan`, `POST /signal`, `Handler.predRT()`.

- [ ] **Step 1: Start the merge**

```sh
git merge --no-ff --signoff fix/tool-output-system-reminder
git checkout --ours proxy/keepalive.go
git rm proxy/keepalive_test.go
```

- [ ] **Step 2: Resolve `proxy/thread_test.go`**

Keep this branch's version of the conflict, and add `main`'s one line from 105f78e5 to `TestKeeperSurvivesASideRequestsArrival`: the side request records (and so departs) before the sweep:

```go
	rec(side.ID, start.Add(2*time.Second), sideBody)
```

Place it exactly where `git show 105f78e5 -- proxy/thread_test.go` puts it.

- [ ] **Step 3: Add the host glue to `proxy/keepalive.go`**

1. `type keeper struct`: add `pred *predRuntime`, and in `newKeeperWith` set `k.pred = newPredRuntime(h.campaignDir())`. Add `signalApplied atomic.Int64`.
2. `newKeeperWith`'s `OnTick`: after the existing work, call `k.h.maybeRefreshAutoCampaigns(now)` (main 47ea6476 `start`).
3. `record`: before `resolvePolicy`, add `main`'s check: `if clientSignalEnabled() && (k.Keeper.EndedAfter(tn.ID, session, startedAt) || (client != "" && k.Keeper.EndedAfter(tn.ID, client, startedAt))) { k.Keeper.Depart(key); return }`, with `client := clientSessionAlias(...)` as in `main` L765. Then, after `resolvePolicy` and only when no manager strategy matched, apply the predictive decision as `main` L791-815 does (`k.predictiveDecide(...)`, `pdec.apply(pol)`, `applied = campaignStrategyID(...)`). Pass `Cred: credKey(r.Header)` and `Client: client` in the `keepalive.Record` literal.
4. `credKey`: copy `main`'s `credIDKey` var and `credKey` (`git show 67c5f758:proxy/keepalive.go | sed -n '125,140p;1560,1580p'`) into `proxy/credmask.go`. Drop the tenant prefix from the result, because the package adds the tenant itself.
5. Add `func (h *Handler) KeepAlivePlans(tenantID string, allTenants bool, session string) map[string]dash.KeepAlivePlan` that calls `h.keeper.Plans(...)` and converts each `keepalive.Plan` to `dash.KeepAlivePlan` exactly as `main` L2102 fills `RefreshedMS`, `NextPingMS`, `PingsLeft` and `IdleMS`.
6. Add nil-safe wrappers next to the existing ones: `markEnded`, `endedAfter`, `retireSessionStartedBefore`, `depart`.

- [ ] **Step 4: Rewire main's new files**

| File (main) | It used | Change it to |
|---|---|---|
| `proxy/clientsignal.go` | `k.Stats().Live`, `k.retireSession`, `k.retireSessionStartedBefore`, `k.mu` + `k.markEndedLocked(kaKey(...))`, `k.endedAfter`, `k.markEndedLocked`, `k.signalApplied` | `k.Keeper.Stats().Live`, `k.retireSession`, `k.Keeper.RetireSessionStartedBefore`, `k.Keeper.MarkEnded(tenant, session, at)`, (delete its own `endedAfter`/`markEndedLocked`, now in the package), `k.signalApplied` |
| `proxy/predictive.go` | `k.pred.forTenant`, `k.h.opts.Prices`, `k.now()` | `k.pred.forTenant`, `k.h.opts.Prices`, `k.Keeper.Now()` |
| `proxy/campaignbuild.go` | `h.keeper.pred` | unchanged (the field is now on the host `keeper`) |
| `proxy/control.go` | `h.predRT().eraseTenant` after `h.keeper.forget` | unchanged |
| `proxy/proxy.go` | `h.keeper.arrive` then `cp.predArrival = h.predRT().arrive(...)`; `usage.Live = cp.liveFacts(ttfb)` | unchanged. Also: on every path that returns after `arrive` WITHOUT reaching `record` (upstream error, client gone), call `h.keeper.depart(key)`. Find them with `grep -n 'keeper.arrive\|keeper.record' proxy/proxy.go` |

Delete from `proxy/keepalive.go` anything `main` added that now lives in the package: `departed`, `routeFailed`, `routeOK`, `routeIsTripped`, `kaRouteKey`, `markCredDead`, `credIsDead*`, `credModelKey`, `notePingMiss`, `notePingRead`, `relayServesClaude`, `claudeModel`, `retireSessionStartedBefore`. Keeping OURS in Step 1 already leaves most of them out. Run `go build ./proxy/` and fix each `undefined:` with this table.

- [ ] **Step 5: Port main's proxy tests**

For each test file below, keep the tests that go through the `Handler` (HTTP or `h.keeper.record`) as they are. Rewrite only lines that read keeper internals (`k.live`, `e.missed`, `k.routeTripped`, `k.inflight`, `k.ended`):
- `k.live[...]` → `h.keeper.Entry(keepalive.Key{...})`
- `len(k.live)` → `h.keeper.Stats().Live`
- `e.refreshedAt` → `h.keeper.Plans(...)[thread].RefreshedAt`
- route and credential state → the `Stats` counters, or a further `record` that must or must not be held

Delete a test from `proxy/` only when the same behaviour is already pinned in `keepalive/` (Tasks 1-5). In that case, write the name of the package test that replaces it in the commit message.

Files: `proxy/keepalive_inflight_test.go`, `proxy/keepaliveplan_test.go`, `proxy/clientsignal_test.go`, `proxy/predictive_inert_test.go`, `proxy/campaignbuild_test.go`, `proxy/campaignauto_test.go`, and `proxy/keepalivestrategy_test.go` (auto-merged; check it compiles).

- [ ] **Step 6: Run everything**

```sh
GOWORK=off go build ./...
GOWORK=off go test ./keepalive/ ./proxy/ ./dash/ ./cmd/... ./predictive/ ./apply/
```

Expected: PASS, except `dash`'s `TestCalculatorPricesFromTheRowsModelNotABlend` (fails on `main` too).

- [ ] **Step 7: Commit the merge**

```sh
gofmt -l keepalive proxy; go vet ./keepalive/ ./proxy/
git add -A
git commit -s -F - <<'EOF'
Merge branch 'fix/tool-output-system-reminder' into port/keepalive-main

Brings main at 67c5f758 and the #556 fix. main's keep-alive changes to
the in-proxy keeper are already ported into package keepalive by the
commits before this one; this merge keeps this branch's structure for
proxy/keepalive.go and proxy/keepalive_test.go, and rewires main's new
host files (clientsignal, predictive, campaigns, plans) to the package.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 8: Cortex — depart on every path that skips `Record`

**Files (Cortex repo, worktree `cortex/.worktrees/contextguru-keepalive`):**
- Modify: `core/plugins/contextguru/plugin.go` (where `p.keeper.Arrive` is called, :365, and where `p.keeper.Record` is called, :478)
- Test: `core/plugins/contextguru/keepalive_test.go` (add one test)

**Interfaces:**
- Consumes: `keepalive.Keeper.Depart(Key)`.

- [ ] **Step 1: Point Cortex at the port worktree, for this task only**

In `core/go.mod`, `cmd/cortex/go.mod` and `cmd/cortex-envoy/go.mod`, change the `replace` path from `.worktrees/keeper-export` to `.worktrees/keeper-port`. Task 9 changes it back.

- [ ] **Step 2: Write the failing test**

Add a test in the style of the existing keep-alive tests in `core/plugins/contextguru/keepalive_test.go`: a subagent request (`AgentRoleSubagent`, which `recordFor` skips) runs `OnRequest` and `OnFinish`, then the main session's entry is due. Assert that the sweep sends one ping. Name it `TestASkippedSubagentRequestDoesNotPauseTheMainThread`.

- [ ] **Step 3: Run it to see it fail**

Run: `cd core && GOWORK=off go test ./plugins/contextguru/ -run TestASkippedSubagentRequestDoesNotPauseTheMainThread`
Expected: FAIL (no ping: the subagent's `Arrive` was never departed).

- [ ] **Step 4: Implement**

In `plugin.go`, wherever the request path returns after `p.keeper.Arrive(key)` without calling `p.keeper.Record(...)`, call `p.keeper.Depart(key)` with the same key. Use the same `keepalive.Key{Session: sid}` as the `Arrive` call.

- [ ] **Step 5: Run the plugin tests**

Run: `cd core && GOWORK=off go test ./plugins/contextguru/ ./cost/...`
Expected: PASS.

- [ ] **Step 6: Commit in Cortex**

```sh
git add core/plugins/contextguru/
git commit -s -m "fix(contextguru): Depart from the keeper when a request is not recorded" -m "The exported keeper now pauses a thread's ping while a request on it is open (context-guru main #527). A request the plugin does not record must say so." -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

Do not commit the `go.mod` `replace` change.

---

### Task 9: Land it in the dev worktree

**Files:** none changed in code.

- [ ] **Step 1: Fast-forward the dev branch**

```sh
K=/Users/davidboaz/Documents/GitHub/context-guru/.worktrees/keeper-export
git -C $K status --short          # must be empty
git -C $K merge --ff-only port/keepalive-main
```

Expected: a fast-forward. If it is not one, stop and report.

- [ ] **Step 2: Point Cortex back at the dev worktree, and verify**

Revert the three `go.mod` `replace` edits from Task 8 Step 1. Then:

```sh
cd /Users/davidboaz/Documents/GitHub/cortex/.worktrees/contextguru-keepalive
GOWORK=off go -C core test ./plugins/contextguru/ ./cost/...
GOWORK=off go -C cmd/cortex build ./...
GOWORK=off go -C cmd/cortex-envoy build ./...
```

Expected: PASS and build.

- [ ] **Step 3: Clean up the port worktree**

```sh
CGM=/Users/davidboaz/Documents/GitHub/context-guru
git -C $CGM worktree remove .worktrees/keeper-port
git -C $CGM branch -d port/keepalive-main
```

- [ ] **Step 4: Ask before pushing**

Report the new `feat/export-keepalive-keeper` head and the Cortex commit. Do not push either branch until the user asks.
