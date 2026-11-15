# Cache TTL and keep-alive strategy: analysis on real traffic

*Research note, 2026-11-15.*

**What this file is.** A study of which cache TTL and which keep-alive policy cost the least, measured on one developer's traffic through Context Guru. A TTL (time to live) is how long the provider keeps a cached prompt prefix after its last use.

**Labels used in this file:**

- **Verified** means a vendor document or a vendor changelog says it.
- **Assumption** means it is my own reasoning. No source confirms it.

**Scripts.** Small Python scripts replay the request times from a Context Guru dashboard database, opened read-only: `ttl_sim.py`, `ping_sim.py`, `ping_per_token.py`, `per_token_life.py`, `plan_sim.py`. They are not in this repo.

## Summary

1. **On this traffic, 1 h beats 5 min even at 2x the write price.** It saves 36% of cache spend against 5 min with no pings.
2. **Main agent: 1 h + pings at about 55 minutes is the cheapest policy.** Its costly gaps are waits for a person or a teammate, and many last more than an hour.
3. **Subagents: 5 min + the #867 wait plan ties with 1 h.** Their long gaps are their own tool calls of 5–14 minutes.
4. **A 30 min tier would be worth having only below about 1.5x.** At 2x, 1 h wins.
5. **The TTL is in practice a choice per session, made at the start.** A switch from 5 min to 1 h halfway through rewrites the whole prefix at 2x.

**Open item:** run the same scripts on traffic from more users. This traffic is one developer's, and other users' gaps may differ.

---

## 1. Why 5 min and 1 h, and why no 30 min tier

*Agent work often sits idle for more than 5 minutes. It waits on long tool calls, on subagents or on a person.*

**1. On an API key, 1 h is usually the cheaper choice, not the more expensive one.**

Units: 1 unit = 1 input token at base price.

| Fact | Detail |
|---|---|
| The 1 h premium applies to new tokens only | A write at 1 h costs 2x. A write at 5 min costs 1.25x. Each token is written once if there are no misses. So over a session that grows to C tokens, the total premium is about **0.75C**. |
| A miss rewrites everything | After a 5 min miss at context size c, the next request pays 1.25c instead of 0.1c. That is **1.15c** extra. |

- **Result:** **one** avoided miss late in the session (c ≈ C) pays the full 1 h premium for the whole session.
- **Result:** 1 h loses money only in two cases:
  - Sessions with **no gap between 5 and 60 minutes**, such as batch work, CI, or a user who types without a pause.
  - Sessions where most gaps are longer than 1 h, so the 1 h cache expires too.
- For agent work with tool waits and human review, 1 h almost always wins.
- **So the API default of 5 min in Claude Code is the odd choice, not the 1 h price.**

**2. Idle compaction does not show that "the hold is cheaper than a rewrite" is false. It shows that the hold ends at 1 h.**

- Claude Code uses the public API. The longest TTL the API sells is 1 h, so the harness cannot hold longer.
- Idle compaction is the harness's answer at the edge of that limit: hold for as long as the API allows, then shrink the context before the cache expires.
- **Fleet view (assumption):**
  - Many idle sessions never come back.
  - Keep-alive pings for those sessions cost money every hour with no end.
  - Compaction costs about 40K units one time.
  - Across millions of sessions, a cost with a limit beats a cost without one. This is a likely reason Claude Code compacts instead of pinging.
- **Idle compaction is also evidence about gaps:**
  - It fires only after 54 idle minutes.
  - Anthropic would not build it unless a real share of sessions sit idle for more than 54 minutes.
  - So Anthropic's own data likely shows a real tail of gaps longer than 30 minutes.

**3. Why there is no 30 min tier: a TTL tier is a memory tier, not a time (assumption, with one verified point).**

- **Verified for OpenAI:** `24h` retention "works by offloading the key/value tensors to GPU-local storage when memory is full."
- **My reading:**
  - The short tier keeps the KV in GPU memory. GPU memory is scarce and expensive per minute.
  - The long tier moves the KV to a cheaper tier: host memory or local SSD.
  - After the move, holding the KV for 30 minutes or for 60 minutes costs almost the same. The real cost is the move, plus enough capacity in that tier.
- **Result:** a 30 min tier would cost Anthropic almost as much to run as a 1 h tier. It would have no reason to price 30 min lower, so it sells two tiers that match two kinds of memory.
- OpenAI fits the same pattern:
  - `in_memory` and `24h` before GPT-5.6.
  - `30m` and `12h` on GPT-5.6. These are still two tiers, and **OpenAI picked 30 min as its default**. This supports the view that 30 min is about the right length for most gaps.

**4. The hypothesis "Anthropic wants API users to pay more": what supports it and what does not.**

The hypothesis says that 30 min is the right length, that Anthropic gives 1 h to everyone, and that it prices 1 h high for API users.

**For:**
- OpenAI gives long retention at **no extra price**. So the long tier is likely cheap to run, and Anthropic's 2x write price is margin more than cost.
- Claude Code defaults API users to 5 min. Most users never change a default. Each idle gap between 5 and 60 minutes then costs a full rewrite at 1.25x. **The money is in the default, not in the 1 h price.**

**Against:**
- "Too expensive to use" does not hold. Point 1 shows that 1 h is cheaper than 5 min for most agent sessions, even at 2x.
- Anthropic documents how to turn 1 h on (`promptCacheTtl`, `ENABLE_PROMPT_CACHING_1H`).
- 5 min is a safe default for an unknown workload. Batch and CI jobs have no gaps, and 1 h only adds 60% to their writes.

**A more precise version (assumption):**
- Anthropic charges for retention where retention has value. It leaves the API default at the setting that earns more from users who do not tune it.
- On a subscription, the plan's usage limit caps each user's demand, so Anthropic can give 1 h to everyone there.
- On the API, the 2x price also **limits demand** for the cheaper memory tier. If every API call used 1 h at no premium, that tier would need much more capacity.

**5. How to test this with our own data:**
- Context Guru records the time of each request. From these times we can measure the gaps between turns, by session.
- Split the gaps into four ranges: < 5 min, 5–30 min, 30–60 min, > 60 min. Do this for API sessions and for subscription sessions.
- Price each session under three TTLs: 5 min, a 30 min tier that does not exist (priced like 1 h), and 1 h.
- **If the 30–60 min range is thin,** your sweet-spot claim holds: 30 min would catch almost all of the value of 1 h.
- **If it is thick,** 1 h is the right length, and idle compaction at 54 minutes is aimed at a real group of sessions.

## 2. The test, run on real data (2026-11-15)

**Data:**
- One Context Guru proxy: one developer, API key, all requests on the 5 min TTL.
- 7 days (Nov 8–15), 38 sessions, 174 cache lineages, 8,159 gaps between requests.
- A lineage is one session, thread and model, which share one cache prefix.
- The 1,502 keep-alive pings are left out of the simulation.

**Method:**
- For each TTL, replay the real request times.
- A gap within the TTL counts as a read at 0.1x, plus a write of the new tokens.
- A longer gap counts as a rewrite of the whole cached prefix.
- Prices per model are fitted from the billed rows.
- The script is `ttl_sim.py`.

**1. Gaps by length:**

| Gap (min) | % of gaps | % of miss cost under 5 min |
|---|---|---|
| < 5 | 89.8% | — (hits) |
| 5–15 | 7.3% | **54.3%** |
| 15–30 | 1.2% | **22.1%** |
| 30–45 | 0.4% | 6.2% |
| 45–60 | 0.1% | 2.8% |
| 60–120 | 0.4% | 5.7% |
| > 120 | 0.7% | 8.9% |

- Misses are 10% of the gaps but about **half of all cache spend**: $630 of $1,242.
- **30 min covers 76% of the miss cost. 1 h covers 85%.** By this measure, the sweet-spot claim holds: 30 min gets about 90% of what 1 h gets.

**2. Cache spend under each TTL (simulated, one week):**

| TTL and write price | Cache $ | vs today |
|---|---|---|
| 5 min @1.25x (today, no pings) | 1,242 | — |
| 15 min @2x (hypothetical) | 1,124 | −9.5% |
| 30 min @2x (hypothetical) | 894 | −28.1% |
| 30 min @1.5x (hypothetical) | 805 | −35.2% |
| **1 h @2x** | **800** | **−35.6%** |
| 2 h @2x (hypothetical) | 740 | −40.4% |
| No expiry @1.25x (floor) | 612 | −50.7% |
| *What happened: 5 min + context-guru keep-alive* | *968* | *−22.0%* |

**What the numbers say:**
- **"1 h is too expensive on the API" is false for this traffic.** At 2x, 1 h saves 36% against 5 min.
- **At the same 2x price, 1 h beats 30 min (−35.6% against −28.1%).** The 40 gaps between 30 and 60 minutes are few, but each one is a 2x rewrite of a large context.
- **A 30 min tier would need to cost about 1.5x to equal 1 h at 2x.** For this traffic, a 30 min tier is worth having only below about 1.5x. This sets a price ceiling for the idea that 30 min is the right length.
- **For this traffic, the 1 h TTL alone beats context-guru keep-alive on 5 min by about 17%** ($800 against $968). Keep-alive pings on a 5 min TTL must fire every few minutes. One 2x write covers the whole hour.
  - **Check first:** a comment in the dashboard schema says this gateway refuses `ttl: "1h"` on some models (it was refused on `aws/claude-sonnet-5`). This data has no 1 h request, so it does not show whether the gateway accepts 1 h for Opus 5.5 and Sonnet 5.5.
- **Idle compaction on top of 1 h:**
  - 48 gaps were 54 minutes or longer with a prefix of 200K tokens or more.
  - Assume a summary of 4K output tokens at 5x, and a new prefix of 34K tokens written at 2x. Then their cost falls from $127 to $22 per week.
  - The loss of detail in the summaries is not priced.

**Limits of this test:**
- One developer and one week. Only 40 gaps fall between 30 and 60 minutes, so that row is noisy.
- The simulation counts a context that shrinks by more than half as a new prefix. It does not see sharing between lineages.
- Prices are fitted from the gateway's billing, not taken from a list price. Only the relative numbers matter here.

## 3. Keep-alive pings on 5 min against the 1 h TTL (same data, `ping_sim.py`)

**The question:** 1.25x + 7 pings × 0.1x ≈ 2x, and it covers about 35–40 minutes of idle time. Is that as good as 1 h for about the same price?

**The two prices apply to different token counts:**
- The 1 h premium (0.75x) applies to the **new tokens of each turn**, one time.
- A ping costs 0.1x of the **whole prefix**, for each ping, in each idle gap.
- In this data, a ping reads 119K (Sonnet) to 371K (Opus) tokens. A turn adds about 5K to 15K new tokens.
- So **one ping costs about as much as the 1 h premium on 5 or more turns.**

**Pings are blind:**
- They also fire after the last request of a lineage, where nobody comes back.
- This data has 174 lineages, many of them short subagent runs.

**Results (one week, simulated):**

| Policy | Covers idle | Requests $ | Pings $ | Total $ |
|---|---|---|---|---|
| 5 min, no pings | 5 min | 1,242 | 0 | 1,242 |
| 5 min + 2 pings | 14 min | 931 | 113 | 1,044 |
| 5 min + 5 pings (best ping policy) | 26 min | 776 | 199 | 975 |
| 5 min + 7 pings | 35 min | 746 | 237 | 982 |
| 5 min + 13 pings | 60 min | 704 | 327 | 1,031 |
| **1 h, no pings** | 60 min | 800 | 0 | **800** |
| 1 h + 1 ping at 55 min | 115 min | 742 | 14 | 756 |
| 1 h + 2 pings at 55 min | 170 min | 713 | 24 | 737 |

**What the table shows:**
- 5 min + 13 pings and 1 h have the same misses (96). The difference in request cost, $800 − $704 = **$96 per week**, is the whole 1 h premium. It covers the same 60 minutes that 13 pings buy for $327.
- **Even if the ping cost were half** (the real keeper skips prefixes under 80K), 5 min + 7 pings costs $746 + $118 = $864. That is still more than 1 h at $800.
- **Why 7 pings per hour is the wrong break-even** (`ping_per_token.py`):
  - A ping reads the whole prefix, so it is paid by **every token written earlier in the session**, not only by the last request's tokens.
  - A token stays in the prefix for the rest of the session. So it pays for every ping in every later idle gap, over days for a long main session.
  - The break-even is **7.5 pings over a token's whole life**, not 7 pings within one hour.
  - Main agent: 1.6 pings per hour, but **32 pings per written token** over its life. That is 3.2x in pings against the 0.75x premium.
  - Subagents: 3.6 pings per hour and **7.9 pings per token**. That is about break-even, which is why 2 pings and 1 h cost the same for them.
- **The full lifetime cost of one token** (`per_token_life.py`).
  - This includes the 2x rewrite after each gap longer than 1 h.
  - Units: 1 = one input token at base price. Each cost is divided by the number of distinct tokens.
  - "Rewrites" also holds the first write of new tokens that arrive in a request that missed. This is why the first write on 5 min shows less than 1.25.

  | Main agent | First write | Rewrites (times) | Pings (times) | Reads | **Total** |
  |---|---|---|---|---|---|
  | 5 min + 2 pings | 1.17 | 11.65 (9.3) | 3.24 (32) | 15.24 | **31.3** |
  | 1 h, no pings | 1.92 | 5.25 (2.6) | 0 | 15.91 | **23.1** |
  | 1 h + 2 pings at 55 min | 1.94 | 2.26 (1.1) | 0.61 (6) | 16.06 | **20.9** |

  | Subagents | First write | Rewrites (times) | Pings (times) | Reads | **Total** |
  |---|---|---|---|---|---|
  | 5 min + 2 pings | 1.22 | 0.77 (0.6) | 0.79 (8) | 4.49 | **7.3** |
  | 1 h, no pings | 1.95 | 0.62 (0.3) | 0 | 4.53 | **7.1** |

  - **For the main agent, the large cost on 5 min is the rewrites, not the pings.** A token is written again 9.3 times, on every gap longer than 14 minutes. On 1 h it is written again 2.6 times, on gaps longer than 1 h, at 2x each.
- **With a read price of 0.05x** (the price on this gateway for Opus 5.5 and Sonnet 5.5).
  - Run with `per_token_life.py <db> 0.05` and `ping_sim.py <db> main|sub 0.05`.
  - The base price is fitted again with 0.05x, so these dollar figures are not comparable with the 0.1x tables above.

  | Policy | Main: $ / week | Main: per token | Subagents: $ / week | Subagents: per token |
  |---|---|---|---|---|
  | 5 min + 2 pings | 674 | 22.1 | **105** | **4.63** |
  | 5 min + 3 pings | 612 | 20.0 | 106 | 4.65 |
  | 5 min + 4 pings | 575 | 18.8 | 106 | 4.65 |
  | 5 min + 5 pings | 547 | 17.8 | 108 | 4.72 |
  | 5 min + 7 pings | 534 | 17.4 | 109 | 4.72 |
  | 5 min + 9 pings (best ping policy, main) | 522 | — | 112 | — |
  | **1 h, no pings** | **460** | **15.1** | 107 | 4.83 |
  | 1 h + 2 pings at 55 min | **382** | **12.5** | 104 | 4.68 |

  - Cheaper reads make pings cheaper. For the main agent, the gap between the best ping policy and 1 h falls from 25% to **12%**. 1 h + 2 pings at 55 minutes is still the best (−27% against the best ping policy).
  - For subagents, the result turns: **5 min + 2 pings is now a little cheaper than 1 h** (4.63 against 4.83 per token). The weekly dollars are a tie ($104–107).
- **The best policy here is 1 h plus a few pings at 55 minutes.** It costs $737–756, which is 22–24% below what actually ran ($968).

## 4. The same analysis, main agent and subagents apart

**Groups:**
- Main agent = empty `thread_id`: 37 lineages, all Opus 5.5, average prefix 342K tokens.
- Subagents = `thread_id` that starts with `a:`: 137 lineages, all Sonnet 5.5, average prefix about 100K tokens.
- 4 single requests with other thread ids are left out.

**Run it with:** `python3 -I ttl_sim.py <db> main|sub` and `python3 -I ping_sim.py <db> main|sub`.

**Where the miss cost sits under 5 min:**

| Gap (min) | Main agent | Subagents |
|---|---|---|
| 5–15 | 50% | **84%** |
| 15–30 | 25% | 4% |
| 30–60 | 10% | 5% |
| > 60 | 15% | 8% |
| Total miss $ / week | $554 | $76 |

**Total cache $ per week:**

| Policy | Main agent | Subagents |
|---|---|---|
| 5 min, no pings | 1,054 | 189 |
| 5 min + 2 pings (14 min) | 901 | 143 |
| 5 min + 5 pings (26 min) | 825 | 150 |
| 15 min @2x (hypothetical) | 977 | 148 |
| 30 min @1.5x (hypothetical) | 676 | **129** |
| **1 h, no pings** | **662** | 137 |
| 1 h + 2 pings at 55 min | **600** | 137 |

**What the numbers say:**
- **The main agent pays 85% of the cache bill**, and its misses spread out to an hour and beyond. For it, 1 h wins clearly: −37% alone, −43% with 2 pings at 55 minutes. The best ping policy on 5 min is still 25% more expensive than 1 h.
- **Subagents are short and bursty.** 84% of their miss cost is in gaps of 5–15 minutes. These are most likely waits on long tool runs, such as remote test runs of about 7 minutes (assumption).
  - Two pings capture almost all of it: $143, against $137 for 1 h. The difference is only $6 per week.
  - Here 1 h buys little that 2 pings do not.
  - A 30 min tier at 1.5x would be the best choice for subagents ($129).
  - Subagents gain nothing from more than 2 pings. Each extra ping costs more than it saves, partly because pings after a subagent ends are wasted.
- **Claude Code can set the two apart** (`promptCacheTtl` and `subagentPromptCacheTtl`). For this traffic:
  - Main agent: 1 h + pings at 55 minutes.
  - Subagents: 1 h, or 5 min + 2 pings. Both cost the same.
  - Together that is about $737 per week, against $968 that actually ran.
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

There are also no pings before a thread's first tool call.

**How the replay works:**
- Script: `plan_sim.py <db> 0.05`.
- Reads cost 0.05x. Each gap uses the `waiting_for` value of the request whose response opened it.

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
  - Under the plan, teammate waits still pay $211 per week in misses.
  - Even 11 pings on a teammate wait ($539) do not reach 1 h. 28% of person waits last more than an hour, and no ping budget under the 60-minute hold limit can cover those.
- **Recommendation for this traffic:**
  - Main agent: 1 h + pings at about 55 minutes.
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
- **The result:** switching to 1 h mid-session costs about one 2x rewrite of the current prefix. That is more than a 5 min miss (1.25x). So an upgrade pays only if it is done early, while the prefix is small. **In practice, the TTL is a choice per session, made at the start.**

**Claude Code (verified, Claude Code docs):**
- It sets the TTL per request, in two buckets:
  - the main conversation (`promptCacheTtl` / `CLAUDE_CODE_PROMPT_CACHE_TTL`);
  - everything else, which includes subagents, compaction and titles (`subagentPromptCacheTtl` / `CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL`).
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
- **Gateways:** "part of the one-hour request travels in the `anthropic-beta` header". A gateway that drops that header gets 5 min. This may explain why a gateway refuses 1 h on some models.

**Context Guru:** the proxy sees every request, so it can set the TTL per session or per thread itself (`HeadTTL1h`, `apply/headttl.go`). This is the most flexible place to choose, because the API itself has no session.

---

## Sources

- [Anthropic prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
- [How Claude Code uses prompt caching](https://code.claude.com/docs/en/prompt-caching)
- [Claude Code settings](https://code.claude.com/docs/en/settings)
- [OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching)
- [Idle compaction, 54 minutes](https://claude-me.com/en/practice/claude-code-idle-compaction-prompt-cache-ttl-200k-tokens/)
- [context-guru #489](https://github.com/rossoctl/context-guru/issues/489)
