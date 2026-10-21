# Context operations: counterfactual accounting of cost and savings

**Status:** proposed.
**Date:** 2026-10-18.
**Worked examples:** [`2026-10-18-context-operations-example.md`](2026-10-18-context-operations-example.md).
Start with the basic example. Advanced examples follow in the same file.
**Wiring in hosts:** how Cortex and the context-guru proxy connect to this model is in a separate
document (moved out after review).

## Overview

**In one paragraph.** When a context operator shrinks the context sent to a model, we want to say how much money
that saved. Today every operator answers on its own, in its own way, so the answers disagree and nobody
can check them. This design gives every operator one small record of what it changed, and one shared
calculation that turns those records into tokens and money.

**The story.**

1. **A change is recorded as a fact.** A context operator that shortens a tool output says: "I took 7,000 tokens
   out of message 9." It says nothing about money. We call this record an **operation**.
2. **The same tokens cost different amounts on different days.** A provider keeps the start of your
   prompt in a cache. Reading from the cache is cheap (about a tenth of the normal input price). Writing
   is expensive (more than the normal input price). So a token costs very different amounts
   depending on whether the cache is warm or cold.
3. **So a saving needs a "what if".** The saving is the bill we would have paid without the change,
   minus the bill we paid. To find the first bill we ask: where would these tokens have been billed
   if nobody had touched them? That is the **counterfactual**.
4. **One saving is spread over many requests.** The 7,000 tokens stay out of every later request.
   The value changes with each request, because the cache is warm on one and cold on another. We
   add them up over the **session**.
5. **One library does the calculation.** It takes operations and the cache facts, and returns tokens
   for each billing class. Each host turns tokens into money with its own price list. The result is
   the same tokens for the same request, wherever it is computed.
6. **The library is shared.** It is a third-party library for context-guru and Cortex, and we hope
   other AI frameworks use it too. It does not depend on context-guru or on Cortex. Anyone who can report
   operations and the cache facts can get the same tokens.

**Some changes cost money first.** Not every operation removes tokens. Some make a model call, and
that call is a cost. We record it as a **spend** operation. It carries the tokens the provider
billed for the call.

- **Summarization.** A summarizer reads a long part of the context and writes a short summary. The
  call costs money (the spend). The summary then replaces the long part in every later request (the
  saving). The spend names the change it produced, so the cost and the saving are added together.
  The summary pays back only if the saving is larger than the call.
- **Keep-alive.** A keep-alive sends a small request while the user is idle, so the provider does
  not drop the cache. The request costs money (the spend). Its value shows later, on the next real
  request, which reads a warm cache and not a cold one. The spend and the later rescue share one
  ID, so they are added together. If the user never comes back, the spend has no rescue, and the
  total for that keep-alive is negative.

In both cases the cost and the benefit land on different requests. This is why a spend is a
separate kind of operation, and why we add up over the session.

**A picture of the flow.**

```
context operators ──► operations (facts, no money)
                                        │
        cache facts (hot / cold) ───────┤
                                        ▼
                               one shared library
                                        │
                              tokens for each billing class
                                        │
                      each host's price list ──► dollars, per change / thread / session
```

**Read next.** The basic example in
[`2026-10-18-context-operations-example.md`](2026-10-18-context-operations-example.md) shows the idea
with three requests and one change. The rest of this document is the reference: the problem (§0),
the rules (§1 to §3), the model (§4), the calculation (§5, §6), and the checks (§8, §11).

**Words used here.**

| Word | Meaning |
|---|---|
| Context operator | Anything that changes the context sent to a model or the cache around it: a summarizer, an output filter, a tool pruner, a keep-alive. |
| Operation | A record of one change a context operator made to the context. It has sizes and a position, and no money. |
| Tier | The rate class a provider bills a token at: input, cache write (5m or 1h), or cache read. |
| Cache state | Hot (the provider still holds the start of the prompt), cold (it does not), or unknown. |
| Counterfactual | The bill we would have paid if the change had not been made. |
| Spend | An operation that is a model call (a summarizer call, a keep-alive request). It carries the tokens the provider billed. It is a cost, and its benefit may come later. |
| Thread | One conversation inside a session: the main one, or a subagent's. |
| Session | All the requests of one working session. The period over which we add up. |

## 0. The problem

Context engineering changes what is sent to a model. Examples are summarization, tool-output
reduction, tool pruning, cache splitting and keep-alive. Each change has a cost and a saving.
Today each component computes its own cost and savings.

This causes three problems:

1. **The rules are duplicated.** Each component has its own pricing rule. Example: `extract_llm`
   tail and `cache_aware_summarization` both reduce context, but each prices its own saving in its
   own way. A rule that is wrong in one place is not found by checking another place.
2. **The figures cannot be compared.** Two components can count the same tokens in different ways.
   Nobody can verify one figure against another.
3. **A change to one rule is a change to every component.**

Two things make a correct calculation hard:

- **Aggregation over a period.** A saving is not one event. A change removes tokens from every
  later request that still carries it. To total it, we need one definition of the period. In this
  design the period is the **session**. A thread is a breakdown of a session.
- **Counterfactuals.** A saving is the difference between what happened and what would have
  happened without the change. We must compute the alternative bill. It depends on the cache
  state, and a change can alter the cache state itself.

This document defines one model for the operations, and one way to compute their cost and savings.

### The selected solution

- **Context operators report operations.** Each one reports facts about what it changed, as sizes and
  positions. No money, no rates.
- **One library prices them in tokens.** It is a standalone, third-party library for context-guru,
  Cortex and, we hope, other AI frameworks. It is a pure function. It decides which tier every token
  falls in, using the cache state of the request. It is the only place that holds this logic.
- **Each host keeps its own rates.** A host multiplies tokens by its own price list. Two hosts get
  the same tokens for the same request, and may differ in dollars when their price lists differ.
- **Totals are sums over a session.** A change's total is the sum over the requests that carry it. A
  thread's and a session's totals are sums over their operations.
- **A list of test cases defines correct.** We build it from the current computation before any host
  uses the library (§11).

## 1. Goal

- Each operator reports the **operations** it performed on the context, as **facts**.
- **One library**, standalone and usable by any AI framework, turns operations into **tokens for each billing tier**.
  A **tier** is the rate class a provider bills a token at: input, cache write (5m or 1h), or cache
  read.
- **Each host** converts tokens to dollars with its own rates, and records them. There are two
  hosts: **Cortex** (`pricing.Resolver`) and the **context-guru proxy** (`internal/modelinfo`).
  For the same request, both get the same tokens for each tier.
- **What is centralized is the logic, not the rates.** The `ops` library decides which tier every
  token falls in, in one place. The dollar rate for each tier stays in each host's own rate table,
  by design: Cortex has discounts, overrides and long-context tiers that the proxy does not. So the
  two hosts report the same tokens and can report different dollars for the same request, when
  their tables differ. Equal dollars are a property of the test cases, which use one fixed table
  (§11.2), not of production.
- An **exhaustive list of test cases**, made with the current saving computation, validates the
  implementation before any host uses it (§11).
- The model supports savings **across requests**: removing K tokens from message *i* saves on
  every later request that still carries the removal.
- context-guru is **one plugin in a longer pipeline**. Other plugins (tool-prune, cpex,
  inference-router) also change the request. The model covers all of them.
- Every figure uses the **cache state** of the request: hot, cold, or kept hot by a keep-alive.
- The model covers the operations that change the **cache state**, not only the content: keep-alive
  pings, `cachesplit`, and the TTL of a breakpoint.
- A later operation can **end** the effect of earlier ones. Example: a summary replaces messages
  that `mask` had already reduced.
- Every figure can be summed **for each session and for each thread**, so a subagent's saving is
  separate from the main thread's.
- The model serves **API users and subscription users**. An API user pays for each token, so a
  saving is money. A subscription user pays a fixed price and has a usage limit, so a saving is
  usage-limit headroom. The operations and the library are the same for both; only the conversion
  to a value differs (§7.4).

## 2. Decisions

| # | Question | Decision |
|---|---|---|
| D1 | Does the library need to know that a change is applied for the first time? | **No.** The tier comes from the position and the cache state (§6.2). A report finds the first application as the first event with that `ChangeID`. The library stays stateless, and a stateless operator (tool-prune) reports the same fields as any other (§4.7). |
| D2 | Where is the savings logic? | **In one standalone library,** a third-party library for context-guru and Cortex, and open to other AI frameworks. Hosts call it and keep no operator-specific pricing rules. |
| D3 | What does the library return? | **Tokens for each tier**, not dollars. Cortex prices them with its own rates (discounts, overrides, long-context tiers), so a saving and the request's spend use the same rates. |
| D4 | How is cg's own spend recorded? | **As operations, in the same report as the savings.** A `spend` operation carries the provider's billed usage of the call (`Call`). The host prices it with the savings, in one path. There is no second record and no `CostRecorder` (wiring document). `ops.Price` does not price the call; it returns `Call.Usage` for the host to price (D3). |
| D5 | Shape | **One typed operation for each change unit**, in-process only. The recorded summary (`event.Saving`/`ChangeSaving`) has the aggregate fallback above a limit, not the operation list itself (R20; the recorded form is in the wiring document). |
| D6 | How is a saving across requests counted? | **Realized on each request, not projected.** Each request lists every change in its body, fresh or replayed, and is priced from that request's own cache result. The operator remembers past changes, not the library or the host (§4.7). |
| D7 | Where do the types live? | **A standalone module with no dependency on context-guru, on Cortex, or on any framework.** context-guru and Cortex are two users of it, and other frameworks can be too. The module path `github.com/rossoctl/context-guru/ops` used below is a placeholder; its home and name are not decided. |
| D8 | Which hosts compute dollars from operations? | **Both: Cortex and the context-guru proxy.** The proxy stops using its own saving rules (`dash/event.go`) when it switches. The comparison of request fingerprints (§5) is a pure function in `ops`, so the proxy does not need Cortex to find B\*. |
| D9 | How is the implementation validated? | **Against an exhaustive list of test cases made with the current computation** (§11), before any host switches. |
| D10 | How is the cache state used? | **As an input to every price.** The tier of a content change depends on the state (§6.2). An operation that changes the state (`cacheChange`, §4.4) is priced as the difference between two states. |
| D11 | What happens to a change that a later change ends? | **Its operator stops reporting it.** No link names the change that ended it. The snapshot rule (D6) and the measurement at each operator's own input (R29) give the right price and stop any double count. Each change keeps the total it had until then (§4.5). |
| D12 | Where does the thread ID come from? | **From the engine's `internal/thread`, in both hosts**, copied onto every operation. The host keeps one fingerprint per thread, keyed by `thread.Key`, and compares each request with its own thread's previous request (§5.3). |
| D13 | How does the model serve a subscription user? | **The library does not change.** It already returns tokens for each tier (D3). The request carries a `Billing` fact. The host converts tokens to dollars for an API user, and to usage-limit units for a subscription user, with an API-equivalent dollar figure beside it, marked as such (§7.4). |

## 3. Requirements

The numbers are the order in which the requirements were found, so they are not in sequence.
R35 to R39 were added after the first review, R40 to R43 for subscription plans, R44 and R45 after a
second review that checked the model against the real engine and both hosts (§13), and R46 and R47
from a request to trace every figure to its rules and to cover every model call.
**R13 is withdrawn** (see D). **R10 is withdrawn.** It asked for a flag that separates a replay from a fresh act. The position chooses
the tier, so no flag is needed (§4.2 rule 3).

### A. Boundary

- **R1.** Operations are facts. They carry no rates, no dollars and no tier choice. The one
  exception is a dollar cost that a call's own response reported (R49).
- **R2.** Operations carry no content: no message text, no clipped before/after strings, no
  credentials. They reach the unauthenticated `:9094` API and the session archive. Content identity
  uses hashes only.
- **R3.** The model and the library are in one module. Both repos import it. The module must not
  import either host or any framework, so a third party can use it. Its public types are named for
  the model, not for context-guru or Cortex. A contract test in
  each repo pins it.
- **R26.** The library is a pure function: no state, no I/O. The context-guru proxy and Cortex
  then compute the same figure for the same request.

### B. Coverage

- **R4.** Every operation that changes money is covered, not only reductions: reductions,
  additions, cache breaks, model calls, restores.
- **R48.** An operation that no operator in the pipeline made is also recorded. Example: the
  client's own compaction. It is logged **once**, as an `external` operation with its own ID.
  Operations it affects refer to it by that ID. It is not copied per affected operation, so a total
  counts it once. The model can only infer it (the body shrank with no matching operation), so it
  is marked as inferred.
- **R49.** A `spend` operation names the model the call used. It can differ from the agent's model.
  If the call's response states a dollar cost, the operation carries it as reported, inside the
  call's usage, beside the tokens. The host uses the reported cost when it is present, and prices the tokens with its own
  rates for that model when it is not. Tokens stay the base fact, so the library still returns the
  same tokens in every host.
- **R5.** Spend that happens outside a request (a keep-alive ping, a detached summarizer call)
  carries its own attribution. It uses the same operations and the same recording path as spend
  inside a request (wiring document).
- **R47.** A model call that any plugin makes outside the listener is a `spend` operation, not
  only context-guru's. Example: the `ibac` judge calls a model through `core/llmclient`, so the call
  is not in the ledger today. It is reported the same way as the summarizer (wiring document). Its cost is not
  joined to a saving, because it produces none: `Produced` is empty.
- **R6.** An operation has an ID that is unique inside its request. Results, check failures and
  `cacheChange` attribution refer to it. A reference to a change in another request uses `ChangeID`
  (R22), not the operation ID.
- **R27.** The model is not specific to one operator. Each operation names its operator and its
  pipeline stage.
- **R28.** One collector for each request collects the operations of all plugins, in order, and
  calls the library once.
- **R33.** A `rewrite` that makes the context larger is a cost: an injected tool declaration, injected text, a
  restore.
- **R34.** A change of a request setting is a separate kind, `setting`: the model, the host, or any
  param. Example: a session moves from one model to another, or from low to high effort. It is
  recorded as a fact, because it explains later figures. One operation is one decision and holds
  any number of settings, as a map from the setting name to its `from` and `to` values. Example: a
  router moves a request to a smaller model and a lower effort in one operation. A setting that
  appears or is removed has a nil `from` or `to`. Settings changed for different reasons, or by
  different operators, are separate operations. The saving or cost of the change itself is **not computed** (R50).
- **R50.** The library does not price a param change. Providers do not price `effort` or `thinking`
  per token. These params change how many output tokens the model writes, and sometimes the cache.
  The saving cannot be computed from facts, for four reasons:
  1. *The alternative is not observable.* The tokens the model would have written at the other
     setting were never generated.
  2. *The effect is not fixed.* It depends on the task, the model and the turn.
  3. *Quality can change.* A lower setting can need more turns, so a saving on one request can be a
     loss over the session.
  4. *The cache can break.* On both Anthropic and OpenAI, a change can invalidate cached content.

  What the library does instead: it keeps the `setting` operation; it records a `cacheChange` with
  cause `setting` when a request after the change is cold (§5.4), which is measurable; and a report
  can show output tokens per request before and after, labelled as an observation, never in
  `TotalAvoidedUSD`. A measured saving needs a control (a holdout that keeps the old setting). That
  is out of scope (§10).

### C. Facts for pricing

- **R7.** Each operation has a position: section and index, in the provider's hash order.
- **R8.** Sizes are in bytes, and in tokens with the tokenizer named. An operator with no tokenizer
  sends a byte estimate and marks it (§4.2 rule 8).
- **R9.** A reduction carries hashes of the content it removed.
- **R11.** Model-call usage is the provider's billed usage, read from the call's own response by
  the model client the host gives the engine. The engine does not count it (wiring document).
- **R22.** A change has an ID that is the same on every request that carries it (`ChangeID`).
- **R23.** A one-time cost (a cache break) is linked to the change that caused it, so payback can
  be computed.
- **R24.** Cache facts are scoped to the conversation the provider cached, not to the session as a
  whole. Subagents share a session but not a cache entry.
- **R29.** Each operator's change is measured against that operator's own input.
- **R30.** Cache facts come from the forwarded body, not from one plugin's output.
- **R31.** A cache break with a cause outside the plugins (client edit, expiry, first turn) is not
  charged to a plugin.
- **R32.** Positions resolve in the forwarded body. An operator that adds or removes messages
  reports that.
- **R36.** Every price uses the request's cache state: hot (the previous entry was read), cold
  (expired, or the first request), or rescued (a keep-alive kept the entry hot). The same change
  has a different value in each state.
- **R37.** Cache breakpoints are an input. The provider writes and reads the cache only at a
  breakpoint, so the tier of a position and the size of a cache-break penalty depend on where the
  breakpoints are, and on their TTL.
- **R38.** Operations that change the cache state are covered: a keep-alive ping, `cachesplit`, and
  a breakpoint TTL change. Each one is priced as the difference between two states.
- **R45.** The cache state distinguishes a proven expiry from an unproven floor. A provider whose
  TTL is only a guaranteed minimum (no documented maximum) never scores `cold` from elapsed time
  alone — that would invent a saving the provider may not have billed. See §5.4.

### D. Status and correctness

- **R12.** An operation is applied or a `try_run`. A `try_run` is a change that a component
  computed but did not send: the original body went upstream. Example: a component in trial mode
  finds that it can cut 3,000 tokens from a tool output. The request still carries all 3,000 tokens.
  The operation records the 3,000 as "would have saved". Only applied operations count in totals. A
  change that is not in the forwarded body (reverted by the pipeline) is not reported.
- **R44.** A change the operator made but the WRITEBACK silently discarded (bifrost cannot
  round-trip the message, so the splice is dropped — issue #32) is counted, not treated as absent.
  See §5.1a.
- **R13 is withdrawn.** It asked for a reversibility class and a stash key. No pricing rule, check
  or attribution reads them, and the expand loop finds stashed content by its marker in the body.
  cg's component API keeps the safety rule that a lossy offload must stash its original.
- **R14.** Changes are additive. For each section, the applied changes add up to the client body
  minus the forwarded body. The library checks this.
- **R15.** Each request is complete without history.
- **R16.** Operations are in the order they ran.
- **R25.** When two operators change the same content on one request, each one is credited for
  its own change, in pipeline order.
- **R35.** A later change can end earlier ones (a summary of reduced messages). The model shows
  this by the earlier change not being reported, not by a link. Their totals until then do not
  change (§4.5).

### E. Operability

- **R17.** Each operation names its operator in one field, as a dotted path: `context-guru.mask`,
  `tool-prune`. The first part is the product or plugin, and the framework sets it, so a plugin
  cannot report under another plugin's name. A later part, if any, is the operation type. The
  taxonomy of an operator (what kind of change it makes, and what made it decide) depends on the
  name only, so a report maps the name to it with a table in cg. The operation does not carry it.
  Example: `context-guru.mask` truncates old tool output; it decides by age and size. Parts must
  not contain a `.`. The allow-list and the per-operator breakdown match on the first part.
- **R18.** Each request carries request ID, session, thread (if known), agent, **model** and
  **params**. The model, and the parameters that change what a call costs, are facts of the
  request. Examples of params: `effort`, `thinking` (the thinking budget), `max_output`,
  `service_tier`. The library does not fix the list. A key it does not know is kept and passed on,
  and it is never dropped (R19).
- **R46.** Each recorded saving names the version of the `ops` rules that computed it
  (`Saving.OpsVersion`). An operator can then trace a figure to the rules and the case list that
  define it. A change to the rules changes the version, so figures from two versions are not
  compared as if they were the same.
- **R39.** Each operation carries its session ID and thread ID, so every figure can be summed for
  one session, for one thread, or for one subagent.

### F. Billing plans

- **R40.** Operations and the library are independent of the billing plan. They carry tokens and
  tiers only. The plan is a fact on the request, not a field of any operation's price.
- **R41.** Each request names its billing: `api` (pay for each token) or `subscription` (fixed price
  with a usage limit), and the plan when it is known.
- **R42.** A `spend` operation is priced with the billing of its request. The payer of a call is
  the payer of the request (§10, known limitation).
- **R43.** A subscription saving is never reported as money saved. It is reported as usage-limit
  headroom, and an API-equivalent dollar figure can be shown beside it, labelled as an equivalent.
- **R19.** The model is versioned. An unknown `Kind` is kept and reported as unpriced, never
  dropped.
- **R20.** `RequestOps`/`Operation` is request-scoped and in-process only: input to `ops.Price`,
  discarded once the request's `event.Saving` is written. It is never recorded (R2's `:9094` and
  archive concern, and the session store's own size history, apply to what IS recorded —
  `event.Saving` and `Saving.Changes`/`ChangeSaving` — not to the raw operation list). That
  recorded summary has a size limit, with aggregation above it (wiring document).
- **R21.** Second-order effects are not operations. Example: the agent compacts later because its
  requests are smaller.

## 4. The object model

### 4.1 Types

```go
package ops // github.com/rossoctl/context-guru/ops

// RequestOps is the operations of one request, or of one call made outside a request.
type RequestOps struct {
    Version   int
    RequestID string
    Session   string
    Thread    Thread
    Agent     string
    Model     string            // the model the request was sent for
    Params    map[string]string // other settings that change the cost: effort, thinking, max_output, ...
    At        time.Time
    // Billing prices every operation of the request, a `spend` included (R41, R42, §7.4).
    Billing Billing
    // Ops is in-process only: input to ops.Price for this request, never recorded (R20).
    Ops []Operation // in pipeline order
}

// Billing is a fact the host reads from the request's credential, not a price.
type Billing struct {
    Mode    BillingMode // api | subscription | unknown
    Plan    string      // the plan name when the host knows it, for example "max-5x"; "" otherwise
    Account string      // a hash of the account the credential belongs to; never the credential
}

// ModelCall is one upstream call an operator or the keeper made. The host's model client fills it
// from the call's own response, so the usage is what the provider billed.
type ModelCall struct {
    Host, Model string
    Params      map[string]string // the call's own settings: effort, thinking, ...
    Usage       CallUsage     // everything the call's response bills, as billed
    Status      int           // upstream HTTP status
    Duration    time.Duration // 0 when not measured
    Detached    bool          // finished after the request it served was answered
}

// CallUsage is the whole bill of one call, as its response states it: the prompt side (Usage), the
// output tokens, and the dollar cost when the response states one (some gateways do). The cost is
// a fact the provider or gateway reported, not a rate of ours (R1, R49).
type CallUsage struct {
    Usage                // input, cache read, cache write (and its 1h part)
    Output  int          // output tokens, including the model's reasoning or thinking tokens
    CostUSD float64      // the cost the response stated
    HasCost bool         // false when the response stated none; CostUSD is then 0
}

// Thread identifies one conversation inside a session: the main thread, a subagent, or a fork.
// Each thread has its own provider cache entry.
type Thread struct {
    ID     string       // "main" for the primary thread
    Source ThreadSource // header | prefix | new
    Parent string       // the thread this one was forked from; "" if none
}


type Operation struct {
    ID        string // unique inside the request only; across requests, use ChangeID (R6)
    Kind      Kind   // rewrite | cacheChange | spend | setting | external

    // Session and Thread are copied from RequestOps by the framework. They are on every operation
    // so a store of operations can be summed for one session or one thread with no join.
    Session string
    Thread  string

    Operator  string // "context-guru.mask", "tool-prune"; the first part is set by the framework
    Stage     int    // set by the framework

    TryRun    bool // set by the framework: computed but not sent (R12)
    Discarded bool // set by the framework: the writeback could not splice this change (R44, §5.1a)

    At     Position // first position the change touches, in this operator's output. For a cacheChange with a Cause: the first position where the forwarded body differs from the previous request's. It is a cause, not the start of the penalty (§6.2)
    Spans  []Span   // every touched range, when the change is not contiguous; nil otherwise
    Before Size     // what the operator received over the touched ranges; the sum over Spans
    After  Size     // what it sent on over the same ranges

    // kind = rewrite | cacheChange
    ChangeID     string   // the same on every request that carries this change
    ContentKeys  []string // hashes of removed content

    // kind = cacheChange (§4.4): a prefix range moves from one state to another. A rescue (cold to
    // hot) comes from the keeper or `apply`. A break (hot to cold) is written by the collector and
    // has a Cause.
    StateFrom CacheState
    StateTo   CacheState

    // kind = rewrite: set only when this rewrite is a restore
    Reverses string // ChangeID of the removal this restore adds back

    // kind = spend
    Call     *ModelCall // the call, with the provider's billed usage (wiring document)
    Produced []string   // ChangeIDs this call created

    // kind = external: an operation the pipeline did not make, logged once; others refer to its ID
    Inferred bool

    // kind = setting: one decision, any number of settings. The key is the setting name: model,
    // host, or a key of Params (effort, thinking, ...). One key cannot appear twice.
    Settings map[string]SettingChange

    // kind = cacheChange, a break only (written by the collector only)
    Cause        Cause  // operator | client | expired | setting | unknown
    AttributedTo     string // the cause's operation ID, in this request; "" means nobody

    // a change to the message list, for mapping positions
    MessageSplice *MessageSplice
}

type Position struct {
    Section Section // tools | system | messages
    Index   int     // block or message index in that section
}

// Span is a range of positions in one section: [From, To], both in this operator's output.
// Before and After are the sizes over this range only. The operation's Before and After are the
// sums over its Spans.
type Span struct {
    Section       Section
    From, To      int
    Before, After Size
}

// SettingChange is one setting going from one value to another. A nil pointer means "not set":
// From nil is a setting that appears, To nil is a setting that is removed.
type SettingChange struct {
    From, To *string
}

type Size struct {
    Bytes     int    // always set
    Tokens    int    // required on rewrite; see rule 8
    Tokenizer string // the tokenizer that counted Tokens; "bytes-estimate" when estimated
}

type MessageSplice struct{ At, Removed, Inserted int } // messages only

// CacheState is the state of the thread's provider cache entry when a request arrives.
type CacheState struct {
    Phase    Phase         // hot | cold | unknown
    Extent   *Position     // the last position the entry covers; nil when cold or unknown
    Gap      time.Duration // time since the entry was last written or read
    TTL      time.Duration // lifetime of the entry: 5m or 1h, from its breakpoint
    // Lifetime says whether TTL is a proven expiry (exact) or only a guaranteed floor
    // (minimum): Anthropic's TTL is exact; OpenAI GPT-5.6+ states only a 30-minute minimum
    // with no documented maximum (R45, §5.4). Gap > TTL proves expiry only when exact.
    Lifetime LifetimeKind
}

type Kind string          // rewrite | cacheChange | spend | setting | external
type Phase string         // hot | cold | unknown
type LifetimeKind string  // exact | minimum | unknown — see apply.CacheLifetimeKind
type ThreadSource string  // header | prefix | new
type BillingMode string   // api | subscription | unknown
type Section string       // tools | system | messages
type Cause string         // operator | client | expired | setting | unknown
type Side string          // beforeBoundary | afterBoundary
```

The enums are strings, not integers, so an unknown value from a newer operator survives a decode
and can be reported as unpriced (R19).

### 4.2 Rules

1. **One operation for each change unit.** A change unit is what the operator decides, replays and
   reverses as one. A component that reduces 12 tool outputs with 12 decisions reports 12 `rewrite`
   operations, each with its own `ChangeID`. tool-prune removes the declarations of one account
   list in one decision, so it reports one operation for all of them. Its declarations are not
   next to each other, so it lists them in `Spans`. `At` is the first one.
2. **A replay is a new operation with the same `ChangeID`.** A replay's
   cost, if any, is a `spend` operation, not part of the reduction.
3. **The position chooses the tier** (§6.2), not the history of the change. A report finds the first
   request of a change as the first event with its `ChangeID` (§4.7).
4. **`Before` and `After` are measured at the operator's own input.** The direction of a `rewrite`
   is in the sizes, not in the kind. `After < Before` is a saving, `After > Before` is a cost (an
   injected tool declaration, a restore), and equal sizes change no token count. A restore is a
   `rewrite` with `Reverses` set, and its `After` is larger than its `Before`.
5. **Only the collector writes a `cacheChange` that has a `Cause` (a break).** A rescue has no `Cause`.
6. **A keep-alive ping is an ordinary `spend` operation.** Its `Call` carries the ping's billed
   usage. Its `ChangeID` names the keep-alive window, and the `cacheChange` operation on the
   request it rescued has the same `ChangeID`. So the cost and the value of a window add up
   (§4.4).
7. **`ChangeID` is derived from the operator and the original content key only — never from the
   replacement bytes.** This matches the engine's own stable key (`frozenKey(session, component,
   contentKey(original))`, `components/offload/state.go`) rather than adding to it. Two
   operators that remove the same content in different ways still get different IDs, because
   the operator name is part of the key.

   Replacement bytes were in an earlier draft of this rule, and that was a mistake: `mask.go`'s
   own comment states that `readlifecycle` and `skeleton` are **not** pure functions of
   (content, config) — `readlifecycle`'s marker text depends on later transcript events
   (`classify()`), and `skeleton`'s body depends on the paired tool call, not the message. For
   both, the same logical decision can produce different replacement bytes on different turns
   for reasons that have nothing to do with whether the change is new. Keying `ChangeID` on
   replacement bytes would churn the ID every time that happens — breaking the cross-request fold
   (wiring document) for exactly the two components the engine already flags as unusual.
   Keying on content only, as `frozenKey` does, is stable regardless.
8. **The tokens a change saved come from the operation's own `Size`, not from the forwarded body.**
   Removed tokens are not in the forwarded body, so nothing else can count them. An operator with a
   tokenizer (context-guru) counts them and names the tokenizer. An operator with none (tool-prune)
   estimates them from bytes with the ratio of its own input, and sets
   `Tokenizer = "bytes-estimate"`. The library marks every priced result that uses an estimate as
   `Estimated`.
9. **The engine reports a change that `apply` writes, under the operator that decided it.**
   `toolschema`, `toolfilter` and `cachesplit` are marker components: their `Reformat()` does
   nothing, and `apply` performs the rewrite (`apply/envelope.go`, `apply/prefixsplit.go`). `apply`
   holds the only exact numbers (`Trace.FilteredDeclTokens`, `Trace.SplitStableTokens`). So `apply`
   builds the operation and sets `Operator` to the marker component's name. The mixed-TTL head
   (`apply/headttl.go`) has no component; its operations use `Operator = "context-guru.headttl"`.
10. **A batched decision is one operation.** `coref` plans many cuts across messages that are not
    next to each other, gates them as one batch (`min_batch_frac`, `break_even`), and spends one
    unit of `rewrite_budget` for the whole pass. The batch is the change unit (rule 1): one
    `rewrite` operation, one `ChangeID`, one `Span` for each cut, and at most one attributed
    `cacheChange`. Reporting each cut as its own operation would split one rewrite penalty across N
    changes, and no single cut's payback would then be meaningful.

**A `setting` operation.** It holds all the settings one decision changed, as a map from the setting
name to its `from` and `to` values:

```json
{"kind": "setting", "operator": "inference-router",
 "settings": {"model":  {"from": "large-model", "to": "small-model"},
              "effort": {"from": "high",        "to": "low"},
              "thinking": {"to": "8000"}}}
```

`thinking` has no `from`: it was not set before, and it appears now.

### 4.3 Why positions are (section, index)

The provider hashes `tools`, then `system`, then `messages`. A comparison of two positions in that
order shows whether a change is before a cache boundary. For a change with `Spans`, each span is
compared on its own and priced with its own `Before − After`. So a change with spans on both sides
of B\* gets the right tier for each part. An operator reports positions in its own output. If a
later operator adds or removes messages, it reports a `MessageSplice`, and the library maps earlier
positions into the forwarded body.

### 4.4 Operations that change the cache state

A `cacheChange` operation does not change the content. It changes how the provider bills content
that is already there.

A `cacheChange` carries a prefix range and two states: `StateFrom` and `StateTo`. There is no
mechanism field. Three cases cover it:

- **A prefix is broken.** The provider no longer reads content it read before (`hot` to `cold`). The
  collector writes it, with a `Cause` and `AttributedTo` (§5.4). It is a cost.
- **A prefix is warmed.** The provider read the entry, so it did not expire. Examples: a keep-alive
  ping rescued the entry (`cold` to `hot`); a split of the system block means the stable half is
  read, not written, when the volatile tail changed.
- **A prefix is protected.** A breakpoint changes what is cached, or for how long. Example: the
  head of the request is held for 1h, not 5m. It has **two legs:** a cost on **every request that
  writes** the head at 1h (the premium), and a saving on each request that reads the 1h entry after
  a gap longer than 5m (the rescue).

**When the collector adds a `keepAlive` operation.** All four conditions must be true. They are the
conditions of the current `keepaliveSavedUSD`:

1. The keeper sent at least one ping for this thread during the gap before this request.
2. The gap is longer than the entry's TTL, so without a ping the entry would have expired.
3. The provider read more than it wrote.
4. The tokens are the smaller of what the last ping read and what this request read.

**The ping and the rescue share one `ChangeID`,** the ID of the keep-alive window. A window whose
pings rescued nothing has only `spend` operations, so its total is negative. That is the cost of a
ping that was not needed, and it is visible for each thread.

**`breakpointTTL` is a bet, and both legs are recorded.** The 1h write premium (2.0× input against
1.25× for 5m) is paid on every request that creates the head entry at 1h, whether or not a later
request ever reads it. The benefit appears only on requests that arrive more than 5 and less than
60 minutes later. Measured over 19,805 production requests, a blanket 1h head cost $773.00 extra on
14,499 creating requests to recover $754.66 on 290 — net −$18.34; size-gating turned it positive.
If the model priced only the reading request, every write that never paid off would be missing, and
the figure would overstate the bet. So, like a keep-alive window:

- **On each 1h-writing request,** `apply` reports a `cacheChange` for the head's breakpoint TTL
  and only a cost: `Caused.CacheWrite1h` for the head's tokens, less the 5m write it replaced. Its
  `ChangeID` names the head entry (`"headttl:" + Thread + ":" + head content key`).
- **On each request that reads that entry after the 5m TTL,** `apply` reports a `cacheChange` with
  the same `ChangeID` and the saving (write → read, §6.2).

A head entry that is written at 1h and never read again has only the cost leg, so its total is
negative — the same visibility a keep-alive window that rescued nothing has.

### 4.5 Changes that end

A change can remove content that earlier changes had already reduced. Example: on turn 12 a
summary replaces messages 0 to 8, and messages 3 and 5 were reduced by `mask` and `coref`.

**A change ends when its operator stops reporting it.** There is no field that names the change
that ended it. Nothing more is needed:

1. **The price.** Each request is a snapshot (D6). A change that is not reported on a request saves
   nothing on that request. Its total until then does not change, so a change that did not pay back
   before it ended shows a loss.
2. **No double count.** Each operator's change is measured at its own input (R29), and the sum
   check tests the total (§6.5, check 1). So the tokens are counted once, in either pipeline order:

   | Pipeline order | `mask` | `coref` | summary | Total |
   |---|---|---|---|---|
   | Summarizer first (cg's `medium`, `high`, `xhigh` presets) | not reported | not reported | 40,000 → 1,500 = 38,500 | **38,500** |
   | Summarizer after the reducers | 3,940 | 19,950 | 16,110 → 1,500 = 14,610 | **38,500** |

   The order changes **who gets the credit**, not the total. Credit follows pipeline order (R25).
   In cg's presets the summarizer runs first, so it gets all of it: the reducers find their content
   gone, and they report nothing.
3. **A restore.** If the summary is reversed (a restore, or the summary is dropped), the operator
   replays `mask` and `coref` again. They are reported with their old `ChangeID`.

**Why a change stopped** is not on the operation. A report can find it: the change last appeared at
position P on request N, and request N+1 has a change whose span covers P (`At` and `MessageSplice`).
That is a question for a report, not for a price, so the report answers it.

### 4.6 How operations reach the host

Moved to the Cortex wiring document (`2026-10-18-context-operations-cortex-wiring.md`). This document defines the operations and the pricing. How a host receives them is
not part of the method.

### 4.7 Replays: who remembers past changes

**Each request lists every change in its body, fresh or replayed (D6). The operator remembers past
changes.** The library and the hosts remember nothing except the fingerprints of recent requests
(§5.2). This is a decision, not an oversight, for three reasons:

1. **The operator already re-applies each change on every request.** The client sends its whole
   uncompacted transcript on every turn. To keep the forwarded body the same, context-guru replays
   each frozen decision, and tool-prune removes the same declarations. So the operator already knows
   every change in the body. Reporting it adds no state.
2. **The price changes on every request, even when the change does not.** The same tokens are a
   cache read on a hot turn and a cache write on a cold turn (§6.2). The library needs each change's
   tokens and position **on this request**, and positions move: a summary turns 9 messages into 1.
3. **A register of past changes elsewhere fails in each place it could live:**

   | Where | Why it fails |
   |---|---|
   | The `ops` library | It is no longer a pure function (R26). Two hosts can disagree, and pricing an old request again gives a different figure. |
   | The host's collector | It must move positions after each `MessageSplice`, find which changes a summary ended, and drop changes when the agent compacts its own transcript. It cannot see the content, so it cannot tell whether a change is still in the body, and a wrong guess is silent. |
   | The archive | It has gaps (`droppedEvents`). One lost request makes every later request wrong. |

**The cost is size, not correctness.** A replayed operation repeats about 200 bytes on every
request. If that becomes a problem, a later version can send a compact replay (`changeId`, `at`,
`tokens`) and the full operation only once. That change would not change the model or the price, so
it is not part of this design.

**A stateless operator.** tool-prune keeps no state. It reads its `remove` list from config and
removes the same declarations from every request. It reports every field of a replay without state:

| Field | Set by tool-prune? | How |
|---|---|---|
| `ChangeID` | **Yes** | A hash of the removed set and the config. It is the same on every request with the same config, so no state is needed. |
| `At`, `Spans`, `Before`, `After` | **Yes** | From the body it just changed: facts about this request. |

A reader that needs the first application finds it in the session history: the first event with that
`ChangeID`. No price needs it, because the tier comes from the position and the cache state (rule 3).

**A config change of a stateless operator.** A new `remove` list (for example, a hot reload) gives
a new `ChangeID`. The old change stops and a new one starts. If the new list removes a declaration
that the old list kept, the forwarded body changes inside the cached prefix. The collector then adds
a `cacheChange` attributed to tool-prune (§5.4). That is the real cost of the config change.

### 4.8 Sequence: three flows

Moved to the Cortex wiring document (`2026-10-18-context-operations-cortex-wiring.md`).

## 5. The collector

**Principle (review of #592).** Components push facts, and they do not hand them to something that
collects. Each pushed fact has an identity (a name plus labels, as in Prometheus: session, thread,
change). A push with a known identity is an update, not a new record. The receiver keeps no state
and prices nothing; totals are computed when someone asks. Two points are open (§12): where the
previous request's hashes live, and how many series the labels produce in a long session.

**The rest of this section, and §4.7, still describe the earlier design:** a collector that keeps
one fingerprint for each thread. They are kept until they are rewritten to the push model.

Each host has a collector: Cortex, and the context-guru proxy. The parts are split this way:

| Part | Where | Cortex | Proxy |
|---|---|---|---|
| Resolve the thread | `internal/thread` (engine) | the engine resolves it inside `apply`, as today, and returns it in `apply.Result.Thread` | the same |
| Collect the operations | host | `pctx.ReportOps`, from every plugin, in pipeline order (§5.1) | `apply.Result.Ops`: the engine is the only operator |
| Keep the previous request's fingerprint, per thread | host | a map keyed by `thread.Key(session, thread)` | the same map, beside `dash.Recorder`'s `lastTail` (§5.3) |
| Compare, and find B\*, the break and the cache state | `ops.Compare` (pure) | called after the response | the same call |
| Convert keep-alive reports | `ops.FromKeepAlive` (pure) | in `pingEmitter` | in `dashEmitter` |

```go
func Compare(prev *Fingerprint, cur Fingerprint, usage Usage) (boundary *Position, brk *Operation, state CacheState)
```

`prev` is the fingerprint of **this thread's** previous request, or nil. The host looks it up by
`thread.Key`; `ops.Compare` does no matching of its own.

### 5.1 Collecting

- Each plugin calls `pctx.ReportOps(ops...)` during its pass. The pipeline runs in sequence, so the
  list is in order.
- The framework sets `Operator` and `Stage`. A plugin cannot report under another plugin's name.
- The framework sets `TryRun = true` on every operation of a plugin whose `SetBody` returned
  false (`on_error: observe`).
- An operator reports only changes that are in the forwarded body, or that would be there for a
  `try_run` operation. A change the pipeline reverted (an error, a panic, the never-worse guard) is
  not reported. It saves nothing, as a change that ended (§4.5). Its model call, if any, is still a
  `spend` operation, so the money it cost is counted.
- The host keeps an explicit allow-list of operators, for the reason `savingComponents` has today:
  a plugin must not add itself to the money record only by reporting operations. Operations from
  an operator that is not on the list are kept on the event and marked unpriced.

### 5.1a Discarded, not reverted (R44)

A **reverted** change never touched the forwarded body: the pipeline undid it before anything else
ran, so "it saves nothing" is simply true. A **discarded** change is a different failure, and
treating it the same way reopens an incident this engine already had.

bifrost cannot round-trip an Anthropic `tool_use` block: it drops `id`, `name` and `input` on
unmarshal (`apply/metawrite.go`). So the writeback compares the pre- and post-pipeline marshal of
each changed message, and if splicing the component's change back in would lose a field the
original had, it throws the change away rather than ship a corrupted block. That rule is correct —
but it is also exactly where `cacheinject`'s breakpoints lived, because its targets are the
messages carrying `tool_use`. The component believed it changed 46 messages; the writeback forwarded
0 of them (issue #32). The component's own counters — `Report.Saved()`, `Report.ChangedIdx` — looked
identical to a working run, because they describe what the component decided, not what reached the
wire. `Report.Discarded` exists so a reader can compare it against `ChangedIdx` and see the gap.

If an `Operation` for a discarded change is simply never reported — the current wording of this
section — the new model reproduces the same blind spot `Report.Discarded` was built to close, with
no equivalent counter to catch it. So:

- **A discarded change IS reported**, with `Discarded = true` and `TryRun = false`. Its
  `Before`/`After` describe what the operator attempted, not what shipped.
- **It is not priced as a saving.** The sum check (§6.5, check 1) and conservation (check 6) treat
  it the same as a reverted or `try_run` change: left out, because its content did not change on
  the wire.
- **It is counted.** The host keeps a per-operator count of discarded operations,
  surfaced the way `Report.Discarded` is today — on `/v1/pipeline` or an equivalent metrics path —
  so an operator can tell "this component is declining" from "this component's writes keep being
  thrown away," which is exactly the distinction #32 took two benchmark studies to find without
  this field.
- **Its model call, if any, is still a `spend` operation** and is still priced: the money was
  spent whether or not the change survived to the wire.

```go
// on Operation, alongside TryRun:
Discarded bool // the writeback could not splice this change back (R44); never priced as a saving
```

### 5.2 The counterfactual boundary B\*

The provider reports this request's cache boundary as `cache_read`. But a plugin's cache break
moves it: in the coref example, `cache_read` stops at `messages[5]` because of the coref change.
Without it, the cache would have read through `messages[11]`. So the library needs **B\***, the
boundary that would have applied if no plugin had changed anything:

```
B* = min( prevCachedExtent,   // what this thread's previous request left cached
          clientFirstDiff )   // where the client changed its own body
B* = 0 when the cache expired
```

The collector keeps one fingerprint for each thread: the most recent request of that thread (§5.3).

```go
type Fingerprint struct {
    Thread    string
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

### 5.3 The thread, and the previous request

**The thread comes from `internal/thread`, not from the collector.** The engine already resolves
each request's thread (`thread.Result`): the agent's header first, then prefix continuity, then the
session as a last resort. It also returns two signals that a weaker rule would lose:

- `Disagree`: the header named a known thread, but prefix continuity matched a different one. The
  header wins; the host logs the drift.
- `Inherits`: a new thread whose content is mostly an existing thread's — a rewind, an edited
  earlier message, a fork. Its first request is that thread's next turn, not a cold start.

The engine runs inside both hosts, so both get the same `thread.Result` for the same traffic. The
operations copy `Thread.ID` (R39), and `RequestOps.Thread` carries `Source` and `Parent`
(`Parent = Inherits`).

**The previous request is this thread's previous request.** The host keeps one fingerprint per
thread, keyed by `thread.Key(session, thread)` — the key `dash.Recorder` already uses for
`lastTail` and `lastSeen`. A new thread with `Inherits` set starts from the inherited thread's
fingerprint, so a fork reads its parent's cache instead of scoring cold.

An earlier draft matched each request against the last 4 fingerprints of the session by longest
common prefix. That answered a weaker question. After interleaved subagent requests the best prefix
match can be an older request, and a main thread whose previous turn is more than 4 requests back
drops out of the window. `cachesplit`'s saving depends on comparing the volatile tail with **this
thread's immediately previous request** (`cachesplitSavedUSD`, `TailChanged`), so a stale match
would misattribute it. Keying by thread gives the right baseline for every rule that needs one.

### 5.4 Classifying a break

`fwdFirstDiff` is where this thread's previous forwarded body first differs from this request's
forwarded body. `clientFirstDiff` is the same comparison on client bodies.

| Comparison | Result |
|---|---|
| `fwdFirstDiff < clientFirstDiff` | A plugin broke the cache. Add `cacheChange{cause: operator, at: fwdFirstDiff}`, attributed to the earliest operation at or before `fwdFirstDiff`: its `ID` in `AttributedTo`. |
| `fwdFirstDiff ≥ clientFirstDiff` | The client changed its body. Add `cacheChange{cause: client}`, attributed to nobody. |
| A `setting` operation changed the model or a param (`effort`, `thinking`) since the previous request, and the cache did not hold | Add `cacheChange{cause: setting}`, attributed to the `setting` operation. If it changed several settings, the break names the operation, not one setting: the library cannot say which of them caused it. This is the one cost of a param change that can be measured. |
| No diff, but `cache_read` is far below the previous request's cached extent | The cache expired. Add `cacheChange{cause: expired}`, attributed to nobody. |
| No previous fingerprint for this thread | First turn of the thread with no `Inherits`, or a restart: `cause: unknown`. B\* is unknown (§6.4). |

The same comparison gives the **cache state** for `Input.State` (R36):

| Case | `Phase` | `Extent` |
|---|---|---|
| A previous fingerprint exists, and `Gap` ≤ its TTL | `hot` | the previous request's cached extent |
| A previous fingerprint exists, `Gap` > TTL, and a ping of this thread read the entry inside its TTL | `hot`, and the collector adds a `keepAlive` operation (§4.4) | the extent the ping read |
| A previous fingerprint exists, `Gap` > TTL, no ping, `Lifetime = exact` | `cold` | nil |
| A previous fingerprint exists, `Gap` > TTL, no ping, `Lifetime = minimum` | `unknown` | nil |
| No previous fingerprint for this thread | `unknown` | nil |

`Gap` is measured from the start of the last request or ping that touched the entry, because the
provider measures the TTL from the start of a request.

**Why `minimum` is not `cold` (R45).** `TTL` can be a proven expiry or only a guaranteed floor
(`apply.CacheLifetimeKind`: Anthropic's is `Exact`; OpenAI GPT-5.6+ states only a 30-minute
`Minimum`, with no documented maximum — "a minimum may prove an entry is still eligible, but
cannot prove it has expired"). On an `Exact` provider, `Gap` past `TTL` proves the entry gone, so
`cold` is correct and B\* = 0 is the honest answer. On a `Minimum` provider, the same `Gap` proves
nothing — the entry may still be live — so scoring it `cold` invents a cache-write saving that may
not exist, in the unsafe direction. The host already carries this distinction
(`keepaliveCreditFloorMs`: pre-5.6 OpenAI is never credited, exactly because `Minimum` cannot stand
in for proof of expiry). The collector sets `CacheState.Lifetime` from the same source and the
table above routes a `Minimum`-with-no-proof gap to `unknown`, not `cold` — §6.4 then applies
(`penaltyUnknown`), the safe direction.

### 5.5 Tokens for each position

These are the tokens of content that **was forwarded**: the library needs them for the penalty of a
cache break (§6.2), which counts forwarded content between the break and B\*. The tokens a change
**removed** are not in the forwarded body; they come from the operation's own `Size` (§4.2 rule 8).

Cortex has no tokenizer. The collector sends bytes for each forwarded position, and the library
converts them with the request's own ratio (billed prompt tokens ÷ forwarded body bytes), as
`pricing.EstimateTokensFromBytes` does today. Results that use the ratio are marked `Estimated`.

### 5.6 Limits

- **Memory:** 12 bytes for each position (`PosHash`, §5.2). 2 bodies × 2,000 positions is about
  48 KB for each thread. `internal/thread` caps threads at 64 for each session; a typical session
  has 1 to 5. `session.max_sessions` (100) bounds the total.
- **Restart:** fingerprints are lost. Only the next request of each session gets `penaltyUnknown`.
  They are not persisted (§10).
- context-guru keeps its own `MaxCachedIdx` for its own decisions. For pricing, the collector's B\*
  is the only authority (R30), because it is computed from the forwarded body after every plugin
  ran. In the proxy there is no later plugin, so the two must agree: the proxy checks
  `MaxCachedIdx` against B\* and counts a disagreement (a bug in one of them), at no extra cost.

## 6. The library

### 6.1 Call

```go
func Price(in Input) Result // pure

type Input struct {
    Ops         []Operation  // operators' operations + the collector's cacheChange
    Usage       Usage        // billed usage of this request
    Forwarded   []PosBytes   // every position of the forwarded body, in hash order
    Breakpoints []Breakpoint // cache_control breakpoints of the forwarded body, in hash order
    Boundary    *Position    // B*; nil = unknown
    State       CacheState   // the thread's cache state on arrival (§5.4)
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

**The state.** B\* comes from the state: B\* = `State.Extent` when `Phase = hot`, limited by where
the client changed its own body (§5.2). When `Phase = cold`, B\* = 0, so every position takes the
write or input tier. So the same 8,000 tokens are worth 8,000 × read on a hot turn and 8,000 ×
write on a cold turn.

**A break, and the breakpoints.** A `cacheChange` from `hot` to `cold`, attributed to an operation,
moves content from the read tier to the write tier. The content that moves is:

- **from** the end of what the provider still read on this request (`Usage.CacheRead`, mapped onto
  the forwarded positions),
- **to** B\*, as forwarded on this request.

The start is the read extent, not the position of the break (`At`, the first position that differs). The provider reads only up to a
breakpoint. If the last breakpoint before the break is the end of `system`, the provider reads only
`tools` and `system`, and every message before the break is written again too. So a break deep in
the messages costs the same as a break at `messages[0]` when there is no breakpoint between them.
The new tail after B\* is written anyway, so it is not counted.

**A rescue.** A `cacheChange` from `cold` to `hot` moves its tokens from the tier of `StateFrom` to
the tier of `StateTo`: for `keepAlive` and `cacheSplit`, from write to read. For `breakpointTTL` there
are two legs (§4.4): on a reading request, from write (the 5m entry would have expired) to read; on
every writing request, the premium alone, as `Caused.CacheWrite1h` minus the 5m write it replaced.

**The interaction.** On a rescued request, the content changes are priced in the hot state (rule
above). Without the ping they would have been priced cold, which is worth more. The difference is
neither the ping's nor the content changes' alone. The library reports it as
`Result.Interaction`, attributed to nobody, so the request total still equals the bill without
the operations minus the bill with them.

### 6.3 Output

```go
type Result struct {
    Version     int
    Session     string
    Thread      string
    Ops         []PricedOp     // one for each input operation, same order
    Interaction TierTokens     // §6.2: on a rescued request; attributed to nobody
    Unpriced []Unpriced     // with a machine-stable reason
    Checks   []CheckFailure // §6.5
    Boundary BoundaryInfo   // the B* used, and its source
}

type Unpriced struct {
    OpID   string
    Reason string // "unknown_kind", "negative_size", "operator_not_allowed", ...
}

type CheckFailure struct {
    Check    string // "sum" | "order" | "unique_change" | "spans" | "attribution" |
                    // "conservation" | "thread" (§6.5)
    Operator string // "" when the check is not about one operator
    Detail   string // numbers only, no content
}

type BoundaryInfo struct {
    Pos    *Position // nil when unknown
    Source string    // "collector" | "cacheRead" | "unknown"
}

type PricedOp struct {
    OpID      string
    Session   string
    Thread    string
    Operator  string
    ChangeID  string
    TryRun    bool
    Discarded bool // never priced as a saving (R44, §5.1a); still counted for diagnosis
    Saved     TierTokens // tokens the provider did not bill, by tier; never negative
    Caused    TierTokens // tokens the provider billed because of this operation; never negative
    Call      *ModelCall // spend only: copied from the operation, for the host to price
    Estimated bool
    // Reason explains the tier for a reader: "cached_prefix", "new_tail", "cold", "break_penalty",
    // "rescued", ... It is derived from the other facts, and no pricing rule or host reads it.
    Reason    string
}

type TierTokens struct {
    Input, CacheWrite5m, CacheWrite1h, CacheRead int
}
```

`Saved` and `Caused` are separate and both non-negative, because the money layer rejects negative
values (`pricing.MicrosFromUSD`). Cortex prices each side with the existing code.

A break penalty has both sides: the tokens are written (`Caused.CacheWrite5m`) instead of read
(`Saved.CacheRead`). The net is the rate difference, with no special rule.

A `cacheChange` is priced as its own operation. On its `PricedOp`, `ChangeID` is the `ChangeID` of
the operation named by `AttributedTo`, so the host adds the penalty to that change's total without
a lookup. The input carries only `AttributedTo`; the library copies the `ChangeID` once.
`spend` operations are not priced in tokens. `PricedOp.Call` returns their `Call.Usage`
unchanged, and the host prices it with the same resolver as the savings (wiring document).

**Conservation.** For each request, the priced `Saved − Caused` of all content changes, cache
breaks and state changes, plus `Interaction`, equals the bill of the client body in the state it
would have had without any operation, minus the bill of the forwarded body. The library checks
this in tokens (§6.5, check 6).

### 6.4 Unknown boundary

If `Boundary` is nil, the library uses the extent `cache_read` covers as the boundary, charges no
penalty, and marks the result `penaltyUnknown`. This reports too little, which is the safe
direction.

### 6.5 Checks

The library reports failed checks in `Result.Checks`. A failure does not stop pricing.

1. **Sum:** for each section, the operations that are applied, not `try_run`, and not discarded —
   `Before − After` adds up to the client size minus the forwarded size. The collector sends both.
   A failure means a plugin changed the body and did not report it.
2. **Order:** stages do not decrease.
3. **Unique `ChangeID` inside a request.**
4. **Spans:** for an operation with `Spans`, its `Before` and `After` equal the sums over its spans.
5. **Attribution:** each `cacheChange` names at most one operation, and that operation is at or
   before the break.
6. **Conservation:** the request total of the applied operations equals the bill difference (§6.3).
   `try_run` operations are left out, because their content went upstream unchanged.
7. **Thread:** every operation has the request's `Session` and `Thread`.

## 7. Recording the result

### 7.1 to 7.3

Recording the result in Cortex and in the context-guru proxy moved to the Cortex wiring document (`2026-10-18-context-operations-cortex-wiring.md`).
The size limit on recorded changes and the totals over a session or thread are defined there.

### 7.4 Billing plans

`ops.Price` returns the same tokens for each tier for an API user and for a subscription user. The
host converts them to a value by the request's `Billing.Mode`:

| `Billing.Mode` | Value of a saving | Value of a spend | Shown as |
|---|---|---|---|
| `api` | tokens × the provider's rates | `Call.Usage` × the provider's rates | money saved; money spent |
| `subscription` | tokens × the plan's **weights**, in usage-limit units | `Call.Usage` × the same weights | usage-limit headroom; an API-equivalent dollar figure beside it, labelled "API equivalent" |
| `unknown` | as `api`, with every figure marked `Estimated` | as `api` | money, marked estimated |

**Weights.** A weight says how much one token of a tier counts against the usage limit, compared
with one input token. The provider does not publish weights for each tier. So the default weights
are the ratios of the API rates (input 1, cache write 5m 1.25, cache write 1h 2, cache read 0.1,
output 5), and every subscription figure is marked `Estimated`. A host can set measured weights in
its config (§12, question 3).

**Keep-alive on a subscription.** A ping on the user's own credential uses the user's usage limit,
not money. The keep-alive window still adds up (§4.4), in usage-limit units.

**Detection.** The host reads `Billing.Mode` from the credential type on the request: an API key
means `api`; an OAuth token from a subscription login means `subscription`. If it cannot tell, it
uses `unknown`. Cortex and the proxy must use one rule, so the rule is a function in `ops`:
`ops.BillingFrom(headers)`. It reads only the credential type and returns no part of the credential
(R2). Both hosts must call it on the **client's** upstream credential header (`x-api-key` or
`Authorization`) before anything rewrites it — not on the proxy's control-plane login
(`proxy/authmode.go`), which is a different credential. A contract test in each host feeds the same
captured headers and expects the same `Billing.Mode`.

**Recording.** `event.Saving` gains `Basis`: `usd` or `subscriptionEquivalent`. `TotalAvoidedUSD`
and the cost ledger count only `Basis = usd`. A subscription saving goes in a new usage-limit
column, so API-equivalent dollars never enter a money total.

## 8. Errors

The rule: an error in savings never changes the request and never invents money. If a fact is
missing, report too little and say why.

| Situation | Result |
|---|---|
| `ops.Price` panics | The host recovers, writes no saving for the request, and increments `ops_price_panics`. The request and its spend record are unchanged. |
| A malformed operation (unknown kind, negative size) | Goes to `Unpriced` with a reason. The other operations are priced. |
| Sum check fails | Pricing goes on. The failure is counted for each plugin on `/v1/pipeline`. |
| No previous fingerprint for the thread | `Boundary.source = unknown`, §6.4. |
| Response has no usage | Nothing is priced, as today. |
| No rate for a tier | `Saving.Provenance` is empty, as today. Tokens are on the record. |
| Operator not on the allow-list | Kept on the event, unpriced. |
| `on_error: observe` | `TryRun = true`. Priced and shown as "would have saved", never in totals. |
| The writeback cannot splice a change back (bifrost tool_use round-trip) | `Discarded = true`. Not priced as a saving; counted in `DiscardedCount` so the gap is visible, not silent (R44, §5.1a). |
| `Billing.Mode` is `unknown` | Priced as `api`, every figure marked `Estimated`. |

## 9. Migration

Moved to the Cortex wiring document (`2026-10-18-context-operations-cortex-wiring.md`). The first two steps are the method's own: make the test-case list (§11), then build
the `ops` module and pass the list.

## 10. Out of scope

- Signed totals in the ledger.
- Pricing of `setting`: the saving or cost of a model or param change (R50). The cache break it
  causes is priced; the change in output tokens is shown as an observation, not as a saving.
- A measured saving of a param change. It needs a control that keeps the old setting.
- Persisting fingerprints across a restart.
- **Known limitation: the payer of a call is the payer of its request.** A `spend` operation has
  no payer of its own. It is priced with the `Billing` of its request. If an operator calls a model
  on another credential (for example a summarizer on the deployer's API key, in a session on the
  user's subscription), the call is priced as the user's. A standalone `RequestOps`, such as a
  ping, carries the `Billing` of the credential it used.

### 10.1 What this design does not replace

`Operation`/`RequestOps` carries money facts only — tokens, tiers, the IDs needed to join and
price them. The engine and the proxy's dashboard also need facts an `Operation` deliberately does
not carry (R2: no content; no names): tool/MCP names, invocation signals, declaration ownership,
replay text for a holdout, per-ping strategy metadata, per-component decline counters. None of
that is rebuilt from `Operation`. This design changes nothing about where it lives:

- **Component `Gate`/`Event` counters** (`Report.Gates`, `Report.Events`) — the deployer's answer
  to "is this component declining, or just absent on this traffic" — are unaffected. They are not
  money, carry no `ChangeID`, and have no place in `Operation`; they keep rolling up into `/stats`
  the way they do today. Migration step 10 removes the pricing code this design replaces
  (`saving.go`, `unique.go`, the per-operator draft functions); it removes no gate/event path.
- **Tool/MCP advisory surfaces** (`dash/toolinventory.go`, `toolsuggest.go`, `toolremoval.go`,
  `adapttools.go`, `declcredit.go`) read declaration **names**, per-call invocation signals, and
  the fact that a declaration lives in `tools`. `tp:decl-set-7f3a`'s operation (§4, worked example
  op1) reports "3 declarations, 8,000 tokens," never which ones — these surfaces keep reading the
  engine's own declaration-scan data, not `Operation`.
- **Holdout/control-arm replay** (`dash/kvcachesuggest.go`, `kvcacheholdout.go`,
  `adaptcompaction.go`) needs the pre/post **text** at a message index to re-run a component under
  a different config. `Operation` carries no text (R2). These keep their own stored-text path;
  they are not driven by `ChangeID`/`Size`.
- **Keep-alive strategy attribution** (`dash/campaignsavings.go`) groups by strategy id, preset,
  stop reason — fields `components.KeepAliveReport` already carries (the wiring document keeps it as the
  keeper's report path) but a bare `spend` operation does not. The dashboard columns these feed keep
  reading `KeepAliveReport`, not the `spend` operation's `Call`.
- **The headline money total** (`dash/savings.go`'s `baseline − cost − cgllm + cachesplit + kaNet`)
  IS reconstructable from `event.Saving`/`ChangeSaving`, because the recorded form keeps the cachesplit,
  keep-alive and spend terms as separate, summable fields. Only the advisory/diagnostic detail
  above is out of this design's reach.

If a later design wants to drive any of the surfaces above from `ops`, that is new scope, not an
extension of this one.

## 11. Test cases

Before the implementation starts, we make an **exhaustive list of test cases** with the current
saving computation. The list is the specification the implementation must meet. It also records
each place where the new figure differs from the current one on purpose.

### 11.1 Where the current figures come from

| Current rule | Repo | Used for |
|---|---|---|
| `dash.Event.Price`: `ComponentSavedUSD`, `uniqueRate`, `repeatRate`, `baselineDeltaUSD`, `cachesplitSavedUSD`, `keepaliveSavedUSD`, `BilledTokenFactor` | context-guru | every context-guru case |
| `settle`: `pruneDraft`, `compactionDraft`, `rewritePenalty` | Cortex (`feat/contextguru-keepalive`) | tool-prune cases, rewrite-penalty cases |

Cortex's `repeatUsage` is a copy of the proxy's `repeatRate`, so the proxy is the source for the
context-guru reductions. The library imports neither context-guru nor Cortex: the comparison with the Cortex rules
runs in Cortex (§11.4).

### 11.2 One case

Each case is a JSON file in `ops/opscases/`. The package embeds them and exports `All() []Case`,
so a test in Cortex can run the same list. A case holds only what the spec decides: the input and
the expected result. It does not hold the current code's figure, nor a judgement about it.

```json
{
  "id": "rewrite/replay/before-bstar/hit/5m",
  "dims": {"kind": "rewrite", "position": "beforeBStar",
           "cache": "hit", "ttl": "5m"},
  "input": { "...": "an ops.Input" },
  "rates": "fixture-v1",
  "expected": { "...": "the ops.Result, tokens for each tier" }
}
```

- `expected` is what `ops.Price` must return, by this spec.
- `rates` names a fixed rate table in the package. `fixture-v1` is: input $3.00/M, cache write 5m
  $3.75/M, cache write 1h $6.00/M, cache read $0.30/M. With one table, `expected` and the current
  code's figure can be compared in dollars.

The places where the current code is meant to give a different figure are in one separate file,
`ops/opscases/differences.json`. Each entry has a case ID, a kind, and a reason:

- `differs`: the figures differ on purpose. The reason cites the spec section that causes it.
- `notCovered`: the current code prices something the model does not. The case is listed in §12.

A case that is not in this file must agree with the current code. The current figure is never
stored: the agreement test computes it each time it runs (§11.4).

### 11.3 What makes the list exhaustive

**A. Generated cases.** A generator makes one case for each combination of these dimensions. It
drops the combinations that cannot occur, and states the reason for each drop in its code.

| Dimension | Values |
|---|---|
| Kind | `rewrite` smaller, `rewrite` larger, `rewrite` with `Reverses` (a restore), `cacheChange` |
| Position | before B\*; after B\* and at or before `L`; after `L`; no breakpoint in the body; spans on both sides of B\* |
| Cache state | hot; cold (expired, `Lifetime: exact`); unknown (gap past an unproven `Lifetime: minimum`, R45); cold (first request); rescued by a ping; unknown (restart) |
| Cache result | full hit; partial hit; miss with write ≥ input; miss with write < input |
| Breakpoints | end of `system` + last message (Claude Code); one after `tools` only; none; one inside the messages before a break |
| Billing | `api`; `subscription`; `unknown` |
| TTL of the next breakpoint | 5m, 1h |
| Component type | summarizer (`summarize`, `cache_aware_summarizer`); other context-guru component; a marker component whose bytes `apply` writes (`toolschema`, `toolfilter`, `cachesplit`, `headttl`); tool-prune |
| Lifetime | `exact` (Anthropic); `minimum` (OpenAI GPT-5.6+); `unknown` |
| Tokens | counted by a tokenizer; `bytes-estimate`; with and without the proxy's `BilledTokenFactor` |

**B. Hand-written cases.** These test how operations interact, which a cross product cannot do:

1. The worked example: turn 5, the turn-8 expiry and the turn-7 coref break, with every figure in
   the example file.
2. Cache breaks: one for each `Cause`. An operator break attributed to the earliest operation. Two
   operators whose changes are both before the break.
3. Two operators on the same content (R25), and a message splice that moves positions (`MessageSplice`).
4. A `try_run` operation: priced, shown as "would have saved", and not counted. A reverted change
   that still made a model call: no reduction, and its `spend` operation counted. A discarded
   change: reported, not priced as a saving, counted in `DiscardedCount`, and its `spend` operation
   (if any) still priced (R44).
5. Each `Unpriced` reason, and each `CheckFailure`.
6. `Saving.Changes` over the limit, which becomes an `Aggregate` (wiring document).
7. `spend`, `setting` and `external` operations, which pass through unpriced.
8. Keep-alive: a window that rescued its request; a window whose pings did not stop the expiry; a
   gap inside the TTL, where the ping is not credited; two threads with their own windows.
9. `cachesplit` and `breakpointTTL`, with the current `cachesplitSavedUSD` as the source.
10. Changes that end: a summary of reduced messages, in both pipeline orders, with the same total;
    an ended change that is not reported again; a restore of the summary, after which the earlier
    changes are reported again.
11. A subagent thread: its operations, its totals, and a session total that adds both threads.
12. The interaction on a rescued request, and the conservation check on every case.
13. The worked example on a subscription plan: the same tokens, in usage-limit units, with a
    summarizer call.
14. A `coref` batch: three cuts in messages that are not next to each other, as one operation with
    three spans, one `ChangeID` and one attributed `cacheChange`; its payback across later turns.
15. `extract_llm_sweep`: a prefix ask that reads the previous turn's cache, and a fallback ask that
    pays fresh input. Both are `spend` operations; their `Call.Usage` differs.
16. `breakpointTTL`: a 1h head written and read after 20 minutes (both legs); a 1h head written and
    never read (cost leg only, negative total); the measured blanket-versus-gated case from
    `apply/headttl.go`.
17. A discarded change (R44): a `cacheinject`-shaped change on a `tool_use` message that the
    writeback drops. Reported, not priced as a saving, counted in `DiscardedCount`.
18. `readlifecycle` and `skeleton`: the same decision with different replacement bytes on two turns
    keeps one `ChangeID` (§4.2 rule 7).
19. Interleaved threads: a main thread whose previous request is five requests back, behind four
    subagent requests, still compares with its own previous request (§5.3).
20. `ops.FromKeepAlive`: a `KeepAliveReport` from each host's emitter gives the same `spend`
    operation.

**C. Cases from real traffic.** For each difference that a shadow step (wiring document, migration steps 5 and 8) logs and
no case matches, add a case. Then decide: it is a bug in `ops`, or a new `differs` entry with a
reason.

### 11.4 How the list validates the implementation

| Test | Where | Passes when |
|---|---|---|
| `ops` unit test | context-guru | `ops.Price` returns `expected` for every case. `ops.Compare` returns the expected B\* and break for every collector case. |
| Agreement with the current code | context-guru | The test runs `dash.Event.Price` on each case's input. Each case not in `differences.json` equals `expected` within one micro-dollar. Each `differs` entry really differs, so a stale entry fails. |
| Agreement with the Cortex rules | Cortex, until step 10 | The same test with `pruneDraft`, `compactionDraft` and `rewritePenalty`, for the tool-prune and rewrite-penalty cases. It is removed with those rules. |
| Review of the differences | PR review | Every `differs` entry cites a spec section, and a reviewer approves `differences.json`. |
| Cortex contract test | Cortex | Cortex builds the same inputs through its own plugins and collector, and gets `expected`. A type change in the library fails the Cortex build on upgrade. |
| Shadow steps | both | Every logged difference matches a `differs` entry. |
| Removal gate | both | No rule is removed while a `notCovered` entry uses it. |

### 11.5 Other tests

1. **Property tests in `ops`:** `Saved` and `Caused` are never negative; an empty list gives an
   empty result; reordering operations of different operators does not change each one's price,
   except for break attribution.
2. **End-to-end:** the LiteLLM integration test (`06c712cb` on `feat/contextguru-keepalive`) checks
   that the reported saving equals the change in billed cost against the control arm, within a
   tolerance.

## 12. Open questions

Each has test cases that are marked `differs` or `notCovered` in `differences.json` until it is decided (§11.3).

1. **The interaction on a rescued request (§6.2).** It is attributed to nobody. An alternative is
   to give it to the keep-alive window, because the window caused the state. That would make the
   keep-alive figure larger on every request with large reductions. A decision is needed before the
   test cases are frozen.
2. **Content shared by two threads — closed.** A fork starts with its parent's content, so the
   provider can read the parent's entry. `internal/thread` marks such a request with `Inherits`,
   and the host starts the fork from the parent's fingerprint (§5.3). So the fork's first request
   is scored against the parent's cache, not as `cold`.
3. **Usage-limit weights.** The provider publishes no weights for each tier, so the defaults (§7.4)
   are the API rate ratios, an estimate. It is not verified whether a subscription response carries
   usage-limit headers that a host could use to measure the weights. If it does, a calibration
   could replace the defaults for each plan.
4. **The tokenizer correction.** The proxy multiplies its counted tokens by `BilledTokenFactor`
   (o200k_base to the provider's tokenizer). Cortex uses a bytes ratio. Option: `Input` takes a
   factor for each tokenizer name, so `ops.Price` applies the correction in one place. The
   generated cases test both, with and without the factor.

5. **Where the previous request's hashes live (review of #592).** The counterfactual needs the
   previous request's hashes on the same thread. If the receiver keeps nothing, they can travel in
   each push (with the ID of the previous request), or the operator can keep them. Who classifies a
   `cacheChange` follows from this. It is an implementation detail and is left open.
6. **Label volume.** Pushing `change` and `request` as labels on every series gives many series in a
   long session. This must be measured before the push model is fixed.
7. **Asynchronous recalculation (review of #592).** A replay should not cause a recalculation every
   time. Recalculation should be async and buffered. A newer request for the same key replaces the
   queued one and keeps its queue position, so nothing starves. The queue design is not written.

## 13. Second review: what changed, and what was checked

A second review checked the model against the real engine (`components/`, `apply/`, `expand/`,
`keepalive/`, `internal/thread/`, `dash/`), Cortex's cost code, and both deployments. It found the
items below. Each is fixed in the section named.

| Finding | Fix |
|---|---|
| A change the writeback discarded was "not reported", so issue #32's blind spot came back | R44, §5.1a: reported with `Discarded`, never priced, counted in `DiscardedCount` |
| A gap past an OpenAI-style minimum TTL scored `cold`, inventing a saving | R45, §5.4: `CacheState.Lifetime`; a `minimum` with no proof scores `unknown` |
| Dashboard and diagnostic surfaces could lose their data at step 10, with no statement | §10.1 lists what keeps its own path; step 10 must not remove it |
| `ChangeID` included replacement bytes, so `readlifecycle` and `skeleton` churned it | §4.2 rule 7: content key only, as `frozenKey` |
| A new `Sink` duplicated the keeper's existing `KeepAliveEmitter` | wiring document: keep `KeepAliveEmitter`; convert with `ops.FromKeepAlive`; `OpsEmitter` for detached calls |
| Thread matching reinvented `internal/thread` and lost `Inherits`/`Disagree` | §5.3: use `thread.Result`; one fingerprint per `thread.Key` |
| The cachesplit baseline could be a stale request | §5.3: compare with this thread's previous request |
| The proxy had no specified place to collect operations | wiring document, §5: `apply.Result.Ops`, the same for both hosts |
| Migration compressed the proxy's collector into one switch step | wiring document: step 4 builds it, step 6 retires the duplicate cache-state rule |
| `headttl`'s premium was priced only when the entry was read | §4.4: a cost leg on every writing request |
| `coref`'s batch was shown as a single cut | §4.2 rule 10, test case 14 |
| Marker components: the decider is not the writer | §4.2 rule 9 |
| "One central place" read as one rate table | §1: the logic is central; rates stay host-local |
| `AttributedChange` duplicated a fact | removed; the library copies the `ChangeID` onto the priced penalty (§6.3) |

**Checked and found correct, no change:**

- `extract_llm_sweep`'s prefix ask fits a `spend` operation; only test cases were missing (§11.3, 15).
- Withdrawing R13 (reversibility, stash key) holds: `expand` finds stashed content by its marker.
- Two components that produce identical bytes count separately today too (component is in the
  `seenKeys` key), so per-component `ChangeID` is not a regression.
- Session, thread, agent and billing account are four different facts; none can be dropped.
- A one-operation plugin has a one-part operator name (`tool-prune`), so no empty second field is needed.

**Kept as it is, with the trade-off stated:**

- The collector computes B\* from the forwarded body instead of trusting the engine's
  `MaxCachedIdx`, because in Cortex later plugins change the body after the engine. In the proxy
  the two must agree, and a counter checks it (§5.6).

