# Context operations: a worked example

**Status:** draft, for the context-operations design. Not yet approved.

This file shows the operations object model on one session. It shows:

1. the context (the request body) on one turn,
2. the list of operations the plugins report for that turn,
3. how the savings library prices them,
4. how one change saves money across later requests,
5. how a change inside the cached prefix costs money, and when it pays back.

The model includes two corrections found while writing this example:

- The **tier** of a saved token comes from its **position** compared with the previous request's
  cache boundary. It does not come from `firstApplied`.
- `removalId` is renamed **`changeId`**, because an `add` operation also needs a stable ID.

## Setup

**Pipeline**, in order:

| Stage | Producer | What it does in this example |
|---|---|---|
| 0 | `tool-prune` | Removes 12 tool declarations the account does not use |
| 1 | `context-guru` | `mask`, `cmdfilter`, `coref`; adds the `context_guru_expand` tool |

**Rates** (example values, per million tokens):

| Tier | Rate |
|---|---|
| input | $3.00 |
| cache write (5m) | $3.75 |
| cache read | $0.30 |

**Who does what:**

| Step | Done by |
|---|---|
| Report operations | each producer (plugin) |
| Collect operations, compare with the previous forwarded body, add `cacheBreak` | Cortex collector |
| Choose the counterfactual tier, return tokens per tier | cg `ops` library (pure function) |
| Convert tokens to dollars | Cortex `pricing.Resolver` |

---

## Turn 5

### 1. The context

The body the **client** sends. Token counts in brackets.

```
tools      30 declarations                                 [20,000]
system     Claude Code system prompt                       [ 3,000]
messages
  [0..2]   user prompt, assistant, ...                     [10,000]
  [3]      tool_result: Read a.go                          [ 4,000]
  [4]      assistant                                       [ 1,000]
  [5]      tool_result: grep over the repo                 [20,000]
  [6..8]   assistant, tool calls, results                  [ 5,000]
  [9]      tool_result: pytest output            (NEW)     [ 8,000]
                                                   total   [71,000]
```

**State from earlier turns:**

- `tool-prune` removed the same 12 declarations (8,000 tokens) on every turn since turn 1.
- On turn 3, cg's `mask` reduced message 3 to a 60-token marker. cg replays that decision on every
  later turn.
- cg added the `context_guru_expand` tool (300 tokens) on every turn since turn 1.
- On turn 4, Cortex forwarded messages 0 to 8. The provider cached them. So the **previous cache
  boundary** for this thread is `messages[8]`. This is B* in the design (§5.2).

### 2. The operations

What the collector holds after the pipeline ran:

```json
{
  "version": 1,
  "requestId": "req-5",
  "session": "s1",
  "thread": "main",
  "ops": [
    {
      "id": "op1", "kind": "reduce",
      "producer": "tool-prune", "stage": 0,
      "class": "envelope", "signal": "account",
      "status": "applied", "reversibility": "irreversible",
      "at": {"section": "tools", "index": 0},
      "before": {"tokens": 20000}, "after": {"tokens": 12000},
      "changeId": "tp:decl-set-7f3a", "firstApplied": false
    },
    {
      "id": "op2", "kind": "reduce",
      "producer": "context-guru", "stage": 1, "component": "mask",
      "class": "reduce", "signal": "age",
      "status": "applied", "reversibility": "reversible", "stashKey": "cg:91ab",
      "at": {"section": "messages", "index": 3},
      "before": {"tokens": 4000}, "after": {"tokens": 60},
      "changeId": "cg:mask:91ab", "firstApplied": false
    },
    {
      "id": "op3", "kind": "reduce",
      "producer": "context-guru", "stage": 1, "component": "cmdfilter",
      "class": "reduce", "signal": "shape",
      "status": "applied", "reversibility": "reversible", "stashKey": "cg:c40e",
      "at": {"section": "messages", "index": 9},
      "before": {"tokens": 8000}, "after": {"tokens": 900},
      "changeId": "cg:cmdfilter:c40e", "firstApplied": true
    },
    {
      "id": "op4", "kind": "add",
      "producer": "context-guru", "stage": 1, "component": "expand",
      "class": "inject",
      "status": "applied", "reversibility": "lossless",
      "at": {"section": "tools", "index": 18},
      "before": {"tokens": 0}, "after": {"tokens": 300},
      "changeId": "cg:expand-tool:v3", "firstApplied": false
    }
  ]
}
```

Bytes and the tokenizer name are left out to keep the example short.

### 3. What the collector adds

The collector compares the forwarded body with the turn-4 forwarded body. They are the same up to
`messages[8]`. So no plugin broke the cache, and the collector adds no `cacheBreak` operation.

### 4. What the provider bills

The **forwarded** body:

```
tools      18 declarations + expand tool          [12,300]
system                                            [ 3,000]
messages   [0..8], with [3] masked                [36,060]
           [9] filtered                           [   900]
                                          total   [52,260]
```

The response usage: `cache_read 51,360`, `cache_write 900`, `input 0`.

### 5. The library: choose the counterfactual tier

The question for each operation: *"If this operation had not happened, how would the provider
have billed these tokens on this request?"*

The library input:

```
ops                 the four operations above
boundary (B*)       messages[8]     (from the collector: design §5.2)
usage               cache_read 51,360, cache_write 900, input 0
```

The rule:

- If the operation's position is **at or before** the previous boundary, the tokens were in the
  cached prefix. On a cache hit, they would have been a **cache read**.
- If the position is **after** the previous boundary, the tokens are new this turn. They would have
  been a **cache write** if the response shows a cache write, else **input**.
- On a cache miss (no cache read), every position is new: cache write or input, by the same test.

| Op | Tokens changed | Position vs `messages[8]` | Counterfactual tier |
|---|---|---|---|
| op1 | −8,000 (saved) | `tools`: before | cache read |
| op2 | −3,940 (saved) | `messages[3]`: before | cache read |
| op3 | −7,100 (saved) | `messages[9]`: after | cache write |
| op4 | +300 (cost) | `tools`: before | cache read |

The library output, in tokens for each tier. A positive number is a saving, a negative number is a
cost:

```
op1  {cacheRead: +8000}
op2  {cacheRead: +3940}
op3  {cacheWrite: +7100}
op4  {cacheRead: -300}
```

### 6. Cortex: convert to dollars

| Op | Calculation | Result |
|---|---|---|
| op1 | 8,000 × $0.30/M | +$0.00240 |
| op2 | 3,940 × $0.30/M | +$0.00118 |
| op3 | 7,100 × $3.75/M | +$0.02663 |
| op4 | 300 × $0.30/M | −$0.00009 |
| **Request total** | | **+$0.03012** |

For each producer:

| Producer | Result |
|---|---|
| `tool-prune` | +$0.00240 |
| `context-guru` | +$0.02772 |

---

## Across requests: change `cg:cmdfilter:c40e`

cg replays op3 on every later turn. Each replay is a new operation with the **same `changeId`** and
`firstApplied: false`. The library prices it again on each request, from that request's own
position and cache result.

| Turn | Situation | Tier for the 7,100 tokens | Saving | Total for this change |
|---|---|---|---|---|
| 5 | first applied; after the boundary | cache write | $0.02663 | $0.02663 |
| 6 | replay; `messages[9]` is now cached | cache read | $0.00213 | $0.02876 |
| 7 | replay; cache hit | cache read | $0.00213 | $0.03089 |
| 8 | replay; **cache TTL expired** (`cache_read 0`) | cache write | $0.02663 | $0.05751 |

On turn 8, the collector adds an operation with no producer:

```json
{ "id": "op9", "kind": "cacheBreak", "cause": "expired" }
```

The break was not caused by a plugin, so the library charges it to nobody. On turn 8, op1, op2 and
op4 also move to the cache-write tier, by the same rule.

**What this shows.** A change of K tokens saves on every later request that still carries it. On
each request it saves K × the rate of the tier those tokens would have had **on that request**. It
does not save K × the input rate every time. With a warm cache, most turns are at the cache-read
rate.

---

## A change that costs money first: `coref` on turn 7

### The context on turn 7

Turn 6 added messages 10 and 11 (2,000 tokens). Cortex forwarded messages 0 to 11 on turn 6, so
the **previous boundary** on turn 7 is `messages[11]`.

On turn 7, cg's `coref` finds that no later turn uses anything from message 5, the 20,000-token
grep output. It reduces it to a 50-token marker. Message 5 is **inside** the cached prefix.

### The operation

```json
{
  "id": "op12", "kind": "reduce",
  "producer": "context-guru", "stage": 1, "component": "coref",
  "class": "reduce", "signal": "reference",
  "status": "applied", "reversibility": "reversible", "stashKey": "cg:5e2d",
  "at": {"section": "messages", "index": 5},
  "before": {"tokens": 20000}, "after": {"tokens": 50},
  "changeId": "cg:coref:5e2d", "firstApplied": true
}
```

### What the collector adds

The forwarded body first differs from the turn-6 forwarded body at `messages[5]`. The earliest
operation at or before that position is op12. So the collector adds:

```json
{
  "id": "op13", "kind": "cacheBreak", "cause": "producer",
  "at": {"section": "messages", "index": 5},
  "attributedTo": "op12"
}
```

### The calculation

**Saving.** 19,950 tokens. The position is before the previous boundary, so the tier is cache read.

```
19,950 × $0.30/M = +$0.00599
```

**Penalty.** Everything from the break to the previous boundary was cached, and is now written
again. The new turn-7 tail after the boundary is written anyway, so it is not counted.

| Content from `messages[5]` to `messages[11]` | Tokens |
|---|---|
| message 5, now the marker | 50 |
| messages 6 to 8 | 5,000 |
| message 9, filtered | 900 |
| messages 10 and 11 | 2,000 |
| **Re-written** | **7,950** |

The library output for op13: `{cacheWrite: −7950, cacheRead: +7950}`. The tokens move from the
read tier to the write tier.

```
7,950 × ($3.75 − $0.30)/M = −$0.02743
```

**Net on turn 7** for change `cg:coref:5e2d`:

```
+$0.00599 − $0.02743 = −$0.02144
```

### Payback

cg replays the coref change on every later turn. On each cache hit it saves 19,950 tokens at the
cache-read rate, +$0.00599.

| Turn | Saving | Penalty | Total for this change |
|---|---|---|---|
| 7 | +$0.00599 | −$0.02743 | −$0.02144 |
| 8 | +$0.00599 | | −$0.01545 |
| 9 | +$0.00599 | | −$0.00946 |
| 10 | +$0.00599 | | −$0.00347 |
| 11 | +$0.00599 | | **+$0.00252** |

The change pays back on turn 11. If the session had ended on turn 10, the change would have lost
$0.00347. The total for each `changeId` shows this, because the penalty and the savings carry the
same ID.

The penalty is exact here: it counts only the cached content after the break. The current
`rewritePenalty` uses the response's whole `cache_write`, which is an upper bound.

---

## Summary of the model, as the example uses it

| Field | Used for |
|---|---|
| `kind` | `reduce` saves; `add` costs; `cacheBreak` moves tokens from read to write |
| `producer`, `stage`, `component` | attribution for each plugin and each component |
| `at` | choose the tier (compared with the previous boundary); find the cause of a break |
| `before`, `after` | the token change, measured at the producer's own input |
| `changeId` | sum one change across requests; link a penalty to the change that caused it |
| `firstApplied` | count fresh acts; mark where the payback of a change starts |
| `status` | only `applied` operations count |
| `attributedTo` | which operation a `cacheBreak` is charged to; absent means nobody |
