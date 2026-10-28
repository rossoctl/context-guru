# More example scenarios: a plan

**Status:** 27 of the 46 scenarios are written as test cases. Section 6 says which, and why the other 19 are not.
**Part of:** [`design.md`](design.md). The numbers in section references (§5.2, R45, Q12) refer to that document.
**Builds on:** [`examples.md`](examples.md), which has the first eight worked examples.

The goal is to add scenarios that vary the cache state, the position of a change, the operator and the
provider, so that the cases test logic that the first eight do not reach. This document lists them. It
does not price them.

**Where the list comes from.** The operators come from the components registered in
`components/offload/` and `components/reformat/`, the engine code in `apply/` and `proxy/proxy.go`, and
the Cortex plugins `tool-prune`, `inference-router` and the keeper. I read their doc comments, not whole
implementations. The changes that break the cache come from [`cached-prefix.md`](cached-prefix.md).

**About the "cg" column.** It is a **prediction from reading the code** about whether
`dash.Event.Price` would agree with the spec. Nothing in this plan was run. "Differs" means I expect a
difference. "Not covered" means cg has no rule for the situation. "Agrees" means I expect the same
figure. Each prediction is a hypothesis for the case to confirm or refute.

## 1. The operators in cg

| Layer | Operators | What they can do to the cache |
|---|---|---|
| Reformat (lossless) | `format`, `toon`, `textclean`, `searchfold` | Repack tool output. Stable on the prefix. |
| Reformat, engine-applied markers | `toolfilter`, `toolschema`, `cachesplit` | Edit `tools` or `system`. `cachesplit` moves the breakpoint off a volatile tail. |
| Reformat | `cacheinject` | Adds `cache_control` breakpoints. Changes no content. |
| Offload, age or supersession | `mask`, `failed_run`, `collapse`, `readlifecycle`, `skeleton` | Tail only by default. Allowed at depth on a cold turn (`cold_cache`). |
| Offload, content | `dedup`, `cmdfilter`, `linecap`, `extract`, `smartcrush` | Byte-stable on the prefix. |
| Offload, may pay a break on purpose | `coref`; `extract_llm` with `allow_cached_prefix` | Rewrites the cached prefix when a break-even test passes (`S × T > 11.5 × W`). |
| Offload, model call | `summarize`, `cache_aware_summarizer`, `extract_llm`, `extract_llm_sweep`, `agentdiet` | A `spend`, plus a removal. A summary sits at the start of the messages. |
| Engine (`apply/`, `proxy/`) | the expand tool, fixed restore, `headttl` | The expand tool is appended last to `tools` and is sticky. Restore adds content back. `headttl` sets a 1h head. |
| Cortex | `tool-prune`, `inference-router`, the keeper | `tool-prune` removes declarations. The router changes the model. The keeper sends pings. |

Not read, so not used in the scenarios: the Cortex plugins `staticinject` and `litellm_budgettrack`.

## 2. What changes the cache, as the model sees it

1. **Content** is a `rewrite`. Example: a tool output, a tool declaration.
2. **A hidden setting** is a `setting`. Example: model, effort, thinking, `tool_choice`. The bodies do not
   differ, so the setting carries the position where the provider stops matching (`At`, §4.2).
3. **A client edit** has no operator. It reaches §5.2 row 2 or row 4, and the break has no `ChangeID`.

Provider facts that need cases of their own ([`cached-prefix.md`](cached-prefix.md)):

- Anthropic: the lookback of 20 blocks, the minimum cacheable length, the 5m and 1h lifetimes.
- OpenAI: a lifetime that is a minimum and not an expiry (R45), the 128-token rounding before GPT-5.6.

## 3. Scenarios

Already covered by `examples.md`: a single rewrite, a replay that is hot, hot and cold, tool spans, a
summary with its break, a model change, an effort change, four operations on one request, a coref break
and a ping rescue.

### A. Cache state and time (§5.2)

| ID | Scenario | What it tests | cg |
|---|---|---|---|
| A1 | The client edits message 3. An operator shortens message 9. | Row 2: a break with no `ChangeID`. B\* is before the edit. | Differs: no break. |
| A2 | The bodies are equal, the clock says hot, but `CacheRead` is low (the lookback window). | Row 4: an early loss, no `ChangeID`. | Not covered. |
| A3 | A cold request with equal bodies. | Row 5: a break that moves no tokens. | Agrees on tokens. |
| A4 | OpenAI GPT-5.6, a 40-minute gap, `CacheRead` above 0. | Phase `unknown`, then `hot` by evidence. No rescue. | Not covered. |
| A5 | The first request of a thread, `CacheRead` above 0 (a restart). | The same evidence rule. | Not covered. |

### B. Keep-alive

| ID | Scenario | What it tests | cg |
|---|---|---|---|
| B1 | A ping inside the TTL. | No rescue. A window with only a cost. | Agrees. |
| B2 | A ping after expiry (the entry gap is above the TTL). | `cold`, no rescue. | Probably agrees. |
| B3 | GPT-5.6, a 35-minute gap, a ping. | A `minimum` lifetime gives no rescue (R45). | Differs: cg credits after 30 minutes. |
| B4 | A 1h entry, a 9-minute gap, a ping. | No rescue: the entry was alive. | Differs: cg uses the 5m TTL. |
| B5 | Two pings in one window, then one rescue. | The credit is the smaller of what the last ping read and what the request read. Both pings cost. | Agrees. |
| B6 | A main thread and a subagent, each with a window. | State per thread. The session total adds both. | Not covered. |

### C. Position and tier

| ID | Scenario | What it tests | cg |
|---|---|---|---|
| C1 | A rewrite after the last breakpoint. | Input tier. | Differs: cg prices a first removal as a write. |
| C2 | No breakpoint in the body. | Everything is input. | Differs. |
| C3 | One `failed_run` removes two runs, one cached and one new. | Two spans, two tiers, one `ChangeID`. | Differs: one rate for both. |
| C4 | A fixed restore adds content back. | A larger rewrite is a cost, by position. | Not covered. |
| C5 | `mask` at depth on a cold turn, then warm replays. | No break when cold. Later replays are reads. | Probably agrees. |
| C6 | A prompt below the minimum cacheable length. | B\* is 0. Everything is input. | Not covered. |

### D. Breaks caused by an operator

| ID | Scenario | What it tests | cg |
|---|---|---|---|
| D1 | The `tool-prune` list changes on a hot reload. | A break at `tools` with the new `ChangeID`. The old change ends (Q12). | Not covered. |
| D2 | The expand tool is injected the first time. | A whole-cache break once (Anthropic `tools` rule). Then it is sticky. | Not covered. |
| D3 | `toolschema` is turned on mid-session. | A whole-cache break once, then a saving on every request. | Not covered. |
| D4 | A summary on a cold turn. | No break. Contrast with the summary case in `examples.md`. | Probably agrees. |
| D5 | `extract_llm_sweep`: a prefix ask and a fallback ask. | Two `spend`s with different `Call.Usage`. Many spans, one `ChangeID`. | Partly. |
| D6 | `cache_aware_summarizer` against `summarize`. | A `spend` with `cacheRead` against one with fresh input. | Not covered. |

### E. Breakpoints and TTL

| ID | Scenario | What it tests | cg |
|---|---|---|---|
| E1 | `cachesplit`, the volatile tail changed. | A `split` effect: the stable half is read, not written. | Agrees if its three gates hold. |
| E2 | `cachesplit`, the tail unchanged. | No effect. | Agrees. |
| E3 | `headttl`: a 1h head, read after 20 minutes. | `ttlPremium` plus `ttlRescue`. | Not covered. |
| E4 | `headttl`: written, never read. | A cost only. A negative total. | Not covered. |
| E5 | `headttl`: read after 3 minutes. | A premium and no rescue. | Not covered. |
| E6 | `cacheinject` adds a breakpoint. | The tiers change with the breakpoints. The spec has no worked rule for it. | Not covered. |

### F. Settings

| ID | Scenario | What it tests | cg |
|---|---|---|---|
| F1 | `tool_choice` changes. | `At` = `messages[0]`. `tools` and `system` stay. | Not covered. |
| F2 | The web-search toggle. | `At` = `system[0]`. | Not covered. |
| F3 | A thinking-budget change, on two models. | `At` depends on the model. | Not covered. |
| F4 | OpenAI effort changes, but `CacheRead` stays whole. | `At` is given and no break is added (rule 2 of §4.2). | Agrees (nothing). |
| F5 | A setting with a nil `At`. | `penalty_unknown`. | Not covered. |
| F6 | The model flips A to B to A inside 5 minutes. | A's entry is still alive. The spec keeps one fingerprint for each thread, so this may expose a gap. | Not covered. |
| F7 | A model change plus `cmdfilter` in one request. | Which rates price the saved tokens (Q10). | Not covered. |

### G. Billing and measurement

| ID | Scenario | What it tests | cg |
|---|---|---|---|
| G1 | The same sequence in `api`, `subscription` and `unknown`. | The tokens are identical. Only the host converts. | Not applicable. |
| G2 | The `tool-prune` byte estimate, and cg's Sonnet factor of 1.69. | `Estimated`. Q3. | Differs on every case. |
| G3 | A `DryRun` operator. | Priced, not counted, present on every request. | Not covered. |
| G4 | OpenAI before GPT-5.6. | An implicit end breakpoint. 128-token rounding. | Not covered. |

### H. Structure

| ID | Scenario | What it tests | cg |
|---|---|---|---|
| H1 | `mask`, then a summary of the same content, in both orders. | The total is equal. Credit follows pipeline order (Q12). | Not covered. |
| H2 | A `MessageSplice` before a later operation. | Position mapping and the order check. | Not covered. |
| H3 | The main thread behind four subagent requests. | It compares with its own previous request. | Not covered. |
| H4 | Two requests of one thread in flight. | `Recomputed`. | Not applicable. |
| H5 | An unknown kind, a negative size, an unreported body edit. | `Unpriced` and the `sum` check. | Not applicable. |
| H6 | The Tracker restarts mid-session. | The active change is lost. Every later `sum` check fails (Q13). | Not applicable. |

## 4. Suggested order

1. **D2, D3, E1, E3 and F6.** They use cg operators and test spec logic that no case reaches.
2. **B3 and B4.** They are the two `differs` entries that the spec already announces (§10.2).
3. **C1 and C3.** They show whether cg's novelty rule fails on position alone.
4. **A1.** It is the client-edit break.

H1, H6 and D5 depend on open questions 12 and 13. Write them last.

## 5. Open points before writing

- **E6 and G4** have no worked rule in the spec. A case would fix one, so a decision comes first.
- **F6** may show that one fingerprint for each thread is not enough. If it does, that is a finding about the
  design, not about a case.
- **D2 and D3** depend on how the pipeline reports an injected tool. I did not check whether the expand tool
  is reported as an operation at all. If it is not, the first request that carries it breaks the cache with
  no cause, and the case would show a break with no `ChangeID`.

## 6. What was written, and what was skipped

The cases are in `counterfactual/opscases/cases/`, generated by `gen_cases.py`. Each has `source:
more_examples.md#<ID>`. This plan was drafted before any scenario was priced, so a scenario was skipped
when it did not hold up.

**Written (27).** A1, A2, A3, A4, B1, B2, B3, B4, B5, C1, C2, C3, C5, D2, D3, D4, D5, D6, E2, E3, E4, E5,
F2, F4, F5, H2, H5. Where the result is not what the plan predicted or needs a note:

- **A1** expects a break effect that moves no token. The client's last breakpoint before the edit is also
  where the provider stopped reading, so nothing moves. Whether the library should add the effect at all
  when nothing moves is not stated in §5.2. The case lists this in `open`.
- **A4** agrees with cg, so it needs no entry in `differences.json`. **B3** and **B4** differ, as the plan
  said. The play adapter leaves the provider and the TTL tier unset, so these two cases show cg's default
  5m floor, not a measured GPT-5.6 or 1h session.
- **E3, E4, E5** are listed `notCovered`. cg books its `headttl` credit outside the per-component figures
  and the adapter does not feed it, so cg shows 0.
- **C3, H5** and **F2** were not predicted to differ in this way. **C3** and **F2** differ for the reasons
  the plan gave for C1 and F1: no tier by position, no setting break. **H5** differs by 1,500 µ$ and the
  cause was not traced.
- **D5** is the fallback ask only. The prefix ask is the earlier case
  `rewrite+spend/extract-llm-sweep/ask-then-cold-apply/5m`.

**Already covered by an existing case, so skipped (5).**

| ID | Covered by |
|---|---|
| A5 | `boundary/state-lost-reads-cache`: the same evidence rule, a read on the first request of a thread |
| C4 | `rewrite/fixed-restore-cost/hot/5m` and `rewrite/restore-in-place/hot/5m` |
| E1 | `split/delivered/stable-half-read` |
| F1 | `setting/effort-change/break-from-messages`: `tool_choice` has the same shape, `At` = `messages[0]` |
| F3 | The same two shapes as `setting/model-change/break` and the effort case. A second model adds no new rule |

**Skipped because the case would not mean anything (7).**

| ID | Why |
|---|---|
| C6 | The minimum cacheable length is a property of the model. Nothing in `RequestMeta` or `EntryFacts` carries it, so a case cannot state it and the library cannot apply it. |
| F6 | The model flips A, B, A inside 5 minutes. The design keeps one fingerprint per thread, so what the library should answer is a design decision. A case would only record a guess. |
| G1 | Not applicable: the tokens are identical in every billing mode and only the host converts them. |
| G2 | The marking of an estimate is already pinned by `estimate/bytes-ratio-json-removal`. Sonnet's factor of 1.69 is a host rate, not library logic. |
| G3 | The case format has no field for a `DryRun` operation, so it cannot say "priced, shown, not counted". |
| G4 | No worked rule in the spec for the implicit breakpoint and the 128-token rounding (section 5). A case would fix the rule. |
| H4 | Not applicable: two requests in flight is about concurrency (`Recomputed`), which a sequential case cannot show. |

**Blocked on an open question (5).** D1, F7, H1 and H6 wait for design §11 questions 12, 10, 12 and 13. E6
waits for a decision on a rule (section 5).

**Not written yet, though they hold up (2).** B6 and H3 need a thread other than `main` and a parent
thread. `step()` in `gen_cases.py` fixes the thread to `main`.

**What the cases have and have not been checked against.** Each new case passes the shape, conservation and
billed-token checks in `opscases_test.go`, and each is played through `dash.Event.Price`. Those checks do
not run `Compute`: `computeImplemented` is false, so `TestComputeMeetsEveryCase` is skipped. The expected
figures come from reading the design, not from a run of the library.
