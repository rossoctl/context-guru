# Context operations: wiring in Cortex and the context-guru proxy

**Status:** proposed. Split out of the context operations design after review of
rossoctl/context-guru#592.
**Companion:** `2026-10-18-context-operations-design.md` in `rossoctl/context-guru` defines the
method (the operation model, the counterfactual, the tiers). This document says how Cortex and the
context-guru proxy connect to it. Text below is moved from that design without change, except
where it says otherwise. Section numbers are the numbers it had there, so the references in it
point at the method document.

**Problem statement:** [rossoctl/cortex#1417](https://github.com/rossoctl/cortex/issues/1417).

**Open from review:**
- The method document now says components push facts with an identity, and the receiver keeps no
  state. The collector, fingerprint and `Compare` text below describes the earlier design (a
  collector that keeps one fingerprint for each thread). It must be rewritten to the push model.
- A `ping` is now an ordinary `spend` operation. The `ping` text below predates that.
- Do `pctx.ReportOps` and the Cortex name for the component that writes to the cost ledger need a
  name? None is chosen.

### 0.1 The questions in cortex#1417

[cortex#1417](https://github.com/rossoctl/cortex/issues/1417) lists the open questions from the
Cortex side. This spec answers them as follows.

| # | Question in the issue | Answer in this spec |
|---|---|---|
| 1 | One set of numbers: spend and savings priced from different rate tables | Inside one host, spend and savings use one resolver (D3, D4, §4.6). The tier logic is in one library. The rates stay host-local by design (§1). |
| 2 | A figure deployers can trust: how it was computed, and who stands behind it | Each recorded figure names its operator, its component, `Estimated` and `Projected` (§7.1), and the `ops` version that computed it (R46). The rules are reviewed in context-guru through the case list and `differences.json` (§11.4). |
| 3a | Cache effects: a saving priced at the tier the rewritten request landed in | Saved tokens are priced at the tier they would have had without the change: B\* and the breakpoints (§5.2, R36, R37). A break that a change caused is a separate `cacheChange` cost, linked to that change (R23). |
| 3b | The plugin's own LLM calls through `core/llmclient`, not in the ledger; the `ibac` judge too | `spend` operations with the call's billed usage (D4, §4.6). This applies to every plugin that calls a model, not only context-guru (R47). |
| 3c | Keep-alive: the cost and the benefit land on different requests | `ping` operations, and the rescue rule (§4.6, §6.2). |
| 4 | Content re-sent every turn: overcount for compaction, correct for tool-prune | `ChangeID` (R22, D1); the tier comes from the position, so tool-prune is priced on every turn and a compaction is not counted again at the full rate (§4.7). |
| 5 | Two ways of counting tokens | `Size.Tokenizer`, `bytes-estimate` and `Estimated` (R8); the correction factor is open (§12, Q4). |
| 6 | Credit across several plugins | Each operator is measured against its own input (R29), and R25 for the same content. |
| 7 | Observe mode | `Projected` (R12), never counted in totals. |
| — | Where the baseline lives, and how changes to it are reviewed | In the `ops` library in context-guru (D2). A change to it changes the case list, and every intended difference is in `differences.json` (§11.2, §11.4). |

### 4.6 How operations reach the host

There are three kinds of operator, and each reaches the host through a path that already exists.
The design adds no new reporting interface for the engine or the keeper.

| Operator | When it runs | How its operations reach the host |
|---|---|---|
| **The context-guru engine** (every component, including the ones whose bytes `apply` writes, §4.2 rule 9) | inside a request | **`apply.Result.Ops`**: the engine returns its operations with the rewritten body, in both hosts. In Cortex the context-guru plugin passes them to `pctx.ReportOps(result.Ops...)`. In the proxy there is no other operator, so the proxy reads `result.Ops` directly. |
| **Another Cortex plugin** (tool-prune, cpex) | inside a request | `pctx.ReportOps(ops...)`, during its own pass. Cortex only. |
| **The keeper**, and a **detached** engine model call | between requests, or after the response | the host's `components.Emitter`, through its optional capabilities: `KeepAliveEmitter.KeepAlivePing(KeepAliveReport)` for a ping (exists today), and `OpsEmitter.Ops(RequestOps)` for a detached call (new, same pattern). |

**Why not a new `Sink`.** An earlier draft added a `Sink` interface for pings. The keeper already
has a host-agnostic report path: `components.KeepAliveEmitter`, added so the keeper could be
exported from the proxy without a direct call into `dash` (issue #382, PR #591). The proxy wires
it to its `dashEmitter`; Cortex wires it to `pingEmitter` (`core/plugins/contextguru/spend.go`). A
second interface would make the keeper report each ping twice, or retire `KeepAliveReport` and lose
the fields the dashboard groups by (`Strategy`, `Preset`, `StopReason`, `MaxTokens`,
`FreshInput`). So the keeper keeps reporting `KeepAliveReport`, and **each host converts it** to a
`RequestOps` with one `ping` operation:

| `KeepAliveReport` | `ping` operation |
|---|---|
| `Session`, `Thread` | `RequestOps.Session`, `RequestOps.Thread`, and the same on the operation |
| `Window` (new, additive: the start of the idle gap the ping served) | `ChangeID = "ka:" + Thread + ":" + Window` |
| `Model`, `Provider`, `Route`, `Status`, `DurationMs` | `Call.Model`, `Call.Host`, `Call.Status`, `Call.Duration` |
| `CacheRead`, `CacheWrite`, `CacheWrite1h`, `FreshInput`, `Output` | `Call.Usage` (its prompt fields and `Output`) |
| `TS` | `RequestOps.At` |
| `Strategy`, `Preset`, `StopReason`, `MaxTokens`, `Agent` | not on the operation; the dashboard keeps reading them from `KeepAliveReport` (§10.1) |
| `CostUSD` | not used: the host prices `Call.Usage` with its own rates, like every other call |

The conversion is a pure function in `ops` (`ops.FromKeepAlive(KeepAliveReport, Billing)`), so both
hosts build the same operation. The host supplies `Billing`, because only it knows which credential
the ping used.

```go
// OpsEmitter is an optional Emitter capability, the same pattern as KeepAliveEmitter: the engine
// type-asserts for it and does nothing if it is absent. It carries a RequestOps that has no
// request to travel with: today, only a detached model call.
type OpsEmitter interface {
    Ops(RequestOps)
}
```

The contract of both capabilities is the contract `CostRecorder` had: safe for concurrent use; no
blocking I/O; never makes the session active (a ping is not agent activity); may drop a report for
a session that no longer exists.

**Who builds a standalone `RequestOps`.** No collector is open, so the caller fills it from what it
knows about the thread it acts on: `Session` and `Thread`. It sets `Billing` from the credential its
call used (R41, §7.4).

**Where the usage comes from.** The host gives the engine its model client (`components.Model`;
in Cortex, `cgModel`). The client reads the usage from each response, and fills `ModelCall`. The
engine's own usage accounting is not used, because under Cortex it is zero (2026-10-14 spec, §0).

**A detached call.** A summarizer call can finish after its request was answered. Its `spend`
operation then cannot travel in that request's `apply.Result`. The engine sends it through
`OpsEmitter`, as a standalone `RequestOps` with `Detached = true` on the operation, and
`Session`/`Thread` from the request that caused the call. Its `Produced` names the change it made,
so the cost is still joined to that change.

**A prefix ask.** `extract_llm_sweep` asks the request's own model a question, as a separate
upstream call whose body is the previous turn's forwarded bytes plus one trailing user message
(`PrefixAsker`). That is an ordinary `spend` operation: its `Call.Usage` shows the cache read it
was designed for (measured 19,595 read, 0 written). When the sweep falls back to a call without the
cached prefix, the same operation shows fresh input instead. No new kind is needed; the test cases
cover both (§11.3).

**How the host records it.** The host handles operations from `apply.Result`, from `ReportOps`,
and from either Emitter capability the same way. It prices `Saved`, `Caused` and `Call.Usage` with
one resolver. So spend has the same rates, provenance, currency and billing basis (§7.4) as the
savings.

**What it replaces.** `core/cost/spend` (`Charge`, `CostRecorder`, `RecorderConsumer`) is removed.
Today both Cortex wirings inject `spend.LogRecorder{}`, which only logs, and the recorder that would
write the ledger was never built. So no recorded figure changes. Cortex's `pingEmitter` keeps
implementing `KeepAliveEmitter`, and calls `ops.FromKeepAlive` instead of building a `spend.Charge`.

### 4.8 Sequence: three flows, and which repo each step is in

`participant` labels mark which repo owns each piece: Cortex (the pipeline, the collector,
`settle`) or context-guru (a plugin, the keeper, `ops`). `ops` is a library import in both; it
runs in-process in whichever repo calls it, never as its own service.

**A. A normal request.** A plugin reduces a tool output; the collector finds no break; `settle`
prices and records.

```mermaid
sequenceDiagram
    participant Client
    participant Pipeline as Pipeline collector<br/>(Cortex)
    participant Plugin as context-guru plugin<br/>(context-guru)
    participant Ops as ops.Price / ops.Compare<br/>(context-guru, imported)
    participant Settle as settle<br/>(Cortex)
    participant Store as Session store<br/>(Cortex)
    participant Upstream

    Client->>Pipeline: request
    Pipeline->>Pipeline: open RequestOps<br/>(Session, Agent, Billing from credential)
    Pipeline->>Plugin: OnRequest(pctx)
    Plugin->>Plugin: apply.Apply: resolve thread,<br/>rewrite a tool output (mask/cmdfilter/coref/...)
    Plugin->>Pipeline: pctx.ReportOps(result.Ops...)<br/>+ result.Thread
    Pipeline->>Pipeline: stamp Operator<br/>append into RequestOps.Ops
    Pipeline->>Upstream: forward (smaller) body
    Upstream-->>Pipeline: response + usage
    Pipeline->>Pipeline: prev = fingerprint[thread.Key]<br/>ops.Compare(prev, cur, usage)<br/>-> B*, cacheChange, CacheState
    Pipeline->>Ops: Price(RequestOps.Ops, usage, B*, breakpoints, state)
    Ops-->>Pipeline: Result (PricedOp[], in-process only)
    Pipeline->>Settle: Result
    Settle->>Settle: price Saved/Caused with pricing.Resolver
    Settle->>Settle: discard RequestOps/Operation (R20)
    Settle->>Store: event.Saving{Changes: []ChangeSaving, ...}
    Pipeline-->>Client: response
```

**B. A detached call.** The request has already been answered; its `RequestOps` may already be
closed and recorded. The summarizer call finishes afterward and reports through the host's
`OpsEmitter`.

```mermaid
sequenceDiagram
    participant Pipeline as Pipeline collector<br/>(Cortex)
    participant Store as Session store<br/>(Cortex)
    participant Plugin as context-guru plugin<br/>(context-guru)
    participant Model as Summarizer model call<br/>(context-guru, via cgModel)
    participant Emitter as OpsEmitter<br/>(host: Cortex pingEmitter / proxy dashEmitter)
    participant Settle as settle<br/>(Cortex)

    Note over Pipeline,Store: request N already answered<br/>its RequestOps/event.Saving already recorded
    Plugin->>Model: summarizer call (async)
    Model-->>Plugin: response + billed usage
    Plugin->>Plugin: build standalone RequestOps<br/>Session/Thread from request N (kept)<br/>Billing from the call's credential, one spend op (Detached=true, Call, Produced)
    Plugin->>Emitter: Ops(RequestOps)
    Emitter->>Settle: RequestOps
    Settle->>Settle: price Call.Usage with pricing.Resolver<br/>(same path as a request's Saved/Caused)
    Settle->>Store: event.Saving (joined to the change via Produced)
```

**C. A standalone keep-alive ping.** No request is open at all; the keeper acts between turns.

```mermaid
sequenceDiagram
    participant Keeper as Keeper<br/>(context-guru)
    participant Upstream
    participant Emitter as KeepAliveEmitter<br/>(host: Cortex pingEmitter / proxy dashEmitter)
    participant Settle as settle<br/>(Cortex)
    participant Store as Session store<br/>(Cortex)

    Note over Keeper: idle gap on thread T exceeds the cache TTL
    Keeper->>Upstream: ping (re-sends thread T's last forwarded body)
    Upstream-->>Keeper: response + billed usage
    Keeper->>Emitter: KeepAlivePing(KeepAliveReport)<br/>(existing path, unchanged)
    Emitter->>Emitter: ops.FromKeepAlive(report, Billing)<br/>-> RequestOps with one ping op<br/>(ChangeID = window ID, Billing from the ping's credential)
    Emitter->>Settle: RequestOps
    Settle->>Settle: price Call.Usage
    Note over Settle: on the NEXT request on thread T,<br/>the collector adds a cacheChange (keepAlive)<br/>with the SAME ChangeID -- ping cost and rescue<br/>value join under one change (§4.4, §4.6)
    Settle->>Store: event.Saving (ping leg)
```

### 7.1 From `Result` to `event.Saving`

`settle` calls `ops.Price` once for each request, over the in-process `RequestOps`/`Operation`
list (R20): that list is never recorded. `settle` prices `Saved` and `Caused` with its
`pricing.Resolver`, at the same model and long-context tier as the request itself, then discards
the operations and writes one `event.Saving` for each (operator, component) — the only form that
reaches the session store, `:9094`, and the archive:

| `event.Saving` field | Source |
|---|---|
| `Component` | the operator name, for example `context-guru.mask` |
| `TokensAvoided` | sum of `Saved` over all tiers |
| `USD` | `Saved`, priced |
| `PenaltyUSD` | `Caused`, priced, plus the attributed `cacheChange` operations |
| `Tier` | the tier with the largest `Saved` |
| `Projected` | `Projected` |
| `DiscardedCount` (new, additive) | per (operator, component), the count of operations with `Discarded = true` on this request (R44, §5.1a) — never priced, surfaced for diagnosis the way `Report.Discarded` is today |
| `Estimated` | any operation `Estimated` |
| `OpsVersion` (new, additive) | `Result.Version`, the version of the `ops` rules (R46) |

New fields, all additive (the ledger rule is "add, never rename"):

| Field | Where | Why |
|---|---|---|
| `Saving.Operator` | event and ledger | group `context-guru/*` in the per-plugin breakdown |
| `Saving.Changes []ChangeSaving` | event only | `{changeId, savedUSD, causedUSD}`, for totals and payback for each change. Not in the ledger, which keys by minute. |
| `Event.UnattributedBreakUSD` | event | client edits and expired caches. Shown, never charged to a plugin. |
| `Event.InteractionUSD` | event | the interaction on a rescued request (§6.2). Counted in the session total, never charged to a plugin. |
| `Saving.Thread`, `ChangeSaving.Thread` | event and ledger | sum for each thread; a subagent's saving is separate from the main thread's |

`TotalAvoidedUSD` does not change: `max(0, USD − PenaltyUSD)` for each saving, floored at zero.
Signed totals are out of scope (§10).

**Size.** `Saving.Changes` is already small per request — one `ChangeSaving` per change touched,
not per operation — but a request can still touch many changes (a `coref` batch cut, a wide
`mask`/`cmdfilter` pass). So `Saving.Changes` has a size limit, with aggregation above it:

```go
// Aggregate replaces entries past the limit. One bucket per key, so a thread/session total is
// still exact; only each individual change's own cross-request total is lost for entries folded
// in here.
type Aggregate struct {
    Limit   int
    Dropped int // ChangeSaving entries folded into Entries
    Entries []AggregateEntry
}

type AggregateEntry struct {
    Operator            string
    SavedUSD, CausedUSD float64 // summed
}
```

### 7.2 Totals for each change across requests

A total for one `changeId` is a fold over the session's events: add `Changes[i]` where the
`changeId` matches. The session archive holds each request's event for the whole session, so agentop
can compute the fold when it opens a session. There is no ledger column for each change.

Without an archive (a cluster sidecar), totals for each change cover only the events in memory. The
per-request savings and the ledger totals are correct either way.

**Totals for a session and for a thread.** `Saving.Thread`/`ChangeSaving.Thread` carry the thread
(§7.1), so the same fold over recorded events gives:

| Total | Sum of |
|---|---|
| A change | `ChangeSaving`s with one `changeId` in one thread |
| A thread | every `Saving` of one `Thread`, plus its spend/ping and `Event.InteractionUSD` |
| A session | every thread of the session |
| A subagent | the threads whose `Parent` chain leads to the main thread, one at a time |

Spend (`spend`, `ping`) is shown beside the savings, never inside them, as today.

### 7.3 The context-guru proxy

The proxy computes its dollar savings from `ops.Price`, as Cortex does:

1. The engine returns the request's operations (migration step 3).
2. The proxy's collector calls `ops.Compare` and adds the `cacheChange` operation.
3. The proxy calls `ops.Price`.
4. The proxy converts each `PricedOp` to dollars with `internal/modelinfo.Price` for the request's
   model.
5. The proxy stores the dollars in the columns its dashboard reads today: the saving of each
   component (`ComponentSavedUSD`) and the baseline delta (`baselineDeltaUSD`).

The `cachesplit` and keep-alive savings are `cacheChange` operations now (§4.4), so the proxy
prices them through `ops.Price` too. Their current rules (`cachesplitSavedUSD`,
`keepaliveSavedUSD`) are the source of their test cases (§11), and are removed at step 10.

**Agreement between hosts.** For the same request, `ops.Price` returns the same tokens for each
tier in both hosts. The dollars are equal only when the two rate tables are equal (§1). The test
cases use one fixed rate table, so they can compare dollars (§11.2). A drift between the tables —
a model priced in one and not the other — shows as a dollar difference on identical tokens, which
the cross-host comparison test (§9 step 8) reports.

## 9. Migration

Each step merges alone and leaves the totals correct.

| Step | Repo | Change | Verification |
|---|---|---|---|
| 1 | context-guru | **Make the test-case list** (§11), with the current computation. No `ops` code yet. | The list is reviewed. Each `differs` entry cites a spec section. Each `notCovered` entry is in §12. |
| 2 | context-guru | Create the `ops` module: types, `Price`, `Compare`, checks. No callers. | Every case in the list passes (§11.4). |
| 3 | context-guru | The engine builds `[]ops.Operation` from `Trace` and the reports, and returns them in `apply.Result.Ops` with `apply.Result.Thread`. Marker and apply-only changes are built by `apply` (§4.2 rule 9). **This is new work for each change, not a read of existing fields.** The frozen state is keyed by content hash (`components/offload/state.go`), so `ChangeID` can be derived. But replays are counted only for each component today (`Report.Replays`), and `apply.Trace` records no ID for each change. Each offloader must record, for each change, its `ChangeID`. | Replay tests: the same `ChangeID` on each turn. |
| 4 | context-guru | **The proxy's collector.** It reads `apply.Result.Ops`; keeps one fingerprint per `thread.Key` beside `dash.Recorder`; calls `ops.Compare`; sets `Billing` with `ops.BillingFrom`; implements `OpsEmitter` on `dashEmitter`; and converts each `KeepAliveReport` with `ops.FromKeepAlive`. Nothing reads its output yet. This is the largest proxy step, and it uses only engine types: none of it depends on Cortex. | The `Compare` cases pass through the proxy's collector. The `MaxCachedIdx`/B\* agreement counter is zero on captured sessions (§5.6). |
| 5 | context-guru | The proxy computes dollars from `ops` **in shadow** (§7.3): it stores its current figure, and logs the `ops` figure and the difference. | Each logged difference matches a `differs` entry (§11.4). |
| 6 | context-guru | The proxy **switches** to the `ops` figure. `dash.Recorder`'s `TailChanged` and cold-start attribution read `ops.Compare`'s state instead of their own copy, so the proxy has one cache-state rule, not two. | Dashboard totals before and after on the same captured sessions. |
| 7 | Cortex | **The Cortex collector.** `pctx.ReportOps`; the context-guru plugin passes `apply.Result.Ops` through it; one fingerprint per `thread.Key`; `cgModel` fills `ModelCall`; `pingEmitter` calls `ops.FromKeepAlive` and implements `OpsEmitter`. `plugins.Deps.Costs` and `core/cost/spend` are removed. No pricing change. | The `Compare` cases pass through the Cortex collector. |
| 8 | Cortex | tool-prune and context-guru report operations beside their current events. `settle` computes both figures, writes the current one, logs the difference. | Each logged difference matches a `differs` entry; also the Cortex / cg proxy / control comparison test (`62333ee9` on `feat/contextguru-keepalive`). |
| 9 | Cortex | `settle` switches to `ops.Price`. The new event fields. | Ledger totals before and after on the same replay. |
| 10 | both | Remove the current events, `saving.go`, `unique.go`, the per-operator draft functions, and the proxy's rules that `ops` replaced (`uniqueRate`, `repeatRate`, the reduction part of `baselineDeltaUSD`, `cachesplitSavedUSD`, `keepaliveSavedUSD`). **Do not remove a rule that a `notCovered` entry still uses, nor any path §10.1 keeps** (gates, events, tool inventory, holdout replay, `KeepAliveReport`). | `go mod tidy -diff` in every module. All cases pass in both repos. |
| 11 | Cortex | agentop's per-change view. | TUI tests. |

Expected differences at step 9 (and, for the proxy, at step 6). Each one corrects a known error, so none is a regression:

- The penalty is lower. The current one uses the whole `cache_write` as an upper bound.
- The tier for each change comes from its position, not from a unique fraction for the request.
- The `context_guru_expand` declaration becomes a cost. The current figure does not count it.
- A cache-break penalty starts at the read extent, not at the break (§6.2). With few breakpoints it
  is larger than a count from the break.
- A keep-alive window whose pings rescued nothing is a negative figure for its thread. Today the
  ping cost and the saving are in separate columns.

