# Context operations: counterfactual accounting of cost and savings

**Status:** proposed.
**Date:** 2026-10-18.
**Worked examples:** [`examples.md`](examples.md).
Start with the basic example. Advanced examples follow in the same file.
**Wiring in hosts:** how Cortex and the context-guru proxy connect to this model is in a separate
document.

**Index**

- [0. The problem](#0-the-problem)
  - [The selected solution](#the-selected-solution)
- [1. Goal](#1-goal)
- [2. Decisions](#2-decisions)
- [3. Requirements](#3-requirements)
  - [A. Boundary](#a-boundary)
  - [B. Coverage](#b-coverage)
  - [C. Facts for pricing](#c-facts-for-pricing)
  - [D. Status and correctness](#d-status-and-correctness)
  - [E. Operability](#e-operability)
  - [F. Billing plans](#f-billing-plans)
- [4. The object model](#4-the-object-model)
  - [4.1 Types](#41-types)
  - [4.2 Rules](#42-rules)
  - [4.3 Why positions are (section, index)](#43-why-positions-are-section-index)
  - [4.4 Context operations that change the cache state](#44-operations-that-change-the-cache-state)
  - [4.5 Changes that end](#45-changes-that-end)
  - [4.6 Replays: who remembers past changes](#46-replays-who-remembers-past-changes)
- [5. Comparing requests](#5-comparing-requests)
  - [5.1 The counterfactual boundary B\*](#51-the-counterfactual-boundary-b)
  - [5.2 Classifying a break](#52-classifying-a-break)
  - [5.3 Tokens for each position](#53-tokens-for-each-position)
- [6. The library](#6-the-library)
  - [6.1 Call](#61-call)
  - [6.2 Tier rule](#62-tier-rule)
  - [6.3 Output](#63-output)
  - [6.4 Unknown boundary](#64-unknown-boundary)
  - [6.5 Checks](#65-checks)
- [7. Billing plans and recording](#7-billing-plans-and-recording)
  - [7.1 Billing plans](#71-billing-plans)
- [8. Errors](#8-errors)
- [9. Out of scope](#9-out-of-scope)
  - [9.1 What this design does not replace](#91-what-this-design-does-not-replace)
- [10. Test cases](#10-test-cases)
  - [10.1 Where the current figures come from](#101-where-the-current-figures-come-from)
  - [10.2 One case](#102-one-case)
  - [10.3 What makes the list exhaustive](#103-what-makes-the-list-exhaustive)
  - [10.4 How the list validates the implementation](#104-how-the-list-validates-the-implementation)
  - [10.5 Other tests](#105-other-tests)
- [11. Open questions](#11-open-questions)

## Overview

**In one paragraph.** When a context operator shrinks the context sent to a model, we want to say how much money
that saved. Today every context operator answers on its own, in its own way, so the answers disagree and nobody
can check them. This design gives every context operator one small record of what it changed, and one shared
calculation that turns those records into tokens and money.

**The story.**

1. **A change is recorded as a fact.** A context operator that shortens a tool output says: "I took 7,000 tokens
   out of message 9." It says nothing about money. We call this record an **context operation**.
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
5. **One library does the calculation.** It takes context operations and the cache facts, and returns tokens
   for each billing class. Each host turns tokens into money with its own price list. The result is
   the same tokens for the same request, wherever it is computed.
6. **The library is shared.** It is a shared library for context-guru and Cortex, and we hope
   other AI frameworks use it too. It does not depend on context-guru or on Cortex. Anyone who can report
   context operations and the cache facts can get the same tokens.

**Some changes cost money first.** Not every context operation removes tokens. Some make a model call, and
that call is a cost. We record it as a **spend** context operation. It carries the tokens the provider
billed for the call.

- **Summarization.** A summarizer reads a long part of the context and writes a short summary. The
  call costs money (the spend). The summary then replaces the long part in every later request (the
  saving). The spend carries the `ChangeID` of the change it created, so the cost and the saving are added together.
  The summary pays back only if the saving is larger than the call.
- **Keep-alive.** A keep-alive sends a small request while the user is idle, so the provider does
  not drop the cache. The request costs money (the spend). Its value shows later, on the next real
  request, which reads a warm cache and not a cold one. The spend and the later rescue share one
  ID, so they are added together. If the user never comes back, the spend has no rescue, and the
  total for that keep-alive is negative.

In both cases the cost and the benefit land on different requests. This is why a spend is a
separate kind of context operation, and why we add up over the session.

**The packages.** Context operators report context operations to a host. Cortex is the host today. The
context operators and the host import the `context_counterfactual` library. The library imports none of them (R3).

![The context_counterfactual library, Cortex as its consumer, and the context operators that report context operations](packages.svg)

**Read next.** The basic example in
[`examples.md`](examples.md) shows the idea
with three requests and one change. The rest of this document is the reference: the problem (§0),
the rules (§1 to §3), the model (§4), the calculation (§5, §6), and the checks (§8, §10).

**Words used here.**

| Word | Meaning |
|---|---|
| Context operator | Anything that changes the context sent to a model or the cache around it: a summarizer, an output filter, a tool pruner, a keep-alive. |
| Context operation | A record of one change a context operator made to the context. It has sizes and a position, and no money. |
| Tier | The rate class a provider bills a token at: input, cache write (5m or 1h), or cache read. |
| Cache state | Hot (the provider still holds the start of the prompt), cold (it does not), or unknown. |
| Counterfactual | The bill we would have paid if the change had not been made. |
| Spend | A context operation that is a model call (a summarizer call, a keep-alive request). It carries the tokens the provider billed. It is a cost, and its benefit may come later. |
| ChangeID | The ID of one change. It is the same on every request that carries the change, and a break or a spend that belongs to it carries it too. |
| B\* | The last position the provider would have read from its cache on this request, if no context operator had changed anything (§5.1). |
| Breakpoint, `L` | A `cache_control` mark in the body. The provider caches up to a breakpoint. `L` is the last one in the body (§6.2). |
| Break | A cache change from hot to cold. The provider no longer reads content it read before. |
| Rescue | A cache change from cold to hot. A keep-alive kept the entry alive, so the next request reads it. |
| Host | Cortex or the context-guru proxy: the program that runs the pipeline, calls the library and turns tokens into money. |
| Collector | A class of the library. It keeps the state of one thread, in memory or persisted: the context operations that are still active. A context operation is reported once, on the first request of its change. On each later request `Compute` applies the active ones again, until they end. What else the state holds is open (§11, question 11). The host's pipeline adds each reported context operation to it (wiring document, section 5). |
| Keeper | The context-guru component that sends keep-alive pings between turns. |
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

This document defines one model for the context operations, and one way to compute their cost and savings.

### The selected solution

- **Context operators report context operations.** Each one reports facts about what it changed, as sizes and
  positions. No money, no rates.
- **One library prices them in tokens.** It is a standalone, shared library for context-guru,
  Cortex and, we hope, other AI frameworks. It is a pure function. It decides which tier every token
  falls in, using the cache state of the request. It is the only place that holds this logic.
- **Each host keeps its own rates.** A host multiplies tokens by its own price list. Two hosts get
  the same tokens for the same request, and may differ in dollars when their price lists differ.
- **Totals are sums over a session.** A change's total is the sum over the requests that carry it. A
  thread's and a session's totals are sums over their context operations.
- **A list of test cases defines correct.** We build it from the current computation before any host
  uses the library (§10).

## 1. Goal

- Each context operator reports the **context operations** it performed on the context, as **facts**.
- **One library**, standalone and usable by any AI framework, turns context operations into **tokens for each billing tier**.
  A **tier** is the rate class a provider bills a token at: input, cache write (5m or 1h), or cache
  read.
- **Each host** converts tokens to dollars with its own rates, and records them. There are two
  hosts: **Cortex** (`pricing.Resolver`) and the **context-guru proxy** (`internal/modelinfo`).
  For the same request, both get the same tokens for each tier.
- **What is centralized is the logic, not the rates.** The `context_counterfactual` library decides which tier every
  token falls in, in one place. The dollar rate for each tier stays in each host's own rate table,
  by design: Cortex has discounts, overrides and long-context tiers that the proxy does not. So the
  two hosts report the same tokens and can report different dollars for the same request, when
  their tables differ. Equal dollars are a property of the test cases, which use one fixed table
  (§10.2), not of production.
- An **exhaustive list of test cases**, made with the current saving computation, validates the
  implementation before any host uses it (§10).
- The model supports savings **across requests**: removing K tokens from message *i* saves on
  every later request that still carries the removal.
- context-guru is **one plugin in a longer pipeline**. Other plugins (tool-prune, cpex,
  inference-router) also change the request. The model covers all of them.
- Every figure uses the **cache state** of the request: hot, cold, or kept hot by a keep-alive.
- The model covers the context operations that change the **cache state**, not only the content: keep-alive
  pings, `cachesplit`, and the TTL of a breakpoint.
- A later context operation can **end** the effect of earlier ones. Example: a summary replaces messages
  that `mask` had already reduced.
- Every figure can be summed **for each session and for each thread**, so a subagent's saving is
  separate from the main thread's.
- The model serves **API users and subscription users**. An API user pays for each token, so a
  saving is money. A subscription user pays a fixed price and has a usage limit, so a saving is
  usage-limit headroom. The context operations and the library are the same for both; only the conversion
  to a value differs (§7.1).

## 2. Decisions

| # | Question | Decision |
|---|---|---|
| D1 | Does the library need to know that a change is applied for the first time? | **No.** The tier comes from the position and the cache state (§6.2). A report finds the first application as the first event with that `ChangeID`. The library stays stateless, and a stateless context operator (tool-prune) reports the same fields as any other (§4.6). |
| D2 | Where is the savings logic? | **In one standalone library,** shared by context-guru and Cortex, and open to other AI frameworks. Hosts call it and keep no operator-specific pricing rules. |
| D3 | What does the library return? | **Tokens for each tier**, not dollars. Cortex prices them with its own rates (discounts, overrides, long-context tiers), so a saving and the request's spend use the same rates. |
| D4 | How is cg's own spend recorded? | **As context operations, in the same report as the savings.** A `spend` context operation carries the provider's billed usage of the call (`Call`). The host prices it with the savings, in one path. There is no second record and no `CostRecorder` (wiring document). `context_counterfactual.Compute` does not price the call; it returns `Call.Usage` for the host to price (D3). |
| D5 | Shape | **One typed context operation for each change unit**, in-process only. The recorded summary (`event.Saving`/`ChangeSaving`) has the aggregate fallback above a limit, not the context operation list itself (R20; the recorded form is in the wiring document). |
| D6 | How is a saving across requests counted? | **Realized on each request, not projected.** Each request lists every change in its body, fresh or replayed, and is priced from that request's own cache result. The context operator remembers past changes, not the library or the host (§4.6). |
| D7 | Where do the types live? | **A standalone module with no dependency on context-guru, on Cortex, or on any framework.** context-guru and Cortex are two users of it, and other frameworks can be too. The module path `github.com/rossoctl/context-guru/context_counterfactual` used below is a placeholder; its home and name are not decided. |
| D8 | Which hosts compute dollars from context operations? | **Both: Cortex and the context-guru proxy.** The proxy stops using its own saving rules (`dash/event.go`) when it switches. The comparison of request fingerprints (§5) is a pure function in `context_counterfactual`, so the proxy does not need Cortex to find B\*. |
| D9 | How is the implementation validated? | **Against an exhaustive list of test cases made with the current computation** (§10), before any host switches. |
| D10 | How is the cache state used? | **As an input to every price.** The tier of a content change depends on the state (§6.2). A context operation that changes the state (`cacheChange`, §4.4) is priced as the difference between two states. |
| D11 | What happens to a change that a later change ends? | **Its context operator stops reporting it.** No link names the change that ended it. The snapshot rule (D6) and the measurement at each context operator's own input (R29) give the right price and stop any double count. Each change keeps the total it had until then (§4.5). |
| D12 | Where does the thread ID come from? | **From the engine's `internal/thread`, in both hosts**, carried on the request. The host keeps one fingerprint per thread, keyed by `thread.Key`, and compares each request with its own thread's previous request (wiring document, W5.2). |
| D13 | How does the model serve a subscription user? | **The library does not change.** It already returns tokens for each tier (D3). The request carries a `Billing` fact. The host converts tokens to dollars for an API user, and to usage-limit units for a subscription user, with an API-equivalent dollar figure beside it, marked as such (§7.1). |

## 3. Requirements

The numbers are the order in which the requirements were found, so they are not in sequence. Some
numbers are not used: those requirements were dropped.

### A. Boundary

- **R1.** Context operations are facts. They carry no rates, no dollars and no tier choice. The one
  exception is a dollar cost that a call's own response reported (R49).
- **R2.** Context operations carry no content: no message text, no clipped before/after strings, no
  credentials. They reach the unauthenticated `:9094` API and the session archive. Content identity
  uses hashes only.
- **R3.** The model and the library are in one module. Both repos import it. The module must not
  import either host or any framework, so a third party can use it. Its public types are named for
  the model, not for context-guru or Cortex. A contract test in
  each repo pins it.
- **R26.** The library is a pure function: no state, no I/O. The context-guru proxy and Cortex
  then compute the same figure for the same request.

### B. Coverage

- **R4.** Every context operation that changes money is covered, not only reductions: reductions,
  additions, cache breaks, model calls, restores.
- **R49.** A `spend` context operation names the model the call used. It can differ from the agent's model.
  If the call's response states a dollar cost, the context operation carries it as reported, inside the
  call's usage, beside the tokens. The host prices the tokens with its own rates for that model. The reported cost is kept as a
  fact and is not used in pricing (wiring document). Tokens stay the base fact, so the library still returns the
  same tokens in every host.
- **R5.** Spend that happens outside a request (a keep-alive ping, a detached summarizer call)
  carries its own attribution. It uses the same context operations and the same recording path as spend
  inside a request (wiring document).
- **R47.** A model call that any plugin makes outside the listener is a `spend` context operation, not
  only context-guru's. Example: the `ibac` judge calls a model through `core/llmclient`, so the call
  is not in the ledger today. It is reported the same way as the summarizer (wiring document). Its cost is not
  joined to a saving, because it creates none: its `ChangeID` is empty.
- **R6.** A context operation has an ID that is unique inside its request. Results and check failures refer to it.
  A reference to a change in another request, or from a break to its cause, uses `ChangeID` (R22).
- **R27.** The model is not specific to one context operator. Each context operation names its context operator. The
  context operations are in pipeline order, the order they ran.
- **R33.** A `rewrite` that makes the context larger is a cost: an injected tool declaration, injected text, a
  restore.
- **R34.** A change of a request setting is a separate kind, `setting`: the model, the host, or any
  param. Example: a session moves from one model to another, or from low to high effort. It is
  recorded as a fact, because it explains later figures. One context operation is one decision and holds
  any number of settings, as a map from the setting name to its `from` and `to` values. Example: a
  router moves a request to a smaller model and a lower effort in one context operation. A setting that
  appears or is removed has a nil `from` or `to`. Settings changed for different reasons, or by
  different context operators, are separate context operations. The saving or cost of the change itself is **not computed** (R50).
- **R50.** The library does not price a param change. Providers do not price `effort` or `thinking`
  per token. These params change how many output tokens the model writes, and sometimes the cache.
  The saving cannot be computed from facts, for four reasons:
  1. *The alternative is not observable.* The tokens the model would have written at the other
     setting were never generated.
  2. *The effect is not fixed.* It depends on the task, the model and the turn.
  3. *Quality can change.* A lower setting can need more turns, so a saving on one request can be a
     loss over the session.
  4. *The cache can break.* On both Anthropic and OpenAI, a change can invalidate cached content.

  What the library does instead: it keeps the `setting` context operation; it records a `cacheChange` with
  the `setting` context operation's `ChangeID` when a request after the change is cold (§5.2), which is measurable; and a report
  can show output tokens per request before and after, labelled as an observation, never in
  `TotalAvoidedUSD`. A measured saving needs a control (a holdout that keeps the old setting). That
  is out of scope (§9).

### C. Facts for pricing

- **R7.** Each context operation has a position: section and index, in the provider's hash order.
- **R8.** Sizes are in bytes, and in tokens with the tokenizer named. A context operator with no tokenizer
  sends a byte estimate and marks it (§4.2 rule 7).
- **R11.** Model-call usage is the provider's billed usage, read from the call's own response by
  the model client the host gives the engine. The engine does not count it (wiring document).
- **R22.** A change has an ID that is the same on every request that carries it (`ChangeID`).
- **R23.** A one-time cost (a cache break) is linked to the change that caused it, so payback can
  be computed.
- **R24.** Cache facts are scoped to the conversation the provider cached, not to the session as a
  whole. Subagents share a session but not a cache entry.
- **R29.** Each context operator's change is measured against that context operator's own input.
- **R30.** Cache facts come from the forwarded body, not from one plugin's output.
- **R31.** A cache break with a cause outside the plugins (client edit, expiry, first turn) is not
  charged to a plugin.
- **R32.** Positions resolve in the forwarded body. A context operator that adds or removes messages
  reports that.
- **R36.** Every price uses the request's cache state: hot (the previous entry was read, or a keep-alive kept it), cold
  (expired), or unknown (the first request of a thread, or a gap past an unproven minimum TTL). The same change
  has a different value in each state.
- **R37.** Cache breakpoints are an input. The provider writes and reads the cache only at a
  breakpoint, so the tier of a position and the size of a cache-break penalty depend on where the
  breakpoints are, and on their TTL.
- **R38.** Context operations that change the cache state are covered: a keep-alive ping, `cachesplit`, and
  a breakpoint TTL change. Each one is priced as the difference between two states.
- **R45.** The cache state distinguishes a proven expiry from an unproven floor. A provider whose
  TTL is only a guaranteed minimum (no documented maximum) never scores `cold` from elapsed time
  alone — that would invent a saving the provider may not have billed. See §5.2.

### D. Status and correctness

- **R12.** A context operation is applied or a `DryRun`. A `DryRun` is a change that a component
  computed but did not send: the original body went upstream. Example: a component in trial mode
  finds that it can cut 3,000 tokens from a tool output. The request still carries all 3,000 tokens.
  The context operation records the 3,000 as "would have saved". Only applied context operations count in totals. A
  change that is not in the forwarded body (reverted by the pipeline) is not reported.
- **R15.** Each request is complete without history.
- **R25.** When two context operators change the same content on one request, each one is credited for
  its own change, in pipeline order.
- **R35.** A later change can end earlier ones (a summary of reduced messages). The model shows
  this by the earlier change not being reported, not by a link. Their totals until then do not
  change (§4.5).

### E. Operability

- **R17.** Each context operation names its context operator in one field, as a dotted path: `context-guru.mask`,
  `tool-prune`. The first part is the product or plugin, and the framework sets it, so a plugin
  cannot report under another plugin's name. A later part, if any, is the context operation type. The
  taxonomy of a context operator (what kind of change it makes, and what made it decide) depends on the
  name only, so a report maps the name to it with a table in cg. The context operation does not carry it.
  Example: `context-guru.mask` truncates old tool output; it decides by age and size. Parts must
  not contain a `.`. The allow-list and the per-operator breakdown match on the first part.
- **R18.** Each request carries request ID, session, thread (if known), **model** and
  **params**. The model, and the parameters that change what a call costs, are facts of the
  request. Examples of params: `effort`, `thinking` (the thinking budget), `max_output`,
  `service_tier`. The library does not fix the list. A key it does not know is kept and passed on,
  and it is never dropped (R19).
- **R46.** Each recorded saving names the version of the `context_counterfactual` rules that computed it
  (`Saving.OpsVersion`). A context operator can then trace a figure to the rules and the case list that
  define it. A change to the rules changes the version, so figures from two versions are not
  compared as if they were the same.
- **R39.** Every figure can be summed for one session, for one thread, or for one subagent. The
  session and the thread are on the request (`RequestOps`); the recorded form carries them (wiring
  document).

### F. Billing plans

- **R40.** Context operations and the library are independent of the billing plan. They carry tokens and
  tiers only. The plan is a fact on the request, not a field of any context operation's price.
- **R41.** Each request names its billing: `api` (pay for each token) or `subscription` (fixed price
  with a usage limit).
- **R42.** A `spend` context operation is priced with the billing of its request. The payer of a call is
  the payer of the request (§9, known limitation).
- **R43.** A subscription saving is never reported as money saved. It is reported as usage-limit
  headroom, and an API-equivalent dollar figure can be shown beside it, labelled as an equivalent.
- **R19.** The model is versioned. An unknown `Kind` is kept and reported as unpriced, never
  dropped.
- **R20.** `RequestOps`/`Operation` is request-scoped and in-process only: input to `context_counterfactual.Compute`,
  discarded once the request's `event.Saving` is written. The context operation list is never recorded
  (R2's `:9094` and archive concern, and the session store's own size history, apply to what IS
  recorded — `event.Saving`, `Saving.Changes`/`ChangeSaving` and `Event.Spends` — not to the raw
  context operation list). A `spend` is recorded as one `SpendRecord` (wiring document, W7.1), so a total
  can add it. The recorded summary has a size limit, with aggregation above it (wiring document).
- **R21.** Second-order effects are not context operations. Example: the agent compacts later because its
  requests are smaller.

## 4. The object model

### 4.1 Types

```go
package context_counterfactual // github.com/rossoctl/context-guru/context_counterfactual

// RequestOps is the operations of one request, or of one call made outside a request.
type RequestOps struct {
    Version   int
    RequestID string
    Session   string
    Thread    Thread
    Model     string            // the model the request was sent for
    Params    map[string]string // other settings that change the cost: effort, thinking, max_output, ...
    At        time.Time
    // Billing prices every operation of the request, a `spend` included (R41, R42, §7.1).
    Billing Billing
    // Ops is in-process only: input to context_counterfactual.Compute for this request, never recorded (R20).
    Ops []Operation // in pipeline order
}

// Billing is a fact the host reads from the request's credential, not a price.
type Billing struct {
    Mode    BillingMode // api | subscription | unknown
}

// ModelCall is one upstream call an operator or the keeper made. The host's model client fills it
// from the call's own response, so the usage is what the provider billed.
type ModelCall struct {
    Host, Model string
    Params      map[string]string // the call's own settings: effort, thinking, ...
    Usage       CallUsage     // everything the call's response bills, as billed
    Detached    bool          // finished after the request it served was answered
}

// CallUsage is the whole bill of one call, as its response states it: the prompt side (Usage), the
// output tokens, and the dollar cost when the response states one (some gateways do). The cost is
// a fact the provider or gateway reported, not a rate of ours (R1, R49).
type CallUsage struct {
    Usage                // input, cache read, cache write (and its 1h part)
    Output  int          // output tokens, including the model's reasoning or thinking tokens
    CostUSD float64      // the cost the response stated; 0 when it stated none
}

// Thread identifies one conversation inside a session: the main thread, a subagent, or a fork.
// Each thread has its own provider cache entry.
type Thread struct {
    ID     string       // "main" for the primary thread
    Parent string       // the thread this one was forked from; "" if none
}


type Operation struct {
    ID        string // unique inside the request only; across requests, use ChangeID (R6)
    Kind      Kind   // rewrite | cacheChange | spend | setting

    Operator  string // "context-guru.mask", "tool-prune"; the first part is set by the framework

    DryRun    bool // set by the framework: computed but not sent (R12)

    At     Position // first position the change touches, in this operator's output. Not used by a cacheChange: its range is given by the extents of CacheBefore and CacheAfter
    Spans  []Span   // every touched range, when the change is not contiguous; nil otherwise
    SizeBefore Size // what the operator received over the touched ranges; the sum over Spans
    SizeAfter  Size // what it sent on over the same ranges

    // kind = rewrite | cacheChange | spend | setting
    ChangeID     string   // the same on every request that carries this change. On a spend: the change the call created, or the keep-alive window. On a break: the change that caused it; empty when nobody did

    // kind = cacheChange (§4.4): a prefix range moves from one state to another. The range is
    // between the Extent of CacheBefore and the Extent of CacheAfter. A rescue (cold to hot) and a
    // break (hot to cold) are both written by the library (§5), so neither has an operator. A rescue
    // carries the ChangeID of the keep-alive window. A break carries the ChangeID of its cause.
    CacheBefore CacheState
    CacheAfter  CacheState

    // kind = spend
    Call     *ModelCall // the call, with the provider's billed usage (wiring document)

    // kind = setting: one decision, any number of settings. The key is the setting name: model,
    // host, or a key of Params (effort, thinking, ...). One key cannot appear twice.
    Settings map[string]SettingChange

    // a change to the message list, for mapping positions
    MessageSplice *MessageSplice
}

type Position struct {
    Section Section // tools | system | messages
    Index   int     // block or message index in that section
}

// Span is a range of positions in one section: [From, To], both in this operator's output.
// SizeBefore and SizeAfter are the sizes over this range only. The operation's SizeBefore and SizeAfter are the
// sums over its Spans.
type Span struct {
    Section       Section
    From, To      int
    SizeBefore, SizeAfter Size
}

// SettingChange is one setting going from one value to another. A nil pointer means "not set":
// From nil is a setting that appears, To nil is a setting that is removed.
type SettingChange struct {
    From, To *string
}

type Size struct {
    Tokens    int    // required on rewrite; see rule 7
    Tokenizer string // the tokenizer that counted Tokens; "bytes-estimate" when estimated
}

type MessageSplice struct{ At, Removed, Inserted int } // messages only

// CacheState is the state of the thread's provider cache entry: its phase, the last position it
// covers, and its lifetime. It is what `Compute` reads, and what a `cacheChange` moves from and to.
type CacheState struct {
    CachePhase CachePhase // hot | cold | unknown
    Extent     *Position  // the last position the entry covers; nil when nothing is covered
    TTL        string     // "5m" | "1h": the lifetime the breakpoint asks for; empty when cold or unknown
}

// EntryFacts are the inputs that decide the cache phase. They are part of `Input`; the library
// reads them in its internal comparison (§5), and nowhere else.
type EntryFacts struct {
    Gap  time.Duration // time since the entry was last written or read
    TTL  time.Duration // lifetime of the entry: 5m or 1h, from its breakpoint
    // Lifetime says whether TTL is a proven expiry (exact) or only a guaranteed floor
    // (minimum): Anthropic's TTL is exact; OpenAI GPT-5.6+ states only a 30-minute minimum
    // with no documented maximum (R45, §5.2). Gap > TTL proves expiry only when exact.
    Lifetime LifetimeKind
    // PingRead is the extent a keep-alive ping of this thread read inside the TTL; nil when none did.
    PingRead *Position
}

type Kind string          // rewrite | cacheChange | spend | setting
type CachePhase string    // hot | cold | unknown
type LifetimeKind string  // exact | minimum | unknown — see apply.CacheLifetimeKind
type BillingMode string   // api | subscription | unknown
type Section string       // tools | system | messages
```

The enums are strings, not integers, so an unknown value from a newer context operator survives a decode
and can be reported as unpriced (R19).

### 4.2 Rules

1. **One context operation for each change unit.** A change unit is what the context operator decides, replays and
   reverses as one. A component that reduces 12 tool outputs with 12 decisions reports 12 `rewrite`
   context operations, each with its own `ChangeID`. tool-prune removes the declarations of one account
   list in one decision, so it reports one context operation for all of them. Its declarations are not
   next to each other, so it lists them in `Spans`. `At` is the first one. `coref` is the same: it
   plans many cuts across messages that are not next to each other, gates them as one batch, and
   reports one `rewrite` with one `ChangeID` and one `Span` for each cut. Reporting each cut alone
   would split one rewrite penalty across N changes, and no single cut's payback would then mean
   anything.
2. **A replay is a new context operation with the same `ChangeID`.** A replay's
   cost, if any, is a `spend` context operation, not part of the reduction.
3. **`SizeBefore` and `SizeAfter` are measured at the context operator's own input.** The direction of a `rewrite`
   is in the sizes, not in the kind. `SizeAfter < SizeBefore` is a saving, `SizeAfter > SizeBefore` is a cost (an
   injected tool declaration, a restore), and equal sizes change no token count. A restore is a
   `rewrite` with a `SizeAfter` larger than its `SizeBefore`. It names no earlier change.
4. **Only the library writes a `cacheChange`**, inside `Compute` (§5). The context operators write `rewrite`, `spend` and `setting`.
5. **A keep-alive ping is an ordinary `spend` context operation.** Its `Call` carries the ping's billed
   usage. Its `ChangeID` names the keep-alive window, and the `cacheChange` context operation on the
   request it rescued has the same `ChangeID`. So the cost and the value of a window add up
   (§4.4).
6. **`ChangeID` is derived from the context operator and the original content key only — never from the
   replacement bytes.** This matches the engine's own stable key (`frozenKey(session, component,
   contentKey(original))`, `components/offload/state.go`) rather than adding to it. Two
   context operators that remove the same content in different ways still get different IDs, because
   the context operator name is part of the key.
   `readlifecycle` and `skeleton` give different replacement bytes for the same decision on
   different turns, so keying on the replacement bytes would churn the ID.
7. **The tokens a change saved come from the context operation's own `Size`, not from the forwarded body.**
   Removed tokens are not in the forwarded body, so nothing else can count them. A context operator with a
   tokenizer (context-guru) counts them and names the tokenizer. A context operator with none (tool-prune)
   estimates them from bytes with the ratio of its own input, and sets
   `Tokenizer = "bytes-estimate"`. The library marks every priced result that uses an estimate as
   `Estimated`.
8. **The engine reports a change that `apply` writes, under the context operator that decided it.**
   `toolschema`, `toolfilter` and `cachesplit` are marker components: their `Reformat()` does
   nothing, and `apply` performs the rewrite (`apply/envelope.go`, `apply/prefixsplit.go`). `apply`
   holds the only exact numbers (`Trace.FilteredDeclTokens`, `Trace.SplitStableTokens`). So `apply`
   builds the context operation and sets `Operator` to the marker component's name. The mixed-TTL head
   (`apply/headttl.go`) has no component; its context operations use `Operator = "context-guru.headttl"`.

**A `setting` context operation.** It holds all the settings one decision changed, as a map from the setting
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
compared on its own and priced with its own `SizeBefore − SizeAfter`. So a change with spans on both sides
of B\* gets the right tier for each part. A context operator reports positions in its own output. If a
later context operator adds or removes messages, it reports a `MessageSplice`, and the library maps earlier
positions into the forwarded body.

### 4.4 Context operations that change the cache state

A `cacheChange` context operation does not change the content. It changes how the provider bills content
that is already there.

A `cacheChange` carries two states: `CacheBefore` and `CacheAfter`. The prefix range it covers is the
range between the `Extent` of the two states (a nil `Extent` is the start of the prompt). Three cases
cover it:

- **A prefix is broken.** The provider no longer reads content it read before (`hot` to `cold`). The
  library writes it, and gives it the `ChangeID` of the context operation that caused it (§5.2). It is a cost.
- **A prefix is warmed.** The provider read the entry, so it did not expire. Examples: a keep-alive
  ping rescued the entry (`cold` to `hot`); a split of the system block means the stable half is
  read, not written, when the volatile tail changed.
- **A prefix is protected.** A breakpoint changes what is cached, or for how long. Example: the
  head of the request is held for 1h, not 5m. `CacheBefore.TTL` is `5m` and `CacheAfter.TTL` is `1h`. It has **two legs:** a cost on **every request that
  writes** the head at 1h (the premium), and a saving on each request that reads the 1h entry after
  a gap longer than 5m (the rescue).

**When the library adds a rescue.** All four conditions must be true. They are the
conditions of the current `keepaliveSavedUSD`:

1. The keeper sent at least one ping for this thread during the gap before this request.
2. The gap is longer than the entry's TTL, so without a ping the entry would have expired.
3. The provider read more than it wrote.
4. The tokens are the smaller of what the last ping read and what this request read.

**The ping and the rescue share one `ChangeID`,** the ID of the keep-alive window. A window whose
pings rescued nothing has only `spend` context operations, so its total is negative. That is the cost of a
ping that was not needed, and it is visible for each thread.

**A longer breakpoint TTL is a bet, and both legs are recorded.** The 1h write premium (2.0× input against
1.25× for 5m) is paid on every request that creates the head entry at 1h, whether or not a later
request ever reads it. The benefit appears only on requests that arrive more than 5 and less than
60 minutes later. Measured over 19,805 production requests, a blanket 1h head cost $773.00 extra on
14,499 creating requests to recover $754.66 on 290 — net −$18.34; size-gating turned it positive.
If the model priced only the reading request, every write that never paid off would be missing, and
the figure would overstate the bet. So, like a keep-alive window:

- **On each 1h-writing request,** the library adds a `cacheChange` for the head's breakpoint TTL
  (`TTL` from `5m` to `1h`) and only a cost: `Caused.CacheWrite1h` for the head's tokens, less the 5m write it replaced. Its
  `ChangeID` names the head entry (`"headttl:" + Thread + ":" + head content key`).
- **On each request that reads that entry after the 5m TTL,** the library adds a `cacheChange` with
  the same `ChangeID` and the saving (write → read, §6.2).

A head entry that is written at 1h and never read again has only the cost leg, so its total is
negative — the same visibility a keep-alive window that rescued nothing has.

### 4.5 Changes that end

A change can remove content that earlier changes had already reduced. Example: on turn 12 a
summary replaces messages 0 to 8, and messages 3 and 5 were reduced by `mask` and `coref`.

**A change ends when its context operator stops reporting it.** There is no field that names the change
that ended it. Nothing more is needed:

1. **The price.** Each request is a snapshot (D6). A change that is not reported on a request saves
   nothing on that request. Its total until then does not change, so a change that did not pay back
   before it ended shows a loss.
2. **No double count.** Each context operator's change is measured at its own input (R29), and the sum
   check tests the total (§6.5, check 1). So the tokens are counted once, in either pipeline order:

   | Pipeline order | `mask` | `coref` | summary | Total |
   |---|---|---|---|---|
   | Summarizer first (cg's `medium`, `high`, `xhigh` presets) | not reported | not reported | 40,000 → 1,500 = 38,500 | **38,500** |
   | Summarizer after the reducers | 3,940 | 19,950 | 16,110 → 1,500 = 14,610 | **38,500** |

   The order changes **who gets the credit**, not the total. Credit follows pipeline order (R25).
   In cg's presets the summarizer runs first, so it gets all of it: the reducers find their content
   gone, and they report nothing.
3. **A restore.** If the summary is reversed (a restore, or the summary is dropped), the context operator
   replays `mask` and `coref` again. They are reported with their old `ChangeID`.

**Why a change stopped** is not on the context operation. A report can find it: the change last appeared at
position P on request N, and request N+1 has a change whose span covers P (`At` and `MessageSplice`).
That is a question for a report, not for a price, so the report answers it.

### 4.6 Replays: who remembers past changes

**Each request lists every change in its body, fresh or replayed (D6). The context operator remembers past
changes.** The library and the hosts remember nothing except the fingerprints of recent requests
(§5.1). This is a decision, not an oversight, for three reasons:

1. **The context operator already re-applies each change on every request.** The client sends its whole
   uncompacted transcript on every turn. To keep the forwarded body the same, context-guru replays
   each frozen decision, and tool-prune removes the same declarations. So the context operator already knows
   every change in the body. Reporting it adds no state.
2. **The price changes on every request, even when the change does not.** The same tokens are a
   cache read on a hot turn and a cache write on a cold turn (§6.2). The library needs each change's
   tokens and position **on this request**, and positions move: a summary turns 9 messages into 1.
3. **A register of past changes elsewhere fails in each place it could live:**

   | Where | Why it fails |
   |---|---|
   | The `context_counterfactual` library | It is no longer a pure function (R26). Two hosts can disagree, and pricing an old request again gives a different figure. |
   | The host's collector | It must move positions after each `MessageSplice`, find which changes a summary ended, and drop changes when the agent compacts its own transcript. It cannot see the content, so it cannot tell whether a change is still in the body, and a wrong guess is silent. |
   | The archive | It has gaps (`droppedEvents`). One lost request makes every later request wrong. |

**The cost is size, not correctness.** A replayed context operation repeats about 200 bytes on every
request. If that becomes a problem, a later version can send a compact replay (`changeId`, `at`,
`tokens`) and the full context operation only once. That change would not change the model or the price, so
it is not part of this design.

**A stateless context operator.** tool-prune keeps no state. It reads its `remove` list from config and
removes the same declarations from every request. It reports every field of a replay without state: `ChangeID` is a hash of the removed set and
the config, so it is the same on every request with the same config; the other fields are facts
about the body it just changed.

A reader that needs the first application finds it in the session history: the first event with that
`ChangeID`. No price needs it, because the tier comes from the position and the cache state (D1).

**A config change of a stateless context operator.** A new `remove` list (for example, a hot reload) gives
a new `ChangeID`. The old change stops and a new one starts. If the new list removes a declaration
that the old list kept, the forwarded body changes inside the cached prefix. The library then adds
a `cacheChange` that carries tool-prune's `ChangeID` (§5.2). That is the real cost of the config change.

## 5. Comparing requests

The comparison is **internal to the library**. It is not part of the public API. `context_counterfactual.Compute` runs it
first: it compares this thread's previous request with the current one, and finds B\*, a break and
the cache state. Then it prices with them. The host does not call it and cannot pass in a B\* or a
phase of its own, so every host derives them by the same rule. The host keeps the fingerprints,
finds the previous one by `thread.Key`, and passes both in `Input` (§6.1, wiring document, section 5).

```go
// unexported
func compare(prev *Fingerprint, cur Fingerprint, ops []Operation, usage Usage, facts EntryFacts) (boundary *Position, brk *Operation, phase CachePhase)
```

`prev` is the fingerprint of **this thread's** previous request, or nil. The library does no
matching of its own. `context_counterfactual` are the request's context operators' context operations: the break takes the `ChangeID`
of the one that changed the first differing position, or of the `setting` context operation. The results
come back in `Result` (§6.3): `Boundary`, `Phase`, and the break and any rescue as `PricedOp`s.

### 5.1 The counterfactual boundary B\*

The provider reports this request's cache boundary as `cache_read`. But a plugin's cache break
moves it: in the coref example, `cache_read` stops at `messages[5]` because of the coref change.
Without it, the cache would have read through `messages[11]`. So the library needs **B\***, the
boundary that would have applied if no plugin had changed anything:

```
B* = min( prevCachedExtent,   // what this thread's previous request left cached
          clientFirstDiff )   // where the client changed its own body
B* = 0 when the cache expired
```

The host keeps one fingerprint for each thread: the most recent request of that thread (wiring document, W5.2).

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

```go
// NewFingerprint hashes the two bodies, so every host builds fingerprints the same way. The
// host calls it once for each request and keeps the result as the next request's `Prev`.
func NewFingerprint(thread string, at time.Time, client, forwarded Body, cached Position) Fingerprint
```

`PosHash` is 16 bytes in memory (8 for the hash, 4 for the size, 4 of padding). It holds hashes and sizes only (R2).

### 5.2 Classifying a break

`fwdFirstDiff` is where this thread's previous forwarded body first differs from this request's
forwarded body. `clientFirstDiff` is the same comparison on client bodies.

Rows 1 and 2 need a previous fingerprint and a difference in the body. Row 5 is checked first: with no
previous fingerprint, no other row applies. The other rows are checked in order, and the first match
wins.

| # | Comparison | Result |
|---|---|---|
| 1 | `fwdFirstDiff < clientFirstDiff` | A plugin broke the cache. Add a break with `at: fwdFirstDiff` and the `ChangeID` of the context operation that changed the position `fwdFirstDiff`. |
| 2 | `fwdFirstDiff ≥ clientFirstDiff` | The client changed its body. Add a break with no `ChangeID`. |
| 3 | A `setting` context operation changed the model or a param (`effort`, `thinking`) since the previous request, and the cache did not hold | Add a break with the `ChangeID` of the `setting` context operation. If it changed several settings, the break names the context operation, not one setting: the library cannot say which of them caused it. This is the one cost of a param change that can be measured. |
| 4 | No diff, but `cache_read` is far below the previous request's cached extent | The cache expired. Add a break with no `ChangeID`. |
| 5 | No previous fingerprint for this thread | First turn of the thread with no `Inherits`, or a restart: no break is added. B\* is unknown (§6.4). |

The same comparison gives the **cache state** that `Compute` uses (R36):

| Case | `CachePhase` | `Extent` |
|---|---|---|
| A previous fingerprint exists, and `Gap` ≤ its TTL | `hot` | the previous request's cached extent |
| A previous fingerprint exists, `Gap` > TTL, and `PingRead` is set | `hot`, and the collector adds a rescue (§4.4) | `PingRead` |
| A previous fingerprint exists, `Gap` > TTL, no ping, `Lifetime = exact` | `cold` | nil |
| A previous fingerprint exists, `Gap` > TTL, no ping, `Lifetime = minimum` or `unknown` | `unknown` | nil |
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
in for proof of expiry). The host sets `EntryFacts.Lifetime` from the same source and the
table above routes a `Minimum`-with-no-proof gap to `unknown`, not `cold` — §6.4 then applies
(`penalty_unknown`), the safe direction.

### 5.3 Tokens for each position

These are the tokens of content that **was forwarded**: the library needs them for the penalty of a
cache break (§6.2), which counts forwarded content between the break and B\*. The tokens a change
**removed** are not in the forwarded body; they come from the context operation's own `Size` (§4.2 rule 7).

Cortex has no tokenizer. The host sends bytes for each forwarded position, and the library
converts them with the request's own ratio (billed prompt tokens ÷ forwarded body bytes), as
`pricing.EstimateTokensFromBytes` does today. Results that use the ratio are marked `Estimated`.

## 6. The library

### 6.1 Call

```go
func Compute(in Input) Result // pure

type Input struct {
    Ops         []Operation  // the operators' operations. No cacheChange: the library adds them
    Usage       Usage        // billed usage of this request
    Client      []PosBytes   // every position of the client body, in hash order (checks 1 and 6)
    Forwarded   []PosBytes   // every position of the forwarded body, in hash order
    Breakpoints []Breakpoint // cache_control breakpoints of the forwarded body, in hash order
    Prev        *Fingerprint // this thread's previous request; nil = none (§5.2)
    Cur         Fingerprint  // this request, built with NewFingerprint (§5.1)
    Facts       EntryFacts   // gap, TTL, lifetime kind, ping read (§5.2)
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

**The counterfactual.** The question for each context operation is: *if this change had never been made —
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

**The state.** B\* is an input (§5.1). When the phase is `cold`, B\* = 0, so every position takes the
write or input tier. So the same 8,000 tokens are worth 8,000 × read on a hot turn and 8,000 ×
write on a cold turn.

**Added tokens.** A `rewrite` with a `SizeAfter` larger than its `SizeBefore` (an injected tool
declaration, a restore) is a cost. Its added tokens take the tier of their position, by the same
rule: read before B\*, write after B\* and at or before `L`, input after `L`. They are `Caused`, not
`Saved`.

**A break, and the breakpoints.** A break moves content from the read tier to the write tier. The content that moves is:

- **from** the end of what the provider still read on this request (`Usage.CacheRead`, mapped onto
  the forwarded positions),
- **to** B\*, as forwarded on this request.

The start is the read extent (`CacheAfter.Extent`), not the position of the first difference. The provider reads only up to a
breakpoint. If the last breakpoint before the break is the end of `system`, the provider reads only
`tools` and `system`, and every message before the break is written again too. So a break deep in
the messages costs the same as a break at `messages[0]` when there is no breakpoint between them.
The new tail after B\* is written anyway, so it is not counted.

**A rescue.** A `cacheChange` from `cold` to `hot` moves its tokens from the tier of `CacheBefore` to
the tier of `CacheAfter`: for a keep-alive and a system split, from write to read. For a breakpoint TTL there
are two legs (§4.4): on a reading request, from write (the 5m entry would have expired) to read; on
every writing request, the premium alone, as `Caused.CacheWrite1h` minus the 5m write it replaced.

**The interaction.** On a rescued request, the content changes are priced in the hot state (rule
above). Without the ping they would have been priced cold, which is worth more. The difference is
neither the ping's nor the content changes' alone. The library reports it as
`Result.Interaction`, attributed to nobody, so the request total still equals the bill without
the context operations minus the bill with them.

### 6.3 Output

```go
type Result struct {
    Version     int
    Ops         []PricedOp     // one for each input operation, same order, then the cacheChanges the library added
    Interaction TierTokens     // §6.2: on a rescued request; attributed to nobody
    Unpriced []Unpriced     // with a machine-stable reason
    Checks   []CheckFailure // §6.5
    Boundary *Position      // the B* used; nil when unknown
    Phase    CachePhase     // the cache phase it used (§5.2). For the host's own checks, never an input
}

type Unpriced struct {
    OpID   string
    Reason string // "unknown_kind", "negative_size", "operator_not_allowed", "penalty_unknown", ...
}

type CheckFailure struct {
    Check    string // "sum" | "order" | "unique_change" | "spans" | "attribution" |
                    // "conservation" (§6.5)
    Operator string // "" when the check is not about one operator
    Detail   string // numbers only, no content
}

type PricedOp struct {
    OpID      string
    Operator  string
    ChangeID  string
    DryRun    bool
    Saved     TierTokens // tokens the provider did not bill, by tier; never negative
    Caused    TierTokens // tokens the provider billed because of this operation; never negative
    Call      *ModelCall // spend only: copied from the operation, for the host to price
    Estimated bool
}

type TierTokens struct {
    Input, CacheWrite5m, CacheWrite1h, CacheRead int
}
```

`Saved` and `Caused` are separate and both non-negative, because the money layer rejects negative
values (`pricing.MicrosFromUSD`). Cortex prices each side with the existing code.

A break penalty has both sides: the tokens are written (`Caused.CacheWrite5m`) instead of read
(`Saved.CacheRead`). The net is the rate difference, with no special rule.

A `cacheChange` is priced as its own context operation. On its `PricedOp`, a break's `ChangeID` is the one it was
given, so the host adds the penalty to the total of the change that caused it, without a lookup.
`spend` context operations are not priced in tokens. `PricedOp.Call` returns their `Call.Usage`
unchanged, and the host prices it with the same resolver as the savings (wiring document).

**Conservation.** For each request, the priced `Saved − Caused` of all content changes, cache
breaks and state changes, plus `Interaction`, equals the bill of the client body in the state it
would have had without any context operation, minus the bill of the forwarded body. The library checks
this in tokens (§6.5, check 6).

### 6.4 Unknown boundary

If the boundary is unknown (`Result.Boundary` is nil), the library uses the extent `cache_read` covers as the boundary, charges no
penalty, and adds `penalty_unknown` to `Unpriced`. This reports too little, which is the safe
direction.

### 6.5 Checks

The library reports failed checks in `Result.Checks`. A failure does not stop pricing.

1. **Sum:** for each section, the context operations that are applied, and not `DryRun` —
   `SizeBefore − SizeAfter` adds up to the client size minus the forwarded size. The collector sends both.
   A failure means a plugin changed the body and did not report it.
2. **Order:** the context operations are in pipeline order, so a context operator's output positions are mapped through the `MessageSplice` of the context operations before it.
3. **Unique `ChangeID`:** no two `rewrite` or `setting` context operations of one request have the same `ChangeID`. A `spend` or a `cacheChange` may share the `ChangeID` of the change it belongs to.
4. **Spans:** for a context operation with `Spans`, its `SizeBefore` and `SizeAfter` equal the sums over its spans.
5. **Attribution:** a break's `ChangeID` is the `ChangeID` of a context operation of the same request, or empty.
6. **Conservation:** the request total of the applied context operations equals the bill difference (§6.3).
   `DryRun` context operations are left out, because their content went upstream unchanged.

## 7. Billing plans and recording

How a host records the result, the size limit on recorded changes, and the totals over a session
or thread are in the wiring document (sections 7.1 to 7.3 there).

### 7.1 Billing plans

`context_counterfactual.Compute` returns the same tokens for each tier for an API user and for a subscription user. The
host converts them to a value by the request's `Billing.Mode`:

| `Billing.Mode` | Value of a saving | Value of a spend | Shown as |
|---|---|---|---|
| `api` | tokens × the provider's rates | `Call.Usage` × the provider's rates | money saved; money spent |
| `subscription` | tokens × the **weights**, in usage-limit units | `Call.Usage` × the same weights | usage-limit headroom; an API-equivalent dollar figure beside it, labelled "API equivalent" |
| `unknown` | as `api`, with every figure marked `Estimated` | as `api` | money, marked estimated |

**Weights.** A weight says how much one token of a tier counts against the usage limit, compared
with one input token. The provider does not publish weights for each tier. So the default weights
are the ratios of the API rates (input 1, cache write 5m 1.25, cache write 1h 2, cache read 0.1,
output 5), and every subscription figure is marked `Estimated`. A host can set measured weights in
its config (§11, question 2).

**Keep-alive on a subscription.** A ping on the user's own credential uses the user's usage limit,
not money. The keep-alive window still adds up (§4.4), in usage-limit units.

**Detection.** The host reads `Billing.Mode` from the credential type on the request: an API key
means `api`; an OAuth token from a subscription login means `subscription`. If it cannot tell, it
uses `unknown`. Cortex and the proxy must use one rule, so the rule is a function in `context_counterfactual`:
`context_counterfactual.BillingFrom(headers)`. It reads only the credential type and returns no part of the credential
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
| `context_counterfactual.Compute` panics | The host recovers, writes no saving for the request, and increments `ops_price_panics`. The request and its spend record are unchanged. |
| A malformed context operation (unknown kind, negative size) | Goes to `Unpriced` with a reason. The other context operations are priced. |
| Sum check fails | Pricing goes on. The failure is counted for each plugin on `/v1/pipeline`. |
| No previous fingerprint for the thread | `Boundary` is nil, §6.4. |
| Response has no usage | Nothing is priced, as today. |
| No rate for a tier | `Saving.Provenance` is empty, as today. Tokens are on the record. |
| Context operator not on the allow-list | Kept on the event, unpriced. |
| `on_error: observe` | `DryRun = true`. Priced and shown as "would have saved", never in totals. |
| `Billing.Mode` is `unknown` | Priced as `api`, every figure marked `Estimated`. |

## 9. Out of scope

- Signed totals in the ledger.
- Pricing of `setting`: the saving or cost of a model or param change (R50). The cache break it
  causes is priced; the change in output tokens is shown as an observation, not as a saving.
- A measured saving of a param change. It needs a control that keeps the old setting.
- Persisting fingerprints across a restart.
- A change that no context operator in the pipeline made, such as the client's own compaction. It is not
  recorded as a context operation. A client edit that breaks the cache appears as a `cacheChange`
  with no `ChangeID` (§5.2).
- **Known limitation: the payer of a call is the payer of its request.** A `spend` context operation has
  no payer of its own. It is priced with the `Billing` of its request. If a context operator calls a model
  on another credential (for example a summarizer on the deployer's API key, in a session on the
  user's subscription), the call is priced as the user's. A standalone `RequestOps`, such as a
  ping, carries the `Billing` of the credential it used.

### 9.1 What this design does not replace

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
  the fact that a declaration lives in `tools`. `tp:decl-set-7f3a`'s context operation (example 3 in the examples document) reports "3 declarations, 8,000 tokens," never which ones — these surfaces keep reading the
  engine's own declaration-scan data, not `Operation`.
- **Holdout/control-arm replay** (`dash/kvcachesuggest.go`, `kvcacheholdout.go`,
  `adaptcompaction.go`) needs the pre/post **text** at a message index to re-run a component under
  a different config. `Operation` carries no text (R2). These keep their own stored-text path;
  they are not driven by `ChangeID`/`Size`.
- **Keep-alive strategy attribution** (`dash/campaignsavings.go`) groups by strategy id, preset,
  stop reason — fields `components.KeepAliveReport` already carries (the wiring document keeps it as the
  keeper's report path) but a bare `spend` context operation does not. The dashboard columns these feed keep
  reading `KeepAliveReport`, not the `spend` context operation's `Call`.
- **The headline money total** (`dash/savings.go`'s `baseline − cost − cgllm + cachesplit + kaNet`)
  IS reconstructable from `event.Saving`/`ChangeSaving`, because the recorded form keeps the cachesplit,
  keep-alive and spend terms as separate, summable fields. Only the advisory/diagnostic detail
  above is out of this design's reach.

If a later design wants to drive any of the surfaces above from `context_counterfactual`, that is new scope, not an
extension of this one.

## 10. Test cases

Before the implementation starts, we make an **exhaustive list of test cases** with the current
saving computation. The list is the specification the implementation must meet. It also records
each place where the new figure differs from the current one on purpose.

### 10.1 Where the current figures come from

| Current rule | Repo | Used for |
|---|---|---|
| `dash.Event.Price`: `ComponentSavedUSD`, `uniqueRate`, `repeatRate`, `baselineDeltaUSD`, `cachesplitSavedUSD`, `keepaliveSavedUSD`, `BilledTokenFactor` | context-guru | every context-guru case |
| `settle`: `pruneDraft`, `compactionDraft`, `rewritePenalty` | Cortex (`feat/contextguru-keepalive`) | tool-prune cases, rewrite-penalty cases |

Cortex's `repeatUsage` is a copy of the proxy's `repeatRate`, so the proxy is the source for the
context-guru reductions. The library imports neither context-guru nor Cortex: the comparison with the Cortex rules
runs in Cortex (§10.4).

### 10.2 One case

Each case is a JSON file in `ops/opscases/`. The package embeds them and exports `All() []Case`,
so a test in Cortex can run the same list. A case holds only what the spec decides: the input and
the expected result. It does not hold the current code's figure, nor a judgement about it.

```json
{
  "id": "rewrite/replay/before-bstar/hit/5m",
  "dims": {"kind": "rewrite", "position": "beforeBStar",
           "cache": "hit", "ttl": "5m"},
  "input": { "...": "a context_counterfactual.Input" },
  "rates": "fixture-v1",
  "expected": { "...": "the context_counterfactual.Result, tokens for each tier" }
}
```

- `expected` is what `context_counterfactual.Compute` must return, by this spec.
- `rates` names a fixed rate table in the package. `fixture-v1` is: input $3.00/M, cache write 5m
  $3.75/M, cache write 1h $6.00/M, cache read $0.30/M. With one table, `expected` and the current
  code's figure can be compared in dollars.

The places where the current code is meant to give a different figure are in one separate file,
`ops/opscases/differences.json`. Each entry has a case ID, a kind, and a reason:

- `differs`: the figures differ on purpose. The reason cites the spec section that causes it.
- `notCovered`: the current code prices something the model does not. The case is listed in §11.

A case that is not in this file must agree with the current code. The current figure is never
stored: the agreement test computes it each time it runs (§10.4).

### 10.3 What makes the list exhaustive

**A. Generated cases.** A generator makes one case for each combination of these dimensions. It
drops the combinations that cannot occur, and states the reason for each drop in its code.

| Dimension | Values |
|---|---|
| Kind | `rewrite` smaller, `rewrite` larger (a restore is one), `cacheChange` |
| Position | before B\*; after B\* and at or before `L`; after `L`; no breakpoint in the body; spans on both sides of B\* |
| Cache state | hot; cold (expired, `Lifetime: exact`); unknown (gap past an unproven `Lifetime: minimum`, R45; first request of a thread; restart); hot, kept by a ping |
| Cache result | full hit; partial hit; miss with write ≥ input; miss with write < input |
| Breakpoints | end of `system` + last message (Claude Code); one after `tools` only; none; one inside the messages before a break |
| TTL of the next breakpoint | 5m, 1h |
| Lifetime | `exact` (Anthropic); `minimum` (OpenAI GPT-5.6+); `unknown` |
| Tokens | counted by a tokenizer; `bytes-estimate`; with and without the proxy's `BilledTokenFactor` |

**B. Hand-written cases.** These test how context operations interact, which a cross product cannot do:

1. The worked example: turn 5, the turn-10 keep-alive and the turn-7 coref break, with every figure in
   the example file.
2. Cache breaks: one for each cause (context operator, client, setting, expired). A context operator break gets the `ChangeID` of the context operation that changed the first differing position. Two
   context operators whose changes are both before the break: the break names the one that changed the first
   differing position.
3. Two context operators on the same content (R25), and a message splice that moves positions (`MessageSplice`).
4. A `DryRun` context operation: priced, shown as "would have saved", and not counted. A reverted change
   that still made a model call: no reduction, and its `spend` context operation counted.
5. Each `Unpriced` reason, and each `CheckFailure`.
6. `Saving.Changes` over the limit, which becomes an `Aggregate` (wiring document).
7. `spend` and `setting` context operations, which pass through unpriced.
8. Keep-alive: a window that rescued its request; a window whose pings did not stop the expiry; a
   gap inside the TTL, where the ping is not credited; two threads with their own windows.
9. `cachesplit` and `breakpointTTL`, with the current `cachesplitSavedUSD` as the source.
10. Changes that end: a summary of reduced messages, in both pipeline orders, with the same total;
    an ended change that is not reported again; a restore of the summary, after which the earlier
    changes are reported again.
11. A subagent thread: its context operations, its totals, and a session total that adds both threads.
12. The interaction on a rescued request, and the conservation check on every case.
13. A `coref` batch: three cuts in messages that are not next to each other, as one context operation with
    three spans, one `ChangeID` and one attributed `cacheChange`; its payback across later turns.
14. `extract_llm_sweep`: a prefix ask that reads the previous turn's cache, and a fallback ask that
    pays fresh input. Both are `spend` context operations; their `Call.Usage` differs.
15. `breakpointTTL`: a 1h head written and read after 20 minutes (both legs); a 1h head written and
    never read (cost leg only, negative total); the measured blanket-versus-gated case from
    `apply/headttl.go`.
16. `readlifecycle` and `skeleton`: the same decision with different replacement bytes on two turns
    keeps one `ChangeID` (§4.2 rule 6).
17. Interleaved threads: a main thread whose previous request is five requests back, behind four
    subagent requests, still compares with its own previous request (wiring document, W5.2).
18. Keep-alive conversion: a `KeepAliveReport` from each host's emitter gives the same `spend`
    context operation.

**C. Cases from real traffic.** For each difference that a shadow step (wiring document, migration steps 5 and 8) logs and
no case matches, add a case. Then decide: it is a bug in `context_counterfactual`, or a new `differs` entry with a
reason.

### 10.4 How the list validates the implementation

| Test | Where | Passes when |
|---|---|---|
| `context_counterfactual` unit test | context-guru | `context_counterfactual.Compute` returns `expected` for every case, including the expected `Boundary`, `Phase` and break for every cache case (§5.2). The internal comparison has its own unit tests. |
| Agreement with the current code | context-guru | The test runs `dash.Event.Price` on each case's input. Each case not in `differences.json` equals `expected` within one micro-dollar. Each `differs` entry really differs, so a stale entry fails. |
| Agreement with the Cortex rules | Cortex, until step 10 | The same test with `pruneDraft`, `compactionDraft` and `rewritePenalty`, for the tool-prune and rewrite-penalty cases. It is removed with those rules. |
| Review of the differences | PR review | Every `differs` entry cites a spec section, and a reviewer approves `differences.json`. |
| Cortex contract test | Cortex | Cortex builds the same inputs through its own plugins and collector, and gets `expected`. A type change in the library fails the Cortex build on upgrade. |
| Shadow steps | both | Every logged difference matches a `differs` entry. |
| Removal gate | both | No rule is removed while a `notCovered` entry uses it. |

### 10.5 Other tests

1. **Property tests in `context_counterfactual`:** `Saved` and `Caused` are never negative; an empty list gives an
   empty result; reordering context operations of different context operators does not change each one's price,
   except for break attribution.
2. **End-to-end:** the LiteLLM integration test (`06c712cb` on `feat/contextguru-keepalive`) checks
   that the reported saving equals the change in billed cost against the control arm, within a
   tolerance.

## 11. Open questions

Each has test cases that are marked `differs` or `notCovered` in `differences.json` until it is decided (§10.3).

1. **The interaction on a rescued request (§6.2).** It is attributed to nobody. An alternative is
   to give it to the keep-alive window, because the window caused the state. That would make the
   keep-alive figure larger on every request with large reductions. A decision is needed before the
   test cases are frozen.
2. **Usage-limit weights.** The provider publishes no weights for each tier, so the defaults (§7.1)
   are the API rate ratios, an estimate. It is not verified whether a subscription response carries
   usage-limit headers that a host could use to measure the weights. If it does, a calibration
   could replace the defaults for each plan.
3. **The tokenizer correction.** The proxy multiplies its counted tokens by `BilledTokenFactor`
   (o200k_base to the provider's tokenizer). Cortex uses a bytes ratio. Option: `Input` takes a
   factor for each tokenizer name, so `context_counterfactual.Compute` applies the correction in one place. The
   generated cases test both, with and without the factor.

4. **Where the previous request's hashes live.** The counterfactual needs the previous request's
   hashes on the same thread. The current design (D12, wiring document) has the host keep one
   fingerprint for each thread. An alternative is a push model: components push facts with an
   identity, and the receiver keeps no state. Then the hashes travel in each push, or the context operator
   keeps them, and who classifies a `cacheChange` changes with it. Not decided.
5. **Label volume.** Pushing `change` and `request` as labels on every series gives many series in a
   long session. This must be measured before the push model is fixed.
6. **Asynchronous recalculation.** A replay should not cause a recalculation every
   time. Recalculation should be async and buffered. A newer request for the same key replaces the
   queued one and keeps its queue position, so nothing starves. The queue design is not written.
7. **Where the library lives.** The module path `github.com/rossoctl/context-guru/context_counterfactual` is a
   placeholder. Its home and name are not decided (D7).
8. **Names in Cortex.** `pctx.ReportOps` and the Cortex name for the component that writes to the
   cost ledger have no chosen name (wiring document).
9. **The payer of a call.** A call is priced with the billing of its request (§9, known limitation).
   A summarizer on the deployer's key, in a subscription session, is priced as the user's.
10. **The rates of a setting break.** A break caused by a `setting` change (a new model) has no dollar
    figure in example 5. The proposed rule: price the moved tokens at the rates of the model that
    serves the request after the change. Not decided.
11. **What the Collector's state holds.** The active context operations are in it. Whether the cache
    state (phase, cached extent, TTL, time of the last request or ping) is in it too is not decided.
    Today the host keeps the fingerprint and passes `Prev` and `Facts` in `Input` (§6.1). If the
    state moves into the Collector, those fields leave `Input`, and the host passes only the request.
12. **How a context operation ends.** A change is reported once, and `Compute` applies it on the
    following requests until it is overwritten. `Compute` must find the end by itself, because the
    context operator reports nothing after the first request. The rule is not defined. This blocks
    the test cases that cover an ended change, and the rule for an operator that overwrites its own
    change.
