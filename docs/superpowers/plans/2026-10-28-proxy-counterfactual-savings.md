# Proxy savings from `counterfactual` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `counterfactual.Compute` the only source of savings dollars in the context-guru proxy, and delete the rules in `dash/event.go` that it replaces.

**Architecture:** The pipeline records per-message byte changes on each `components.Report`. `apply` turns them into `counterfactual.Operation`s (positions in wire space, `ChangeID` from a content hash). A proxy-side `savingsTracker` owns one `counterfactual.Trackers`, runs `BeginRequest` / `Add` / `EndRequest` / `Compute` around each request, and hands the `Result` to `dash.Event`, which prices tiers with `modelinfo.Price`. A `--savings` mode (`legacy` | `shadow` | `counterfactual`) lets the new figure run beside the old one before it replaces it.

**Tech Stack:** Go 1.26.4, `github.com/rossoctl/context-guru/counterfactual` (stdlib-only module), `gjson`, SQLite via `dash`.

**Spec:** `docs/superpowers/specs/2026-10-28-proxy-counterfactual-savings-design.md`. Method: `docs/counterfactual/design.md`. Host wiring: `docs/counterfactual/cortex-wiring.md` (W5, W7.3, W9).

## Global Constraints

- Cortex is out of scope. No file outside this repo changes.
- `counterfactual` imports only the standard library (`counterfactual/boundary_test.go` enforces it). The keep-alive conversion therefore takes a library-local struct, never `components.KeepAliveReport`.
- Operation sizes are bytes only. The library converts bytes to tokens (design §4.2 rule 7).
- The proxy never adds token tiers into one token total. Tokens stay per tier; dollars are summed.
- A counterfactual never enters spend: `CostUSD` is untouched by every change here.
- A shadow run still calls `Compute`, so `ThreadState` advances (cortex-wiring W7.3 step 3).
- No `dash` schema change. New figures go into existing columns (`CompRow.SavedUSD`, `BaselineCostUSD`, `KeepAliveSavedUSD`).
- Do not delete `cachesplitSavedUSD` or its inputs. The library does not implement the `split` effect (`counterfactual/doc.go`, "Not implemented").
- Commits: `git commit -s`, trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.
- Before each commit: `gofmt -l <paths>` (always give a path) and `go vet ./<pkg>/...`.

## Review Focus

1. **A change that ended and is reduced again.** The client compacts, the engine re-reduces the same content under the same `ChangeID`. The host must report it again. Test: Task 6, `TestSavings_ReReportsAfterChangeEnded`.
2. **Message count changes.** Summarize, sweep and coref drop messages, so `changedIdx` is nil. They must still produce a `MessageSplice`, or every later position in the thread is wrong. Test: Task 5.
3. **Two requests of one thread in flight.** `Compute` returns `Recomputed` when another request replaced the `ThreadState` first. The host must not crash or double-count. Test: Task 6, `TestSavings_ConcurrentRequests`.
4. **No usage, a streamed error, or an unpriced model.** The row must stay unpriced, not read as zero saving. Test: Task 8.
5. **An `observe`-mode (dry-run) run.** Its operations carry `DryRun`, which must never reach a dollar total. Test: Task 8, `TestPriceCF_DryRunNeverCounted`.

---

## File Structure

| File | Responsibility |
|---|---|
| `counterfactual/host.go` (new) | `BillingFrom`, `PingOps`, `Trackers.IsActive`. Pure host helpers. |
| `components/pipeline.go` (modify) | Fill `Report.Changes` in `runOne`. |
| `components/component.go` (modify) | `Change` type, `Report.Changes`, `KeepAliveReport.Window`. |
| `apply/ops.go` (new) | `OpsFromRun`: `RunReport` + slots → `[]counterfactual.Operation`. |
| `apply/apply.go` (modify) | Fill `Trace.Ops`. |
| `proxy/cfsavings.go` (new) | `savingsTracker`: bodies, breakpoints, Begin/Add/End/Compute, replay filter. |
| `proxy/cfsavings_body.go` (new) | `cfBodies(raw []byte)` and `cfBreakpoints(raw []byte)`. |
| `proxy/dashcapture.go` (modify) | Hold the open request; call `Compute` in `finish`. |
| `dash/cfprice.go` (new) | `Event.PriceCF`: tiers → dollars, per component. |
| `dash/event.go` (modify) | Call `PriceCF` by mode; delete replaced rules in Task 11. |
| `cmd/context-guru-proxy/main.go` (modify) | `--savings` flag; `dashEmitter` converts pings. |

---

### Task 1: Library host helpers

**Files:**
- Create: `counterfactual/host.go`
- Test: `counterfactual/host_test.go`

**Interfaces:**
- Consumes: `Trackers`, `Tracker`, `ThreadState`, `Operation`, `RequestOps`, `ModelCall`, `CallUsage`, `Billing`, `Thread` from `types.go` / `state.go`.
- Produces:
  - `func BillingFrom(mode BillingMode) Billing`
  - `type Ping struct { Session, ThreadID, Model, Host string; Usage CallUsage; At time.Time; Window time.Time }`
  - `type PingFacts struct { LastPing time.Time; PingWindow string }`
  - `func PingOps(p Ping, b Billing) (RequestOps, PingFacts)`
  - `func (ts *Trackers) IsActive(session string, t Thread, changeID string) bool`

- [ ] **Step 1: Write the failing test**

```go
// counterfactual/host_test.go
package counterfactual

import (
	"testing"
	"time"
)

func TestPingOps_OneSpendWithWindowChangeID(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	win := at.Add(-4 * time.Minute)
	ops, facts := PingOps(Ping{
		Session: "s1", ThreadID: "", Model: "m", Host: "h",
		Usage: CallUsage{Usage: Usage{CacheRead: 1000, Input: 10}, Output: 1},
		At:    at, Window: win,
	}, BillingFrom(BillingAPI))

	if ops.Thread.ID != "main" {
		t.Fatalf("thread = %q, want main", ops.Thread.ID)
	}
	if len(ops.Ops) != 1 || ops.Ops[0].Kind != KindSpend || ops.Ops[0].Call == nil {
		t.Fatalf("want one spend with a call, got %+v", ops.Ops)
	}
	wantID := "ka:main:" + win.UTC().Format(time.RFC3339)
	if ops.Ops[0].ChangeID != wantID || facts.PingWindow != wantID {
		t.Fatalf("ChangeID = %q, PingWindow = %q, want %q", ops.Ops[0].ChangeID, facts.PingWindow, wantID)
	}
	if !facts.LastPing.Equal(at) {
		t.Fatalf("LastPing = %v, want %v", facts.LastPing, at)
	}
}

func TestIsActive_FalseForUnknownThread(t *testing.T) {
	ts := NewTrackers()
	if ts.IsActive("s", Thread{ID: "main"}, "cg:x") {
		t.Fatal("an unseen thread has no active change")
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./counterfactual/ -run 'TestPingOps|TestIsActive' -v`
Expected: FAIL, `undefined: PingOps`.

- [ ] **Step 3: Write the implementation**

```go
// counterfactual/host.go
package counterfactual

import "time"

// BillingFrom wraps a billing mode the host read from the request's credential.
func BillingFrom(mode BillingMode) Billing { return Billing{Mode: mode} }

// Ping is a keep-alive ping in the library's own terms, so the library does not import a host
// type. The host maps its report onto this.
type Ping struct {
	Session, ThreadID, Model, Host string
	Usage                          CallUsage
	At                             time.Time // when the ping started
	Window                         time.Time // the start of the idle gap the ping served
}

// PingFacts are what the thread's next real request needs in EntryFacts (cortex-wiring W4.6).
type PingFacts struct {
	LastPing   time.Time
	PingWindow string
}

// PingOps turns a ping into a RequestOps with one spend, and returns the facts for the next
// request. The thread "" is the primary thread and becomes "main".
func PingOps(p Ping, b Billing) (RequestOps, PingFacts) {
	id := p.ThreadID
	if id == "" {
		id = "main"
	}
	change := "ka:" + id + ":" + p.Window.UTC().Format(time.RFC3339)
	return RequestOps{
			Version: 1,
			Session: p.Session,
			Thread:  Thread{ID: id},
			Model:   p.Model,
			At:      p.At,
			Billing: b,
			Ops: []Operation{{
				Kind:     KindSpend,
				Operator: "context-guru.keepalive",
				ChangeID: change,
				Call:     &ModelCall{Host: p.Host, Model: p.Model, Usage: p.Usage},
			}},
		},
		PingFacts{LastPing: p.At, PingWindow: change}
}

// IsActive reports whether the thread still applies the change again on each request. The host
// uses it to report a change once and not again while the library still carries it (§4.6).
func (ts *Trackers) IsActive(session string, t Thread, changeID string) bool {
	ts.mu.Lock()
	tr, ok := ts.m[ThreadKey(session, t)]
	ts.mu.Unlock()
	if !ok {
		return false
	}
	for _, op := range tr.snapshot().Active {
		if op.ChangeID == changeID {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test ./counterfactual/... && gofmt -l counterfactual && go vet ./counterfactual/...`
Expected: PASS, no gofmt output.

- [ ] **Step 5: Commit**

```bash
git add counterfactual/host.go counterfactual/host_test.go
git commit -s -m "feat(counterfactual): Add the host helpers BillingFrom, PingOps and IsActive

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: Make the root module see `counterfactual`

**Files:**
- Modify: `go.mod`

**Interfaces:**
- Produces: `import cf "github.com/rossoctl/context-guru/counterfactual"` compiles in the root module with or without `go.work`.

- [ ] **Step 1: Write a failing compile check**

Create `proxy/cf_import_test.go`:

```go
package proxy

import (
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

func TestCounterfactualImportable(t *testing.T) {
	if cf.NewTrackers() == nil {
		t.Fatal("nil Trackers")
	}
}
```

- [ ] **Step 2: Run it with the workspace off**

Run: `GOWORK=off go test ./proxy/ -run TestCounterfactualImportable`
Expected: FAIL, `no required module provides package github.com/rossoctl/context-guru/counterfactual`.

- [ ] **Step 3: Add the require and replace**

```bash
go mod edit -require=github.com/rossoctl/context-guru/counterfactual@v0.0.0 \
            -replace=github.com/rossoctl/context-guru/counterfactual=./counterfactual
```

- [ ] **Step 4: Verify both ways**

Run: `GOWORK=off go test ./proxy/ -run TestCounterfactualImportable && go test ./proxy/ -run TestCounterfactualImportable && GOWORK=off go mod tidy -diff`
Expected: PASS twice, empty tidy diff.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum proxy/cf_import_test.go
git commit -s -m "build(counterfactual): Require the counterfactual module from the root module

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: Record per-message byte changes on `Report`

`changedIdx` says which messages changed, not by how many bytes. The sum check needs bytes (design §6.5), and `ChangeID` needs a content hash. Fill both in `runOne`, so no offloader changes.

**Files:**
- Modify: `components/component.go` (type `Report`, near line 807)
- Modify: `components/pipeline.go` (`runOne`, default branch near line 211)
- Test: `components/pipeline_changes_test.go` (new)

**Interfaces:**
- Produces:
  - `type Change struct { Idx int; BytesBefore, BytesAfter int; Hash string }` in package `components`
  - `Report.Changes []Change`, one per entry of `ChangedIdx`, same order. `Hash` is the first 12 hex characters of sha256 over the message text before the run.
  - `Report.CountChanged bool`: the run changed the message count (`len(before) != len(after)`), so `Changes` is empty and Task 5 handles it.

- [ ] **Step 1: Write the failing test**

```go
// components/pipeline_changes_test.go
package components

import "testing"

func TestRunOne_RecordsByteChangesAndStableHash(t *testing.T) {
	big := string(make([]byte, 4000))
	run := func() Report {
		p := NewPipeline(shrinkToolOutputs{})
		rr := p.Run(newReq(userMsg("hi"), toolMsg(big)), &Ctx{})
		return rr.Components[0]
	}
	a, b := run(), run()
	if len(a.Changes) != 1 {
		t.Fatalf("Changes = %+v, want one", a.Changes)
	}
	c := a.Changes[0]
	if c.Idx != 1 || c.BytesBefore <= c.BytesAfter || c.BytesAfter <= 0 {
		t.Fatalf("change = %+v", c)
	}
	if c.Hash == "" || c.Hash != b.Changes[0].Hash {
		t.Fatalf("hash %q vs %q: must be equal for equal input", c.Hash, b.Changes[0].Hash)
	}
}
```

Use the existing helpers in `components/pipeline_test.go` for `newReq`, `userMsg`, `toolMsg` and a shrinking component. Open that file first, and if the helper names differ, use the names it defines. Do not add parallel helpers.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./components/ -run TestRunOne_RecordsByteChanges -v`
Expected: FAIL, `Changes undefined`.

- [ ] **Step 3: Add the type and fill it**

In `components/component.go`, above `type Report`:

```go
// Change is one message a component rewrote, in bytes. The pipeline fills it, so no component
// has to. Hash is of the text BEFORE the run: the same content re-sent next turn gets the same
// hash, which is what lets the savings code recognise a replay (counterfactual ChangeID).
type Change struct {
	Idx                       int // index in req.Input
	BytesBefore, BytesAfter   int
	Hash                      string
}
```

In `type Report`, after `ChangedIdx []int`:

```go
	// Changes is ChangedIdx with sizes and a content hash. Empty when CountChanged.
	Changes []Change
	// CountChanged says the run added or removed messages (a summary, a drop). ChangedIdx and
	// Changes are empty then, because indices before and after do not line up.
	CountChanged bool
```

In `runOne`, in the `default:` branch right after `rep.ChangedIdx = changedIdx(before, req.Input)`:

```go
		rep.CountChanged = len(before) != len(req.Input)
		rep.Changes = changesOf(before, req.Input, rep.ChangedIdx)
```

Add to `components/pipeline.go`:

```go
// changesOf sizes each changed message and hashes its pre-run text.
func changesOf(before, after []schemas.ChatMessage, idx []int) []Change {
	out := make([]Change, 0, len(idx))
	for _, i := range idx {
		if i < 0 || i >= len(before) || i >= len(after) {
			continue
		}
		bt, at := schema.MessageText(before[i]), schema.MessageText(after[i])
		sum := sha256.Sum256([]byte(bt))
		out = append(out, Change{Idx: i, BytesBefore: len(bt), BytesAfter: len(at),
			Hash: hex.EncodeToString(sum[:6])})
	}
	return out
}
```

`restoreOwn` can rewrite `req.Input` after `changedIdx`, so compute `Changes` after the `restoreOwn` call, not before it.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test ./components/... && gofmt -l components && go vet ./components/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add components/component.go components/pipeline.go components/pipeline_changes_test.go
git commit -s -m "feat(components): Record the bytes and a content hash of each rewritten message

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: `apply` builds Operations

**Files:**
- Create: `apply/ops.go`
- Modify: `apply/apply.go` (struct `Trace` near line 221; where `Trace.Run` and `slots` are both in scope, near line 487 and the end of `BodyOpts`)
- Test: `apply/ops_test.go`

**Interfaces:**
- Consumes: `components.RunReport`, `components.Report.Changes`, `components.Change`, `wireGroups(slots []slot) []int`.
- Produces:
  - `func OpsFromRun(rr components.RunReport, wire []int, applied bool) (ops []cf.Operation, dropped int)`
  - `Trace.Ops []cf.Operation` and `Trace.OpsDropped int`.
  - Operator name: `"context-guru." + Report.Component`. `ChangeID`: `"cg:" + Component + ":" + Change.Hash`.

`wire[i]` is the wire message index for `req.Input[i]`. When `wire` is nil, `wireGroups` could not map a slot, so `OpsFromRun` returns no ops and counts every change as dropped. Fail open, never guess a position.

- [ ] **Step 1: Write the failing test**

```go
// apply/ops_test.go
package apply

import (
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/components"
)

func TestOpsFromRun_MapsToWirePositionAndSkipsRevertedAndSkipped(t *testing.T) {
	rr := components.RunReport{Components: []components.Report{
		{Component: "mask", Changes: []components.Change{{Idx: 1, BytesBefore: 4000, BytesAfter: 100, Hash: "aa11"}}},
		{Component: "dedup", Reverted: true, Changes: []components.Change{{Idx: 0, BytesBefore: 9, BytesAfter: 1, Hash: "bb22"}}},
		{Component: "toon", Skipped: true},
	}}
	ops, dropped := OpsFromRun(rr, []int{0, 3}, true)
	if dropped != 0 || len(ops) != 1 {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
	o := ops[0]
	if o.Kind != cf.KindRewrite || o.Operator != "context-guru.mask" || o.ChangeID != "cg:mask:aa11" {
		t.Fatalf("op = %+v", o)
	}
	if o.At != (cf.Position{Section: cf.SectionMessages, Index: 3}) {
		t.Fatalf("At = %+v, want messages[3] (wire index, not req.Input index)", o.At)
	}
	if o.SizeBefore.Bytes != 4000 || o.SizeAfter.Bytes != 100 || o.DryRun {
		t.Fatalf("sizes/dryrun = %+v", o)
	}
}

func TestOpsFromRun_DryRunWhenNotApplied(t *testing.T) {
	rr := components.RunReport{Components: []components.Report{
		{Component: "mask", Changes: []components.Change{{Idx: 0, BytesBefore: 10, BytesAfter: 1, Hash: "cc"}}},
	}}
	ops, _ := OpsFromRun(rr, []int{0}, false)
	if len(ops) != 1 || !ops[0].DryRun {
		t.Fatalf("a run that was not applied must be DryRun: %+v", ops)
	}
}

func TestOpsFromRun_NilWireDropsEverything(t *testing.T) {
	rr := components.RunReport{Components: []components.Report{
		{Component: "mask", Changes: []components.Change{{Idx: 0, BytesBefore: 10, BytesAfter: 1, Hash: "cc"}}},
	}}
	ops, dropped := OpsFromRun(rr, nil, true)
	if len(ops) != 0 || dropped != 1 {
		t.Fatalf("ops = %+v dropped = %d", ops, dropped)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./apply/ -run TestOpsFromRun -v`
Expected: FAIL, `undefined: OpsFromRun`.

- [ ] **Step 3: Write the implementation**

```go
// apply/ops.go
package apply

import (
	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/components"
)

// OpsFromRun turns a run's per-message changes into counterfactual operations, in pipeline
// order. wire maps a req.Input index to the wire message index (wireGroups). applied is false for
// an observe-mode run: its operations are DryRun, computed but never sent (R12).
//
// A reverted or skipped report changed nothing upstream and reports nothing. A run that changed
// the message count is Task 5's (spliceOps) and is not handled here.
func OpsFromRun(rr components.RunReport, wire []int, applied bool) (ops []cf.Operation, dropped int) {
	for _, r := range rr.Components {
		if r.Skipped || r.Reverted {
			continue
		}
		for _, c := range r.Changes {
			if wire == nil || c.Idx < 0 || c.Idx >= len(wire) {
				dropped++
				continue
			}
			sz := func(n int) cf.Size { return cf.Size{Bytes: n} }
			ops = append(ops, cf.Operation{
				Kind:       cf.KindRewrite,
				Operator:   "context-guru." + r.Component,
				DryRun:     !applied,
				At:         cf.Position{Section: cf.SectionMessages, Index: wire[c.Idx]},
				SizeBefore: sz(c.BytesBefore),
				SizeAfter:  sz(c.BytesAfter),
				ChangeID:   "cg:" + r.Component + ":" + c.Hash,
			})
		}
	}
	return ops, dropped
}
```

In `apply/apply.go`, add to `Trace`:

```go
	// Ops are the rewrite operations of this request for the savings computation, in pipeline
	// order, with wire positions. OpsDropped counts changes that could not be placed.
	Ops        []cf.Operation
	OpsDropped int
```

and import `cf "github.com/rossoctl/context-guru/counterfactual"`. Where `BodyOpts` has both `slots` and the finished `RunReport` (find the assignment to `tr.Run`), add:

```go
	tr.Ops, tr.OpsDropped = OpsFromRun(*tr.Run, wireGroups(slots), applied)
```

`applied` is true when the body that is forwarded carries the rewrite. Read how `BodyOpts` decides `Changed` and the observe mode, and pass the matching boolean. Do not invent a new rule.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test ./apply/... && gofmt -l apply && go vet ./apply/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add apply/ops.go apply/ops_test.go apply/apply.go
git commit -s -m "feat(apply): Build counterfactual operations from the pipeline run

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 5: Operations for components that change the message count

Summarize, sweep and coref remove or add whole messages. `changedIdx` returns nil for them (`len(before) != len(after)`), so Task 4 reports nothing, and every later position in the thread would be wrong.

**Files:**
- Modify: `components/component.go` (`Change` gains `Splice *Splice`)
- Modify: `components/pipeline.go` (`runOne`)
- Modify: `apply/ops.go`
- Test: `components/pipeline_changes_test.go`, `apply/ops_test.go`

**Interfaces:**
- Consumes: `Report.CountChanged` from Task 3.
- Produces: for a count-changing run, `Report.Changes` has one entry with `Splice: &components.Splice{At, Removed, Inserted int}`, `BytesBefore` = bytes of the removed messages, `BytesAfter` = bytes of the inserted ones. `OpsFromRun` maps it to `Operation.MessageSplice` and `Spans`.

- [ ] **Step 1: Find which components do this**

Run: `grep -rn "len(req.Input)\|req.Input = append\|req.Input\[:" components/offload/*.go components/reformat/*.go | grep -v _test`
Expected: a list. Note which components remove messages. Record them in the commit message.

- [ ] **Step 2: Write the failing pipeline test**

```go
func TestRunOne_CountChangeRecordsASplice(t *testing.T) {
	// dropMiddle removes messages 1..2 and inserts one summary message at index 1.
	p := NewPipeline(dropMiddle{})
	rr := p.Run(newReq(userMsg("a"), toolMsg("bbbb"), toolMsg("cccc"), userMsg("d")), &Ctx{})
	r := rr.Components[0]
	if !r.CountChanged || len(r.Changes) != 1 || r.Changes[0].Splice == nil {
		t.Fatalf("report = %+v", r)
	}
	s := r.Changes[0].Splice
	if s.At != 1 || s.Removed != 2 || s.Inserted != 1 {
		t.Fatalf("splice = %+v", s)
	}
	if r.Changes[0].BytesBefore != 8 {
		t.Fatalf("bytes before = %d, want the 8 bytes removed", r.Changes[0].BytesBefore)
	}
}
```

Define `dropMiddle` in the test file as an `Offload` that does that rewrite and returns one cache key.

- [ ] **Step 3: Run it and confirm it fails**

Run: `go test ./components/ -run TestRunOne_CountChange -v`
Expected: FAIL.

- [ ] **Step 4: Implement**

Add `Splice *Splice` to `Change`, and `type Splice struct{ At, Removed, Inserted int }`. In `runOne`, when `rep.CountChanged`, compute the splice with a common prefix and suffix scan over message text:

```go
// spliceOf finds the one contiguous range that differs between before and after.
func spliceOf(before, after []schemas.ChatMessage) (Change, bool) {
	p := 0
	for p < len(before) && p < len(after) && reflect.DeepEqual(before[p], after[p]) {
		p++
	}
	s := 0
	for s < len(before)-p && s < len(after)-p &&
		reflect.DeepEqual(before[len(before)-1-s], after[len(after)-1-s]) {
		s++
	}
	removed, inserted := before[p:len(before)-s], after[p:len(after)-s]
	var bb, ba int
	h := sha256.New()
	for _, m := range removed {
		t := schema.MessageText(m)
		bb += len(t)
		h.Write([]byte(t))
	}
	for _, m := range inserted {
		ba += len(schema.MessageText(m))
	}
	return Change{Idx: p, BytesBefore: bb, BytesAfter: ba,
		Hash:   hex.EncodeToString(h.Sum(nil)[:6]),
		Splice: &Splice{At: p, Removed: len(removed), Inserted: len(inserted)}}, true
}
```

In `runOne`: `if rep.CountChanged { if c, ok := spliceOf(before, req.Input); ok { rep.Changes = []Change{c} } }`.

In `apply/ops.go`, for a change with `Splice != nil`, set `MessageSplice: &cf.MessageSplice{At: wire[c.Splice.At], Removed: c.Splice.Removed, Inserted: c.Splice.Inserted}`. Guard `At == len(wire)` (an append at the end): use `len(wire)` itself as the wire index. Add an `apply` test for the append case and one for a splice at 0.

- [ ] **Step 5: Run and commit**

Run: `go test ./components/... ./apply/... && gofmt -l components apply && go vet ./components/... ./apply/...`
Expected: PASS.

```bash
git add components apply
git commit -s -m "feat(components): Report a message splice when a run changes the message count

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 6: `savingsTracker` in the proxy

**Files:**
- Create: `proxy/cfsavings_body.go`, `proxy/cfsavings.go`
- Test: `proxy/cfsavings_body_test.go`, `proxy/cfsavings_test.go`

**Interfaces:**
- Consumes: `cf.NewBody`, `cf.Trackers`, `cf.RequestMeta`, `cf.Breakpoint`, `cf.EntryFacts`, `(*cf.Trackers).IsActive`, `apply.Trace.Ops`, `proxy.Usage`.
- Produces:
  - `func cfBodies(raw []byte) cf.Body` — hash of tools, system, messages of a request body
  - `func cfBreakpoints(raw []byte) []cf.Breakpoint`
  - `type savingsTracker struct{ trackers *cf.Trackers }`; `func newSavingsTracker() *savingsTracker`
  - `func (s *savingsTracker) begin(meta cf.RequestMeta, clientBody []byte) *cf.Request`
  - `func (s *savingsTracker) end(req *cf.Request, th cf.Thread, forwarded []byte, ops []cf.Operation, facts cf.EntryFacts)`
  - `func (s *savingsTracker) compute(req *cf.Request, u Usage) cf.Result`

`end` filters `ops`: it skips a rewrite whose `ChangeID` is already active on the thread, and `Add`s the rest, then calls `req.EndRequest`. Filtering happens in `end` because the thread is known only there.

- [ ] **Step 1: Write the body tests**

```go
// proxy/cfsavings_body_test.go
package proxy

import (
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

const cfSample = `{"tools":[{"name":"a"}],"system":"sys",
 "messages":[{"role":"user","content":"hi"},
 {"role":"assistant","content":[{"type":"text","text":"yo","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`

func TestCfBodies_SectionsAndSizes(t *testing.T) {
	b := cfBodies([]byte(cfSample))
	if b.Start != [3]int{0, 1, 2} || len(b.Pos) != 4 {
		t.Fatalf("start = %v pos = %d, want [0 1 2] and 4", b.Start, len(b.Pos))
	}
}

func TestCfBodies_EqualInputEqualHashes(t *testing.T) {
	a, b := cfBodies([]byte(cfSample)), cfBodies([]byte(cfSample))
	for i := range a.Pos {
		if a.Pos[i] != b.Pos[i] {
			t.Fatalf("position %d differs", i)
		}
	}
}

func TestCfBreakpoints_FindsBlockLevelMarkAndTTL(t *testing.T) {
	got := cfBreakpoints([]byte(cfSample))
	want := []cf.Breakpoint{{Pos: cf.Position{Section: cf.SectionMessages, Index: 1}, TTL: "1h"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run, see it fail, then implement**

Run: `go test ./proxy/ -run 'TestCfBodies|TestCfBreakpoints' -v` → FAIL, undefined.

```go
// proxy/cfsavings_body.go
package proxy

import (
	"github.com/tidwall/gjson"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

// cfBodies hashes a request body in the provider's order: tools, system, messages. A string
// system prompt is one block. Raw JSON is hashed as sent, so two hosts hash the same bytes alike.
func cfBodies(raw []byte) cf.Body {
	var tools, system, msgs [][]byte
	gjson.GetBytes(raw, "tools").ForEach(func(_, v gjson.Result) bool {
		tools = append(tools, []byte(v.Raw))
		return true
	})
	if s := gjson.GetBytes(raw, "system"); s.IsArray() {
		s.ForEach(func(_, v gjson.Result) bool { system = append(system, []byte(v.Raw)); return true })
	} else if s.Exists() {
		system = append(system, []byte(s.Raw))
	}
	gjson.GetBytes(raw, "messages").ForEach(func(_, v gjson.Result) bool {
		msgs = append(msgs, []byte(v.Raw))
		return true
	})
	return cf.NewBody(tools, system, msgs)
}

// cfBreakpoints lists every cache_control mark, as a position in the same space as cfBodies.
// A mark inside a message's content blocks belongs to that message. ttl defaults to "5m".
func cfBreakpoints(raw []byte) []cf.Breakpoint {
	var out []cf.Breakpoint
	ttl := func(cc gjson.Result) string {
		if t := cc.Get("ttl").String(); t == "1h" {
			return "1h"
		}
		return "5m"
	}
	scan := func(sec cf.Section, arr gjson.Result) {
		i := 0
		arr.ForEach(func(_, el gjson.Result) bool {
			if cc := el.Get("cache_control"); cc.Exists() {
				out = append(out, cf.Breakpoint{Pos: cf.Position{Section: sec, Index: i}, TTL: ttl(cc)})
			}
			el.Get("content").ForEach(func(_, blk gjson.Result) bool {
				if cc := blk.Get("cache_control"); cc.Exists() {
					out = append(out, cf.Breakpoint{Pos: cf.Position{Section: sec, Index: i}, TTL: ttl(cc)})
				}
				return true
			})
			i++
			return true
		})
	}
	scan(cf.SectionTools, gjson.GetBytes(raw, "tools"))
	if s := gjson.GetBytes(raw, "system"); s.IsArray() {
		scan(cf.SectionSystem, s)
	}
	scan(cf.SectionMessages, gjson.GetBytes(raw, "messages"))
	return out
}
```

- [ ] **Step 3: Write the tracker tests**

```go
// proxy/cfsavings_test.go
package proxy

import (
	"sync"
	"testing"
	"time"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

func rewriteOp(change string, at, before, after int) cf.Operation {
	return cf.Operation{Kind: cf.KindRewrite, Operator: "context-guru.mask", ChangeID: change,
		At:         cf.Position{Section: cf.SectionMessages, Index: at},
		SizeBefore: cf.Size{Bytes: before}, SizeAfter: cf.Size{Bytes: after}}
}

func TestSavings_ReportsOnceAndReReportsAfterChangeEnded(t *testing.T) {
	s := newSavingsTracker()
	th := cf.Thread{ID: "main"}
	big := `{"messages":[{"role":"user","content":"` + string(make([]byte, 0)) + `a"},{"role":"user","content":"b"}]}`
	run := func(n int, ops ...cf.Operation) cf.Result {
		meta := cf.RequestMeta{RequestID: string(rune('0' + n)), Session: "s", At: time.Unix(int64(1000+n*10), 0), Billing: cf.BillingFrom(cf.BillingAPI)}
		r := s.begin(meta, []byte(big))
		s.end(r, th, []byte(big), ops, cf.EntryFacts{TTL: 5 * time.Minute, Lifetime: cf.LifetimeExact})
		return s.compute(r, Usage{FreshInput: 10, CacheRead: 100})
	}
	run(1, rewriteOp("cg:mask:a1", 1, 4000, 100))
	if !s.trackers.IsActive("s", th, "cg:mask:a1") {
		t.Fatal("change must stay active after its first request")
	}
	// the engine replays the reduction: it must not be reported a second time
	res := run(2, rewriteOp("cg:mask:a1", 1, 4000, 100))
	n := 0
	for _, o := range res.Ops {
		if o.ChangeID == "cg:mask:a1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("replayed change priced %d times, want 1 (applied again, not reported again)", n)
	}
}

func TestSavings_ConcurrentRequests(t *testing.T) {
	s := newSavingsTracker()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
			r := s.begin(cf.RequestMeta{RequestID: string(rune('a' + i)), Session: "s", At: time.Now(), Billing: cf.BillingFrom(cf.BillingAPI)}, body)
			s.end(r, cf.Thread{ID: "main"}, body, nil, cf.EntryFacts{TTL: 5 * time.Minute, Lifetime: cf.LifetimeExact})
			_ = s.compute(r, Usage{FreshInput: 5})
		}(i)
	}
	wg.Wait() // run with -race: no data race, no panic
}
```

Add `TestSavings_ReReportsAfterChangeEnded`: send a first body, then a second body whose first message differs (a client edit at or before the change position). Assert `IsActive` is false after the second request, then run a third request that reports the same `ChangeID` and assert it is priced (present in `res.Ops` with `Saved` non-zero).

- [ ] **Step 4: Implement**

```go
// proxy/cfsavings.go
package proxy

import cf "github.com/rossoctl/context-guru/counterfactual"

// savingsTracker runs the counterfactual calls around one request. It holds the one Trackers
// of the proxy: a thread's state lives there between requests.
type savingsTracker struct{ trackers *cf.Trackers }

func newSavingsTracker() *savingsTracker { return &savingsTracker{trackers: cf.NewTrackers()} }

// begin opens the request record with the client body, before any component ran.
func (s *savingsTracker) begin(meta cf.RequestMeta, client []byte) *cf.Request {
	return s.trackers.BeginRequest(meta, cfBodies(client), cfBreakpoints(client))
}

// end records the operations, then the thread and the forwarded body. A rewrite whose ChangeID
// the thread still applies is not added: the library applies it again by itself (§4.6). One that
// ended since is added again, which is how a change after a compaction is priced.
func (s *savingsTracker) end(req *cf.Request, th cf.Thread, forwarded []byte, ops []cf.Operation, facts cf.EntryFacts) {
	for _, op := range ops {
		if op.Kind == cf.KindRewrite && op.ChangeID != "" && !op.DryRun &&
			s.trackers.IsActive(req.Meta.Session, th, op.ChangeID) {
			continue
		}
		req.Add(op)
	}
	req.EndRequest(th, cfBodies(forwarded), cfBreakpoints(forwarded), facts)
}

// compute prices the request. It stores the new ThreadState (so a shadow run still advances it).
func (s *savingsTracker) compute(req *cf.Request, u Usage) cf.Result {
	return req.Compute(cf.Usage{Input: int(u.FreshInput), CacheRead: int(u.CacheRead),
		CacheWrite: int(u.CacheWrite), CacheWrite1h: int(u.CacheWrite1h)})
}
```

- [ ] **Step 5: Run and commit**

Run: `go test -race ./proxy/ -run 'TestCf|TestSavings' && gofmt -l proxy && go vet ./proxy/...`
Expected: PASS.

```bash
git add proxy/cfsavings.go proxy/cfsavings_body.go proxy/cfsavings_test.go proxy/cfsavings_body_test.go
git commit -s -m "feat(proxy): Add the savings tracker around counterfactual.Compute

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 7: Wire the tracker into the request path (no output used yet)

**Files:**
- Modify: `proxy/proxy.go` (field on `Handler` near line 231 and 366; the chat path near lines 1203 to 1340)
- Modify: `proxy/dashcapture.go` (struct `capture` near line 34; `finish` near line 438)
- Modify: `cmd/context-guru-proxy/main.go` (flag)
- Test: `proxy/cfwire_test.go`

**Interfaces:**
- Consumes: `savingsTracker` (Task 6), `apply.Trace.Ops`.
- Produces:
  - `Options.Savings string`: `"legacy"` (default) | `"shadow"` | `"counterfactual"`.
  - `capture.cfReq *cf.Request`, `capture.cfResult *cf.Result` set in `finish`.
  - `Event.cf *cf.Result` (unexported, set via `Event.SetCounterfactual(r cf.Result)`), consumed by Task 8.

- [ ] **Step 1: Write the failing test**

Use the existing proxy test harness (look at how `proxy/dashcapture_test.go` or `proxy/proxy_test.go` builds a `Handler` with a fake upstream, and reuse it). The test sends two chat requests on one session with `Savings: "shadow"` and asserts the recorded events carry a counterfactual result:

```go
func TestShadowMode_AttachesCounterfactualResultAndKeepsLegacyFigure(t *testing.T) {
	h, rec := newTestProxyWithDashboard(t, Options{Savings: "shadow"}) // existing helper; adapt the name
	post(t, h, chatBody("hello"))
	post(t, h, chatBody("hello", "again"))
	evs := rec.Events(t)
	if len(evs) != 2 {
		t.Fatalf("events = %d", len(evs))
	}
	for _, e := range evs {
		if !e.HasCounterfactual() {
			t.Fatal("shadow mode must run Compute on every request")
		}
	}
	// legacy figure unchanged: the event's own Price path ran as before
	if evs[1].BaselineCostUSD < evs[1].CostUSD {
		t.Fatalf("legacy baseline broke: %+v", evs[1])
	}
}
```

Add `func (e *Event) HasCounterfactual() bool` in `dash`.

- [ ] **Step 2: Run, see it fail**

Run: `go test ./proxy/ -run TestShadowMode -v` → FAIL, `Options.Savings undefined`.

- [ ] **Step 3: Implement**

1. `Options.Savings string`; the constructor rejects any value other than `""`, `legacy`, `shadow`, `counterfactual`. `""` means `legacy`.
2. `Handler.savings *savingsTracker`, created in the constructor only when `Savings != "legacy"`.
3. In the chat path, right where the inbound body is final and before `apply.BodyOpts` runs, call `cp.cfBegin(h.savings, meta, body)`. `meta.Billing` is `cf.BillingFrom(cf.BillingAPI)`. The proxy has no subscription detection today, so every request is `api`. Add a one-line comment saying so.
4. After `apply.BodyOpts`, call `cp.cfEnd(th, forwardedBody, tr.Ops, facts)`. `th` is `cf.Thread{ID: tr.Thread.ID, Parent: <tr.Thread.InheritsFrom()>}`. Use `"main"` when `tr.Thread.ID` is `thread.Primary`. `facts.TTL` comes from the forwarded body's first breakpoint at or after the boundary: 1h if any breakpoint is `1h`, else 5m, `Lifetime: cf.LifetimeUnknown` for now. Keep-alive ping facts are added in Task 9.
5. In `capture.finish`, before `e.Price(...)`, if `c.cfReq != nil && usageOK`, call `res := c.savings.compute(c.cfReq, usage)` and `e.SetCounterfactual(res)`. If `!usageOK`, do not call `Compute`. The request leaves `ThreadState` untouched, and the row stays unpriced.
6. For a fork (`Inherits`), copy the parent's state before `EndRequest`. Add `(*cf.Trackers).Fork(session string, parent, child Thread)` to the library in this task (a `snapshot` of the parent installed as the child's initial state when the child has none). Test it in `counterfactual/host_test.go`.

- [ ] **Step 4: Run and commit**

Run: `go test -race ./proxy/... ./dash/... ./counterfactual/... && gofmt -l proxy dash cmd counterfactual && go vet ./proxy/... ./dash/...`
Expected: PASS; every pre-existing test still passes in `legacy` mode.

```bash
git add -A proxy dash cmd counterfactual
git commit -s -m "feat(proxy): Run counterfactual.Compute beside the legacy figure with --savings

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 8: Price the result into dollars (`Event.PriceCF`)

**Files:**
- Create: `dash/cfprice.go`
- Test: `dash/cfprice_test.go`

**Interfaces:**
- Consumes: `cf.Result`, `cf.PricedOp`, `cf.CacheEffect`, `cf.TierTokens`, `modelinfo.Price`.
- Produces:
  - `func tierUSD(p modelinfo.Price, t cf.TierTokens) float64`
  - `type CFFigure struct { ByComponent map[string]float64; DeltaUSD float64; KeepAliveSavedUSD float64; UnattributedBreakUSD float64; Unpriced []string }`
  - `func (e *Event) CounterfactualFigure(p modelinfo.Price) CFFigure`

Tier rates: input `p.Input`; read `p.CacheRead`; 5m write `p.CacheWrite`; 1h write `2*p.Input`, the same premium rule as `Price.CostWithCacheWrite1h`.

- [ ] **Step 1: Write the failing tests**

```go
// dash/cfprice_test.go
package dash

import (
	"math"
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

var testPrice = modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestTierUSD(t *testing.T) {
	got := tierUSD(testPrice, cf.TierTokens{Input: 1000, CacheRead: 1000, CacheWrite5m: 1000, CacheWrite1h: 1000})
	want := 1000*3e-6 + 1000*0.3e-6 + 1000*3.75e-6 + 1000*6e-6
	if !near(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestFigure_NetOfBreakAttributedByChangeID(t *testing.T) {
	e := &Event{}
	e.SetCounterfactual(cf.Result{
		Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1",
			Saved: cf.TierTokens{CacheWrite5m: 1000}}},
		CacheEffects: []cf.CacheEffect{{Kind: cf.EffectBreak, ChangeID: "c1",
			Caused: cf.TierTokens{CacheWrite5m: 200}, Saved: cf.TierTokens{CacheRead: 200}}},
	})
	f := e.CounterfactualFigure(testPrice)
	want := 1000*3.75e-6 - (200*3.75e-6 - 200*0.3e-6)
	if !near(f.ByComponent["mask"], want) || !near(f.DeltaUSD, want) {
		t.Fatalf("component = %v delta = %v, want %v", f.ByComponent["mask"], f.DeltaUSD, want)
	}
}

func TestPriceCF_DryRunNeverCounted(t *testing.T) {
	e := &Event{}
	e.SetCounterfactual(cf.Result{Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1",
		DryRun: true, Saved: cf.TierTokens{Input: 5000}}}})
	f := e.CounterfactualFigure(testPrice)
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("a dry run must not reach a total: %+v", f)
	}
}

func TestFigure_UnattributedBreakStaysOutOfComponents(t *testing.T) {
	e := &Event{}
	e.SetCounterfactual(cf.Result{CacheEffects: []cf.CacheEffect{{Kind: cf.EffectBreak,
		Caused: cf.TierTokens{CacheWrite5m: 100}}}})
	f := e.CounterfactualFigure(testPrice)
	if f.UnattributedBreakUSD <= 0 || len(f.ByComponent) != 0 || f.DeltaUSD != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_UnpricedOperationIsListedNotZeroed(t *testing.T) {
	e := &Event{}
	e.SetCounterfactual(cf.Result{Unpriced: []cf.Unpriced{{OpID: "x", Reason: "no_usage"}}})
	if f := e.CounterfactualFigure(testPrice); len(f.Unpriced) != 1 {
		t.Fatalf("%+v", f)
	}
}
```

- [ ] **Step 2: Run, see it fail**

Run: `go test ./dash/ -run 'TestTierUSD|TestFigure|TestPriceCF' -v` → FAIL, undefined.

- [ ] **Step 3: Implement**

```go
// dash/cfprice.go
package dash

import (
	"strings"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// SetCounterfactual attaches the library's result for this request. Unexported field: it is
// never stored, only priced (no schema change).
func (e *Event) SetCounterfactual(r cf.Result) { e.cfRes = &r }

// HasCounterfactual reports whether Compute ran for this request.
func (e *Event) HasCounterfactual() bool { return e.cfRes != nil }

// tierUSD prices tokens by tier. A 1h write is 2x the input rate, the rule of
// Price.CostWithCacheWrite1h.
func tierUSD(p modelinfo.Price, t cf.TierTokens) float64 {
	return float64(t.Input)*p.Input + float64(t.CacheRead)*p.CacheRead +
		float64(t.CacheWrite5m)*p.CacheWrite + float64(t.CacheWrite1h)*2*p.Input
}

// CFFigure is the request's savings in dollars, from the library's tokens.
type CFFigure struct {
	ByComponent          map[string]float64 // net of the break each component caused
	DeltaUSD             float64            // sum of ByComponent: the baseline delta
	KeepAliveSavedUSD    float64            // rescue effects
	UnattributedBreakUSD float64            // client edits, expired caches: shown, never charged to a component
	Unpriced             []string           // "opID:reason"; an unpriced operation is not a zero saving
}

// CounterfactualFigure prices Result. A DryRun operation is skipped: computed, never counted.
func (e *Event) CounterfactualFigure(p modelinfo.Price) CFFigure {
	f := CFFigure{ByComponent: map[string]float64{}}
	if e.cfRes == nil {
		return f
	}
	owner := map[string]string{} // ChangeID -> component
	for _, o := range e.cfRes.Ops {
		if o.DryRun || o.Call != nil {
			continue
		}
		c := strings.TrimPrefix(o.Operator, "context-guru.")
		owner[o.ChangeID] = c
		net := tierUSD(p, o.Saved) - tierUSD(p, o.Caused)
		f.ByComponent[c] += net
		f.DeltaUSD += net
	}
	for _, ce := range e.cfRes.CacheEffects {
		cost := tierUSD(p, ce.Caused) - tierUSD(p, ce.Saved)
		switch ce.Kind {
		case cf.EffectBreak:
			if c, ok := owner[ce.ChangeID]; ok {
				f.ByComponent[c] -= cost
				f.DeltaUSD -= cost
			} else {
				f.UnattributedBreakUSD += cost
			}
		case cf.EffectRescue:
			f.KeepAliveSavedUSD += tierUSD(p, ce.Saved) - tierUSD(p, ce.Caused)
		}
	}
	for _, u := range e.cfRes.Unpriced {
		f.Unpriced = append(f.Unpriced, u.OpID+":"+u.Reason)
	}
	return f
}
```

Add `cfRes *cf.Result` to `Event` in `dash/event.go`. Confirm it is unexported so `encoding/json` and the SQLite writer ignore it.

- [ ] **Step 4: Run and commit**

Run: `go test ./dash/... && gofmt -l dash && go vet ./dash/...`
Expected: PASS.

```bash
git add dash
git commit -s -m "feat(dash): Price counterfactual tokens into per-component dollars

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 9: Keep-alive pings and detached calls

**Files:**
- Modify: `components/component.go` (`KeepAliveReport.Window time.Time`)
- Modify: `keepalive/keeper.go` and `proxy/keepalive.go` (set `Window`)
- Modify: `cmd/context-guru-proxy/main.go` (`dashEmitter`)
- Modify: `proxy/proxy.go` (pass ping facts into `EntryFacts`)
- Create: `proxy/cfping.go`
- Test: `proxy/cfping_test.go`

**Interfaces:**
- Consumes: `cf.PingOps`, `cf.ComputeStandalone`, `components.KeepAliveReport`.
- Produces:
  - `KeepAliveReport.Window time.Time` — when the idle gap that the ping served began
  - `type pingFacts struct` store: `func (s *pingStore) put(key string, f cf.PingFacts)`, `func (s *pingStore) take(key string) (cf.PingFacts, bool)`; key is `cf.ThreadKey(session, thread)`

- [ ] **Step 1: Find where the window starts**

Run: `grep -n "KeepAliveReport{" keepalive/*.go proxy/*.go | grep -v _test` and read the lines around each hit.
Expected: the place where the report is built. The window start is the time of the last real request on the entry. The keeper already tracks it as the entry's last-activity time. Use that field. If there is no such field, use `TS - interval` of the first ping after the request and say so in a comment.

- [ ] **Step 2: Write the failing test**

```go
// proxy/cfping_test.go
package proxy

import (
	"testing"
	"time"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/components"
)

func TestPingFromReport_MapsAndPrices(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	r := components.KeepAliveReport{Session: "s", Thread: "", Model: "m", Route: "h",
		CacheRead: 5000, FreshInput: 10, Output: 1, TS: at.UnixMilli(), Window: at.Add(-4 * time.Minute)}
	p := pingFromReport(r)
	if p.Usage.CacheRead != 5000 || p.Usage.Output != 1 || p.Host != "h" || p.Session != "s" {
		t.Fatalf("%+v", p)
	}
	ops, facts := cf.PingOps(p, cf.BillingFrom(cf.BillingAPI))
	res := cf.ComputeStandalone(ops)
	if len(res.Ops) != 1 || res.Ops[0].Call == nil || facts.PingWindow == "" {
		t.Fatalf("res = %+v facts = %+v", res, facts)
	}
}

func TestPingStore_TakeClears(t *testing.T) {
	s := newPingStore()
	s.put("k", cf.PingFacts{PingWindow: "w"})
	if _, ok := s.take("k"); !ok {
		t.Fatal("missing")
	}
	if _, ok := s.take("k"); ok {
		t.Fatal("facts must be cleared after the next real request takes them")
	}
}
```

- [ ] **Step 3: Implement**

`proxy/cfping.go` holds `pingFromReport(components.KeepAliveReport) cf.Ping` (field map from cortex-wiring W4.6 table) and `pingStore` (mutex + map). The ping's `Usage` is `cf.CallUsage{Usage: cf.Usage{Input: int(r.FreshInput), CacheRead: int(r.CacheRead), CacheWrite: int(r.CacheWrite), CacheWrite1h: int(r.CacheWrite1h)}, Output: int(r.Output)}`.

In `dashEmitter.KeepAlivePing`, after the existing `d.rec.Record(ev)`, call the shared `pingStore.put(cf.ThreadKey(r.Session, cf.Thread{ID: normalize(r.Thread)}), facts)`. In Task 7's `cfEnd`, call `take` and fill `EntryFacts.LastPing`, `PingWindow`, and `PingRead` (the extent the ping read; use `nil` and leave a comment that the library then falls back to the usage). The existing ping `dash.Event` row stays, so nothing the dashboard reads for pings changes.

A ping does not call `Compute`. It calls `ComputeStandalone` and attaches the spend to nothing in this task. Log it at debug level. Prices for pings stay in the legacy columns until Task 11.

- [ ] **Step 4: Run and commit**

Run: `go test -race ./proxy/... ./keepalive/... ./components/... ./cmd/... && gofmt -l proxy keepalive components cmd && go vet ./proxy/... ./keepalive/...`
Expected: PASS.

```bash
git add -A components keepalive proxy cmd
git commit -s -m "feat(proxy): Convert keep-alive pings into spend operations and pass their facts on

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 10: Shadow comparison and the agreement test

**Files:**
- Create: `proxy/cfshadow.go`
- Create: `dash/cfagree_test.go`
- Test: `proxy/cfshadow_test.go`

**Interfaces:**
- Consumes: `Event.CounterfactualFigure`, legacy `Event.Price`, `opscases.Differences()`, `opscases.All()`, existing `Play` in `dash/counterfactual_play_test.go`.
- Produces:
  - `func shadowDiff(e *dash.Event, p modelinfo.Price) (legacy, cfUSD float64)`
  - a structured log line `slog` key `cf_shadow` with `session`, `thread`, `legacy_usd`, `cf_usd`, `delta_usd`, `components`
  - `TestProxyPathMatchesOpscases` — plays each `opscases.Case` through `savingsTracker` + `CounterfactualFigure` and compares with `Expected`

- [ ] **Step 1: Write the agreement test**

```go
// dash/cfagree_test.go
package dash_test

import (
	"testing"

	"github.com/rossoctl/context-guru/counterfactual/opscases"
)

// Every case must price the same through the proxy's path as through the library directly.
// A mismatch that has a `differs` entry in differences.json is allowed; any other fails.
func TestProxyPathMatchesOpscases(t *testing.T) {
	diffs := opscases.Differences()
	for _, c := range opscases.All() {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			got := playThroughProxyPath(t, c) // new helper, below
			want := expectedMicros(c)         // from opscases.Case.StepPicos
			for i := range want {
				if got[i] == want[i] {
					continue
				}
				if d, ok := diffs[c.ID]; ok && d.Allows(i, "") {
					continue
				}
				t.Errorf("step %d: proxy path %d µ$, expected %d µ$", i, got[i], want[i])
			}
		})
	}
}
```

`playThroughProxyPath` mirrors `Play` in `counterfactual_play_test.go`, but drives `Trackers.BeginRequest/Add/EndRequest/Compute` and `Event.CounterfactualFigure`. Because `savingsTracker` lives in `package proxy`, and `dash` must not import `proxy`, the helper uses `counterfactual.Trackers` directly, with the same replay filter (`IsActive`). Keep that filter in one tiny exported function in the library (`counterfactual.Request.AddUnlessActive(op)`) and have `savingsTracker.end` call it too, so the test and the proxy share the code. Add `AddUnlessActive` to `counterfactual/host.go` with a unit test.

Read `opscases.Difference` (in `opscases.go`) for the real name of the allow-check method. Use that method name, not `Allows`, if it differs.

- [ ] **Step 2: Run it**

Run: `go test ./dash/ -run TestProxyPathMatchesOpscases -v 2>&1 | tail -40`
Expected: either PASS, or FAIL listing steps with no `differs` entry. Each failure is a real gap. Triage each one:
- a gap in the library's "Not implemented" list → add a `notCovered` note in the case, do not "fix" the figure;
- a bug in the proxy path → fix it here.

- [ ] **Step 3: Write the shadow log and its test**

```go
func TestShadowDiff_LogsBothFigures(t *testing.T) {
	// build an Event with legacy Components set and a Result attached
	// call shadowDiff; assert legacy and cf come back and delta = cf - legacy
}
```

Fill the body from the first passing case in `opscases.All()`, so the numbers are real. `shadowDiff` calls `e.Price(p, true)` first (legacy), then `CounterfactualFigure(p)`, and returns `legacy = e.BaselineCostUSD - e.CostUSD`, `cfUSD = fig.DeltaUSD`. Log with `slog.Info("cf_shadow", ...)`. Call it from `capture.finish` only when `Options.Savings == "shadow"`.

- [ ] **Step 4: Run and commit**

Run: `go test ./dash/... ./proxy/... ./counterfactual/... && gofmt -l dash proxy counterfactual && go vet ./dash/... ./proxy/...`
Expected: PASS.

```bash
git add -A dash proxy counterfactual
git commit -s -m "test(counterfactual): Compare the proxy's path with the opscases and log the shadow difference

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 11: Switch, then delete

**Files:**
- Modify: `dash/event.go` (`Price` near line 795; delete rules)
- Modify: `proxy/dashcapture.go`
- Modify: `dash/capture.go` (the `MarkUnique` call site)
- Test: `dash/event_cf_switch_test.go`, then the existing `dash` and `proxy` suites

**Interfaces:**
- Consumes: `CFFigure` (Task 8), `Options.Savings`.
- Produces: with `Savings == "counterfactual"`, `Price` writes `CompRow.SavedUSD`, `BaselineCostUSD = CostUSD + fig.DeltaUSD`, and `KeepAliveSavedUSD = fig.KeepAliveSavedUSD` from the figure. `CachesplitSavedUSD` stays on the legacy rule.

This task has two parts. Land the switch first, merge it, and watch real sessions. Delete only in a later commit.

- [ ] **Step 1: Write the failing switch test**

```go
func TestPrice_CounterfactualModeUsesTheLibraryFigure(t *testing.T) {
	e := &Event{Model: "m", FreshInput: 100, CacheRead: 1000, Components: []CompRow{
		{Component: "mask", Acted: true, SavedGross: 9999, SavedUnique: 9999}, // legacy would price this
	}}
	e.SetCounterfactual(cf.Result{Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "c",
		Saved: cf.TierTokens{CacheRead: 1000}}}})
	e.PriceMode(testPrice, true, SavingsCounterfactual)
	want := 1000 * 0.3e-6
	if !near(e.Components[0].SavedUSD, want) || !near(e.BaselineCostUSD-e.CostUSD, want) {
		t.Fatalf("component = %v baseline delta = %v, want %v",
			e.Components[0].SavedUSD, e.BaselineCostUSD-e.CostUSD, want)
	}
}
```

- [ ] **Step 2: Run, see it fail, implement `PriceMode`**

Add `type SavingsMode int` with `SavingsLegacy`, `SavingsShadow`, `SavingsCounterfactual`. `PriceMode(p, complete, mode)` calls the existing `Price`; when `mode == SavingsCounterfactual && e.cfRes != nil`, it then overwrites `Components[i].SavedUSD`, `BaselineCostUSD` and `KeepAliveSavedUSD` from `CounterfactualFigure(p)`. When `e.cfRes == nil` (no usage, or a path that never opened a request), it keeps the legacy figures and sets nothing new. Change the one call site in `capture.finish` to `PriceMode`. Make `Price` call `PriceMode(..., SavingsLegacy)` so every other caller is unchanged.

- [ ] **Step 3: Run the whole suite**

Run: `go test ./... 2>&1 | tail -30 && GOWORK=off go vet ./...`
Expected: PASS. Fix nothing by editing expected figures unless a `differs` entry in `differences.json` already explains it (cite the entry in the commit message).

- [ ] **Step 4: Commit the switch and flip the default**

Change the `--savings` default to `shadow` first. After a soak on real sessions, change it to `counterfactual` in a separate commit.

```bash
git add -A dash proxy cmd
git commit -s -m "feat(dash): Price savings from counterfactual with --savings=counterfactual

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 5: Delete the replaced rules (separate commit, after the soak)**

Delete, and fix every compile error, in this order:
1. `dash/event.go`: `uniqueRate`, `repeatRate`, `ComponentSavedUSD`, the reduction part of `baselineDeltaUSD`, `keepaliveSavedUSD`.
2. The `MarkUnique` split in `dash/capture.go` and the `unique` map in `proxy/dashcapture.go` (`unique: map[string]int{}`), plus `Event.FromTrace`'s use of it.
3. Dead callers: `dash/readvalue.go` (`DecomposeComponentSavedUSD`, `EstimateComponentSavedUSD`), `dash/summarizeattr.go`, `dash/replaycap.go`, `dash/sweepattr.go`. These reprice **stored** rows with `ComponentSavedUSD`. Rows written before the switch still need that rule. Therefore do not delete these four until a migration has rewritten the stored rows, or the retention window has passed. If neither is true, leave them and the helpers they call, and record why in the commit message.

Do not delete: `cachesplitSavedUSD` (no `split` effect in the library), `TailChanged`, or anything in design §9.1.

- [ ] **Step 6: Final checks**

Run, from each module you touched:

```bash
go build ./... && go test ./... && gofmt -l . && go vet ./...
for m in . counterfactual; do (cd $m && GOWORK=off go mod tidy -diff); done
```

Expected: all pass, empty `gofmt` and `tidy` output.

```bash
git add -A
git commit -s -m "refactor(dash): Remove the savings rules that counterfactual replaced

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage.**
- Spec §4.1 engine Operations → Tasks 3, 4, 5. Deviation: `ChangeID` comes from a content hash made in the pipeline, so no offloader changes. The spec expected per-offloader work. Check that the hash is stable across a component's replays on real traffic during the shadow run.
- Spec §4.2 Tracker → Tasks 6, 7. `Fork` for `Inherits` → Task 7 step 6.
- Spec §4.2 keep-alive, `OpsEmitter` → Task 9. `OpsEmitter` for detached calls is **not** built: the engine's detached summarizer spend has no emitter hook yet. Add it in a follow-up once Task 9 shows how pings reach the store. It does not block the switch.
- Spec §4.2 `MaxCachedIdx` vs B\* counter → **not covered**. Add as Task 7 step 7 if the shadow run shows disagreement: compare `Trace.MaxCachedIdx` with `Result.Boundary` in `cfEnd`/`finish` and count mismatches in `/stats`.
- Spec §4.3 pricing → Task 8. Spec §3 and §5 deletion → Task 11.
- Library additions the spec assumed but the code lacks (`BillingFrom`, ping conversion, `IsActive`, `Fork`, `AddUnlessActive`) → Tasks 1, 7, 10.

**Placeholder scan.** Three steps say "read the file, then use its real name": Task 3 test helpers, Task 4 the `applied` boolean, Task 10 the allow-check method. Each names the file and the question. The first two exist because I did not read those lines; the third because `Difference` was not printed. None hides logic.

**Type consistency.** `Change` / `Splice` (Task 3 → 5), `Trace.Ops` (4 → 7), `savingsTracker.begin/end/compute` (6 → 7, 10), `Event.SetCounterfactual` / `CounterfactualFigure` (8 → 10, 11), `PingOps` / `PingFacts` (1 → 9) agree across tasks.

**Open decisions this plan makes without asking.**
1. `Billing` is `api` for every request (no subscription detection exists in the proxy).
2. `EntryFacts.Lifetime` is `unknown` until the keeper reports a proven TTL, so the phase is often `unknown` and carries `penalty_unknown`.
3. Task 11 leaves the four stored-row repricers alone until stored rows are migrated.
