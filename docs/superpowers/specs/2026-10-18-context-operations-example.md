# Context operations: a worked example

**Status:** draft, for the context-operations design. Not yet approved.

This file follows one session from turn 5 to turn 13, with one subagent. It shows:

1. the context (the request body), and the operations the plugins report;
2. how the library prices them, from the **cache state** and the **breakpoints**;
3. how one change saves money across later requests;
4. how a change inside the cached prefix costs money, and whether it pays back;
5. a **keep-alive ping**, and the request it rescues;
6. a **summary** that ends earlier changes;
7. a **subagent**, and the totals for each thread and for the session;
8. the same session for a user with a **subscription plan**.

A script computed every figure. For each request, the operations add up to the bill without the
operations minus the bill with them (design §6.3, "Conservation").

## Setup

**Pipeline**, in order:

| Stage | Producer | What it does in this example |
|---|---|---|
| 0 | `tool-prune` | Removes 12 tool declarations the account does not use |
| 1 | `context-guru` | `mask`, `cmdfilter`, `coref`, `summarize`; adds the `context_guru_expand` tool |
| — | keeper | Sends keep-alive pings between turns (not in the pipeline) |

**Rates** (example values, per million tokens):

| Tier | Agent model | Summarizer model |
|---|---|---|
| input | $3.00 | $1.00 |
| cache write (5m) | $3.75 | |
| cache read | $0.30 | |
| output | $15.00 | $5.00 |

**Breakpoints.** The client is Claude Code. It sets a 5m breakpoint at the end of `system`, and one
on the **last message**. So the provider can read or write the cache at two points only: the end of
`system`, and the end of the body.

**Who does what:**

| Step | Done by |
|---|---|
| Report operations | each producer (plugin), and the keeper for pings |
| Collect operations; find the thread, the cache state and B\*; add `cacheBreak` and `keepAlive` | the host's collector, with `ops.Compare` |
| Choose the tier; return tokens for each tier | `ops.Price` (pure) |
| Convert tokens to dollars, for savings and for spend | the host (Cortex `pricing.Resolver`, or the proxy's `modelinfo`) |

**Session and thread.** The session is `s1`. The main thread is `main`. A subagent thread
`t-a1b2` runs once, between turns 8 and 9. Every operation carries both IDs. To keep the JSON
short, the examples show them once, in `RequestOps`.

## The session at a glance

| Request | Thread | Arrives | Cache state | What happens | Net |
|---|---|---|---|---|---|
| T5 | main | 1 min after T4 | hot | `cmdfilter` reduces a new output | +$0.03012 |
| T6 | main | 1 min after T5 | hot | replays only | +$0.00562 |
| T7 | main | 1 min after T6 | hot | `coref` reduces message 5, **inside** the cached prefix | −$0.05398 |
| T8 | main | 1 min after T7 | hot | replays only | +$0.01161 |
| S1 | t-a1b2 | — | cold (first request) | subagent: `cmdfilter` | +$0.01575 |
| T9 | main | 1 min after T8 | hot | replays only | +$0.01161 |
| p1 | main | 4 min after T9 | | **keep-alive ping** | −$0.01214 |
| T10 | main | 8 min after T9 | **rescued** | the ping kept the cache hot | +$0.28416 |
| T11 | main | 1 min after T10 | hot | replays only | +$0.01161 |
| T12 | main | 1 min after T11 | hot | **`summarize`** replaces messages 0 to 8 | −$0.05730 |
| p2, p3 | main | 4 and 8 min after T12 | | two pings; the keeper's limit is 2 | −$0.01911 |
| T13 | main | 14 min after T12 | **cold** (expired) | the pings did not reach | +$0.19988 |

---

## Turn 5: one request in detail

### The context

The body the **client** sends. Token counts in brackets.

```
tools      30 declarations                                 [20,000]
system     Claude Code system prompt                       [ 3,000]   ← breakpoint 1
messages
  [0..2]   user prompt, assistant, ...                     [10,000]
  [3]      tool_result: Read a.go                          [ 4,000]
  [4]      assistant                                       [ 1,000]
  [5]      tool_result: grep over the repo                 [20,000]
  [6..8]   assistant, tool calls, results                  [ 5,000]
  [9]      tool_result: pytest output            (NEW)     [ 8,000]   ← breakpoint 2 (L)
                                                   total   [71,000]
```

State from earlier turns:

- `tool-prune` removed the same 12 declarations (8,000 tokens) on every turn since turn 1.
- On turn 3, `mask` reduced message 3 to a 60-token marker. cg replays it on every later turn.
- cg added the `context_guru_expand` tool (300 tokens) on every turn since turn 1.
- Turn 4 forwarded messages 0 to 8, and the provider cached them.

### The cache state

The collector finds turn 4's fingerprint. The gap is 1 minute, inside the 5m TTL:

```json
"state": {"phase": "hot", "extent": {"section": "messages", "index": 8}, "gap": "1m", "ttl": "5m"}
```

So B\* = `messages[8]`.

### The operations

```json
{
  "version": 1, "requestId": "req-5", "session": "s1",
  "thread": {"id": "main", "source": "header"},
  "ops": [
    {
      "id": "op1", "kind": "reduce",
      "producer": "tool-prune", "stage": 0,
      "class": "envelope", "signal": "account",
      "status": "applied", "reversibility": "irreversible",
      "at": {"section": "tools", "index": 2},
      "spans": [
        {"section": "tools", "from": 2,  "to": 2,
         "before": {"tokens": 900, "tokenizer": "bytes-estimate"}, "after": {"tokens": 0}},
        {"section": "tools", "from": 9,  "to": 9,
         "before": {"tokens": 450, "tokenizer": "bytes-estimate"}, "after": {"tokens": 0}},
        {"section": "tools", "from": 15, "to": 15,
         "before": {"tokens": 700, "tokenizer": "bytes-estimate"}, "after": {"tokens": 0}}
      ],
      "before": {"tokens": 8000, "tokenizer": "bytes-estimate"},
      "after": {"tokens": 0},
      "changeId": "tp:decl-set-7f3a"
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

To keep the example short, bytes and the cg tokenizer name are left out, and op1 lists 3 of its 12
spans. op1 is **one** operation for 12 declarations, because tool-prune removes one account list in
one decision (design §4.2 rule 1). Its sizes cover only the declarations it removed. The 12 span
sizes add up to 8,000 (the 3 shown add up to 2,050).

op1 has no `firstApplied`. tool-prune keeps no state, so it cannot know whether an earlier request
carried the same change (design §4.7). Its price does not need it.

The `tools` section sizes are not on any operation. The collector sends them to the sum check
(design §6.5): 20,000 − 8,000 (op1) + 300 (op4) = 12,300 forwarded.

### The bill

The **forwarded** body is 52,260 tokens. The provider bills `cache_read 51,360`,
`cache_write 900`, `input 0`.

### The price

The question for each operation (design §6.2): *if this change had never been made, how would the
provider have billed these tokens on this request?*

| Position of the tokens | Tier |
|---|---|
| at or before B\* | cache read |
| after B\*, at or before `L` | cache write, at the TTL of the next breakpoint |
| after `L` | input |

| Op | Tokens | Position | Tier | Result |
|---|---|---|---|---|
| op1 | −8,000 | `tools`: before B\* | cache read | 8,000 × $0.30 = **+$0.00240** |
| op2 | −3,940 | `messages[3]`: before B\* | cache read | 3,940 × $0.30 = **+$0.00118** |
| op3 | −7,100 | `messages[9]`: after B\*, at `L` | cache write 5m | 7,100 × $3.75 = **+$0.02663** |
| op4 | +300 | `tools`: before B\* | cache read | 300 × $0.30 = **−$0.00009** |
| **Turn 5** | | | | **+$0.03012** |

All rates are per million tokens. If the client had put no breakpoint on `messages[9]`, op3's
tokens would be after `L`. The tier would then be input, and the saving $0.02130.

---

## One change across requests: `cg:cmdfilter:c40e`

cg replays op3 on every later turn: a new operation with the same `changeId` and
`firstApplied: false`. The library prices it on each request from that request's own state.

| Request | State | Tier for the 7,100 tokens | Saving | Total |
|---|---|---|---|---|
| T5 | hot; position new | cache write | +$0.02663 | +$0.02663 |
| T6 to T12 | hot; position cached | cache read | +$0.00213 each | +$0.04154 at T12 |
| T13 | **cold** | cache write | +$0.02663 | **+$0.06816** |

On a hot turn the change saves at the cache-read rate. On a cold turn it saves at the cache-write
rate, 12.5 times more. The same change has a different value in each state.

---

## A change inside the cached prefix: `coref` on turn 7

### The context and the operation

On turn 7, `coref` reduces message 5 (the 20,000-token grep output) to 50 tokens. Message 5 is
inside the cached prefix: turn 6 cached through `messages[11]`, so B\* = `messages[11]`.

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

The forwarded body first differs from turn 6's at `messages[5]`. The client did not change, so the
collector adds a break attributed to op12:

```json
{ "id": "op13", "kind": "cacheBreak", "cause": "producer",
  "at": {"section": "messages", "index": 5}, "attributedTo": "op12" }
```

### Where the breakpoints matter

The provider reads the cache only up to a breakpoint. The last breakpoint before `messages[5]` is
the end of `system`. So the provider reads only `tools` and `system` (15,300 tokens). It writes
**everything** after them again, including messages 0 to 4, which coref did not touch.

The bill: `cache_read 15,300`, `cache_write 21,010`.

| Content written again, from the read extent to B\* | Tokens |
|---|---|
| messages 0 to 4 (not changed, but after the last breakpoint before the break) | 11,060 |
| message 5, now the marker | 50 |
| messages 6 to 11 | 7,900 |
| **Penalty** | **19,010** |

```
saving    19,950 × $0.30            = +$0.00599
penalty   19,010 × ($3.75 − $0.30)  = −$0.06558
net on turn 7                       = −$0.05960
```

If the client had a breakpoint on `messages[4]`, the penalty would start at message 5: 7,950
tokens, −$0.02743. Here the breakpoints, not the size of the change, decide most of the cost.

### Does it pay back?

coref saves +$0.00599 on each later hot turn. The summary on turn 12 ends it (see below).

| Request | Saving | Penalty | Total |
|---|---|---|---|
| T7 | +$0.00599 | −$0.06558 | −$0.05960 |
| T8 to T11 | +$0.00599 each | | −$0.03566 at T11 |
| T12 | ended by the summary | | **−$0.03566** (final) |

The change did not pay back. It needed 10 more hot turns, and it had 4 before the summary ended
it. The total for its `changeId` shows this.

---

## A keep-alive ping: p1, and turn 10

### The ping

The user is idle after turn 9. The keeper sends a ping 4 minutes after turn 9. The ping is its own
`RequestOps`:

```json
{
  "version": 1, "requestId": "ping-p1", "session": "s1",
  "thread": {"id": "main", "source": "header"}, "ping": true,
  "ops": [
    { "id": "op1", "kind": "ping", "producer": "keeper",
      "changeId": "ka:main:w1", "firstApplied": true,
      "call": {
        "host": "api.anthropic.com", "model": "agent-model",
        "usage": {"input": 10, "cacheRead": 40310, "cacheWrite": 0},
        "output": 1, "status": 200,
        "paidBy": {"billing": {"mode": "api"}, "operator": false}
      } }
  ]
}
```

The ping re-sends turn 9's forwarded body (40,310 tokens) and gets a 1-token reply. The keeper sends
this report through the host's `Sink`, because a ping has no request to travel with (design §4.6).
The usage in `call` is what the provider billed. `ops.Price` returns it unchanged, and the host
prices it with the same rates as the savings: 40,310 read + 10 input + 1 output = **−$0.01214**.

### Turn 10: the rescued request

Turn 10 arrives 8 minutes after turn 9. Without the ping, the 5m entry would have expired at 6
minutes. The ping read it at 4 minutes, so it is hot until 9 minutes. The collector checks the four
conditions of design §4.4, and adds:

```json
{
  "id": "op20", "kind": "stateChange", "mechanism": "keepAlive",
  "producer": "keeper",
  "stateFrom": {"phase": "cold"},
  "stateTo": {"phase": "hot", "extent": {"section": "messages", "index": 17}},
  "before": {"tokens": 40310}, "after": {"tokens": 40310},
  "changeId": "ka:main:w1", "firstApplied": false
}
```

Each turn adds two messages, so turn 9 forwarded messages 0 to 17. The extent is `messages[17]`.
The ping and the rescue have the same `changeId`, so the cost and the value of the window add up.

| Item | Calculation | Result |
|---|---|---|
| Content changes, priced **hot** | the replays of tool-prune, expand, mask, coref, cmdfilter | +$0.01161 |
| Rescue (op20) | 40,310 × ($3.75 − $0.30) | +$0.13907 |
| Interaction (design §6.2) | 38,690 × ($3.75 − $0.30) | +$0.13348 |
| **Turn 10** | | **+$0.28416** |

**The interaction.** The content changes are priced in the hot state, which the ping caused.
Without the ping, the same changes would be priced cold, and would be worth more. That difference
is the interaction. It belongs to neither of them alone. So it is attributed to nobody, and it is
counted only in the thread and session totals (design §12, question 1).

**The window `ka:main:w1`:** −$0.01214 (p1) + $0.13907 (rescue) = **+$0.12693**.

---

## A summary that ends earlier changes: turn 12

### The context

On turn 12, cg's `summarize` replaces messages 0 to 8 with one 1,500-token summary. In the
**client** body those messages are 40,000 tokens. Two of them were already reduced:

| Message | Client tokens | Reduced by | Forwarded before the summary |
|---|---|---|---|
| 3 | 4,000 | `mask` (`cg:mask:91ab`) | 60 |
| 5 | 20,000 | `coref` (`cg:coref:5e2d`) | 50 |
| 0 to 2, 4, 6 to 8 | 16,000 | — | 16,000 |
| **Total** | **40,000** | | **16,110** |

### The operation

```json
{
  "id": "op30", "kind": "reduce",
  "producer": "context-guru", "stage": 1, "component": "summarize",
  "class": "summarize", "signal": "age",
  "status": "applied", "reversibility": "reversible", "stashKey": "cg:ab12",
  "at": {"section": "messages", "index": 0},
  "before": {"tokens": 40000}, "after": {"tokens": 1500},
  "supersedes": ["cg:mask:91ab", "cg:coref:5e2d"],
  "inherited": {"tokens": 23890},
  "changeId": "cg:summarize:ab12", "firstApplied": true,
  "structure": {"at": 0, "removed": 9, "inserted": 1}
}
```

- `before` is the **client's** 40,000 tokens, so the summary's saving (38,500) covers all of it.
- `supersedes` names the two changes it ends. From turn 12, cg does not report them again.
- `inherited` is what they had removed: (4,000 − 60) + (20,000 − 50) = 23,890. The summary's own
  new work is 38,500 − 23,890 = 14,610. The price does not use `inherited`. A report can.
- `structure` tells the library that 9 messages became 1, so later positions move.

The summarizer call is a `spend` operation, linked by `produced`:

```json
{ "id": "op31", "kind": "spend", "producer": "context-guru", "component": "summarize",
  "produced": ["cg:summarize:ab12"],
  "call": {
    "host": "api.anthropic.com", "model": "summarizer-model",
    "usage": {"input": 16110, "cacheRead": 0, "cacheWrite": 0},
    "output": 1500, "status": 200, "detached": false,
    "paidBy": {"billing": {"mode": "api"}, "operator": false}
  } }
```

The call finished before the response, so op31 travels in turn 12's own report, through
`pctx.ReportOps`. The host prices `call.usage` on the summarizer model: 16,110 input + 1,500 output
= **−$0.02361**.

### The price

The summary is at `messages[0]`, inside the cached prefix, so the collector adds a `cacheBreak`
attributed to op30. The provider reads `tools` and `system` (15,300) and writes the rest (16,400).

| Item | Calculation | Result |
|---|---|---|
| Saving (op30) | 38,500 × $0.30 | +$0.01155 |
| Penalty (cacheBreak) | 14,400 × ($3.75 − $0.30) | −$0.04968 |
| Summarizer call (op31) | `call.usage`, priced by the host | −$0.02361 |
| **The summary on turn 12** | | **−$0.06174** |

The other operations on turn 12 (tool-prune, expand, cmdfilter) add +$0.00444. Turn 12 is
−$0.05730 in total.

### What happens to `mask` and `coref`

| Change | Last request | Total | Note |
|---|---|---|---|
| `cg:mask:91ab` | T11 | +$0.00827 | ended by the summary; its total does not change |
| `cg:coref:5e2d` | T11 | −$0.03566 | ended before it paid back |

From turn 12, their tokens are part of the summary's saving. Nothing is counted twice: on turn 12
the summary saves 38,500 tokens, and `mask` and `coref` save nothing.

On turn 13 (cold), the summary saves 38,500 × $3.75 = +$0.14438. Its total is then **+$0.08264**.

---

## A cold request: turn 13

The user is idle after turn 12. The keeper sends ping p2 at 4 minutes and ping p3 at 8 minutes.
Each costs −$0.00956. The keeper's limit is 2 pings for each idle gap, so it sends no more. p3
kept the entry hot until 13 minutes.

Turn 13 arrives at 14 minutes. The entry has expired, so the state is **cold**. The collector adds:

```json
{ "id": "op40", "kind": "cacheBreak", "cause": "expired" }
```

The break is attributed to nobody. Every content change is priced cold (cache write):

| Change | Tokens | Result |
|---|---|---|
| `tp:decl-set-7f3a` | 8,000 | +$0.03000 |
| `cg:cmdfilter:c40e` | 7,100 | +$0.02663 |
| `cg:summarize:ab12` | 38,500 | +$0.14438 |
| `cg:expand-tool:v3` | +300 | −$0.00113 |
| **Turn 13** | | **+$0.19988** |

**The window `ka:main:w2`:** −$0.00956 (p2) − $0.00956 (p3), and no rescue = **−$0.01911**. A
window that rescues nothing is a cost, and the thread total shows it.

---

## A subagent: S1

Between turns 8 and 9, the agent starts a subagent. Its request has a thread header, so the
collector sets `thread: {"id": "t-a1b2", "source": "header", "parent": "main"}`. The thread has no
earlier fingerprint, so its state is `cold`. Its client body is 14,000 tokens.

| Op | Tokens | Tier | Result |
|---|---|---|---|
| `cg:cmdfilter:77d0` (reduce) | 4,500 | cache write (cold) | +$0.01688 |
| `cg:expand-tool:v3` (add) | +300 | cache write (cold) | −$0.00113 |
| **S1** | | | **+$0.01575** |

`cg:expand-tool:v3` has the same `changeId` in both threads. Its totals stay apart, because the
total for a change is summed inside one thread (design §7.2).

---

## Totals

### For each change, in thread `main`

| Change | Kind | Requests | Total |
|---|---|---|---|
| `tp:decl-set-7f3a` | reduce | T5 to T13 | +$0.04920 |
| `cg:expand-tool:v3` | add | T5 to T13 | −$0.00185 |
| `cg:mask:91ab` | reduce | T5 to T11 (ended) | +$0.00827 |
| `cg:cmdfilter:c40e` | reduce | T5 to T13 | +$0.06816 |
| `cg:coref:5e2d` | reduce + break | T7 to T11 (ended) | −$0.03566 |
| `cg:summarize:ab12` | reduce + break + spend | T12 to T13 | +$0.08264 |
| `ka:main:w1` | ping + rescue | p1, T10 | +$0.12693 |
| `ka:main:w2` | ping | p2, p3 | −$0.01911 |
| interaction | — | T10 | +$0.13348 |
| **Thread `main`** | | | **+$0.41207** |

### For each thread, and for the session

| | Savings | Spend (summarizer, pings) | Net |
|---|---|---|---|
| Thread `main` | +$0.46692 | −$0.05486 | **+$0.41207** |
| Thread `t-a1b2` (subagent) | +$0.01575 | $0.00000 | **+$0.01575** |
| **Session `s1`** | **+$0.48267** | **−$0.05486** | **+$0.42782** |

"Savings" are content changes, cache breaks, state changes and the interaction. "Spend" is the
money that cg and the keeper paid for their own calls. The two are shown apart and never netted
inside one column, as the cost ledger does today.

---

## The same session on a subscription plan

Now the user has a subscription plan, not an API key. The host reads the credential type and sets:

```json
"billing": {"mode": "subscription", "plan": "max-5x", "account": "acct-9c1e"}
```

**The operations do not change.** Every operation above, and every token count that `ops.Price`
returns, is the same. Only the host's conversion changes (design §7.4).

The host converts tokens to **usage-limit units**: one unit is one input token. It uses the default
weights (input 1, cache write 1.25, cache read 0.1, output 5), so every figure is marked
`Estimated`.

| Item | API user | Subscription user |
|---|---|---|
| Turn 5 saving | +$0.03012 | 11,640 × 0.1 + 7,100 × 1.25 = **10,039 units** (API equivalent $0.03012) |
| Session savings | +$0.48267 | **160,890 units** (API equivalent $0.48267) |
| 3 keep-alive pings, on the user's credential | −$0.03125 | **10,416 units** of the user's limit |
| Summarizer call, on the **operator's** API key | −$0.02361 | **−$0.02361 to the operator**, in dollars |

So the subscription user's net figure is two numbers, and they are not added together:

- **Usage-limit headroom gained:** 160,890 − 10,416 = **150,474 units**, about 50 turns of 3,000
  input tokens.
- **Operator money spent:** **$0.02361**, for the summarizer call.

The summarizer call names its payer:

```json
{ "id": "op31", "kind": "spend", "producer": "context-guru", "component": "summarize",
  "produced": ["cg:summarize:ab12"],
  "call": { "...": "the same usage as above",
            "paidBy": {"billing": {"mode": "api"}, "operator": true} } }
```

A ping on the user's own credential names the user's subscription, inside its `call`:

```json
"call": { "...": "the ping's usage",
          "paidBy": {"billing": {"mode": "subscription", "plan": "max-5x"}, "operator": false} }
```

The API-equivalent dollars are labelled as an equivalent, and they never enter `TotalAvoidedUSD` or
the cost ledger's money columns.

---

## Summary of the model, as the example uses it

| Field | Used for |
|---|---|
| `kind` | `reduce` saves; `add` costs; `stateChange` moves tokens between tiers; `ping` and `spend` carry a call's billed usage; `cacheBreak` moves tokens from read to write |
| `session`, `thread` | totals for each session, each thread, each subagent |
| `Input.State` | the tier of every change: hot (read) or cold (write) |
| `at`, `spans` | the tier, compared with B\* and with the last breakpoint `L` |
| `Input.Breakpoints` | where the provider can read and write; the size of a break penalty |
| `before`, `after` | the token change, measured at the producer's own input (the client size for a summary) |
| `changeId` | sum one change across requests; join a ping with its rescue; join a penalty with its cause |
| `firstApplied` | count fresh acts; mark where payback starts |
| `supersedes`, `inherited` | end earlier changes; show how much a summary took over |
| `structure` | move later positions after a summary |
| `call` | the billed usage of a ping or a model call; the host prices it with the savings |
| `produced` | join a call's cost to the change it paid for |
| `attributedTo` | which change a `cacheBreak` is charged to; absent means nobody |
| `billing` (`RequestOps`) | how the host values the tokens: money, or usage-limit units |
| `call.paidBy` | who paid for a spend or a ping: the user's plan, or the operator's key |
