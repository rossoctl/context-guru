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
  - [What changes the cached prefix](#what-changes-the-cached-prefix)
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
  - [4.4 Context operations that change the cache state](#44-context-operations-that-change-the-cache-state)
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
   out of message 9." It says nothing about money. We call this record a **context operation**.
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
context operators and the host import the `counterfactual` library. The library imports none of them (R3).

![The counterfactual library, Cortex as its consumer, and the context operators that report context operations](packages.svg)

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
| Tracker | A class of the library. It keeps the state of one thread, in memory or persisted: the context operations that are still active. A context operation is reported once, on the first request of its change. On each later request `Compute` applies the active ones again, with the same context operator, until they end. What else the state holds is open (§11, question 11). The host's pipeline adds each reported context operation to it (wiring document, section 5). |
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
  Cortex and, we hope, other AI frameworks. `Compute` is deterministic: it depends on the request and on the state of the thread. It decides which tier every token
  falls in, using the cache state of the request. It is the only place that holds this logic.
- **Each host keeps its own rates.** A host multiplies tokens by its own price list. Two hosts get
  the same tokens for the same request, and may differ in dollars when their price lists differ.
- **Totals are sums over a session.** A change's total is the sum over the requests that carry it. A
  thread's and a session's totals are sums over their context operations.
- **A list of test cases defines correct.** We build it from the current computation before any host
  uses the library (§10).

### What changes the cached prefix

A provider reuses a cache entry only when the rendered prefix of a new request is the same as the
prefix of the entry, up to a cache breakpoint. A change before the breakpoint breaks the match from
the first changed token onward. The change can be in the content, or in a setting that the provider
renders into the prefix. The model keeps these changes as a `setting` context operation (R50) when a
context operator makes them, and as a client edit when the client makes them (§5.2).

The settings are different for each provider. The list is in [`cached-prefix.md`](cached-prefix.md).
It covers OpenAI and Anthropic so far, with the provider facts that the model depends on (lifetime,
write price, reporting, granularity). It grows as each provider is read.

## 1. Goal

- Each context operator reports the **context operations** it performed on the context, as **facts**.
- **One library**, standalone and usable by any AI framework, turns context operations into **tokens for each billing tier**.
  A **tier** is the rate class a provider bills a token at: input, cache write (5m or 1h), or cache
  read.
- **Each host** converts tokens to dollars with its own rates, and records them. There are two
  hosts: **Cortex** (`pricing.Resolver`) and the **context-guru proxy** (`internal/modelinfo`).
  For the same request, both get the same tokens for each tier.
- **What is centralized is the logic, not the rates.** The `counterfactual` library decides which tier every
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
| D1 | Does the library need to know that a change is applied for the first time? | **No.** The tier comes from the position and the cache state (§6.2). A report finds the first application as the first event with that `ChangeID`. A context operator reports a change once, and `Compute` applies it again on later requests (D14, §4.6). |
| D2 | Where is the savings logic? | **In one standalone library,** shared by context-guru and Cortex, and open to other AI frameworks. Hosts call it and keep no operator-specific pricing rules. |
| D3 | What does the library return? | **Tokens for each tier**, not dollars. Cortex prices them with its own rates (discounts, overrides, long-context tiers), so a saving and the request's spend use the same rates. |
| D4 | How is cg's own spend recorded? | **As context operations, in the same report as the savings.** A `spend` context operation carries the provider's billed usage of the call (`Call`). The host prices it with the savings, in one path. There is no second record and no `CostRecorder` (wiring document). `counterfactual.Compute` does not price the call; it returns `Call.Usage` for the host to price (D3). |
| D5 | Shape | **One typed context operation for each change unit**, in-process only. The recorded summary (`event.Saving`/`ChangeSaving`) has the aggregate fallback above a limit, not the context operation list itself (R20; the recorded form is in the wiring document). |
| D6 | How is a saving across requests counted? | **Realized on each request, not projected.** Each request is priced from its own cache result. A context operator reports a change once, on its first request. The Tracker remembers the active changes of the thread, and `Compute` applies them again on each later request (D14, §4.6). **The risk:** a remembered change can be wrong, because the Tracker cannot see the content. The sum check (§6.5) can catch it, but a lost state does not heal by itself (§4.6, §11 question 13). |
| D7 | Where do the types live? | **A standalone module with no dependency on context-guru, on Cortex, or on any framework.** context-guru and Cortex are two users of it, and other frameworks can be too. The module path `github.com/rossoctl/context-guru/counterfactual` used below is a placeholder; its home and name are not decided. |
| D8 | Which hosts compute dollars from context operations? | **Both: Cortex and the context-guru proxy.** The proxy stops using its own saving rules (`dash/event.go`) when it switches. The comparison of request fingerprints (§5) is inside `Compute`, so the proxy does not need Cortex to find B\*. |
| D9 | How is the implementation validated? | **Against an exhaustive list of test cases made with the current computation** (§10), before any host switches. |
| D10 | How is the cache state used? | **As an input to every price.** The tier of a content change depends on the state (§6.2). A change in how the provider bills content that is already there (a cache effect, §4.4) is priced as the difference between two states. |
| D11 | What happens to a change that a later change ends? | **`Compute` finds that it has ended and stops applying it.** The context operator reports nothing after the first request (D14), so it cannot say that a change ended. No link names the change that ended it. `Compute` infers the end from the bodies and the splice (§11, question 12). The measurement at each context operator's own input (R29) gives the right price and stops any double count, while the change is in the state. Each change keeps the total it had until then (§4.5). |
| D12 | Where does the thread ID come from? | **From the engine's `internal/thread`, in both hosts**, carried on the request. The host keeps one fingerprint per thread, keyed by `thread.Key`, and compares each request with its own thread's previous request (wiring document, W5.2). |
| D13 | How does the model serve a subscription user? | **The library does not change.** It already returns tokens for each tier (D3). The request carries a `Billing` fact. The host converts tokens to dollars for an API user, and to usage-limit units for a subscription user, with an API-equivalent dollar figure beside it, marked as such (§7.1). |
| D14 | Is `Compute` stateful? | **The library is stateful. `Compute` is deterministic.** A context operator reports a change once, on the first request. `Compute` applies it again on later requests until it ends, so a result depends on earlier requests. The state is a `ThreadState`, kept by the Tracker, one for each thread. `Compute` is a method of the request record. The record takes a copy of the `ThreadState` in `EndRequest`, because the thread is known only after the plugins ran (§6.1). As its last step `Compute` replaces the thread's `ThreadState` with the new one, so the host has nothing to commit and cannot forget to. A failure or a panic leaves the old `ThreadState`. Two requests of one thread at the same time (a retry, parallel calls of one agent) are safe: the replace is guarded by the `Version` of the `ThreadState` (optimistic locking, R53, §6.1). Rejected: a lock from `EndRequest` to `Compute`, because requests of one thread would wait for each other. `Preview` does the same calculation and stores nothing, for pricing an old request again and for a look that must not move the thread forward. A test case is a sequence of requests. Rejected: `Compute` returning the new state and a separate `Commit`. It is purely functional, but a host that forgets the commit gets a wrong figure with no error. Rejected: `Compute` writing the state in the middle of the call, because a failure would leave it half updated. What the state holds is open (§11, question 11). How a change ends is decided (§11, question 12). |

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
- **R26.** `Compute` is deterministic: its result depends only on its record and on the
  `ThreadState` that the record started from, and it does no I/O. The Tracker holds the `ThreadState`.
  As its last step `Compute` replaces it with the new one; a failure leaves the old one (D14). The same
  `ThreadState` and the same request give the same figure in the context-guru proxy and in Cortex.
  `Preview` gives the same figure and stores nothing.

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

  What the library does instead: it keeps the `setting` context operation; it records a break (a cache effect) with
  the `setting` context operation's `ChangeID` when a request after the change is cold (§5.2), which is measurable; and a report
  can show output tokens per request before and after, labelled as an observation, never in
  `TotalAvoidedUSD`. A measured saving needs a control (a holdout that keeps the old setting). That
  is out of scope (§9).

### C. Facts for pricing

- **R7.** Each context operation has a position: section and index, in the provider's hash order.
- **R8.** Sizes are in bytes. A context operator sends no tokens. The library converts bytes to
  tokens and marks the result `Estimated` (§4.2 rule 7).
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
- **R15.** A request is priced from its input and from the state of its thread. A request is **not**
  complete without history: the active context operations of the thread are part of the input. If the
  state is missing (a restart, an eviction), the library does not guess. A change it was never told
  about is not applied, so its saving is missing, and the sum check (§6.5, check 1) fails on that
  request. Prices that depend on the state say so (§4.6).
- **R25.** When two context operators change the same content on one request, each one is credited for
  its own change, in pipeline order.
- **R51.** The saving or cost of a request can be attributed to each context operator. The result
  gives tokens per tier for each context operator (R27, R29), and the sum over the context operators and
  the items attributed to nobody (a cache break with no plugin cause, R31, and the interaction, §6.2)
  equals the request total. A context operation that is applied again on a later request keeps its
  context operator, so its saving stays with the same plugin.
- **R53.** Two requests of one thread can run at the same time (a retry, parallel calls of one agent).
  A subagent is not one of them: it is its own thread, with its own Tracker. A keep-alive ping and a
  detached call are not either: each is a standalone `RequestOps`, which reads and writes no
  `ThreadState` (§6.1).
  The Tracker is safe for concurrent use. No request loses the changes of another. A `Compute` that
  finds that the `ThreadState` changed since its `EndRequest` calculates again from the current
  `ThreadState` and says so in the `Result` (`Recomputed`). The order of the results is the order of the
  `Compute` calls (D14, §6.1).
- **R52.** The library never returns one token total. It returns tokens for each tier. Tiers differ in
  price by up to 12.5 times, so a sum across them is not an invariant. A host that wants one figure
  prices the tiers and sums dollars. This is the rule of the tool-prune accounting (cortex#952).
- **R35.** A later change can end earlier ones (a summary of reduced messages). The model shows
  this by `Compute` no longer applying the earlier change, not by a link and not by the context
  operator: after its first report the context operator says nothing about the change. `Compute`
  infers the end (§11, question 12). The totals of the earlier changes until then do not
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
- **R46.** Each recorded saving names the version of the `counterfactual` rules that computed it
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
- **R20.** `RequestOps`/`Operation` is request-scoped and in-process only: input to `counterfactual.Compute`,
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
package counterfactual // github.com/rossoctl/context-guru/counterfactual

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
    // Ops is in-process only: input to counterfactual.Compute for this request, never recorded (R20).
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
    ID     string       // "main" for the primary thread. internal/thread gives the primary thread
                        // the ID "", and the host maps "" to "main"
    Parent string       // the thread this one was forked from; "" if none
}


type Operation struct {
    ID        string // unique inside the request only; across requests, use ChangeID (R6)
    Kind      Kind   // rewrite | spend | setting. There is no kind for a cache effect: only the library finds one (§4.4).
                     // setting: a request parameter that changes the cost or the cache and that the body does not show (§4.2)

    Operator  string // "context-guru.mask", "tool-prune"; the first part is set by the framework

    DryRun    bool // set by the framework: computed but not sent (R12)

    At     Position // first position the change touches, in this operator's output.
    Spans  []Span   // every touched range, when the change is not contiguous; nil otherwise
    SizeBefore Size // what the operator received over the touched ranges; the sum over Spans
    SizeAfter  Size // what it sent on over the same ranges

    // kind = rewrite | spend | setting
    ChangeID     string   // the same on every request that carries this change. On a spend: the change the call created, or the keep-alive window. 

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
    // At is the first position of the prefix that the provider renders this setting into: the first
    // position whose cache entry the change can invalidate (cached-prefix.md). A model change starts at
    // tools[0]; an effort change on a model that renders it ahead of the messages only starts at
    // messages[0]. The bodies do not differ where a setting differs, so the library cannot find
    // this position by comparing hashes (§5.2). Nil means the reporter does not know.
    At *Position
}

// Size is bytes only. A context operator does not count tokens: the library converts bytes to tokens (§4.2 rule 7).
type Size struct {
    Bytes int // required on rewrite; the sum check compares it with the Body sizes (§6.5)
}

type MessageSplice struct{ At, Removed, Inserted int } // messages only

// CacheState is the state of the thread's provider cache entry: its phase, the last position it
// covers, and its lifetime. It is what `Compute` reads, and what a cache effect moves from and to.
// It is internal: no input type carries one.
type CacheState struct {
    CachePhase CachePhase // hot | cold | unknown
    Extent     *Position  // the last position the entry covers; nil when nothing is covered
    TTL        string     // "5m" | "1h": the lifetime the breakpoint asks for; empty when cold or unknown
}

// EntryFacts are the facts that decide the cache phase and that only the host knows. The host passes
// them in `EndRequest`; the library reads them in its internal comparison (§5), and nowhere else.
// The library finds the two gaps itself (§5.2), from `ThreadState.PrevPrint.At`, `LastPing` and the
// time of the request, so a recompute after another request (§6.1) uses the current state.
type EntryFacts struct {
    TTL  time.Duration // lifetime of the entry: 5m or 1h, from the breakpoint that wrote it
    // Lifetime says whether TTL is a proven expiry (exact) or only a guaranteed floor
    // (minimum): Anthropic's TTL is exact; OpenAI GPT-5.6+ states only a 30-minute minimum
    // with no documented maximum (R45, §5.2). A gap past TTL proves expiry only when exact.
    Lifetime LifetimeKind
    // The last keep-alive ping of this thread since its previous request: when it started, the
    // extent it read, and the ChangeID of its keep-alive window. Zero values when no ping was sent.
    LastPing   time.Time
    PingRead   *Position
    PingWindow string
}

type Kind string          // rewrite | spend | setting
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
4. **Only the library finds a cache effect**, inside `Compute` (§5). A context operator reports `rewrite`, `spend` and `setting`. The input type has no form for a break, a rescue or a change of breakpoint, so a host cannot report one by mistake. The output has a `CacheEffect` type for them (§6.3).
5. **A keep-alive ping is an ordinary `spend` context operation.** Its `Call` carries the ping's billed
   usage. Its `ChangeID` names the keep-alive window, and the rescue (a cache effect) on the
   request it rescued has the same `ChangeID`. So the cost and the value of a window add up
   (§4.4).
6. **`ChangeID` is derived from the context operator and the original content key only — never from the
   replacement bytes.** This matches the engine's own stable key (`frozenKey(session, component,
   contentKey(original))`, `components/offload/state.go`) rather than adding to it. Two
   context operators that remove the same content in different ways still get different IDs, because
   the context operator name is part of the key.
   `readlifecycle` and `skeleton` give different replacement bytes for the same decision on
   different turns, so keying on the replacement bytes would churn the ID.
   The content key alone is not unique in a session: the same tool output can appear twice, and
   the engine then replays one frozen decision at two positions. These are two changes, so the
   `ChangeID` also takes the ordinal of the occurrence (the first, the second, ... position in the
   client body with that content key). Check 3 (§6.5) then holds.
7. **A context operator reports bytes only. The library calculates the tokens.**
   Removed tokens are not in the forwarded body, so the library takes the bytes from the context
   operation's own `Size` and converts them. The conversion uses the request's own ratio: the
   bytes of the forwarded body divided by the billed input tokens of the request (input + cache read +
   cache write). The provider bills the forwarded body, so the ratio is taken on it. If the request has no usage, the library uses a fixed fallback ratio. The library
   marks every priced result that uses this conversion as `Estimated`. No operator names a tokenizer,
   so two operators give the same tokens for the same bytes.
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
 "settings": {"model":  {"from": "large-model", "to": "small-model", "at": {"section": "tools", "index": 0}},
              "effort": {"from": "high",        "to": "low",         "at": {"section": "messages", "index": 0}},
              "thinking": {"to": "8000",                             "at": {"section": "messages", "index": 0}}}}
```

`thinking` has no `from`: it was not set before, and it appears now.

**What a setting change is.** A setting is a request parameter that the provider uses to build the
prompt or to choose the model, and that the body the host sends does not show. Two requests with the
same body can have different settings, so a comparison of body hashes (§5) cannot see the change. A
change of content is not a setting: a different tool definition is a body change, and `Compute` finds
it by hash. The point of the `setting` kind is that it records a change the hashes cannot find, and
`At` says where in the prefix the provider would stop matching.

The settings, and where each provider renders them, differ by provider. [`cached-prefix.md`](cached-prefix.md)
holds the full lists with their sources (read 2026-10-25). This is what the model takes from them:

| Provider | Setting | First level that stops matching | `At` |
|---|---|---|---|
| Anthropic | Web search toggle, citations toggle, `speed` | `system`, and `messages` after it | `system[0]` |
| Anthropic | `tool_choice` | `messages` only | `messages[0]` |
| Anthropic | Thinking parameters (mode, `budget_tokens`) | `messages`; `tools` and `system` too on models that render the configuration ahead of them | `messages[0]`, or `tools[0]` on those models |
| Anthropic | `output_config.effort` | `messages`; the model default is the same as omitting it, so nothing breaks. `tools` and `system` too on some models | `messages[0]`, or `tools[0]` on those models |
| Anthropic | `model` | Not in the provider's table. The cache is keyed to the exact prompt prefix and the model is part of the request, so the design treats it as a break (an inference) | `tools[0]` |
| OpenAI | `model` | The guide lists it: a different model can use different weights and caching behaviour. It names no level, so the design starts the break at the start of the prompt | `tools[0]` |
| OpenAI | `reasoning.effort`, `text.verbosity`, `parallel_tool_calls`, `text.format` | The guide says the rendered prefix may change, and gives no level | nil |

Rules that follow from the table:

1. **A level that stops matching breaks every later level.** On Anthropic the cache has three levels in
   the order `tools`, `system`, `messages`. `At` is the first position of the first level that breaks.
2. **`At` is where a break would start, not a statement that one happened.** A provider does not always
   discard an entry when a setting changes. OpenAI says so, and for `reasoning.effort` on supported
   models a configuration update keeps the earlier prefix. The library adds a break only when
   `Usage.CacheRead` is below the read the forwarded body predicts (§5.2, row 3).
3. **Where the behaviour depends on the model, the reporter chooses.** A thinking or effort change
   starts at `tools[0]` on one model and at `messages[0]` on another. The reporter knows the model.
   When it does not know the behaviour, it leaves `At` nil.
4. **A nil `At` is safe.** The library then takes the read that the usage shows as the end of the valid
   prefix, and adds `penalty_unknown` to `Unpriced` (§6.4). This reports too little, never too much. OpenAI
   settings are nil for this reason: the guide names no level.
5. **A change that the client makes is not a `setting` operation.** The client's own change of a
   setting has no operator and no `ChangeID`. With equal bodies it reaches row 4 of §5.2: the entry
   was lost early, and the break has no `ChangeID`.
6. **Content that a provider also lists is not a setting.** `tools` on OpenAI, tool definitions and
   images on Anthropic, and OpenAI `context_management` (compaction) change the body. They are
   `rewrite` operations or client edits.

The table covers what `cached-prefix.md` has read. It does not cover `host`, `max_output`, other
providers, or settings added later. A setting that is not in the table has an unknown effect on the
cache, so its `At` is nil (rule 4).

### 4.3 Why positions are (section, index)

The provider hashes `tools`, then `system`, then `messages`. A comparison of two positions in that
order shows whether a change is before a cache boundary. For a change with `Spans`, each span is
compared on its own and priced with its own `SizeBefore − SizeAfter`. So a change with spans on both sides
of B\* gets the right tier for each part. A context operator reports positions in its own output. If a
later context operator adds or removes messages, it reports a `MessageSplice`, and the library maps earlier
positions into the forwarded body.

**Two coordinate spaces.** The library uses two spaces, and every rule names the one it uses:

- **Client space**, the client body. B\*, `clientFirstDiff`, the tier of saved tokens (§6.2), and
  the counterfactual bill are in client space, because the counterfactual is the body no context
  operator touched.
- **Forwarded space**, the forwarded body. `fwdFirstDiff`, the read extent of `Usage.CacheRead`,
  `L`, the forwarded breakpoints and the break penalty are in forwarded space, because that is
  what the provider cached.

To map an operation's position into client space, the library undoes the `MessageSplice` of every
operation before it in pipeline order. To map it into forwarded space, it applies the splices of
the operations after it. A position inside a range that a later splice removed has no forwarded
position: the change has ended there (§4.5).

**Active operations on a later request.** An active operation keeps the position it had in client
space on its first request. On a later request the client body can grow only at the end, or change
by a client edit, so its client-space position is still valid until the client edits before it.
Then the library maps it into forwarded space through this request's splices, as for a reported
operation. A client edit before an active change, or a client compaction, is a case for the end
rule (§11, question 12): it ends the change.

### 4.4 Cache effects

A cache effect does not change the content. It changes how the provider bills content that is already
there. **It is not an operation.** No context operator reports one: the library finds it by comparing
requests (§5), so it exists only in the output, as a `CacheEffect` in `Result.CacheEffects` (§6.3).
The input type has no kind for it. An earlier version had a `cacheChange` operation that only the
library was allowed to write. That rule was a convention a host could break, so the type no longer
allows it.

A `CacheEffect` has a kind, a prefix range, and a `ChangeID`. The range is `From` to `To`. A nil `From`
is the start of the prompt, and a nil `To` is the end of the body. One exception: a `break` with no B\*
(no client breakpoint at or before the first difference, §5.1) has an empty range. Its `To` is nil, it moves
no tokens, and the `Saved` and `Caused` tiers are zero. Five kinds cover it:

- **`break`: a prefix is broken.** The provider no longer reads content it read before. `From` is the
  end of what it still reads and `To` is B\* (§6.2). The `ChangeID` is the change that caused it (§5.2),
  or empty when nobody did. It is a cost.
- **`rescue`: a prefix is warmed.** The provider read an entry that a keep-alive ping kept alive, so it
  did not expire (`cold` to `hot`). The `ChangeID` is the keep-alive window.
- **`split`: a prefix is warmed by a split.** A split of the system block means the stable half is read,
  not written, when the volatile tail changed. The `ChangeID` is the split's `rewrite`.
- **`ttlPremium` and `ttlRescue`: a prefix is protected.** A breakpoint changes for how long content is
  cached. Example: the head of the request is held for 1h, not 5m. It has **two legs:** a cost on
  **every request that writes** the head at 1h (`ttlPremium`), and a saving on each request that reads
  the 1h entry after a gap longer than 5m (`ttlRescue`). `TTL` says which entry the effect is about.

**When the library adds a rescue.** All four conditions must be true. They are the
conditions of the current `keepaliveSavedUSD`:

1. The keeper sent at least one ping for this thread during the gap before this request
   (`EntryFacts.LastPing` is set).
2. The **request gap** (§5.2), from the previous real request of the thread, is longer than the
   entry's TTL, so without a ping the entry would have expired. The TTL is the lifetime of the
   breakpoint that wrote the entry, so a 1h entry is not rescued after 9 minutes. And the lifetime
   is `exact`: on a `minimum` provider an elapsed gap cannot prove that the entry would have
   expired (R45), so no rescue is added. This is stricter than the current
   `keepaliveCreditFloorMs`, which credits OpenAI GPT-5.6+ after its 30-minute minimum, and uses
   the 5m TTL for a 1h entry. Both are `differs` entries (§10.2).
3. The provider read more than it wrote.
4. The tokens are the smaller of what the last ping read and what this request read.

**The ping and the rescue share one `ChangeID`,** the ID of the keep-alive window. The host passes
it in `EntryFacts.PingWindow`, built by the same rule as the ping's `spend` (wiring document, W4.6). A window whose
pings rescued nothing has only `spend` context operations, so its total is negative. That is the cost of a
ping that was not needed, and it is visible for each thread.

**A longer breakpoint TTL is a bet, and both legs are recorded.** The 1h write premium (2.0× input against
1.25× for 5m) is paid on every request that creates the head entry at 1h, whether or not a later
request ever reads it. The benefit appears only on requests that arrive more than 5 and less than
60 minutes later. Measured over 19,805 production requests, a blanket 1h head cost $773.00 extra on
14,499 creating requests to recover $754.66 on 290 — net −$18.34; size-gating turned it positive.
If the model priced only the reading request, every write that never paid off would be missing, and
the figure would overstate the bet. So, like a keep-alive window:

- **On each 1h-writing request,** the library adds a `ttlPremium` effect for the head's breakpoint TTL
  (`TTL` `1h`, in place of `5m`) and only a cost: `Caused.CacheWrite1h` for the head's tokens, less the 5m write it replaced. Its
  `ChangeID` names the head entry (`"headttl:" + Thread + ":" + head content key`).
- **On each request that reads that entry after the 5m TTL,** the library adds a `ttlRescue` effect with
  the same `ChangeID` and the saving (write → read, §6.2).

A head entry that is written at 1h and never read again has only the cost leg, so its total is
negative — the same visibility a keep-alive window that rescued nothing has.

### 4.5 Changes that end

A change can remove content that earlier changes had already reduced. Example: on turn 12 a
summary replaces messages 0 to 8, and messages 3 and 5 were reduced by `mask` and `coref`.

**A change ends when `Compute` stops applying it.** The context operator reports a change once (D14), so
after that it cannot report that the change ended, and there is no field that names the change
that ended it. `Compute` has to find the end by itself, and drop the change from the state of the
thread. **The rule that finds the end is decided** (§11, question 12): `Compute` infers it from the
bodies and the splice. What follows describes what must hold once the end is found.

1. **The price.** Each request is priced from its own cache result (D6). A change that has ended saves
   nothing on later requests. Its total until then does not change, so a change that did not pay back
   before it ended shows a loss.
2. **No double count.** Each context operator's change is measured at its own input (R29), and the sum
   check tests the total (§6.5, check 1). So the tokens are counted once, in either pipeline order:

   | Pipeline order | `mask` | `coref` | summary | Total |
   |---|---|---|---|---|
   | Summarizer first (cg's `medium`, `high`, `xhigh` presets) | ended | ended | 40,000 → 1,500 = 38,500 | **38,500** |
   | Summarizer after the reducers | 3,940 | 19,950 | 16,110 → 1,500 = 14,610 | **38,500** |

   The order changes **who gets the credit**, not the total. Credit follows pipeline order (R25).
   In cg's presets the summarizer runs first, so it gets all of it: the reducers find their content
   gone, so `Compute` must not apply `mask` and `coref` again.
3. **A restore.** If the summary is reversed (a restore, or the summary is dropped), the earlier
   changes are back in the body. The context operator reported them once, and does not report them
   again, so `Compute` has to apply them again from the state, or they stay missing. This is the case
   that tests the end rule: a change that `Compute` dropped for good cannot come back (§11, question 13).

**What happens if `Compute` gets it wrong.** If it applies a change that has ended, the sum check fails
on that request, because the context operations remove more than the client body held. If it drops a
change that is still there, the sum check fails because the context operations remove less. Both are
reported in `Result.Checks`, and pricing goes on (§4.6).

**Why a change stopped** is not on the context operation. A report can find it: the change last appeared at
position P on request N, and request N+1 has a change whose span covers P (`At` and `MessageSplice`).
That is a question for a report, not for a price, so the report answers it.

### 4.6 Replays: who remembers past changes

**A context operator reports a change once, on the first request of the change. The Tracker remembers
it, for each thread. `Compute` applies it again on every later request of the thread, until it ends
(D14).** The context operators report less. They do not repeat a change on each request.

**What the old rule was, and why it is replaced.** The old rule was the opposite: each request listed
every change in its body, fresh or replayed, and the context operator was the only one that
remembered. The reason was that the context operator sees the content. It re-applies each change on
every request anyway, because the client sends its whole transcript on every turn, so it already knows
every change in the body. The cost of that rule was size: a replayed context operation repeated about
200 bytes on every request, and every context operator had to carry the replay in its report. The new
rule removes that cost. It moves the memory to a place that cannot see the content.

**The risk, stated plainly.** The old design rejected a register of past changes in the host,
for this reason: it must move positions after each `MessageSplice`, find which changes a
summary ended, and drop changes when the agent compacts its own transcript. It cannot see the content,
so it cannot tell whether a change is still in the body, and a wrong guess is silent. That reason still
holds for the Tracker. It is the risk of the new rule. What the new design does about it:

1. **A wrong guess is no longer silent in sizes.** `Compute` has the size of every position of the
   client body and of the forwarded body (taken in `BeginRequest` and `EndRequest`, §6.1). The sum check (§6.5, check 1)
   compares them with the sum of the context operations, those reported now and those applied again. If
   `Compute` applies a change that has ended, or misses one that is still there, the sums differ and
   the check fails on that request. The conservation check (check 6) fails too.
2. **What the checks do not catch.** A check works on sizes for each section. Two errors of the same
   size in one section cancel out. The checks also do not say which change is wrong. And a failed check
   does not stop pricing: the figure is still produced, with the failure in `Result.Checks`.
3. **The end rule is an inference.** `Compute` must find by itself that a change has ended, because the
   context operator reports nothing after the first request (§11, question 12). A change that ends
   with bytes identical to the changed form is not found. For that case the guard in point 1 is the
   only protection, and it detects an error after the fact. It does not prevent one.

**A lost state does not heal by itself.** This is new, and it is the main cost of the rule. If the
Tracker loses a thread's state (a restart, an eviction, a thread that is created again), the context
operator does not report the change again. The change stays missing for as long as it lasts. Every later
request of the thread fails the sum check, and its saving is too low, until the change ends. Under the
old rule, one missing request was only one missing figure, because the next request listed the change
again. How the state recovers is open (§11, question 13).

**Re-pricing an old request needs its state.** The old design could price an old request again from
that request alone. Now `Compute` also needs the state that the thread had at that time. A replay of
a session must start from the first request of the thread, or from a stored state. The session archive
has gaps (`droppedEvents`). One lost request there makes every later request in a replay wrong, so the
archive alone cannot rebuild the state.

**What stays in the state after a request.** The Tracker keeps the context operations that are still
applied on the next request, and drops the others:

| Context operation | Kept in `ThreadState.Active` | Why |
|---|---|---|
| `rewrite` (tool-prune, mask, a summary) | yes, until it ends | The client sends the same transcript again, so the change applies again on every later request |
| `setting` (model, effort) | yes, until the setting changes again | The setting still holds on the next request |
| `spend` | no | A model call happened once. Only its `ChangeID` is kept, to join it with the later saving |
| a cache effect | no | It is not an operation. `Compute` finds it for one request and returns it in `Result.CacheEffects` |
| a `rewrite` or `setting` with `DryRun` | yes, as `DryRun`, until it ends | A component in observe mode computes the same change on every request. If it were dropped, "would have saved" would show on the first request only, and an applied change would add up on every request, so the two could not be compared. It is never counted in totals (R12) |
| a `rewrite` that has ended | no | See §4.5 |

**From observe to enforce.** When the plugin leaves observe mode (a config reload), the change is
applied from the next request on, but the context operator does not report it again. So the
framework reports the switch: on the first request after it, it reports the plugin's active
`ChangeID`s again with `DryRun = false`. The Tracker replaces the `DryRun` entry with the applied
one, under the same `ChangeID`. The forwarded prefix changes at the first such position, so §5.2
row 1 adds a break with that `ChangeID`. That is the real cost of leaving observe mode.

So the state grows with the number of changes that are alive in the thread, and not with the number of
requests. If nothing ever ended, it would grow with the session. That is why the end rule (§11,
question 12) decides whether the memory is bounded: a change that is not found ending stays.

**What does not change.**

- The tier comes from the position and the cache state (§6.2, D1). The library does not need to know
  that a change is applied for the first time.
- `ChangeID` is the same on every request that carries the change (R22). It is the key that joins the
  first report to the later requests.
- A change keeps its context operator (R51), so its saving stays with the same plugin.

**A stateless context operator.** tool-prune keeps no state. It reads its `remove` list from config and
removes the same declarations from every request. It reports the change once. `ChangeID` is a hash of
the removed set and the config, so it is the same for the same config. The Tracker holds the rest.

**A config change of a stateless context operator.** A new `remove` list (for example, a hot reload) gives
a new `ChangeID`. The old change ends and a new one starts. `Compute` learns that the old change
ended by the end rule (§11, question 12). If the new list removes a declaration
that the old list kept, the forwarded body changes inside the cached prefix. The library then adds
a break that carries tool-prune's `ChangeID` (§5.2). That is the real cost of the config change.

## 5. Comparing requests

The comparison is **internal to the library**. It is not part of the public API. `counterfactual.Compute` runs it
first: it compares this thread's previous request with the current one, and finds B\*, a break and
the cache state. Then it prices with them. The host does not call it and cannot pass in a B\* or a
phase of its own, so every host derives them by the same rule. The host keeps the fingerprints,
finds the previous one by `thread.Key`, and keeps it in the thread's `ThreadState` (§6.1, wiring document, section 5).

```go
// unexported
func compare(prev *Fingerprint, cur Fingerprint, ops []Operation, usage Usage, facts EntryFacts) (boundary *Position, brk *Operation, phase CachePhase)
```

`prev` is the fingerprint of **this thread's** previous request, or nil. The library does no
matching of its own. `ops` are the request's context operations, reported and active: the break takes the `ChangeID`
of the one that changed the first differing position, or of the `setting` context operation. The results
come back in `Result` (§6.3): `Boundary`, `Phase`, and the break and any rescue as `PricedOp`s.

### 5.1 The counterfactual boundary B\*

The provider reports this request's cache boundary as `cache_read`. But a plugin's cache break
moves it: in the coref example, `cache_read` stops at `messages[5]` because of the coref change.
Without it, the cache would have read through `messages[11]`. So the library needs **B\***, the
boundary that would have applied if no plugin had changed anything:

```
x  = min( prevCachedExtent,   // what this thread's previous request left cached, in client space
          clientFirstDiff )   // the last position before the client changed its own body
B* = the last client breakpoint at or before x
B* = 0 when the cache expired, or when no client breakpoint is at or before x
```

All three terms are in client space (§4.3). `prevCachedExtent` is the previous request's cached
extent, mapped from its forwarded body back into its client body through its splices. That is the
extent the client body would have left cached with no context operator: a change that shortened the
previous body does not shorten it.

**Why B\* snaps to a breakpoint.** The provider reads the cache only up to a breakpoint (§6.2). If
the client edits `messages[5]` and its last breakpoint before that is the end of `system`, the
counterfactual read ends at the end of `system`, not at `messages[4]`. The breakpoints are the
client's own (`BeginRequest`, §6.1), because the counterfactual is the client body as sent. Without
this rule, every saving between the breakpoint and `x` would be priced as a read when it would have
been a write.

**Provider details that lower B\* further.** The formula uses the breakpoints only. Two facts in
[`cached-prefix.md`](cached-prefix.md) can make the real read shorter: the Anthropic lookback of 20
blocks, and the minimum cacheable length. The library applies the minimum length (a prefix shorter
than the model's minimum caches nothing, so B\* = 0). It does not model the lookback window. A
breakpoint more than 20 blocks after the previous entry can miss with an unchanged prefix, and the
library then prices too high a read. Shadow differences of this kind go to the case list (§10.3 C).

On OpenAI models before GPT-5.6 there are no explicit breakpoints: the provider caches the longest
prefix it can, in steps of 128 tokens. For those models, the host passes one implicit breakpoint at
the end of the body, and the library rounds B\* and the read extent down to a multiple of 128
tokens.

The host keeps one fingerprint for each thread: the most recent request of that thread (wiring document, W5.2).

```go
type Fingerprint struct {
    Thread    string
    At        time.Time // the start of the request; the request gap is measured from it (§5.2)
    Client    Body      // client body, before any plugin
    Forwarded Body      // body as sent upstream
    ClientBreakpoints, ForwardedBreakpoints []Breakpoint
    Cached    Position  // extent cached after its response: cache_read + cache_write, in forwarded space
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
// NewBody hashes one body in hash order, so every host builds the same Body for the same bytes.
// The host calls it for the client body (before BeginRequest) and for the forwarded body (before
// EndRequest).
func NewBody(tools, system, messages [][]byte) Body
```

The fingerprint itself is built inside `Compute`. `Cached` comes from the usage, which arrives only
after the response, so no earlier call can build it. `Compute` keeps the result in
`ThreadState.PrevPrint` for the next request.

`PosHash` is 16 bytes in memory (8 for the hash, 4 for the size, 4 of padding). It holds hashes and sizes only (R2).

### 5.2 Classifying a break

`fwdFirstDiff` is the first position where this thread's previous forwarded body differs from this
request's forwarded body, in forwarded space. `clientFirstDiff` is the same comparison on client
bodies, in client space. A body that only grew at the end has its first diff at the old end of the
body. `prevExtent` is the previous request's cached extent (`Fingerprint.Cached`).

Before a diff is compared with `clientFirstDiff`, it is mapped into the same space (§4.3).

**A diff breaks the cache only if it is inside what was cached.** A normal turn appends messages, so
both first diffs are at the old end of the body, after `prevExtent`. That is no break: the new tail
is written in both bills, and the tier rule (§6.2) already prices it. So rows 1 and 2 need a diff
**at or before `prevExtent`**.

**Two gaps.** The library finds two gaps from the times it holds. Both are measured from the
*start* of a request, because the provider measures the TTL from the start of the request that wrote
or read the entry:

- The **request gap**: from the start of the thread's previous real request (`PrevPrint.At`) to the
  start of this request. It says whether the entry would have expired **without** a ping.
- The **entry gap**: from the later of `PrevPrint.At` and `EntryFacts.LastPing` to the start of this
  request. It says whether the entry is alive **with** the pings that were sent.

Row 6 is checked first: with no previous fingerprint, no other row applies. The other rows are
checked in order, and the first match wins.

| # | Comparison | Result |
|---|---|---|
| 1 | `fwdFirstDiff` ≤ `prevExtent`, and `fwdFirstDiff` < `clientFirstDiff` | A plugin broke the cache. Add a break with `at: fwdFirstDiff` and the `ChangeID` of the context operation, reported or active, that changed the position `fwdFirstDiff`. |
| 2 | `clientFirstDiff` ≤ `prevExtent`, and `fwdFirstDiff` ≥ `clientFirstDiff` | The client changed its body inside the cached prefix. Add a break with no `ChangeID`. |
| 3 | A `setting` context operation changed the model or a param (`effort`, `thinking`) since the previous request, and `Usage.CacheRead` is below the read that the forwarded body and the phase predict | Add a break with the `ChangeID` of the `setting` context operation. The break starts at the earliest `SettingChange.At` of the operation, snapped down to the last breakpoint before it (§6.2), because the bodies are equal there and no hash shows the position. If `At` is nil, the break starts at the read the usage shows and `penalty_unknown` is added (§6.4). If the operation changed several settings, the break names the operation, not one setting: the library cannot say which of them caused it. This is the one cost of a param change that can be measured. |
| 4 | No diff inside `prevExtent`, the phase is `hot`, but `Usage.CacheRead` is below the read the phase predicts: the end of `prevExtent`, snapped down to the last forwarded breakpoint at or before it | The entry was lost early (eviction, the lookback window, another reason the library cannot see). Add a break with no `ChangeID`. A one-block tolerance absorbs the 128-token rounding of older OpenAI models. |
| 5 | No diff inside `prevExtent`, and the phase is `cold` | The cache expired. Add a break with no `ChangeID`, so the expiry is on the record (R31). It moves no tokens: B\* = 0 already prices every position as a write (§6.2), so the range from the read extent to B\* is empty, and the expiry is not counted twice. |
| 6 | No previous fingerprint for this thread | First turn of the thread with no `Inherits`, or a restart: no break is added. B\* is unknown (§6.4). |

The phase is found **before** the rows, from the gaps. It is the **cache state** that `Compute` uses (R36):

| Case | `CachePhase` | `Extent` |
|---|---|---|
| A previous fingerprint exists, and the request gap ≤ TTL | `hot` | `prevExtent` |
| A previous fingerprint exists, request gap > TTL, entry gap ≤ TTL, `PingRead` is set, and the lifetime is `exact` | `hot`, and the library adds a rescue with `ChangeID = EntryFacts.PingWindow` (§4.4) | `PingRead` |
| A previous fingerprint exists, request gap > TTL, entry gap > TTL, and the lifetime is `exact` | `cold`. A ping was sent but too late: its window gets no rescue | nil |
| A previous fingerprint exists, request gap > TTL, and the lifetime is `minimum` or `unknown` | `unknown`, unless `Usage.CacheRead` proves a read (below) | nil |
| No previous fingerprint for this thread | `unknown`, unless `Usage.CacheRead` proves a read (below) | nil |

**A read in the usage is evidence.** The provider bills a read only for an entry it holds. So when
the phase is `unknown` and `Usage.CacheRead` is larger than zero, the entry was alive up to the read
extent. The library then sets the phase to `hot` with `Extent` = the read extent, and uses it as
B\*. This is the safe direction: it lowers a saving (a read is priced below a write), and never
raises one. It does not give a rescue, because a read does not prove that the entry would have
expired without a ping. When `Usage.CacheRead` is zero, the phase stays `unknown`.

**A read from another thread's entry.** The provider keys its cache on content, not on threads. A
subagent that declares the same `tools` and `system` as the main thread can read the main thread's
entry on its first request. The rule above covers it: the read proves the entry, so the subagent's
first request is `hot` up to that extent, not `unknown`. A ping of one thread can keep another
thread's shared prefix alive the same way. That is not credited, and the rescue stays a ceiling, as
the current `keepaliveSavedUSD` says.

**Why `minimum` is not `cold` (R45).** `TTL` can be a proven expiry or only a guaranteed floor
(`apply.CacheLifetimeKind`: Anthropic's is `Exact`; OpenAI GPT-5.6+ states only a 30-minute
`Minimum`, with no documented maximum — "a minimum may prove an entry is still eligible, but
cannot prove it has expired"). On an `Exact` provider, a gap past `TTL` proves the entry gone, so
`cold` is correct and B\* = 0 is the honest answer. On a `Minimum` provider, the same gap proves
nothing — the entry may still be live — so scoring it `cold` invents a cache-write saving that may
not exist, in the unsafe direction. The host already carries part of this distinction
(`keepaliveCreditFloorMs`: pre-5.6 OpenAI is never credited, exactly because `Minimum` cannot stand
in for proof of expiry). The host sets `EntryFacts.Lifetime` from the same source
(`apply.CacheLifetime`), and the table above routes a `Minimum`-with-no-proof gap to `unknown`, not
`cold`. §6.4 then applies (`penalty_unknown`). For the same reason a `minimum` provider gets no
rescue (§4.4): a rescue is a claim that the entry would have expired.

### 5.3 Tokens for each position

These are the tokens of content that **was forwarded**: the library needs them for the penalty of a
cache break (§6.2), which counts forwarded content between the break and B\*. The tokens a change
**removed** are not in the forwarded body; their bytes come from the context operation's own `Size`
(§4.2 rule 7).

Both hosts send bytes for each forwarded position (`Body`) and for each operation (`Size`). The
library converts both with the request's own ratio (billed prompt tokens ÷ body bytes), as
`pricing.EstimateTokensFromBytes` does today. Results that use the ratio are marked `Estimated`.
No host sends a token count, so the same tokens in both hosts holds whenever the two hosts receive
the same bodies and the same operations.

## 6. The library

### 6.1 Call

One Tracker keeps the state of one thread between requests. The host holds them in a `Trackers`,
keyed by `thread.Key(session, thread)`. For each request the host calls four methods, in this order:
`BeginRequest`, `Add` (once for each reported context operation), `EndRequest`, and `Compute`.
`Compute` also stores the new thread state. There is no separate commit.

**The key has no tenant.** `ThreadKey` is the session ID and the thread ID. If two tenants can send the
same session ID, the host makes the session ID unique for each tenant (for example, it prefixes the
tenant). Otherwise one tenant's request replaces the other's `ThreadState`, and the next request shows a
break that nobody caused. The library cannot tell the two tenants apart.

**The thread is known only at the end of the pass.** The engine resolves the thread inside
`apply`, while the plugins run (wiring document, W5.2). So `BeginRequest` opens a record that
belongs to no thread yet. `EndRequest` names the thread, and only then does the record take a copy
of that thread's `ThreadState` and find `clientFirstDiff`. If no context operator resolved the thread
(context-guru is not in the pipeline), the host resolves it with `internal/thread` itself, from the
client body, before `EndRequest`.

```go
// ThreadState is kept between requests and replaced after each one.
type ThreadState struct {
    Active    []Operation  // rewrite and setting context operations still applied (§4.6)
    PrevPrint *Fingerprint // this thread's previous request; nil = none (§5.2)
    Cache     CacheState   // what the previous request left cached; zero value = unknown (question 11)
    Version   uint64       // +1 at each replace; the optimistic lock (R53)
}

// Trackers holds one Tracker for each thread. It is safe for concurrent use.
type Trackers struct{ /* map from thread.Key to *Tracker */ }

// RequestMeta is what the host knows about the request before the pass: the fields of RequestOps
// that are not operations.
type RequestMeta struct {
    RequestID, Session string
    Model              string
    Params             map[string]string
    At                 time.Time
    Billing            Billing
}

// 1. Before the pass. The client body is hashed once, here (the body before any plugin), with the
//    client's own cache breakpoints, which B* needs (§5.1) and which cachesplit and headttl are
//    priced against.
func (ts *Trackers) BeginRequest(meta RequestMeta, client Body, clientBreakpoints []Breakpoint) *Request

// 2. During the pass. Each plugin reports; the framework sets Operator and DryRun.
func (r *Request) Add(op Operation)

// 3. End of the pass. The thread, the forwarded body, its cache breakpoints, and the facts only
//    the host knows. The record takes a copy of the thread's ThreadState here.
func (r *Request) EndRequest(thread Thread, forwarded Body, breakpoints []Breakpoint, facts EntryFacts)

// 4. After the response. The billed usage is the only input that arrives late.
//    Compute is deterministic and does no I/O (R26). It prices the request, then replaces the
//    thread's ThreadState with the new one as its last step. If Compute fails or panics, the
//    ThreadState is unchanged.
func (r *Request) Compute(usage Usage) Result

// Preview does the same calculation and stores nothing. It is for pricing an old request again, and
// for a test or a tool that wants to look without moving the thread forward. A shadow run (migration
// step 5) calls Compute, because the ThreadState must advance on every request.
func (r *Request) Preview(usage Usage) Result

// A standalone RequestOps: a keep-alive ping or a detached call, with no request record. It holds
// only spend operations. It is priced without any ThreadState and changes none, so it is not one of
// the concurrent requests of R53. Its ChangeID joins it to the change or the window it belongs to.
func ComputeStandalone(ops RequestOps) Result
```

**What each call writes.**

| Call | Who calls | What it writes |
|---|---|---|
| `BeginRequest(meta, client, clientBreakpoints)` | host, before the plugins | into the record: the request facts, the client sizes and fingerprint, the client breakpoints |
| `Add(op)` | the pipeline, once for each reported context operation | into the record: the reported context operations, in pipeline order |
| `EndRequest(thread, forwarded, ...)` | host, after the last plugin | into the record: the thread, a copy of its `ThreadState`, `clientFirstDiff`, the forwarded sizes and fingerprint, the breakpoints, the facts (TTL, lifetime kind, the last ping) |
| `Compute(usage)` | host, after the response | the thread's `ThreadState`: the only write to the part that lives between requests, and the last step of the call |
| `Preview(usage)` | host, when it must not change the state | nothing |
| `ComputeStandalone(ops)` | host, for a ping or a detached call | nothing |

**A ping is not a request of the thread.** The keeper's ping touches the thread's cache entry, but it
does not change the thread's content, and it reports only a `spend`. So it opens no record, and it
does not move `PrevPrint`. Its effect on the next real request comes only through
`EntryFacts.LastPing`, `PingRead` and `PingWindow`, which the host keeps beside the Tracker from the
`KeepAliveReport` (wiring document, W4.6).

The record of a request is private to that request. Two requests of one thread in flight at the same
time (a retry, parallel calls of one agent) each have their own record (R53). Each record keeps
the `Version` of the `ThreadState` it copied in `EndRequest`. `Compute` works like this:

1. Read the current `ThreadState` of the Tracker.
2. If its `Version` is the one the record started from, calculate and go to step 4.
3. If it is not, another request ended first. Take the current `ThreadState`, find `clientFirstDiff`
   again from the client fingerprint that the record keeps, find the two gaps again from the new
   `PrevPrint.At` (§5.2), and calculate. Set `Result.Recomputed`. The host facts (TTL, lifetime,
   the last ping) do not depend on the `ThreadState`, so they are still valid.
4. Replace the `ThreadState` only if its `Version` has not changed since step 1. If it has, go to step 1.
   The new `ThreadState` has `Version` + 1.

The compare and the replace are one atomic step. The calculation is outside the lock, so requests of one
thread do not wait for each other, except in a retry. A retry needs no I/O. `Preview` does steps 1 to 3
and does not replace.

The result depends on the order of the `Compute` calls. It is not the order in which the requests
reached the provider. When the two orders differ, the cache result of the later `Compute` can be wrong.
The host can reduce this by calling `Compute` in the order of the responses.

A missing call is not a compile error. If `EndRequest` was not called, or a fact is missing, `Compute` returns
the result with the missing items in `Unpriced` and a reason. It does not guess.

```go
// Usage is the provider's billed prompt usage. CacheWrite1h is the part of CacheWrite billed at
// the one-hour rate, never an addition to it.
type Usage struct {
    Input, CacheRead, CacheWrite, CacheWrite1h int
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

**The rule.** `L` is the last breakpoint in the **client** body. The provider writes the cache only
up to a breakpoint, and bills content after `L` as input. The saved tokens are priced in client space
(§4.3), against the client's breakpoints, because they are tokens of the client body. The added
tokens of a `rewrite` and the break penalty are tokens of the forwarded body, so they are priced in
forwarded space, against the forwarded breakpoints.

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
If the previous request already forwarded the added content and its prefix is unchanged, the provider reads it: it is priced at the read tier, whatever B\* says. Only the request that first adds it pays the write.

**A break, and the breakpoints.** A break moves content from the read tier to the write tier. The content that moves is:

- **from** the end of what the provider still read on this request (`Usage.CacheRead`, mapped onto
  the forwarded positions),
- **to** B\*, as forwarded on this request.

The start is the read extent (`CacheEffect.From`), not the position of the first difference. The provider reads only up to a
breakpoint. If the last breakpoint before the break is the end of `system`, the provider reads only
`tools` and `system`, and every message before the break is written again too. So a break deep in
the messages costs the same as a break at `messages[0]` when there is no breakpoint between them.
The new tail after B\* is written anyway, so it is not counted.

**A rescue.** A `rescue` or `split` effect moves its tokens from the write tier to the read tier, over its range: for a keep-alive and a system split. For a breakpoint TTL there
are two legs (§4.4): on a reading request, from write (the 5m entry would have expired) to read; on
every writing request, the premium alone, as `Caused.CacheWrite1h` minus the 5m write it replaced.

**Changes to the breakpoints.** `cachesplit` and `headttl` change no content. They change the
breakpoints. So the library finds them by comparing the client breakpoints with the forwarded ones:
a breakpoint that a context operator added, moved or gave a longer TTL. Their saving is the
difference between the bill with the client breakpoints and the bill with the forwarded ones, on the
same content. `cachesplit` reports a `rewrite` with equal sizes, whose `ChangeID` names the split; the
library adds a `split` or `ttlPremium` effect with that `ChangeID`.

**The long-context tier.** Some models bill every token of a request at a higher rate when the
prompt is past a size threshold. A change that moves a request below the threshold changes the rate
of every token, not only of the removed ones. The library returns tokens and knows no thresholds
(D3). The host prices the forwarded tokens at the tier of the forwarded request, and the saved tokens
at the tier of the client request. A saving that crosses the threshold is marked `Estimated`, and
the size of the rate step on the forwarded tokens is not in the saving. That is an understatement,
the safe direction.

**The interaction.** On a rescued request, the content changes are priced in the hot state (rule
above). Without the ping they would have been priced cold, which is worth more. The difference is
neither the ping's nor the content changes' alone. The library reports it as
`Result.Interaction`, attributed to nobody, so the request total still equals the bill without
the context operations minus the bill with them.

### 6.3 Output

```go
type Result struct {
    Version     int
    Ops         []PricedOp     // one for each input operation, same order: the reported operations,
                               // then the active ones applied again
    CacheEffects []CacheEffect // the breaks, rescues and breakpoint changes the library found (§4.4)
    Interaction TierTokens     // §6.2: on a rescued request; attributed to nobody
    Unpriced []Unpriced     // with a machine-stable reason
    Checks   []CheckFailure // §6.5
    Boundary *Position      // the B* used; nil when unknown
    Recomputed bool         // another request replaced the ThreadState first (R53, §6.1)
    Phase    CachePhase     // the cache phase it used (§5.2). For the host's own checks, never an input
}

type Unpriced struct {
    OpID   string
    Reason string // "unknown_kind", "negative_size", "penalty_unknown", "ping_result_unknown", ...; the host adds
                  // "operator_not_allowed" from its own allow-list (wiring document, W5.1)
}

type CheckFailure struct {
    Check    string // "sum" | "order" | "unique_change" | "spans" | "attribution" |
                    // "conservation" (§6.5)
    Operator string // "" when the check is not about one operator
    Detail   string // numbers only, no content
}

// OpID is unique inside the Result. A reported operation keeps its own ID. An active operation
// applied again gets "active:" + its ChangeID, so it cannot collide with the IDs of this request (R6).
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

type CacheEffectKind string // break | rescue | split | ttlPremium | ttlRescue

// CacheEffect is a change in how the provider bills content that is already there (§4.4). It is
// output only. ID is unique inside the Result: "ce:" + a number.
type CacheEffect struct {
    ID        string
    Kind      CacheEffectKind
    ChangeID  string      // the cause of a break, the window of a rescue, the head entry of a TTL leg; empty on a break nobody caused
    From, To  *Position   // the prefix range; nil From = the start of the prompt, nil To = the end of the body
    TTL       string      // "5m" | "1h"; set on the TTL kinds
    Saved     TierTokens
    Caused    TierTokens
    Estimated bool
}
```

`Saved` and `Caused` are separate and both non-negative, because the money layer rejects negative
values (`pricing.MicrosFromUSD`). Cortex prices each side with the existing code.

A break penalty has both sides: the tokens are written (`Caused.CacheWrite5m`) instead of read
(`Saved.CacheRead`). The net is the rate difference, with no special rule.

A cache effect is priced as its own item, in `Result.CacheEffects`. A break's `ChangeID` is the one it was
given, so the host adds the penalty to the total of the change that caused it, without a lookup. The `Kind`
tells the host where to settle it: a break goes to its cause, a rescue to the keeper by its window.
`spend` context operations are not priced in tokens. `PricedOp.Call` returns their `Call.Usage`
unchanged, and the host prices it with the same resolver as the savings (wiring document).

**Conservation.** For each request, the priced `Saved − Caused` of all content changes, cache
breaks and state changes, plus `Interaction`, equals the bill of the client body in the state it
would have had without any context operation, minus the bill of the forwarded body. The library checks
this in tokens (§6.5, check 6).
A `break` with no `ChangeID` is in this sum only when the context operators caused it. A break that the client
body would have had too (an entry lost early, a client edit) is on the record and cancels out. A break
that a context operator left behind, after its change ended, is not in the baseline, so it counts.

### 6.4 Unknown boundary

If the boundary is unknown (`Result.Boundary` is nil), the library uses the extent `cache_read` covers as the boundary, charges no
penalty, and adds `penalty_unknown` to `Unpriced`.

This is the safe direction for the **gross** saving: removed tokens are priced at the read tier
whenever the request read them, and never higher. It is **not** the safe direction for the **net**
figure: if a plugin did break the cache on this request, the missing penalty makes the net saving
too high. So a host must not show a net figure from a request with `penalty_unknown` as a measured
one. It shows the gross saving, and marks the net as `Estimated`.

### 6.5 Checks

The library reports failed checks in `Result.Checks`. A failure does not stop pricing.

1. **Sum:** for each section, the context operations that are applied, and not `DryRun` —
   `SizeBefore.Bytes − SizeAfter.Bytes` adds up to the client size minus the forwarded size, in
   bytes (`Body`). The context operations
   are those reported on this request and the active ones that `Compute` applies again. The host sends
   both sizes (taken in `BeginRequest` and `EndRequest`, §6.1). A failure means one of three things: a plugin changed
   the body and did not report it, `Compute` applied a change that has ended, or `Compute` dropped a
   change that is still in the body (§4.5, §4.6).
2. **Order:** the context operations are in pipeline order, so a context operator's output positions are mapped through the `MessageSplice` of the context operations before it. An active context operation keeps its client-space position and its pipeline position, and is mapped through this request's splices (§4.3).
3. **Unique `ChangeID`:** no two `rewrite` or `setting` context operations of one request, reported or active, have the same `ChangeID`. A `spend` or a cache effect may share the `ChangeID` of the change it belongs to.
4. **Spans:** for a context operation with `Spans`, its `SizeBefore` and `SizeAfter` equal the sums over its spans.
5. **Attribution:** a `break` effect's `ChangeID` is the `ChangeID` of a context operation of the same request, or empty. A `rescue`, `ttlPremium` and `ttlRescue` are exempt: their `ChangeID` is a keep-alive window or a head entry, which names no operation of this request.
6. **Conservation:** the request total of the applied context operations and the cache effects equals the bill difference (§6.3).
   `DryRun` context operations are left out, because their content went upstream unchanged.

## 7. Billing plans and recording

How a host records the result, the size limit on recorded changes, and the totals over a session
or thread are in the wiring document (sections 7.1 to 7.3 there).

### 7.1 Billing plans

`counterfactual.Compute` returns the same tokens for each tier for an API user and for a subscription user. The
host converts them to a value by the request's `Billing.Mode`:

| `Billing.Mode` | Value of a saving | Value of a spend | Shown as |
|---|---|---|---|
| `api` | tokens × the provider's rates | `Call.Usage` × the provider's rates | money saved; money spent |
| `subscription` | tokens × the **weights**, in usage-limit units | `Call.Usage` × the same weights | usage-limit headroom; an API-equivalent dollar figure beside it, labelled "API equivalent" |
| `unknown` | as `api`, with every figure marked `Estimated`, recorded with `Basis = unknown` | as `api` | money, marked estimated, never in a money total |

**Weights.** A weight says how much one token of a tier counts against the usage limit, compared
with one input token. The provider does not publish weights for each tier. So the default weights
are the ratios of the API rates (input 1, cache write 5m 1.25, cache write 1h 2, cache read 0.1,
output 5), and every subscription figure is marked `Estimated`. A host can set measured weights in
its config (§11, question 2).

**Keep-alive on a subscription.** A ping on the user's own credential uses the user's usage limit,
not money. The keep-alive window still adds up (§4.4), in usage-limit units.

**Detection.** The host reads `Billing.Mode` from the credential type on the request: an API key
means `api`; an OAuth token from a subscription login means `subscription`. If it cannot tell, it
uses `unknown`. Cortex and the proxy must use one rule, so the rule is a function in `counterfactual`:
`counterfactual.BillingFrom(headers)`. It reads only the credential type and returns no part of the credential
(R2). Both hosts must call it on the **client's** upstream credential header (`x-api-key` or
`Authorization`) before anything rewrites it — not on the proxy's control-plane login
(`proxy/authmode.go`), which is a different credential. A contract test in each host feeds the same
captured headers and expects the same `Billing.Mode`.

**Recording.** `event.Saving` and `SpendRecord` gain `Basis`: `usd`, `subscriptionEquivalent` or
`unknown`. `TotalAvoidedUSD` and the cost ledger count only `Basis = usd`. A subscription saving goes
in a new usage-limit column, so API-equivalent dollars never enter a money total. An `unknown` figure
is shown beside the totals and is not added to them, because it may be a subscription user's (R43).

## 8. Errors

The rule: an error in savings never changes the request and never invents money. If a fact is
missing, report too little and say why.

| Situation | Result |
|---|---|
| `counterfactual.Compute` panics | The host recovers, writes no saving for the request, and increments `ops_price_panics`. The request and its spend record are unchanged. |
| A malformed context operation (unknown kind, negative size) | Goes to `Unpriced` with a reason. The other context operations are priced. |
| A `spend` whose `Call` has no usage (a ping that timed out: the host sent it and read no response) | `Unpriced` with `ping_result_unknown`. The cost is missing, not zero, so the window must not show its rescue as a net gain. The rescue is still credited from the read on the request that follows (§4.4) |
| Sum check fails | Pricing goes on. The failure is counted for each plugin on `/v1/pipeline`. |
| No previous fingerprint for the thread | `Boundary` is nil, §6.4. |
| Response has no usage | Nothing is priced, as today. |
| No rate for a tier | `Saving.Provenance` is empty, as today. Tokens are on the record. |
| Context operator not on the allow-list | Kept on the event, unpriced. |
| `on_error: observe` | `DryRun = true`. Priced and shown as "would have saved", never in totals. |
| `Billing.Mode` is `unknown` | Priced as `api`, every figure marked `Estimated`, recorded with `Basis = unknown`, never in a money total (§7.1). |

## 9. Out of scope

- Signed totals in the ledger.
- Pricing of `setting`: the saving or cost of a model or param change (R50). The cache break it
  causes is priced; the change in output tokens is shown as an observation, not as a saving.
- A measured saving of a param change. It needs a control that keeps the old setting.
- Persisting fingerprints across a restart.
- A change that no context operator in the pipeline made, such as the client's own compaction. It is not
  recorded as a context operation. A client edit that breaks the cache appears as a `break` cache effect
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

If a later design wants to drive any of the surfaces above from `counterfactual`, that is new scope, not an
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

Each case is a JSON file in `counterfactual/cases/`. The package embeds them and exports `All() []Case`,
so a test in Cortex can run the same list. A case holds only what the spec decides: the input and
the expected result. It does not hold the current code's figure, nor a judgement about it.

`Compute` depends on the state of the thread (D14), so a case is a **sequence of requests** on a
fresh `Trackers`. Each step holds the calls of one request, and the `Result` its `Compute` must
return. A step can also be a standalone `RequestOps` (a ping), priced with `ComputeStandalone`.

```json
{
  "id": "rewrite/applied-again/before-bstar/hit/5m",
  "dims": {"kind": "rewrite", "position": "beforeBStar",
           "cache": "hit", "ttl": "5m"},
  "rates": "fixture-v1",
  "steps": [
    {"begin": {"...": "RequestMeta, client Body, client breakpoints"},
     "add":   [{"...": "the operations reported on this request"}],
     "end":   {"...": "thread, forwarded Body, breakpoints, EntryFacts"},
     "usage": {"...": "Usage"},
     "expected": {"...": "the counterfactual.Result, tokens for each tier"}},
    {"...": "the next request of the thread"}
  ]
}
```

- `expected` is what `counterfactual.Compute` must return on that step, by this spec.
- `rates` names a fixed rate table in the package. `fixture-v1` is: input $3.00/M, cache write 5m
  $3.75/M, cache write 1h $6.00/M, cache read $0.30/M. With one table, `expected` and the current
  code's figure can be compared in dollars.

The places where the current code is meant to give a different figure are in one separate file,
`counterfactual/cases/differences.json`. Each entry has a case ID, a kind, and a reason:

- `differs`: the figures differ on purpose. The reason cites the spec section that causes it.
- `notCovered`: the current code prices something the model does not. The case is listed in §11.

Known `differs` entries from this spec, for the keep-alive credit (§4.4): the current code credits a
rescue on OpenAI GPT-5.6+ after the 30-minute minimum, and credits a 1h entry after a 5m gap
(`providerCacheTTLMs`, `dash/event.go`). This spec credits neither.

**Rounding.** The case list and the examples compute in micro-dollars, exactly, and round once, half
up, when a figure is shown. A total adds the exact amounts.

A case that is not in this file must agree with the current code. The current figure is never
stored: the agreement test computes it each time it runs (§10.4).

### 10.3 What makes the list exhaustive

**A. Generated cases.** A generator makes one case for each combination of these dimensions. It
drops the combinations that cannot occur, and states the reason for each drop in its code.

| Dimension | Values |
|---|---|
| Kind | `rewrite` smaller, `rewrite` larger (a restore is one), cache effect (`break`, `rescue`, `split`, `ttlPremium`, `ttlRescue`) |
| Position | before B\*; after B\* and at or before `L`; after `L`; no breakpoint in the body; spans on both sides of B\* |
| Cache state | hot; cold (expired, `Lifetime: exact`); unknown (gap past an unproven `Lifetime: minimum`, R45; first request of a thread; restart); hot, kept by a ping |
| Cache result | full hit; partial hit; miss with write ≥ input; miss with write < input |
| Breakpoints | end of `system` + last message (Claude Code); one after `tools` only; none; one inside the messages before a break |
| TTL of the next breakpoint | 5m, 1h |
| Lifetime | `exact` (Anthropic); `minimum` (OpenAI GPT-5.6+); `unknown` |
| Tokens | request ratio (usage present); fallback ratio (no usage) |

**B. Hand-written cases.** These test how context operations interact, which a cross product cannot do:

1. The worked examples: session `s1` (turn 5 and the turn-7 coref break) and session `s2` (the
   turn-10 keep-alive), with every figure in the examples file.
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
    an ended change that `Compute` no longer applies; a restore of the summary, after which the earlier
    changes must be applied again from the state. This case cannot be frozen until the end rule exists
    (§11, questions 12 and 13).
11. A subagent thread: its context operations, its totals, and a session total that adds both threads.
12. The interaction on a rescued request, and the conservation check on every case.
13. A `coref` batch: three cuts in messages that are not next to each other, as one context operation with
    three spans, one `ChangeID` and one attributed `break` effect; its payback across later turns.
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
19. A normal turn that only appends messages: no break (§5.2, the diff is after `prevExtent`).
20. A client edit at `messages[5]` with breakpoints at the end of `system` and on the last message:
    B\* snaps to the end of `system` (§5.1).
21. A summary on turn N, then `mask` applied again on turn N+1 at a position that the summary's
    splice moved (§4.3).
22. Observe mode: a `DryRun` change shown on every request; then the switch to enforce, with its
    break (§4.6).
23. The same tool output twice in one session: two `ChangeID`s (§4.2 rule 6).
24. A ping on a `minimum` provider: no rescue (§4.4). A ping on a 1h entry after 9 minutes: no rescue.
25. A subagent's first request that reads the main thread's `tools` and `system`: `hot` from the
    usage, not `unknown` (§5.2).

**C. Cases from real traffic.** For each difference that a shadow step (wiring document, migration steps 5 and 8) logs and
no case matches, add a case. Then decide: it is a bug in `counterfactual`, or a new `differs` entry with a
reason.

### 10.4 How the list validates the implementation

| Test | Where | Passes when |
|---|---|---|
| `counterfactual` unit test | context-guru | `counterfactual.Compute` returns `expected` for every case, including the expected `Boundary`, `Phase` and break for every cache case (§5.2). The internal comparison has its own unit tests. |
| Agreement with the current code | context-guru | The test runs `dash.Event.Price` on each case's input. Each case not in `differences.json` equals `expected` within one micro-dollar. Each `differs` entry really differs, so a stale entry fails. |
| Agreement with the Cortex rules | Cortex, until step 10 | The same test with `pruneDraft`, `compactionDraft` and `rewritePenalty`, for the tool-prune and rewrite-penalty cases. It is removed with those rules. |
| Review of the differences | PR review | Every `differs` entry cites a spec section, and a reviewer approves `differences.json`. |
| Cortex contract test | Cortex | Cortex builds the same inputs through its own plugins and its Tracker, and gets `expected`. A type change in the library fails the Cortex build on upgrade. |
| Shadow steps | both | Every logged difference matches a `differs` entry. |
| Removal gate | both | No rule is removed while a `notCovered` entry uses it. |

### 10.5 Other tests

1. **Property tests in `counterfactual`:** `Saved` and `Caused` are never negative; an empty list gives an
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
3. **Accuracy of the bytes ratio.** The request ratio is an average. Removed content (large JSON
   tool output) may have fewer bytes per token than the average request. Option: `Size` takes a
   content class (`text`, `json`, `binary`) and the library keeps a ratio for each class. Decide
   after measuring the error on real sessions. An optional `Tokens` field on `Size`, which
   the library would use instead of estimating, is also possible.

4. **Where the previous request's hashes live.** The counterfactual needs the previous request's
   hashes on the same thread. The current design (D12, wiring document) has the host keep one
   fingerprint for each thread. An alternative is a push model: components push facts with an
   identity, and the receiver keeps no state. Then the hashes travel in each push, or the context operator
   keeps them, and who classifies a cache effect changes with it. Not decided.
5. **Label volume.** Pushing `change` and `request` as labels on every series gives many series in a
   long session. This must be measured before the push model is fixed.
6. **Asynchronous recalculation.** A replay should not cause a recalculation every
   time. Recalculation should be async and buffered. A newer request for the same key replaces the
   queued one and keeps its queue position, so nothing starves. The queue design is not written.
7. **Where the library lives.** The module path `github.com/rossoctl/context-guru/counterfactual` is a
   placeholder. Its home and name are not decided (D7).
8. **Names in Cortex.** `pctx.ReportOps` and the Cortex name for the component that writes to the
   cost ledger have no chosen name (wiring document).
9. **The payer of a call.** A call is priced with the billing of its request (§9, known limitation).
   A summarizer on the deployer's key, in a subscription session, is priced as the user's.
10. **The rates of a setting break.** A break caused by a `setting` change (a new model) needs the
    rates of one model. The proposed rule, used by example 5a and its case: price the moved tokens at
    the rates of the model that serves the request after the change, so both sides of the break use
    one table and the figure is the cache effect alone. The alternative is the model before the change.
    Not decided.
11. **What the Tracker's state holds.** `ThreadState` has three parts (§6.1): the active context
    operations, the previous fingerprint (`PrevPrint`), and the cache state. The active context
    operations are decided. The other two are in `ThreadState` in this design, and not in what the host
    passes in, so a restart loses them. Whether `ThreadState` is kept in memory or persisted is not
    decided (question 13). Whether the cache state belongs in `ThreadState` at all, or is only derived
    from `PrevPrint` and the gap, is not decided.
12. **How a context operation ends.** Decided: `Compute` infers the end from the bodies and the splice.
    A change is reported once, and `Compute` applies it on later requests until it ends. It ends when:
    - **a splice removed its position** (a summary replaced it, or the client compacted), or the client
      edited at or before it (§4.3, §5.2);
    - **the client's own item is back at its position.** The forwarded item at the change's position has
      the client item's size again, although the change made it smaller or larger. This is a restore
      in place. The size is compared, not the hash, because the hashes are cumulative and differ when
      another change sits earlier in the body. A `DryRun` is exempt, because it forwards the client's item by design;
    - **for content the operator added** (a size before of zero), the client edited something before
      its position. Its position is in forwarded space, so the client body having nothing there does not
      end it.

    A break that a change leaves behind when it ends has no `ChangeID` (§5.2 row 1), because the change
    is no longer reported or active. It is part of the bill difference (§6.3).

    It does not find a change that ends with bytes identical to the changed form. The sum and
    conservation checks (§6.5) still catch a wrong guess after the fact. The proposal that the operator
    names its active `ChangeID`s on each request stays available if this rule proves too weak. It would
    also answer question 13.
13. **How the state recovers.** A context operator reports a change once. If the Tracker loses a
    thread's state (a restart, an eviction, a thread created again), the change is not reported
    again, and it stays missing until it ends (§4.6). Options: the Tracker persists its state; the
    context operator reports the active changes again when it sees a new thread, or on request; or
    the host replays the session from the first request of the thread. Each has a cost. Persisting
    adds I/O to a library that does none today (R26). Asking the operator breaks the rule that it
    reports once. A replay needs the whole history, which the archive may not have. The second
    proposal of question 12 (the operator names its active `ChangeID`s on each request) detects the
    loss, and the operator can then report the full operations once more. Not decided.
14. **Two requests of one thread in flight.** A retry and parallel calls of one agent can overlap.
    Each opens its own request record, copies the thread's `ThreadState` in `EndRequest`, and each
    `Compute` replaces it when it ends. Decided: optimistic locking (D14, R53, §6.1). Decided: a
    ping and a detached call open no record and change no `ThreadState` (`ComputeStandalone`,
    §6.1), and a subagent is its own thread. The order of the `Compute` calls can differ from the
    order at the provider. Open: whether the host must call `Compute` in the order of the responses.
15. **Re-pricing with `Preview`.** `Preview` prices a request from the current `ThreadState`. To price
    an old request again it needs the state the thread had then (§4.6), which the Tracker does not
    keep. Open: whether `Preview` takes a stored `ThreadState`, or is only for the current request.
