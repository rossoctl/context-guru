# Context operations: one object model for the changes plugins make, priced in one place

**Status:** proposed.
**Date:** 2026-10-18.
**Repos:** `rossoctl/context-guru` (the `ops` module) and `rossoctl/cortex` (the collector, `settle`,
the plugins).
**Worked example:** [`2026-10-18-context-operations-example.md`](2026-10-18-context-operations-example.md).
Read it first. It shows one session from body to dollars.

## 0. Base, and the problem

This design starts from the savings code on branch `feat/contextguru-keepalive`, which is not yet
on `main`. That branch adds:

- a `context-guru/event` that the context-guru plugin publishes for each request
  (`core/plugins/contextguru/saving.go`, `unique.go`),
- context-guru's own pricing rules in `core/cost/settle/avoided.go` (`compactionDraft`,
  `compactionUsage`, `repeatUsage`, `rewritePenalty`), beside tool-prune's (`pruneDraft`),
- `event.Saving.PenaltyUSD`,
- the spec `2026-10-14-contextguru-savings-and-spend-design.md`.

Three problems remain with that approach:

1. **Each producer has its own event shape and its own pricing rule.** tool-prune publishes
   `{bytesRemoved, bodyBytesAfter}`. context-guru publishes an aggregate for the whole request.
   Each new producer needs a new rule in `settle`.
2. **The pricing rule exists twice.** The context-guru proxy (`dash.Event.repeatRate`) and Cortex
   (`repeatUsage`) keep copies that must agree, and they are kept in sync by hand.
3. **The facts are too coarse.** One aggregate for each request cannot give the saving of one
   change across requests, or the payback of a change that broke the cache. `PrefixRewritten` is a
   boolean, so the penalty is an upper bound (the whole `cache_write`).

## 1. Goal

- Each producer reports the **operations** it performed on the context, as **facts**.
- **One library**, owned by context-guru, turns operations into **tokens for each billing tier**.
- **Cortex** converts tokens to dollars with its own `pricing.Resolver`, and records them.
- The model supports savings **across requests**: removing K tokens from message *i* saves on
  every later request that still carries the removal.
- context-guru is **one plugin in a longer pipeline**. Other plugins (tool-prune, cpex,
  inference-router) also change the request. The model covers all of them.

## 2. Decisions

| # | Question | Decision |
|---|---|---|
| D1 | Who knows that a change is applied for the first time? | **The producer.** context-guru knows it from its frozen state, which is keyed by content hash and survives agent compaction. The library stays stateless. |
| D2 | Where is the savings logic? | **In a context-guru library.** Cortex calls it and keeps no producer-specific pricing rules. |
| D3 | What does the library return? | **Tokens for each tier**, not dollars. Cortex prices them with its own rates (discounts, overrides, long-context tiers), so a saving and the request's spend use the same rates. |
| D4 | Does the library also price cg's own spend? | **No.** A `spend` operation carries a `SpendID` that links it to the `CostRecorder` record Cortex measured at its transport. |
| D5 | Shape | **One typed operation for each item**, with an aggregate fallback above a limit. |
| D6 | How is a saving across requests counted? | **Realized on each request, not projected.** Each request lists every change in its body, fresh or replayed, and is priced from that request's own cache result. |
| D7 | Where do the types live? | **A standalone module in the context-guru repo,** `github.com/rossoctl/context-guru/ops`, with no dependency on the engine. Cortex's pipeline and every plugin import it without the engine's dependencies. |

## 3. Requirements

The numbers are the order in which the requirements were found, so they are not in sequence.
**R10 is withdrawn.** It asked for a flag that separates a replay from a fresh act. `FirstApplied`
(§4.1) replaces it.

### A. Boundary

- **R1.** Operations are facts. They carry no rates, no dollars and no tier choice.
- **R2.** Operations carry no content: no message text, no clipped before/after strings, no
  credentials. They reach the unauthenticated `:9094` API and the session archive. Content identity
  uses hashes only.
- **R3.** The model and the library are in one module. Both repos import it. A contract test in
  each repo pins it.
- **R26.** The library is a pure function: no state, no I/O. The context-guru proxy and Cortex
  then compute the same figure for the same request.

### B. Coverage

- **R4.** Every operation that changes money is covered, not only reductions: reductions,
  additions, cache breaks, model calls, restores.
- **R5.** Spend that happens outside a request (a keep-alive ping, a detached summarizer call)
  carries its own attribution.
- **R6.** An operation has an ID, so a later operation can refer to it.
- **R27.** The model is not specific to one producer. Each operation names its producer and its
  pipeline stage.
- **R28.** One collector for each request collects the operations of all plugins, in order, and
  calls the library once.
- **R33.** Operations that add tokens are costs: an injected tool declaration, injected text, a
  restore.
- **R34.** A destination change (inference-router) is a separate kind. Its pricing is out of
  scope (§10).

### C. Facts for pricing

- **R7.** Each operation has a position: section and index, in the provider's hash order.
- **R8.** Sizes are in bytes, and in tokens with the tokenizer named. A producer with no tokenizer
  sends a byte estimate and marks it (§4.2 rule 8).
- **R9.** A reduction carries hashes of the content it removed.
- **R11.** Model-call usage is the provider's billed usage, measured by Cortex (D4).
- **R22.** A change has an ID that is the same on every request that carries it (`ChangeID`).
- **R23.** A one-time cost (a cache break) is linked to the change that caused it, so payback can
  be computed.
- **R24.** Cache facts are scoped to the conversation the provider cached, not to the session as a
  whole. Subagents share a session but not a cache entry.
- **R29.** Each producer's change is measured against that producer's own input.
- **R30.** Cache facts come from the forwarded body, not from one plugin's output.
- **R31.** A cache break with a cause outside the plugins (client edit, expiry, first turn) is not
  charged to a plugin.
- **R32.** Positions resolve in the forwarded body. A producer that adds or removes messages
  reports that.

### D. Status and correctness

- **R12.** Each operation has a status: `applied`, `projected`, `reverted`, `discarded`. Only
  `applied` counts in totals.
- **R13.** Each operation has a reversibility class: `lossless`, `reversible` (with a stash key),
  `irreversible`.
- **R14.** Changes are additive. For each section, the applied changes add up to the client body
  minus the forwarded body. The library checks this.
- **R15.** Each request is complete without history.
- **R16.** Operations are in the order they ran.
- **R25.** When two producers change the same content, each one is credited for its own change,
  in order.

### E. Operability

- **R17.** Each operation names its component, taxonomy class and decision signal.
- **R18.** Each request carries request ID, session, thread (if known) and agent.
- **R19.** The model is versioned. An unknown kind or class is kept and reported as unpriced, never
  dropped.
- **R20.** The operation list has a size limit, with aggregation above it.
- **R21.** Second-order effects are not operations. Example: the agent compacts later because its
  requests are smaller.

## 4. The object model

### 4.1 Types

```go
package ops // github.com/rossoctl/context-guru/ops

type RequestOps struct {
    Version   int
    RequestID string
    Session   string
    Thread    string // "" when the host cannot resolve threads
    Agent     string
    Ops       []Operation // in pipeline order
    Truncated *Aggregate  // set when Ops went over the limit
}

// Aggregate replaces the operations past the limit. It keeps one entry for each key, so the tier
// stays exact; only the totals for each change are lost.
type Aggregate struct {
    Limit   int
    Dropped int // operations folded into Entries
    Entries []AggregateEntry
}

type AggregateEntry struct {
    Producer, Component string
    Class               Class
    FirstApplied        bool
    Side                Side // beforeBoundary | afterBoundary, from the producer's own view
    Ops                 int
    Before, After       Size
}

type Operation struct {
    ID        string // unique inside the request
    Kind      Kind   // reduce | add | reroute | spend | cacheBreak
    Producer  string // set by the framework, not by the producer
    Stage     int    // set by the framework
    Component string // "mask", "dedup"; "" for a one-operation plugin
    Class     Class  // reencode | envelope | reduce | summarize | restore | inject
    Signal    Signal // shape | redundancy | age | reference | relevance | account; "" if none

    Status        Status        // applied | projected | reverted | discarded
    Reversibility Reversibility // lossless | reversible | irreversible
    StashKey      string        // set when reversible

    At     Position // first position the change touches, in this producer's output
    Spans  []Span   // every touched range, when the change is not contiguous; nil otherwise
    Before Size     // what the producer received over the touched ranges
    After  Size     // what it sent on

    // kind = reduce | add
    ChangeID     string   // the same on every request that carries this change
    FirstApplied bool     // true only on the first request that carries it
    ContentKeys  []string // hashes of removed content

    // class = restore
    Reverses string // ChangeID this restore adds back

    // kind = spend
    SpendID  string   // the same ID as the CostRecorder record
    Produced []string // ChangeIDs this call created

    // kind = reroute
    FromModel, ToModel, FromHost, ToHost string

    // kind = cacheBreak (written by the collector only)
    Cause        Cause  // producer | client | expired | unknown
    AttributedTo string // operation ID; "" means nobody

    // a change to the message list, for mapping positions
    Structure *Splice
}

type Position struct {
    Section Section // tools | system | messages
    Index   int     // block or message index in that section
}

// Span is a range of positions in one section: [From, To], both in this producer's output.
type Span struct {
    Section  Section
    From, To int
}

type Size struct {
    Bytes     int    // always set
    Tokens    int    // required on reduce and add; see rule 8
    Tokenizer string // the tokenizer that counted Tokens; "bytes-estimate" when estimated
}

type Splice struct{ At, Removed, Inserted int } // messages only

type Kind string          // reduce | add | reroute | spend | cacheBreak
type Class string         // reencode | envelope | reduce | summarize | restore | inject
type Signal string        // shape | redundancy | age | reference | relevance | account
type Status string        // applied | projected | reverted | discarded
type Reversibility string // lossless | reversible | irreversible
type Section string       // tools | system | messages
type Cause string         // producer | client | expired | unknown
type Side string          // beforeBoundary | afterBoundary
```

The enums are strings, not integers, so an unknown value from a newer producer survives a decode
and can be reported as unpriced (R19).

### 4.2 Rules

1. **One operation for each change unit.** A change unit is what the producer decides, replays and
   reverses as one. A component that reduces 12 tool outputs with 12 decisions reports 12 `reduce`
   operations, each with its own `ChangeID`. tool-prune removes the declarations of one account
   list in one decision, so it reports one operation for all of them. Its declarations are not
   next to each other, so it lists them in `Spans`. `At` is the first one.
2. **A replay is a new operation with the same `ChangeID` and `FirstApplied = false`.** A replay's
   cost, if any, is a `spend` operation, not part of the reduction.
3. **`FirstApplied` does not choose the tier.** The position does (§6.2). `FirstApplied` counts
   fresh acts and marks where a change's payback starts.
4. **`Before` and `After` are measured at the producer's own input.** For `add` and for a restore,
   `After > Before`.
5. **Only the collector writes `cacheBreak`.**
6. **A keep-alive ping is not in `RequestOps`.** It reaches the ledger through `CostRecorder`, with
   its `SpendID` and thread. The value of a ping is out of scope (§10).
7. **`ChangeID`** is derived from the component, the original content key and the replacement
   bytes. Two components that remove the same content in different ways have different IDs.
8. **The tokens a change saved come from the operation's own `Size`, not from the forwarded body.**
   Removed tokens are not in the forwarded body, so nothing else can count them. A producer with a
   tokenizer (context-guru) counts them and names the tokenizer. A producer with none (tool-prune)
   estimates them from bytes with the ratio of its own input, and sets
   `Tokenizer = "bytes-estimate"`. The library marks every priced result that uses an estimate as
   `Estimated`.

### 4.3 Why positions are (section, index)

The provider hashes `tools`, then `system`, then `messages`. A comparison of two positions in that
order shows whether a change is before a cache boundary. For a change with `Spans`, each span is
compared on its own, and the change's tokens are split over the spans by their `Before` bytes. A
producer reports positions in its own output. If a later producer adds or removes messages, it reports a `Splice`, and the library maps
earlier positions into the forwarded body.

## 5. The collector (Cortex)

### 5.1 Collecting

- Each plugin calls `pctx.ReportOps(ops...)` during its pass. The pipeline runs in sequence, so the
  list is in order.
- The framework sets `Producer` and `Stage`. A plugin cannot report under another plugin's name.
- The framework sets `Status = projected` on every operation of a plugin whose `SetBody` returned
  false (`on_error: observe`).
- `settle` keeps an explicit allow-list of producers, for the reason `savingComponents` has today:
  a plugin must not add itself to the money record only by reporting operations. Operations from
  a producer that is not on the list are kept on the event and marked unpriced.

### 5.2 The counterfactual boundary B\*

The provider reports this request's cache boundary as `cache_read`. But a plugin's cache break
moves it: in the coref example, `cache_read` stops at `messages[5]` because of the coref change.
Without it, the cache would have read through `messages[11]`. So the library needs **B\***, the
boundary that would have applied if no plugin had changed anything:

```
B* = min( prevCachedExtent,   // what the matching previous request left cached
          clientFirstDiff )   // where the client changed its own body
B* = 0 when the cache expired
```

The collector keeps a fingerprint of each recent request, for each session:

```go
type Fingerprint struct {
    At        time.Time
    Client    Body     // client body, before any plugin
    Forwarded Body     // body as sent upstream
    Cached    Position // extent cached after its response: cache_read + cache_write
}

// Body lists every position in hash order: tools, then system, then messages. The slice index is
// the position, so PosHash does not repeat it. Start gives the first index of each section.
type Body struct {
    Start [3]int
    Pos   []PosHash
}

type PosHash struct {
    Hash  uint64 // cumulative: equal hashes mean an equal prefix up to this position
    Bytes int32
}
```

`PosHash` is 12 bytes. It holds hashes and sizes only (R2).

### 5.3 Finding the matching previous request

Subagents share the session with the main thread, and Cortex cannot tell which subagent sent a
request. The provider's cache is addressed by content, not by thread. So the collector compares the
request with the last K = 4 fingerprints of the session, and selects the one with the longest
common client prefix. This meets R24 without a thread ID.

### 5.4 Classifying a break

`fwdFirstDiff` is where the matching fingerprint's forwarded body first differs from this request's
forwarded body. `clientFirstDiff` is the same comparison on client bodies.

| Comparison | Result |
|---|---|
| `fwdFirstDiff < clientFirstDiff` | A plugin broke the cache. Add `cacheBreak{cause: producer, at: fwdFirstDiff}`, attributed to the earliest operation at or before `fwdFirstDiff`. |
| `fwdFirstDiff ≥ clientFirstDiff` | The client changed its body. Add `cacheBreak{cause: client}`, attributed to nobody. |
| No diff, but `cache_read` is far below the matching request's cached extent | The cache expired. Add `cacheBreak{cause: expired}`, attributed to nobody. |
| No matching fingerprint | First turn or restart: `cause: unknown`. B\* is unknown (§6.4). |

### 5.5 Tokens for each position

These are the tokens of content that **was forwarded**: the library needs them for the penalty of a
cache break (§6.2), which counts forwarded content between the break and B\*. The tokens a change
**removed** are not in the forwarded body; they come from the operation's own `Size` (§4.2 rule 8).

Cortex has no tokenizer. The collector sends bytes for each forwarded position, and the library
converts them with the request's own ratio (billed prompt tokens ÷ forwarded body bytes), as
`pricing.EstimateTokensFromBytes` does today. Results that use the ratio are marked `Estimated`.

### 5.6 Limits

- **Memory:** 12 bytes for each position (`PosHash`, §5.2). 2 bodies × 4 fingerprints × 2,000
  positions is about 200 KB for each session. `session.max_sessions` (100) limits the total to about 20 MB.
- **Restart:** fingerprints are lost. Only the next request of each session gets `penaltyUnknown`.
  They are not persisted (§10).
- context-guru keeps its own `MaxCachedIdx` for its own decisions. For pricing, the collector's B\*
  is the only authority (R30).

## 6. The library

### 6.1 Call

```go
func Price(in Input) Result // pure

type Input struct {
    Ops         []Operation  // producers' operations + the collector's cacheBreak
    Usage       Usage        // billed usage of this request
    Forwarded   []PosBytes   // every position of the forwarded body, in hash order
    Breakpoints []Breakpoint // cache_control breakpoints of the forwarded body, in hash order
    Boundary    *Position    // B*; nil = unknown
}

// Usage is the provider's billed prompt usage. CacheWrite1h is the part of CacheWrite billed at
// the one-hour rate, never an addition to it.
type Usage struct {
    Input, CacheRead, CacheWrite, CacheWrite1h int
}

type PosBytes struct {
    Pos   Position
    Bytes int
}

type Breakpoint struct {
    Pos Position
    TTL string // "5m" | "1h"
}
```

### 6.2 Tier rule

**The counterfactual.** The question for each operation is: *if this change had never been made —
on this request or on any earlier request of the same conversation — how would the provider have
billed these tokens on this request?*

That is the question for a change across requests. A different question is possible: *what if the
change was not made on this request only?* For a replayed change, the answer to that question is a
cache break, because the earlier requests cached the body without the content. That figure is the
cost of **stopping** the change, not the value of the change, so the library does not use it.

**The rule.** `L` is the last breakpoint in the forwarded body. The provider writes the cache only
up to a breakpoint, and bills content after `L` as input.

| Position of the tokens | Tier |
|---|---|
| at or before B\* | **cache read** |
| after B\*, at or before `L` | **cache write**, at the TTL of the first breakpoint at or after the position (5m or 1h) |
| after `L` | **input** |
| any position, with no breakpoint in the body | **input** |

On a cache miss, B\* = 0, so the first row matches nothing. A position is compared in hash order:
section first (tools, system, messages), then index. For a change with `Spans`, each span is
compared on its own (§4.3).

A `cacheBreak` attributed to an operation moves the cached content from the break to B\*, as
forwarded on this request, from the read tier to the write tier. The new tail after B\* is written
anyway, so it is not counted.

### 6.3 Output

```go
type Result struct {
    Version  int
    Ops      []PricedOp     // one for each input operation, same order
    Unpriced []Unpriced     // with a machine-stable reason
    Checks   []CheckFailure // §6.5
    Boundary BoundaryInfo   // the B* used, and its source
}

type Unpriced struct {
    OpID   string
    Reason string // "unknown_kind", "negative_size", "producer_not_allowed", ...
}

type CheckFailure struct {
    Check    string // "sum" | "order" | "unique_change" | "attribution" (§6.5)
    Producer string // "" when the check is not about one producer
    Detail   string // numbers only, no content
}

type BoundaryInfo struct {
    Pos    *Position // nil when unknown
    Source string    // "collector" | "cacheRead" | "unknown"
}

type PricedOp struct {
    OpID      string
    Producer  string
    Component string
    ChangeID  string
    Status    Status
    Saved     TierTokens // tokens the provider did not bill, by tier; never negative
    Caused    TierTokens // tokens the provider billed because of this operation; never negative
    Estimated bool
    Reason    string // "cached_prefix", "new_tail", "expired", "break_penalty", ...
}

type TierTokens struct {
    Input, CacheWrite5m, CacheWrite1h, CacheRead int
}
```

`Saved` and `Caused` are separate and both non-negative, because the money layer rejects negative
values (`pricing.MicrosFromUSD`). Cortex prices each side with the existing code.

A break penalty has both sides: the tokens are written (`Caused.CacheWrite5m`) instead of read
(`Saved.CacheRead`). The net is the rate difference, with no special rule.

A `cacheBreak` is priced as its own operation. Cortex adds it to the operation it is attributed to.
`spend` operations are not priced. They pass through with their `SpendID`.

### 6.4 Unknown boundary

If `Boundary` is nil, the library uses the extent `cache_read` covers as the boundary, charges no
penalty, and marks the result `penaltyUnknown`. This reports too little, which is the safe
direction.

### 6.5 Checks

The library reports failed checks in `Result.Checks`. A failure does not stop pricing.

1. **Sum:** for each section, the applied operations' `Before − After` add up to the client size
   minus the forwarded size. The collector sends both. A failure means a plugin changed the body
   and did not report it.
2. **Order:** stages do not decrease.
3. **Unique `ChangeID` inside a request.**
4. **Attribution:** each `cacheBreak` names at most one operation, and that operation is at or
   before the break.

## 7. Recording in Cortex

### 7.1 From `Result` to `event.Saving`

`settle` calls `ops.Price` once for each request. It prices `Saved` and `Caused` with its
`pricing.Resolver`, at the same model and long-context tier as the request itself. It writes one
`event.Saving` for each (producer, component):

| `event.Saving` field | Source |
|---|---|
| `Component` | `producer` or `producer/component`, for example `context-guru/mask` |
| `TokensAvoided` | sum of `Saved` over all tiers |
| `USD` | `Saved`, priced |
| `PenaltyUSD` | `Caused`, priced, plus the attributed `cacheBreak` operations |
| `Tier` | the tier with the largest `Saved` |
| `Projected` | `Status == projected` |
| `Estimated` | any operation `Estimated` |

New fields, all additive (the ledger rule is "add, never rename"):

| Field | Where | Why |
|---|---|---|
| `Saving.Producer` | event and ledger | group `context-guru/*` in the per-plugin breakdown |
| `Saving.Changes []ChangeSaving` | event only | `{changeId, savedUSD, causedUSD, firstApplied}`, for totals and payback for each change. Not in the ledger, which keys by minute. |
| `Event.UnattributedBreakUSD` | event | client edits and expired caches. Shown, never charged to a plugin. |

`TotalAvoidedUSD` does not change: `max(0, USD − PenaltyUSD)` for each saving, floored at zero.
Signed totals are out of scope (§10).

### 7.2 Totals for each change across requests

A total for one `changeId` is a fold over the session's events: add `Changes[i]` where the
`changeId` matches. The session archive holds each request's event for the whole session, so agentop
can compute the fold when it opens a session. There is no ledger column for each change.

Without an archive (a cluster sidecar), totals for each change cover only the events in memory. The
per-request savings and the ledger totals are correct either way.

## 8. Errors

The rule: an error in savings never changes the request and never invents money. If a fact is
missing, report too little and say why.

| Situation | Result |
|---|---|
| `ops.Price` panics | `settle` recovers, writes no saving for the request, and increments `ops_price_panics`. The request and its spend record are unchanged. |
| A malformed operation (unknown kind, negative size) | Goes to `Unpriced` with a reason. The other operations are priced. |
| Sum check fails | Pricing goes on. The failure is counted for each plugin on `/v1/pipeline`. |
| No matching fingerprint | `Boundary.source = unknown`, §6.4. |
| Response has no usage | Nothing is priced, as today. |
| No rate for a tier | `Saving.Provenance` is empty, as today. Tokens are on the record. |
| Producer not on the allow-list | Kept on the event, unpriced. |
| `on_error: observe` | `Status = projected`. Priced and shown as "would have saved", never in totals. |

## 9. Migration

Each step merges alone and leaves the totals correct.

| Step | Repo | Change | Verification |
|---|---|---|---|
| 1 | context-guru | Create the `ops` module: types, `Price`, checks. No callers. | Unit tests; the worked example as a golden test. |
| 2 | context-guru | The engine builds `[]ops.Operation` from `Trace` and the reports. **This is new work for each change, not a read of existing fields.** The frozen state is keyed by content hash (`components/offload/state.go`), so `ChangeID` can be derived. But replays are counted only for each component today (`Report.Replays`), and `apply.Trace` records no ID or replay flag for each change. Each offloader must record, for each change, whether it replayed a frozen decision or made a new one. | Replay tests: the same `ChangeID` on each turn, `FirstApplied` once. |
| 3 | context-guru | The proxy's `dash` calls `ops.Price` in shadow, and logs the difference from its current figure. | Comparison on captured sessions. |
| 4 | Cortex | `pctx.ReportOps`, the collector, fingerprints. No pricing change. | Tests for each row of §5.4. |
| 5 | Cortex | tool-prune and context-guru report operations beside their current events. `settle` computes both figures, writes the current one, logs the difference. | The Cortex / cg proxy / control comparison test (`62333ee9` on `feat/contextguru-keepalive`), on both figures. |
| 6 | Cortex | `settle` switches to `ops.Price`. The new event fields. | Ledger totals before and after on the same replay. |
| 7 | both | Remove the current events, `saving.go`, `unique.go`, the per-producer draft functions, and the cg proxy's own repeat-rate rule. | `go mod tidy -diff` in every module. |
| 8 | Cortex | agentop's per-change view. | TUI tests. |

Expected differences at step 6. Each one corrects a known error, so none is a regression:

- The penalty is lower. The current one uses the whole `cache_write` as an upper bound.
- The tier for each change comes from its position, not from a unique fraction for the request.
- The `context_guru_expand` declaration becomes a cost. The current figure does not count it.

## 10. Out of scope

- Signed totals in the ledger.
- The value of a keep-alive ping (a later cache hit it caused).
- Pricing of `reroute` (inference-router).
- Persisting fingerprints across a restart.

## 11. Testing

1. **Golden test from the worked example.** The turn-5 operations, the turn-8 expiry and the turn-7
   coref break are table tests in `ops`. The expected tokens for each tier are the figures in the
   example file. If the two disagree, one of them is wrong.
2. **Property tests in `ops`:** `Saved` and `Caused` are never negative; an empty list gives an
   empty result; reordering operations of different producers does not change each one's price,
   except for break attribution.
3. **Contract test in Cortex** that builds operations through its plugins and passes them to
   `ops.Price`, so a type change in context-guru fails the Cortex build on upgrade.
4. **Collector tests** for each row of §5.4, and for two threads sharing a session (§5.3).
5. **End-to-end:** the LiteLLM integration test (`06c712cb` on `feat/contextguru-keepalive`) checks
   that the reported saving equals the change in billed cost against the control arm, within a
   tolerance.
