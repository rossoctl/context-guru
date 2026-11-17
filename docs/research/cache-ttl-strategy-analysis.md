# Cache TTL and keep-alive strategy: analysis on real traffic

*Research note, 2026-11-15.*

**What this file is.** A study of which cache TTL and which keep-alive policy cost the least, measured on one developer's traffic through Context Guru. A TTL (time to live) is how long the provider keeps a cached prompt prefix after its last use.

**Labels used in this file:**

- **Verified** means a vendor document or a vendor changelog says it.
- **Assumption** means it is my own reasoning. No source confirms it.
- **Third party** means a source that is not the vendor reports it. No vendor document confirms it.

**Scripts.** Small Python scripts replay the request times from a Context Guru dashboard database, opened read-only: `ttl_sim.py`, `ping_sim.py`, `ping_per_token.py`, `per_token_life.py`, `plan_sim.py`. They are not in this repo, so a reader cannot run them from here.

## Summary

**Precondition: everything below about 1 h holds only if the gateway really grants 1 h.** On this repo's gateway, a request for 1 h on `aws/claude-sonnet-5` came back with HTTP 200 and all tokens on the 5 min tier (`apply/headttl.go`). Nothing reports this silent downgrade. This study did not check whether Opus 5.5 and Sonnet 5.5 get 1 h. Before you switch, send one request with `ttl: "1h"` and check that `cache_write_1h` in the dashboard is above zero.

1. **On this traffic, 1 h beats 5 min even at 2x the write price.** It cuts cache spend by 47% against 5 min with no pings, and by 17% against what actually ran (5 min + Context Guru keep-alive).
2. **Main agent: 1 h + pings near the end of the hour is the cheapest policy.** Its costly gaps are waits for a person or a teammate, and many of them last more than an hour.
3. **Subagents: 5 min + the #867 wait plan ties with 1 h.** Their long gaps are their own tool calls of 5–14 minutes.
4. **A 30 min tier would be worth having only below about 1.5x.** At 2x, 1 h wins.
5. **The TTL is in practice a choice per session, made at the start (my reading of the docs).** A switch from 5 min to 1 h halfway through likely rewrites the whole prefix at 2x.
6. **This result is not general.** An earlier measurement on production service traffic found that a blanket 1 h loses money there (section 2).

**Prices.** All dollar figures use a cache read price of 0.05x. That is the vendor list price for Opus 5.5 and Sonnet 5.5, the only two models in this data. Most other models read at 0.1x.

**Open item:** run the same scripts on traffic from more users. This traffic is one developer's, and other users' gaps may differ.

---

## 1. Why 5 min and 1 h, and why no 30 min tier

*Agent work often sits idle for more than 5 minutes. It waits on long tool calls, on subagents or on a person.*

**1. For agent sessions with idle gaps, 1 h is often the cheaper choice, not the more expensive one.**

Units: 1 unit = 1 input token at base price. Read price r (0.05x on Opus 5.5 and Sonnet 5.5, 0.1x on most other models).

| Fact | Detail |
|---|---|
| The 1 h premium applies to new tokens only | A write at 1 h costs 2x. A write at 5 min costs 1.25x. Each token is written once if there are no misses. So over a session that grows to C tokens, the total premium is about **0.75C**. |
| A miss rewrites everything | After a 5 min miss at context size c, the next request pays 1.25c instead of r·c. That is about **1.2c** extra. |

- **Result:** **one** avoided miss late in the session (c ≈ C) pays the full 1 h premium for the whole session.
- **Result:** 1 h loses money in two cases:
  - Sessions with **no gap between 5 and 60 minutes**, such as batch work, CI, service traffic, or a user who types without a pause.
  - Sessions where most gaps are longer than 1 h, so the 1 h cache expires too.
- Which case is common depends on the traffic. Section 2 shows one case of each.

**2. Idle compaction does not show that "the hold is cheaper than a rewrite" is false. It shows that the hold ends at 1 h.**

- Claude Code uses the public API. The longest TTL the API sells is 1 h, so the harness cannot hold longer.
- Idle compaction is the harness's answer at the edge of that limit: hold for as long as the API allows, then shrink the context before the cache expires.
- **Fleet view (assumption):**
  - Many idle sessions never come back.
  - Keep-alive pings for those sessions cost money every hour with no end.
  - Compaction has a one-time cost: about 40K units if the user never comes back (a cache read plus the summary output), and about 88K more if the user comes back after expiry (section 2).
  - Across millions of sessions, a cost with a limit beats a cost without one. This is a likely reason Claude Code compacts instead of pinging.
- **Idle compaction is also evidence about gaps:**
  - It fires only after about 54 idle minutes (third party, not in a vendor document).
  - Anthropic would not build it unless a real share of sessions sit idle for that long (assumption).
  - So Anthropic's own data likely shows a real tail of gaps longer than 30 minutes (assumption).

**3. Why there is no 30 min tier: a TTL tier is a memory tier, not a time (assumption).**

- **Verified for OpenAI:** "Prompt caching may store encrypted key/value tensors in GPU-local storage as application state." The page does not tie this to one retention option.
- **My reading:**
  - The short tier keeps the KV in GPU memory. GPU memory is scarce and expensive per minute.
  - The long tier moves the KV to a cheaper tier: host memory or local SSD.
  - After the move, holding the KV for 30 minutes or for 60 minutes costs almost the same. The real cost is the move, plus enough capacity in that tier.
- **Result:** a 30 min tier would cost Anthropic almost as much to run as a 1 h tier. It would have no reason to price 30 min lower, so it sells two tiers that match two kinds of memory.
- OpenAI fits the same pattern (verified):
  - `in_memory` and `24h` on earlier models, with no cache-write charge.
  - `30m` and `12h` on GPT-5.6 and later. Writes cost 1.25x for `30m` (the default) and 2.5x for `12h`. These are still two tiers, and **OpenAI picked 30 min as its default**.

**4. The hypothesis "Anthropic wants API users to pay more": what supports it and what does not.**

The hypothesis says that 30 min is the right length, that Anthropic gives 1 h to everyone, and that it prices 1 h high for API users.

**For:**
- Claude Code defaults API users to 5 min. Most users never change a default. Each idle gap between 5 and 60 minutes then costs a full rewrite at 1.25x. **The money is in the default, not in the 1 h price.**

**Against:**
- "Too expensive to use" does not hold for traffic like this one. Section 2 shows that 1 h is cheaper than 5 min here, even at 2x.
- OpenAI now also charges for long retention: 2.5x for `12h` on GPT-5.6. So a long tier likely has a real cost, and Anthropic's 2x is not clearly margin.
- Anthropic documents how to turn 1 h on (`promptCacheTtl`, `ENABLE_PROMPT_CACHING_1H`).
- 5 min is a safe default for an unknown workload. Batch, CI and service traffic have few long gaps, and 1 h only adds 60% to their writes.

**A more precise version (assumption):**
- Anthropic charges for retention where retention has value. It leaves the API default at the setting that earns more from users who do not tune it.
- On a subscription, Claude Code gives 1 h to the main conversation while the plan is within its usage. Subagents, compaction and titles stay on 5 min, and the main conversation drops to 5 min once usage credits start.
- On the API, the 2x price also **limits demand** for the cheaper memory tier. If every API call used 1 h at no premium, that tier would need much more capacity.

**5. How to test this with our own data:**
- Context Guru records the time of each request. From these times we can measure the gaps between turns, by session.
- Split the gaps into ranges: < 5 min, 5–30 min, 30–60 min, > 60 min.
- Price each session under three TTLs: 5 min, a 30 min tier that does not exist, and 1 h.
- **If the 30–60 min range is thin,** the claim that 30 min is enough holds: 30 min would catch almost all of the value of 1 h.
- **If it is thick,** 1 h is the right length, and idle compaction at 54 minutes is aimed at a real group of sessions.

## 2. The test, run on real data (2026-11-15)

**Data:**
- One Context Guru proxy: one developer, API key, all requests on the 5 min TTL.
- 7 days (Nov 8–15), 38 sessions, 174 cache lineages, 8,159 gaps between requests.
- A lineage is one session, thread and model, which share one cache prefix.
- The 1,502 keep-alive pings are left out of the simulation.

**Method:**
- For each TTL, replay the real request times.
- A gap within the TTL counts as a read at 0.05x, plus a write of the new tokens.
- A longer gap counts as a rewrite of the whole cached prefix.
- The base price per model is fitted from the billed rows.
- The script is `ttl_sim.py <db> all|main|sub 0.05`.

**1. Gaps by length:**

| Gap (min) | % of gaps | % of miss cost under 5 min |
|---|---|---|
| < 5 | 89.8% | — (hits) |
| 5–15 | 7.3% | **54.6%** |
| 15–30 | 1.2% | **22.0%** |
| 30–45 | 0.4% | 6.2% |
| 45–60 | 0.1% | 2.8% |
| 60–120 | 0.4% | 5.7% |
| > 120 | 0.7% | 8.8% |

- Misses are 10% of the gaps but about **two thirds of all cache spend**: $704 of $1,066.
- **30 min covers 77% of the miss cost. 1 h covers 85%.** By this measure, 30 min gets about 90% of what 1 h gets.

**2. Cache spend under each TTL (simulated, one week):**

| TTL and write price | Cache $ | vs 5 min |
|---|---|---|
| 5 min @1.25x (no pings) | 1,066 | — |
| 15 min @2x (hypothetical) | 921 | −13.6% |
| 30 min @2x (hypothetical) | 670 | −37.1% |
| 30 min @1.5x (hypothetical) | 575 | −46.1% |
| **1 h @2x** | **567** | **−46.8%** |
| 2 h @2x (hypothetical) | 502 | −52.9% |
| No expiry @1.25x (floor) | 362 | −66.0% |
| *What happened: 5 min + Context Guru keep-alive* | *681* | *−36.1%* |

**What the numbers say:**
- **"1 h is too expensive on the API" is false for this traffic.** At 2x, 1 h saves 47% against 5 min.
- **At the same 2x price, 1 h beats 30 min (−46.8% against −37.1%).** The 40 gaps between 30 and 60 minutes are few, but each one is a rewrite of a large context.
- **A 30 min tier would need to cost about 1.5x to equal 1 h at 2x.** This sets a price ceiling for the idea that 30 min is the right length.
- **For this traffic, the 1 h TTL alone beats Context Guru keep-alive on 5 min by 17%** ($567 against $681). Keep-alive pings on a 5 min TTL must fire every few minutes. One 2x write covers the whole hour.
- **Check first that 1 h is granted.** On this repo's gateway, `aws/claude-sonnet-5` returned HTTP 200 with `ephemeral_1h_input_tokens: 0` for a 1 h request. All tokens went to the 5 min tier, with no error (`apply/headttl.go`, `dash/event.go`). The cause given there is that Bedrock supports 1 h only on the Claude 4.5 family. This data has no 1 h request, so it does not show whether Opus 5.5 and Sonnet 5.5 get 1 h. The `cache_write_1h` column in the dashboard is the only way to confirm it. If 1 h is downgraded, a "1 h" policy runs as 5 min, and pings near 55 minutes become full rewrites.
- **Idle compaction on top of 1 h:**
  - 48 gaps were 54 minutes or longer with a prefix of 200K tokens or more.
  - Assume a summary of 4K output tokens at 5x, and a new prefix of 34K tokens written at 2x. Then their cost falls from $135 to $20 per week.
  - The loss of detail in the summaries is not priced.

**This result is not general: an earlier production measurement found the opposite.**
- `apply/headttl.go` records a blanket 1 h on 19,805 production requests of a service. It cost $773.00 extra on 14,499 cache-creating requests and recovered $754.66 on 290 requests. **Net: −$18.34.**
- What differs (assumption): that traffic writes often and has few gaps between 5 and 60 minutes. Only 290 requests would have missed. Here, one developer's agent sessions wait on tools, teammates and people, and 10% of gaps are misses.
- **So the right TTL depends on the gap mix of the traffic.** Measure the gaps before you choose.

**Limits of this test:**
- One developer and one week. Only 40 gaps fall between 30 and 60 minutes, so that row is noisy.
- The simulation counts a context that shrinks by more than half as a new prefix. It does not see sharing between lineages.
- The base price is fitted from the gateway's billing, not taken from a list price. Only the relative numbers matter here.

## 3. Keep-alive pings on 5 min against the 1 h TTL (same data, `ping_sim.py`)

**The question:** 1.25x plus a number of pings at 0.05x reaches 2x after 15 pings. Is a series of pings as good as 1 h for about the same price?

**The two prices apply to different token counts:**
- The 1 h premium (0.75x) applies to the **new tokens of each turn**, one time.
- A ping costs 0.05x of the **whole prefix**, for each ping, in each idle gap.
- In this data, a ping reads 119K (Sonnet) to 371K (Opus) tokens. A turn adds about 5K to 15K new tokens.
- So **one ping costs about as much as the 1 h premium on 1 to 5 turns.** It is near 5 for Opus with small turns.

**Pings are blind:**
- They also fire after the last request of a lineage, where nobody comes back.
- This data has 174 lineages, many of them short subagent runs.

**Results (one week, simulated, pings every 4.25 minutes):**

| Policy | Covers idle | Requests $ | Pings $ | Total $ |
|---|---|---|---|---|
| 5 min, no pings | 5 min | 1,066 | 0 | 1,066 |
| 5 min + 2 pings | 14 min | 716 | 63 | 779 |
| 5 min + 5 pings | 26 min | 545 | 111 | 655 |
| 5 min + 9 pings (best ping policy) | 43 min | 484 | 150 | 634 |
| 5 min + 13 pings | 60 min | 464 | 183 | 647 |
| **1 h, no pings** | 60 min | 567 | 0 | **567** |
| 1 h + 1 ping at 55 min | 115 min | 504 | 8 | 512 |
| 1 h + 2 pings at 55 min | 170 min | 473 | 13 | 486 |

**What the table shows:**
- 5 min + 13 pings and 1 h have the same misses (96). The difference in request cost, $567 − $464 = **$103 per week**, is the whole 1 h premium. It covers the same 60 minutes that 13 pings buy for $183.
- **Why "15 pings per hour" is the wrong break-even** (`ping_per_token.py`):
  - A ping reads the whole prefix, so it is paid by **every token written earlier in the session**, not only by the last request's tokens.
  - A token stays in the prefix for the rest of the session. So it pays for every ping in every later idle gap, over days for a long main session.
  - The break-even is **0.75 / 0.05 = 15 pings over a token's whole life**, not 15 pings within one hour.
  - Main agent: 1.6 pings per hour, but **32 pings per written token** over its life. That is twice the break-even.
  - Subagents: 3.6 pings per hour and **7.9 pings per token**. That is half the break-even, so pings are the cheaper choice for them.
- **The full lifetime cost of one token** (`per_token_life.py <db> 0.05`).
  - This includes the 2x rewrite after each gap longer than 1 h.
  - Units: 1 = one input token at base price. Each cost is divided by the number of distinct tokens.
  - "Rewrites" also holds the first write of new tokens that arrive in a request that missed. This is why the first write on 5 min shows less than 1.25.

  | Main agent | First write | Rewrites (times) | Pings (times) | Reads | **Total** |
  |---|---|---|---|---|---|
  | 5 min, no pings | 1.15 | 21.09 (16.9) | 0 | 7.25 | **29.5** |
  | 5 min + 2 pings | 1.17 | 11.65 (9.3) | 1.62 (32) | 7.62 | **22.1** |
  | 5 min + 7 pings | 1.19 | 4.83 (3.9) | 3.44 (69) | 7.90 | **17.4** |
  | 1 h, no pings | 1.92 | 5.25 (2.6) | 0 | 7.96 | **15.1** |
  | 1 h + 2 pings at 55 min | 1.94 | 2.26 (1.1) | 0.31 (6) | 8.03 | **12.5** |

  | Subagents | First write | Rewrites (times) | Pings (times) | Reads | **Total** |
  |---|---|---|---|---|---|
  | 5 min, no pings | 1.18 | 4.35 (3.5) | 0 | 2.10 | **7.6** |
  | 5 min + 2 pings | 1.22 | 0.77 (0.6) | 0.40 (8) | 2.25 | **4.6** |
  | 1 h, no pings | 1.95 | 0.62 (0.3) | 0 | 2.26 | **4.8** |
  | 1 h + 2 pings at 55 min | 1.95 | 0.34 (0.2) | 0.12 (2.5) | 2.27 | **4.7** |

  - **For the main agent, the large cost on 5 min is the rewrites, not the pings.** A token is written again 9.3 times with 2 pings, on every gap longer than 14 minutes. On 1 h it is written again 2.6 times, on gaps longer than 1 h, at 2x each.
  - **For subagents, 5 min + 2 pings is a little cheaper than 1 h** (4.6 against 4.8 per token).
- **The best policy here is 1 h plus a few pings near the end of the hour.** It costs $486–512 per week, which is 25–29% below what actually ran ($681).
- **The 55-minute ping is a simulation choice.** The keeper's shipped 1 h schedule pings at 59 minutes (`Policy.ForOneHour`). So this policy needs a configuration change, not new code.

**At a read price of 0.1x** (most other models), pings cost twice as much, and 1 h wins by more. On the same data, the best ping policy cost 25% more than 1 h for the main agent. At 0.05x it costs 13.5% more ($522 against $460).

## 4. The same analysis, main agent and subagents apart

**Groups:**
- Main agent = empty `thread_id`: 37 lineages, all Opus 5.5, average prefix 342K tokens.
- Subagents = `thread_id` that starts with `a:`: 137 lineages, all Sonnet 5.5, average prefix about 100K tokens.
- 4 single requests with other thread ids are left out.

**Run it with:** `ttl_sim.py <db> main|sub 0.05` and `ping_sim.py <db> main|sub 0.05`.

**Where the miss cost sits under 5 min:**

| Gap (min) | Main agent | Subagents |
|---|---|---|
| 5–15 | 50% | **84%** |
| 15–30 | 25% | 4% |
| 30–60 | 10% | 5% |
| > 60 | 15% | 8% |
| Total miss $ / week | $613 | $91 |

**Total cache $ per week:**

| Policy | Main agent | Subagents |
|---|---|---|
| 5 min, no pings | 896 | 170 |
| 5 min + 2 pings (14 min) | 674 | **105** |
| 5 min + 5 pings (26 min) | 547 | 108 |
| 5 min + 9 pings (43 min) | 522 | 112 |
| 15 min @2x (hypothetical) | 802 | 119 |
| 30 min @1.5x (hypothetical) | 477 | **98** |
| **1 h, no pings** | **460** | 107 |
| 1 h + 2 pings at 55 min | **382** | 104 |

**What the numbers say:**
- **The main agent pays about 85% of the cache bill**, and its misses spread out to an hour and beyond. For it, 1 h wins clearly: −49% alone, −57% with 2 pings at 55 minutes. The best ping policy on 5 min still costs 13.5% more than 1 h.
- **Subagents are short and bursty.** 84% of their miss cost is in gaps of 5–15 minutes. These are most likely waits on long tool runs, such as remote test runs of about 7 minutes (assumption).
  - Two pings capture almost all of it: $105, against $107 for 1 h.
  - A 30 min tier at 1.5x would be the best choice for subagents ($98).
  - Subagents gain nothing from more than 2 flat pings. Each extra ping costs more than it saves, partly because pings after a subagent ends are wasted.
- **Claude Code can set the two apart** (`promptCacheTtl` and `subagentPromptCacheTtl`). For this traffic:
  - Main agent: 1 h + pings near the end of the hour.
  - Subagents: 5 min + pings. 1 h costs about the same.
  - Together that is about $486 per week, against $681 that actually ran.
- **Idle compaction is a main-agent feature.** 43 of the 48 candidates are main-agent gaps. Subagents seldom reach 200K tokens and seldom sit idle for an hour.

## 5. The #867 wait plan (ping count by what the thread waits for)

**What #867 does** (merged 2026-11-02). It works only on the 5 min tier. The base K is 2 on this proxy.

| Wait | Pings | Smallest prefix that gets pings |
|---|---|---|
| Own tool call | up to 8 | 20K |
| Main agent, background task open | K+3 | 20K |
| Main agent, waiting for a person | K+1 | 80K |
| Subagent, or teammate wait | K | 20K |
| Thread done | 0 | — |

There are also no pings before a thread's first tool call. The cap is 11 pings (a 60 minute hold).

**How the replay works:**
- Script: `plan_sim.py <db> 0.05`.
- Reads cost 0.05x. Each gap uses the `waiting_for` value of the request whose response opened it.
- Pings fire every 4.13 minutes, as on this proxy. `ping_sim.py` uses 4.25 minutes. This is why "5 min + flat 2 pings" costs $678 here and $674 in section 4.

**How long each kind of wait lasts:**

| Thread | Wait | Gaps | > 5 min | > 14 min | > 60 min |
|---|---|---|---|---|---|
| Main | own tool | 2,580 | 0.3% | 0% | 0% |
| Main | teammate | 456 | 35% | 21% | 7% |
| Main | background | 436 | 28% | 12% | 1% |
| Main | person | 105 | 54% | 42% | **28%** |
| Sub | own tool | 4,204 | 9.5% | 0.2% | 0% |
| Sub | person (subagent turn ended) | 333 | 20% | 13% | 7% |

**Cost per week ($, read 0.05x):**

| Policy | Main agent | Subagents |
|---|---|---|
| 5 min + flat 2 pings (old) | 678 | 106 |
| **5 min + wait plan, K=2 (#867)** | 615 | **104** |
| 5 min + wait plan, K=3 | 576 | 105 |
| 5 min + plan, main teammate wait = 8 pings (hypothetical) | 549 | — |
| 5 min + plan, main teammate wait = 11 pings (hypothetical) | 539 | — |
| **1 h, no pings** | **460** | 107 |
| **1 h + 2 pings at 55 min** | **382** | 104 |

**What the numbers say:**
- **Subagents: the wait plan is the best 5 min policy, and it ties with 1 h** ($104 against $104–107).
  - Their long gaps are their own tool calls of 5–14 minutes. The 8-ping tool budget covers all of them.
  - **The hybrid works here:** 5 min + the wait plan for subagents.
- **Main agent: 1 h still wins clearly** ($382–460 against $615 for the plan).
  - The main agent almost never waits long on its own tools: 0.3% of those gaps are over 5 minutes.
  - **Its expensive gaps are teammate waits and person waits.** The plan gives these the fewest pings.
  - Under the plan, teammate waits still pay $211 per week in misses. Even 11 pings on a teammate wait ($539) do not reach 1 h.
  - Person waits are worse: 28% of them last more than an hour. No ping budget under the 60-minute hold limit can cover those.
- **Recommendation for this traffic, if the gateway grants 1 h:**
  - Main agent: 1 h + pings near the end of the hour.
  - Subagents: 5 min + the #867 wait plan.
  - Claude Code can set the two apart (`promptCacheTtl`, `subagentPromptCacheTtl`).
  - Main-agent waits for a person that last more than an hour are what idle compaction is for.

## 6. Can the TTL change during a session, or be set per session?

**API (verified, prompt-caching docs):**
- **The API has no session.** The TTL is a field of each `cache_control` breakpoint, in each request. Any request can use any TTL.
- **Rule:** in one request, 1 h breakpoints must come before 5 min breakpoints.
- **Billing positions:**
  - A = the last cache hit;
  - B = the last 1 h breakpoint after A;
  - C = the last breakpoint.
  - The request pays reads for A, 1 h writes for B − A, and 5 min writes for C − B.
- **Upgrade from 5 min to 1 h:**
  - "A hit on an entry cached with a 5-minute TTL doesn't count if a 1-hour `cache_control` block after it is a cache miss."
  - Also: "A request that reads an entry cached with a 5-minute TTL restarts its 5-minute lifetime, even if the request now sets `"ttl": "1h"`."
  - **My reading:** the first switch to 1 h makes A = 0. So **the whole prefix is written again at 2x, one time.** From then on, the session is on 1 h.
- **Downgrade from 1 h to 5 min:** allowed by the ordering rule, because the 1 h prefix comes first. The docs do not describe it. **My reading:** the 1 h entry is read, and only new tokens are written at 5 min. There is no extra cost.
- **The result (my reading):** switching to 1 h mid-session costs about one 2x rewrite of the current prefix. That is more than a 5 min miss (1.25x). So an upgrade pays only if it is done early, while the prefix is small. **In practice, the TTL is a choice per session, made at the start.**

**Claude Code (verified, Claude Code docs):**
- It sets the TTL per request, in two buckets:
  - the main conversation (`promptCacheTtl` / `CLAUDE_CODE_PROMPT_CACHE_TTL`);
  - everything else, which includes subagents, workflows, in-process teammates, forks, compaction and titles (`subagentPromptCacheTtl` / `CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL`).
- **Precedence:**
  1. `FORCE_PROMPT_CACHING_5M`
  2. the bucket's environment variable
  3. the bucket's setting
  4. a subagent's frontmatter `experimental.cacheTtl` (v2.1.248+)
  5. `ENABLE_PROMPT_CACHING_1H`
  6. the default
- **Per session:** yes, at launch:
  - `CLAUDE_CODE_PROMPT_CACHE_TTL=1h claude`, or
  - `claude --settings '{"promptCacheTtl":"1h"}'`. The docs say `--settings` is "for that session only".
- **Per subagent type:** `experimental.cacheTtl` in the subagent's frontmatter.
- **During a session:** settings files reload live. `promptCacheTtl` is not on the list of keys read only at start, but the docs do not say that it reloads either. There is no slash command for it. **Not verified.**
- **Gateways (verified):** "part of the one-hour request travels in the `anthropic-beta` header". A gateway that drops that header gets 5 min.
  - This is not the cause on this repo's gateway. There, Haiku 4.5 got 1 h, so the `ttl` field arrives. The 5 min result on Sonnet 5 comes from the backend's model support (`apply/headttl.go`).

**Context Guru:** the proxy sees every request, so it can set the TTL per session or per thread itself (`HeadTTL1h`, `apply/headttl.go`). This is the most flexible place to choose, because the API itself has no session. `HeadTTL1h` is off today, because the gateway does not grant 1 h on the models that carry most of the spend.

---

## Sources

- [Anthropic prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching) (read prices: footnote 2 gives 0.05x for Opus 5.5 and Sonnet 5.5)
- [How Claude Code uses prompt caching](https://code.claude.com/docs/en/prompt-caching)
- [Claude Code settings](https://code.claude.com/docs/en/settings)
- [OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching) (read 2026-11-15)
- [Idle compaction, 54 minutes](https://claude-me.com/en/practice/claude-code-idle-compaction-prompt-cache-ttl-200k-tokens/) (third party)
- [context-guru #489](https://github.com/rossoctl/context-guru/issues/489)
- In this repo: `apply/headttl.go`, `dash/event.go`, `keepalive/waitplan.go`
