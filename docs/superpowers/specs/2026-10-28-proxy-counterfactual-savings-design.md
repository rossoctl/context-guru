# Proxy savings from `counterfactual`: design

**Status:** proposed.
**Branch:** `feat/counterfactual-savings`, based on `docs/context-operations-spec`.
**Method:** `docs/counterfactual/design.md`. Host wiring: `docs/counterfactual/cortex-wiring.md` (W5, W7.3, W9).

## 1. Goal and scope

`counterfactual.Compute` becomes the only source of savings figures in the context-guru proxy.
It replaces the savings rules in `dash/event.go`.

In scope: the proxy and the engine, in this repo. This is W9 steps 3 to 6, and the proxy half of step 10.
Out of scope: Cortex. Cortex does not change in this work.

The flow:

1. The proxy creates a Tracker for a thread when the thread first appears.
2. The components create Operations while they change a request.
3. The proxy calls `BeginRequest`, `Add`, `EndRequest` and `Compute`.
4. The proxy prices the returned tokens with `internal/modelinfo.Price` and stores the dollars.

## 2. Assumptions

1. The library is the module `github.com/rossoctl/context-guru/counterfactual`. The proxy uses it through `go.work`
   or a `replace` until the library is on `main`.
2. The library returns tokens per tier. It never returns dollars. The proxy prices.
3. The `dash` columns stay (`ComponentSavedUSD`, `baselineDeltaUSD`). Only their source changes.
4. The library is the fingerprint Tracker. The push model (design §11, question 4) is not decided and is not used.

## 3. What is replaced

In `dash/event.go`:

| Rule | After |
|---|---|
| `ComponentSavedUSD`, `uniqueRate`, `repeatRate` | removed. A component's dollars come from its `PricedOp`s. |
| `baselineDeltaUSD` (reduction part) | removed. The delta is the sum of the priced operations. |
| `cachesplitSavedUSD`, `keepaliveSavedUSD` | removed. They are cache effects found by `Compute`. |
| `MarkUnique` split in the recorder | removed. The tier comes from the position of a change, not from a unique fraction. |
| `TailChanged`, cold-start attribution | read `Result.Boundary` and `Result.Phase`. |

Kept, per design §9.1: gates, events, tool inventory, holdout replay and `KeepAliveReport`.

## 4. Components

### 4.1 Engine: Operations (W9 step 3)

- Each component reports an `Operation` for each change it makes.
- `apply.Result` gains `Ops []counterfactual.Operation` and `Thread`.
- Each change has a stable `ChangeID`, derived from the content hash. The offload state is already keyed by
  content hash (`components/offload/state.go`).
- Replays are counted only per component today (`Report.Replays`). Each offloader must record a `ChangeID` for
  each change. This is new work. It is not a read of existing fields.
- Marker and apply-only changes are built by `apply` (design §4.2 rule 8).
- Sizes are bytes only. The library converts bytes to tokens with the request's ratio.

### 4.2 Proxy: Tracker (W9 step 4)

- One `counterfactual.Trackers`, keyed by `thread.Key(session, thread)`. It lives beside `dash.Recorder`'s `lastTail`.
- Per request:
  1. `BeginRequest` with the client body and the client breakpoints.
  2. `Add` for each Operation in `apply.Result.Ops`.
  3. `EndRequest` with the thread, the forwarded body, the breakpoints and `EntryFacts`.
  4. `Compute(usage)` after the response. `Compute` stores the new `ThreadState`.
- A thread that sets `Inherits` starts from a copy of its parent's `ThreadState`.
- `Preview` is used only to price an old request again. It stores nothing.
- `Billing` comes from `counterfactual.BillingFrom`, from the credential the request used.
- Keep-alive: `dashEmitter` converts each `KeepAliveReport` into a `spend` Operation with the library. It prices
  the result with `ComputeStandalone`. It keeps the last ping facts of each thread and passes them in
  `EntryFacts.LastPing`, `PingRead` and `PingWindow`. A ping does not move `ThreadState`.
- `dashEmitter` implements `OpsEmitter` for a detached model call.
- The proxy checks `MaxCachedIdx` against B\*. It counts each disagreement.

### 4.3 Proxy: pricing (W9 steps 5 and 6)

- Each `PricedOp` is converted to dollars with `modelinfo.Price` for the request's model.
- Dollars are stored in `ComponentSavedUSD` and `baselineDeltaUSD`.
- Tokens stay per tier. The proxy never adds tiers into one token total.
- An operation with a reason in `Unpriced` is shown as unpriced. It is not shown as zero.

## 5. Rollout

Each step merges alone and leaves the totals correct.

| # | Change | Verification |
|---|---|---|
| 1 | Engine emits Operations and `ChangeID`s (4.1). | Replay tests: the same `ChangeID` on every turn. |
| 2 | Tracker, keep-alive conversion and `OpsEmitter` (4.2). Nothing reads the output. | The cache cases (design §5.2) pass through the proxy. The B\* disagreement counter is zero on captured sessions. |
| 3 | Shadow run. `Compute` runs on every request. The old figure is stored. The new figure and the difference are logged. | Every logged difference matches a `differs` entry in `counterfactual/opscases/differences.json`. |
| 4 | Switch. The stored figure comes from `Compute`. The recorder reads `Result.Boundary` and `Result.Phase`. | Dashboard totals before and after, on the same captured sessions. |
| 5 | Delete the rules in section 3. | `go mod tidy -diff` in every module. All cases pass. |

A shadow run still calls `Compute`. Only the use of the figure is shadowed. `ThreadState` must advance.

## 6. Expected differences

Each one corrects a known error (W9):

- The cache-break penalty is lower. The old one used the whole `cache_write` as an upper bound.
- The tier of a change comes from its position, not from a unique fraction of the request.
- The `context_guru_expand` declaration becomes a cost. The old figure did not count it.
- A cache-break penalty starts at the read extent, not at the break.
- A keep-alive window whose pings rescued nothing is a negative figure for its thread.

## 7. Risks

- **Library gaps** (`counterfactual/doc.go`, "Not implemented"):
  - the `split`, `ttlPremium` and `ttlRescue` effects, so `cachesplit` and head-TTL savings lose their source;
  - the order and conservation checks;
  - the long-context tier and the minimum cacheable length.

  The shadow run shows how far these gaps move the figures. Step 5 must not delete a rule that a `notCovered`
  entry still uses.
- **State loss on restart.** Active Operations are not persisted. Until each change ends, later requests of the
  thread miss their saving and fail the sum check. Recovery is open (design §11, question 13).
- **Unbounded active list.** The list of active operations stays bounded only if the end-of-change rule works
  (design §11, question 12).
- **`ChangeID` work in step 1** is the largest unknown. It touches every offloader.
- **Tokenizer estimate.** Bytes are converted to tokens with a ratio. Every priced rewrite is `Estimated`.
  The accuracy of the ratio is open (design §11, question 3).

## 8. Testing

- The `opscases` list is the contract. Every case passes in the library before the proxy uses it.
- The shadow-run difference log is checked against `differences.json`.
- The existing `TestAggregator`-style invariant is kept: a counterfactual never enters spend.
- Dashboard totals are compared before and after on captured sessions at the switch.
