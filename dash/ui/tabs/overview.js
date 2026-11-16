// context-guru dashboard: the Overview tab.
// Moved out of app.js (#1089). app.js loads first and holds the shared DOM helpers,
// formatting, state and router that this file uses. See the file list in index.html.
'use strict';

// ── overview ───────────────────────────────────────────────────────────────

/**
 * TILE_INFO is the plain-English explanation of every headline figure, keyed by the tile's
 * own key.
 *
 * It is a TABLE rather than an argument on tile() so that adding an explanation costs one
 * entry and no call-site churn, and — more importantly — so the whole set can be read side
 * by side. Forty explanations written one at a time next to their call sites drift into
 * forty different voices and forty different levels of honesty; read as a block it is
 * obvious when one of them is dodging.
 *
 * Three rules, because the point of this table is that the reader can TRUST it:
 *
 *   1. Say what it is before saying how it is computed, in words a reader who has never
 *      seen this codebase can follow. "Tokens the provider never had to read" first;
 *      `SUM(saved_unique)` second, if at all.
 *   2. Name the catch. Every figure on this page has one — a denominator that is not what
 *      you would guess, a unit that is not billed tokens, a number that is a ceiling
 *      rather than a forecast. The explanation is the only place the catch fits, and an
 *      explanation that omits it is worse than no explanation, because it is trusted.
 *   3. Never describe a number the code does not compute. Every entry here was written
 *      against the expression behind it, and where the two disagreed the CODE was fixed
 *      or the label changed — see the cache-split description, which had drifted a whole
 *      condition away from the gate that computes it.
 */
const TILE_INFO = {
  // ── headline ──────────────────────────────────────────────────────────────
  'total-saved-usd': {
    what: 'The money context-guru avoided in total, after paying for itself. This is the '
      + 'one number to look at if you only look at one.',
    how: 'Four savings added together. First, what your preset avoided: what this traffic '
      + 'would have cost had we removed nothing, minus what it actually cost, minus what '
      + "context-guru's own model calls cost. Second, what the prefix-cache split avoided. "
      + 'Third, your cache strategy\u2019s NET \u2014 what it avoided minus what its pings cost. '
      + 'Fourth, the tool, MCP and skill declarations the removal filter stopped sending, counted '
      + 'from what the filter itself recorded on each request it acted on. The four touch '
      + 'different tokens, so adding them does not count anything twice.',
    catch: 'It can be NEGATIVE, and it is shown in red when it is \u2014 that means context-guru '
      + 'spent more on its own model calls than it saved you. Nothing here is hidden when '
      + 'the answer is unflattering. Your cache strategy contributes its NET, not its saving: the '
      + 'pings it sent are real spend on your key, that spend is inside this figure, and it is '
      + 'NOT in the billed-cost line above \u2014 a ping is not traffic your agent produced, so '
      + 'it is excluded from the request count and every average. And the cache strategy\u2019s '
      + 'saving half is a CEILING: the provider\u2019s cache is keyed on content, so another '
      + 'session sending a byte-identical prompt would have refreshed the same entry for free. '
      + 'What is NOT in here is anything YOU removed by hand \u2014 an MCP server you dropped, a '
      + 'skill you switched off. That reduction is real and it is not ours, so it has its own '
      + 'tile and its own total beside this one.',
  },
  // ── Keep-alive tab ────────────────────────────────────────────────────────
  // Every one of these carries the same two disclosures: the saving is a CEILING, and a zero
  // may be an absence rather than a measurement.
  'ka-position': {
    what: 'Whether your cache strategy is ahead or behind on YOUR traffic over this window.',
    how: 'Your own net: what the pings this service sent on your behalf cost, subtracted from '
      + 'the prefix re-creations they avoided. "No pings yet" means it has not run for you, '
      + 'which is different from having broken even.',
    catch: 'Most sessions this mechanism touches LOSE a little. Across this service 34 of 119 '
      + 'came out ahead and 85 paid about a third of a cent for nothing \u2014 the tax on the '
      + 'many is how the rebate on the few is funded. A green verdict here is an average over '
      + 'that spread, not a promise about your next session.',
  },
  'ka-net': {
    what: 'What your cache strategy has actually earned you: the re-creations it avoided, minus the '
      + 'pings that bought them.',
    how: 'Both halves come from real rows. Every ping is its own dashboard row priced from its '
      + 'own usage; every credit sits on the real request that resumed from a cache that would '
      + 'otherwise have lapsed. Nothing here is an estimate or a model.',
    catch: 'The saving half is a CEILING. The provider\u2019s cache is keyed on content, so if '
      + 'another session had sent a byte-identical prompt during your idle gap, the same entry '
      + 'would have been refreshed for nothing and our ping bought you no more than a warm '
      + 'feeling. That confound cannot be measured from this side \u2014 the counts beside this '
      + 'figure (pings sent, pings that read nothing) are what to check it against.',
  },
  'ka-net-tok': {
    what: 'On a Claude subscription: what your cache strategy earned you, in tokens. The first line is '
      + 'the rescues: the cache writes they avoided and the cache reads they paid. The second line is '
      + 'what the pings paid.',
    how: 'Both lines come from real rows, by token kind (wr cache write, rd cache read, in fresh input). '
      + 'The rescues are the accountant’s figure on the request that resumed; the pings are their '
      + 'own billed usage. The two lines are never added: Anthropic does not say how the limit weights '
      + 'one kind against another.',
    catch: 'The saving line is a CEILING: another session with a byte-identical prompt may have kept the '
      + 'same cache warm for free. Ahead or behind on this tab is decided at API-price weights.',
  },
  'ka-pings': {
    what: 'How many minimal requests this service sent on your behalf while you were away, and '
      + 'what they cost.',
    how: 'One row per ping, in your own request log, billed to your own credential and priced '
      + 'from the usage the provider reported. Filter the Requests tab by cache strategy to read '
      + 'them one at a time.',
    catch: 'This is money spent while nobody was at the keyboard, which is why the mechanism is '
      + 'opt-in and why this figure is beside the saving rather than behind it. Ping cost is '
      + 'very uneven: about $0.0004 at the median and $0.23 at the 99th percentile, so an '
      + 'average of them describes almost no ping.',
  },
  'ka-saved': {
    what: 'The prefix re-creations those pings avoided \u2014 requests that resumed after a long '
      + 'idle gap and were served from cache anyway.',
    how: 'Credited only where all four hold: a ping of ours went out during the gap, the gap was '
      + 'wider than the provider\u2019s five-minute lifetime, the provider then read more than '
      + 'it wrote, and the credit is capped at the tokens that ping actually refreshed. Priced '
      + 'at the WRITE PREMIUM \u2014 what a re-creation costs over a read \u2014 because those '
      + 'tokens carry cache_control and a miss bills them at 1.25x rather than 1x.',
    catch: 'A CEILING, and the only figure on this dashboard that is. The provider\u2019s cache '
      + 'is content-keyed, so another session with the same prefix would have refreshed the '
      + 'entry for free and we would have been paid for nothing. Read it with the ping counts '
      + 'beside it, never on its own.',
  },
  'ka-latency': {
    what: 'How much faster a large request ran when it hit cache than when it had to re-create '
      + 'its prefix, measured on this window\u2019s own traffic.',
    how: 'The mean upstream time of requests that hit cache, against the mean of requests that '
      + 'wrote it, both restricted to requests carrying 100K+ combined tokens \u2014 below that '
      + 'size the two populations skew toward different KINDS of request (a quick one-shot call '
      + 'vs. a slower follow-up turn) and the comparison runs backwards. Absent, not a number, '
      + 'below 20 requests on either side: a difference of two means from a handful of rows is '
      + 'not a measurement.',
    catch: 'A diagnostic about the MECHANISM \u2014 a cache hit is faster than a cache write \u2014 '
      + 'not a per-request or per-dollar claim. Nothing here says how much of THIS SPECIFIC '
      + 'saving is latency; it says what a hit is worth in time, on average, over the window.',
  },
  'ka-addressable': {
    what: 'What cache expiries are still costing you in this window \u2014 the money this '
      + 'mechanism exists to go after.',
    how: 'Requests whose cache missed because the entry had lapsed AND where the provider then '
      + 'wrote a prefix a ping could have kept alive. Summed at what they were actually billed.',
    catch: 'Not all of it is reachable. A ping keeps an entry alive for K\u00d7X + 300 seconds, '
      + 'so an expiry after a four-hour gap is beyond any setting worth having. The ladder below '
      + 'says what share each policy reaches. Expiries that wrote nothing are excluded '
      + 'entirely \u2014 there was no prefix to protect.',
  },
  'ka-rowgrowth': {
    what: 'What this mechanism adds to the dashboard database per day, in bytes.',
    how: 'Your own ping rate over this window times the measured footprint of one ping row: '
      + '294 bytes of row plus about 103 bytes of index. Nothing about a ping stores text.',
    catch: 'Shown because keeping a session warm by hand is the one way you could raise it. Even '
      + 'ungated, service-wide, this is under a megabyte a day.',
  },
  'ka-rec-range': {
    what: 'What this policy would be worth over a window like the last one \u2014 as a RANGE, '
      + 'because that is what the data supports.',
    how: 'A bootstrap over your own SESSIONS: 2,000 resamples of the sessions that had a cache '
      + 'expiry, replaying this policy over each one\u2019s real gaps, reported as the 90% '
      + 'interval. Sessions rather than requests, because expiries inside one session are not '
      + 'independent draws.',
    catch: 'There is deliberately NO single number here, and none on the wire either. On this '
      + 'service no account can pin its own figure closer than about a factor of two, and for '
      + '5 of 12 accounts the interval includes zero \u2014 which is why a recommendation is '
      + 'refused for those rather than rounded off. The direction is resolvable; the size is not.',
  },
  'ka-rec-policy': {
    what: 'The idle interval and ping count suggested for this account.',
    how: 'The live policy (240 s, base of 2 pings, with the per-wait plan on unless your account switched it off), offered only when your own history can tell its effect apart '
      + 'from zero: at least 20 addressable cache expiries, at least 200 requests, and a 90% '
      + 'interval that excludes no-change.',
    catch: 'One ping is never suggested. Not because it loses money \u2014 it does not \u2014 '
      + 'but because it reaches only 9.7 minutes against two pings\u2019 14.3 (K\u00d7280s plus '
      + 'the 5-minute lifetime the last ping itself refreshes), for a cost that scales with the '
      + 'pings sent. The K ladder above shows what each rung converts on YOUR OWN gaps; that is '
      + 'the comparison to make, not a service-wide figure. Two and three pings are a measured '
      + 'tie on our traffic, so two is chosen for the lower request volume and not for a dollar '
      + 'reason. Nothing is applied for you.',
  },
  'estimator-divergence-compacted': {
    what: 'The same over-the-provider ratio as the tile beside it, but measured on the '
      + 'requests where your preset actually removed something.',
    how: 'Median of (provider-billed input ÷ our own token count) over requests your preset trimmed.',
    catch: 'This is a LEVEL, not a correction factor for the savings. A level carries the '
      + 'system prompt, the tool declarations and the JSON envelope, none of which any '
      + 'component removes — so it is 3-4x while the correction the savings actually need is '
      + 'about 1.4-1.7x. Issue #240 was filed partly on that confusion. The two tiles are '
      + 'shown together because the older one is measured on the requests where nothing was '
      + 'removed, which is the exact complement of the rows the savings come from.',
  },
  'pre-correction': {
    what: 'How much of the savings in this window was computed before the tokenizer '
      + 'correction (issue #240) existed.',
    how: 'Requests whose billed_token_factor is 0 — written by the older code — with the '
      + 'baseline-minus-actual dollars they contributed.',
    catch: 'These rows were deliberately NOT re-priced. Re-pricing history would rewrite '
      + 'measurements nobody re-measured, so the old figures stand and this tile says how '
      + 'much of the total they are. While it is non-zero, the headline spans two different '
      + 'definitions of "saved" and the difference between them is understatement, not '
      + 'overstatement.',
  },
  'saved-usd': {
    what: 'What your preset alone avoided, after paying for the model calls context-guru '
      + 'made to trim your prompts.',
    how: 'Baseline cost − actual cost − our own LLM cost. The baseline is what the very '
      + 'same requests would have been billed if context-guru had removed nothing.',
    catch: 'It differs from "Total dollars avoided" only by the prefix-cache saving, which '
      + 'is usually cents. Two large dollar figures side by side look like a contradiction '
      + 'and are not — one just includes a second, smaller mechanism.',
  },
  'saved-unique': {
    what: 'Content that genuinely never reached the model provider — counted once, no '
      + 'matter how many turns it stayed absent for.',
    how: 'Each piece of removed content is fingerprinted. The first time we remove it, it '
      + 'counts here. When the agent re-sends it on the next turn and we remove it again, '
      + 'it does not count again — that repeat is the "re-earned" figure in the sub-line.',
    catch: 'This is measured with a local tokenizer over MESSAGE TEXT only, so it is NOT a '
      + 'count of tokens the provider billed. See "Tokens before" for the size of that gap. '
      + 'The DOLLAR figures on this page do not have this problem: they come from the '
      + "provider's own reported usage.",
  },
  'prefix-change-exposure': {
    what: 'Money spent re-sending whole prompts because the cached prefix changed and the '
      + "provider's cache missed. This is the failure this project exists to avoid.",
    how: 'The actual billed cost of every request whose cache missed with the reason '
      + '"prefix change", added up.',
    catch: 'This is NOT subtracted from the savings above, and it is not a bill '
      + 'context-guru ran up. Prompts change prefix for many reasons of their own. It sits '
      + 'here at full size because on this traffic it is far LARGER than the savings, and '
      + 'showing a small saving in big type while hiding a bigger exposure in a footnote '
      + 'would not be honest. Deciding how much of it is our fault needs a controlled '
      + 'experiment, not a bigger query.',
  },
  requests: {
    what: 'How much traffic every other number on this page is computed over.',
    how: 'Captured requests in the selected time range and filters, and how many distinct '
      + 'agent sessions they came from.',
    catch: 'Only requests that went through this proxy AND were captured are here.',
  },

  // ── content tokens ────────────────────────────────────────────────────────
  'tokens-before': {
    what: 'How big the conversation was when it arrived, before context-guru removed '
      + 'anything.',
    how: 'The text of every message in the request, measured with the tokenizer built into '
      + 'context-guru.',
    catch: 'This is NOT the number of input tokens the provider bills. It counts message '
      + 'TEXT only — it leaves out the system prompt, the tool definitions, and the JSON '
      + 'structure around every message — and it uses our own tokenizer rather than the '
      + "provider's. Measured on requests where we removed nothing, it comes to roughly a "
      + 'third of the input the provider actually billed. Use it to compare token figures '
      + 'on this page with each other; use "Billed tokens" below to compare against a bill.',
  },
  'tokens-after': {
    what: 'How big the conversation was when we sent it on, after removals.',
    how: 'The same measurement as "Tokens before", taken after the pipeline ran.',
    catch: 'Same unit, same caveat: message text measured locally, not billed tokens.',
  },
  'saved-gross': {
    what: 'Every removal we made, counted on every turn we made it.',
    how: 'Tokens before − tokens after, added up across all requests.',
    catch: 'This DOUBLE-COUNTS on purpose, and it is the least useful of the three token '
      + 'figures on its own. The agent re-sends its whole conversation each turn, so the '
      + 'same removal is made again and counted again. On this traffic the same content is '
      + 'removed dozens of times. "Saved (unique)" is the honest count of content; this '
      + 'figure is only meaningful once each repeat is priced at what that turn actually '
      + 'paid, which is what the cost figures do.',
  },
  'saved-adjusted': {
    what: 'Unique savings, minus content we removed and then had to fetch back because the '
      + 'model asked for it.',
    how: 'Tokens saved (unique) − tokens restored.',
    catch: 'Its label names the restore subtraction, but the unique calculation is what '
      + 'dominates it — restores are usually zero. It is not "gross minus restores".',
  },
  overcount: {
    what: 'How many times, on average, each removal is re-earned. A removal made on turn 5 '
      + 'is still absent from turns 6, 7, 8… so the agent never pays for it again on any of '
      + 'them.',
    how: 'Gross savings ÷ unique savings.',
    catch: 'A high number here is not a measurement error — it means one removal kept paying '
      + 'off, and each repeat is already priced at the cheap cache-read rate the turn actually '
      + 'paid. But a ratio of exactly 1.0 does NOT mean every removal was new content: it also '
      + 'happens when a component reports no content fingerprint at all, in which case there '
      + 'is nothing to deduplicate against and the figure is not a measurement. Every '
      + 'reformatter is currently in that position — the Preset steps tab marks them.',
  },

  'billed-input': {
    what: 'The input the PROVIDER counted and charged for, across the same requests.',
    how: "Fresh input + cache reads + cache writes, from the provider's own reported usage.",
    catch: 'Compare this with "Tokens before" beside it: they are DIFFERENT UNITS and this one '
      + 'is about three times larger. Ours counts message text with our own tokenizer; the '
      + 'provider also bills the system prompt, every tool definition and the JSON structure '
      + 'around each message. Neither is wrong, but a percentage computed from one must never '
      + 'be read against the other. Every DOLLAR figure on this page uses this side, so the '
      + 'money is not affected by the gap.',
  },
  'estimator-divergence': {
    what: 'How far our own token counter falls below what the provider actually billed, on a '
      + 'typical request. This is the honesty check for every token figure on this page.',
    how: 'Measured only on requests where context-guru removed nothing — there, our count and '
      + "the provider's describe the same prompt and should agree. The figure is the MEDIAN of "
      + 'the per-request ratios, not a ratio of totals, because a handful of enormous requests '
      + 'would otherwise dominate and the number would stop describing a typical row.',
    catch: 'Most of this gap is SCOPE, not a broken tokenizer: our counter measures message '
      + 'text only, while the provider also bills the system prompt, every tool definition and '
      + 'the JSON structure around each message. Treat every token count on this page as a '
      + 'lower bound on billed tokens, and roughly this far below one. The DOLLAR figures are '
      + "unaffected — they come from the provider's own reported usage, not from this counter.",
  },
  'attempted-tokens': {
    what: 'The part of each conversation your preset was actually allowed to rewrite.',
    how: 'The uncached tail of the request when running cache-aware; the whole request when '
      + 'there is no cache to protect.',
    catch: 'This is the denominator of the headline savings percentage, and it had no tile '
      + 'until now — so the percentage could not be sized by anyone reading it.',
  },
  'frozen-tokens': {
    what: 'Content we deliberately did NOT touch, because it is already in the provider\'s '
      + 'cache and rewriting it would cost far more than the removal saves.',
    how: 'Everything before the cache boundary: tokens before, minus the attempted tokens '
      + 'beside this tile.',
    catch: 'This is the single biggest limit on what this product can do — around half of all '
      + 'context is off-limits for exactly this reason. It is a deliberate choice, not a '
      + 'failure: rewriting a cached prefix makes the provider re-charge creation price for '
      + 'everything after it. It is also where the remaining opportunity is, if a safe way to '
      + 'reach it is ever found.',
  },
  'compaction-resets': {
    what: 'Turns that arrived SMALLER than the previous turn of the same conversation — the '
      + 'agent reset or compacted its own context.',
    how: 'Counted from the requests themselves: this turn\'s arriving size is less than the '
      + 'previous turn\'s in the same session.',
    catch: 'This is the honest bound on the "Replay ceiling" two tiles left. That ceiling '
      + 'assumes every removal survives to the end of its session; each reset is a place '
      + 'where one did not. Derived here rather than counted by the proxy, so it respects the '
      + 'time range and filters like everything else on this page — and so it will NOT match '
      + 'the compaction_resets counter on /stats, which counts a different window by a '
      + 'different method.',
  },
  'sse-buffered': {
    what: 'How often a streaming response was held back and delivered all at once, so the user '
      + 'saw nothing until it had entirely finished.',
    how: 'Counted per response, against all streamed responses.',
    catch: 'Reads "not recorded yet" over history: this is newly captured, and every earlier '
      + 'row defaults to zero — which would look like a measurement of "never buffered" if it '
      + 'were shown as 0%.',
  },
  'ttfb-streamed': {
    what: 'How long a user waits before the first text appears, when streaming works.',
    how: 'A real measurement: from the start of the request to the first byte actually written '
      + 'to the client, averaged over responses that really did stream.',
  },
  'ttfb-buffered': {
    what: 'How long a user waits before ANY text appears when the response was buffered '
      + 'instead of streamed.',
    how: 'The whole response time. On a buffered response nothing is written to the client '
      + 'until the entire response has arrived, so there is no earlier moment to measure — the '
      + 'first thing the user sees and the last are the same instant.',
    catch: 'This is NOT a time-to-first-byte in the way the tile beside it is, and it is '
      + 'deliberately not labelled as one: the two are different measurements and comparing '
      + 'them as a like-for-like pair would be wrong. What IS comparable is the experience — '
      + 'on measured traffic a buffered response leaves the user staring at nothing for about '
      + '21 seconds longer, on roughly a third of responses. That is far more user-visible '
      + 'delay than your preset itself adds, though it is a property of how the response is '
      + 'handled rather than of your preset.',
  },

  // ── amortization ──────────────────────────────────────────────────────────
  'replay-realized': {
    what: 'How much of our removals kept paying off on later turns — the repeat business.',
    how: 'Gross savings − unique savings: every re-removal of content we had already '
      + 'removed once.',
    catch: 'About 57% of the DOLLARS on this page — and 92% of the removed TOKENS. That gap '
      + 'is the whole point: each repeat is worth roughly a tenth of a first removal, because '
      + 'a token kept out of an already-cached prompt saves only the cache-read rate. Quoting '
      + 'the token share as if it were the dollar share overstates this by about 1.6×.',
  },
  'replay-projected': {
    what: 'The most the repeat business COULD have come to, if every removal had stayed '
      + 'gone for the rest of its session.',
    how: 'Each unique removal multiplied by the number of turns that came after it in its '
      + 'own session.',
    catch: 'A CEILING, not a forecast or a target. Nothing is expected to reach it: the '
      + 'cache-safety freeze deliberately stops us touching the already-cached part of a '
      + 'prompt, and long conversations get reset. The gap between this and the realized '
      + 'figure is the honest size of the remaining opportunity.',
  },
  'replay-pct': {
    what: 'How much of that ceiling we actually collected.',
    how: 'Realized repeat savings ÷ the ceiling above.',
    catch: 'A low number here is mostly NOT waste — it is the cost of playing safe. '
      + 'Rewriting a part of the prompt the provider has already cached would make the '
      + 'provider re-charge full price for everything after it, which costs far more than '
      + 'the removal saves. The frozen-token figure in the sub-line is how much we left '
      + 'alone for exactly that reason.',
  },

  // ── cost ──────────────────────────────────────────────────────────────────
  'cost-baseline': {
    what: 'What this traffic would have cost with context-guru doing nothing at all.',
    how: "Start from what was actually billed, then add back what we removed. Content "
      + 'removed for the first time is added back at the rate it would have entered the '
      + 'prompt at; content we removed again on a later turn is added back at the rate '
      + 'THAT turn actually paid — usually the cheap cache-read rate, or the expensive '
      + 'cache-creation rate on a turn whose cache had expired.',
    catch: 'This is a counterfactual, so it cannot be checked against an invoice. What CAN '
      + 'be checked is "Actual cost", and it is priced from the usage numbers the provider '
      + 'itself reported for each request.',
  },
  'cost-actual': {
    what: 'What this traffic actually cost — the bill.',
    how: "The four token tiers the provider reported for every request (fresh input, cache "
      + 'reads, cache writes, output), each at that model’s price when the request ran.',
    catch: 'Requests whose model had no price when they ran are excluded rather than counted '
      + 'as free — they are the "estimated" and "unmeasured" slices of the token-accounting '
      + 'chart at the bottom of this page. That is a DIFFERENT population from the "unpriced" '
      + 'count on the "At today’s rates" tile, which counts rows today’s rate list '
      + 'cannot price. A row can be in either, or both.',
  },
  'cost-cg': {
    what: "What context-guru's own model calls cost you. Trimming with an LLM is not "
      + 'free, and this is the bill for it.',
    how: 'The cost of every extraction and summarisation call context-guru made, priced '
      + 'from the gateway’s own rates.',
    catch: 'This is already subtracted from every net and total on this page. On some '
      + 'component configurations it EXCEEDS what the preset saved — check the '
      + "Preset steps tab, which shows each component's net separately.",
  },
  'decl-filter-saved': {
    what: 'Money not spent because the removal filter stopped sending tool schemas, MCP tool '
      + 'schemas and skill listing entries you opted out of.',
    how: 'The filter records, on every request it acts on, how many declaration tokens it '
      + 'dropped. Those are summed and priced at the tier that request actually paid \u2014 the '
      + 'cache-read rate on a warm turn, the cache-creation rate on the turn that wrote the '
      + 'prefix, the full input rate on a turn with no cache. Measured on requests that were '
      + 'really sent; nothing here is a projection.',
    catch: 'This is a saving of ours that appeared in NO total on this page until recently, and '
      + 'the reason was mechanical rather than a decision: the whole savings walk is built on our '
      + 'own token count of the MESSAGES, and a tool declaration is not in the messages \u2014 it '
      + 'is a top-level field. So the largest lever measured in this project (82.7% of a declared '
      + 'catalogue is never invoked) moved none of the figures beside it. Turning the filter on '
      + 'also costs one cache miss per session in flight, once, because the declarations sit at '
      + 'the front of the cached prompt; that charge is inside the billed cost and is not '
      + 'subtracted from this figure.',
  },
  'cachesplit-saved': {
    what: 'Money saved by splitting the prompt so that the stable part stays cached when '
      + 'the volatile part changes.',
    how: 'Summed over the requests where the split ran, the snapshot had actually MOVED '
      + 'since that session’s previous request, and the provider then read at least the '
      + 'stable half from its cache instead of re-creating it. Valued against what a cache '
      + 'miss would have cost.',
    catch: 'A floor, and a small one on this traffic. The credit can only fire when the '
      + 'environment snapshot moves mid-session, and on real Claude Code traffic it '
      + 'does not — it is captured once per process. The mechanism works; there is simply '
      + 'almost nothing for it to earn on here.',
  },

  // ── addressable spend ─────────────────────────────────────────────────────
  addressable: {
    what: 'The part of the bill any input-side tool could possibly reduce.',
    how: 'The input tiers only — fresh input, cache reads and cache writes — priced at '
      + "today's rates.",
    catch: 'Most of a typical bill is OUTPUT tokens, which nothing context-guru does can '
      + 'touch. This is the only defensible denominator for a savings percentage; dividing '
      + 'by the whole bill would flatter or damn us for the size of the model’s replies.',
  },
  'saved-of-addressable': {
    what: 'Our savings as a share of the part of the bill we could actually reach.',
    how: 'Total dollars avoided ÷ addressable spend.',
    catch: 'Deliberately not "% of your bill", which would be a smaller and less honest '
      + 'number, computed against spend no input-side change can affect.',
  },
  'cost-output': {
    what: "What the model's replies cost. Nothing context-guru does can reduce this.",
    how: 'Output tokens at today’s rate for each model.',
    catch: 'Shown precisely so the percentages on this page can be read in proportion: if '
      + 'this is most of your bill, even a perfect preset is a small percentage of it.',
  },
  'tier-reconcile': {
    what: 'A consistency check between the four-tier breakdown above and the bill.',
    how: 'The tier split is priced at TODAY’s rates; the billed figure was priced when '
      + 'each request ran. The two are shown together.',
    catch: 'They disagree by however much prices moved during the window, and a gap over 5% '
      + 'is flagged in red. A gap is a RATE CHANGE, not an arithmetic error — showing both '
      + 'is the only way to tell those apart.',
  },

  // ── prefix split ──────────────────────────────────────────────────────────
  'split-requests': {
    what: 'Requests where the prompt actually got split into a stable and a volatile half.',
    how: 'Counted from the requests where the split component changed something.',
    catch: 'This component MOVES tokens out of the expensive cache-creation tier rather than '
      + 'removing any, so on the Preset steps tab it correctly reports zero "acted" (which counts '
      + 'removed tokens) alongside a large "structural" count. That is not a broken component '
      + 'and not a component doing nothing — the structural column is its real fire count. '
      + 'Separately, zero HERE with a large run count means these prompts carry no volatile '
      + 'tail to separate, which is a fact about the prompt rather than a fault.',
  },
  'split-tail-moved': {
    what: 'Of those, the turns where the volatile part had actually changed — the only '
      + 'turns the split can possibly earn anything on.',
    how: "This request's tail fingerprint differs from the last one recorded for the "
      + 'session. A session with none recorded yet counts as moved.',
  },
  'split-credited': {
    what: 'Of those, the turns where the provider really did serve the stable half from '
      + 'cache instead of re-creating it — the turns it did earn on.',
    how: 'The request read at least as many tokens from cache as the stable half it moved.',
  },
  'split-credited-moved': {
    what: 'A cross-check that the credited turns and the moved turns agree.',
    how: 'The credited turns that a fresh recount also calls moved.',
    catch: 'A gap means old rows are still in the window: a proxy restart used to make a '
      + 'mid-session turn look like a first sighting, and it was credited for a snapshot '
      + 'that had not moved. That is fixed at the write path, so a gap is history, not a '
      + 'live miscount.',
  },
  'split-hit-rate': {
    what: 'How often, when the snapshot moved, the split actually kept the stable half out '
      + 'of the expensive cache-creation tier.',
    how: 'Credited turns ÷ moved-snapshot turns.',
  },
  'split-historical': {
    what: 'What the split earned before we started recording it per request.',
    how: 'Valued when this page loads, from the session starts in that earlier period. It '
      + 'is never stored.',
    catch: 'Kept as its own tile so it is never confused with the measured figure. It reads '
      + '"—" rather than "$0" when those models had no known rates, because an unpriced '
      + 'number must not look like a zero.',
  },

  // ── billed tokens ─────────────────────────────────────────────────────────
  'cache-read': {
    what: 'Input the provider served out of its own prompt cache, at roughly a tenth of '
      + 'full price.',
    how: "The provider's own reported usage for every request, added up.",
    catch: 'On this traffic this is about 90% of all input — which is the single fact that '
      + 'explains why every saving on this page is small. A token removed from an '
      + 'already-cached region only saves a tenth of the rate, and almost everything we '
      + 'could remove is in one.',
  },
  'cache-write': {
    what: 'Input the provider had to READ and then store in its cache, at a premium over '
      + 'full price.',
    how: "The provider's own reported usage, priced at that model's cache-creation rate.",
    catch: 'This is the expensive tier — roughly 12.5× a cache read. Anything that makes a '
      + 'cached prefix change pushes tokens from the read tier into this one, which is why '
      + 'the freeze exists.',
  },
  'fresh-input': {
    what: 'Input that was neither cached nor cached-for-later — billed at full price.',
    how: "The provider's own reported usage.",
    catch: 'Usually a small share, and the only tier where a removal is worth full price.',
  },
  output: {
    what: "Tokens the model generated. Usually the largest part of the bill.",
    how: "The provider's own reported usage.",
    catch: 'Nothing context-guru does affects this. It is here so the savings percentages '
      + 'can be read against the size of the bill they are a percentage of.',
  },

  // ── latency and safety ────────────────────────────────────────────────────
  'cg-latency': {
    what: 'How much time context-guru itself added to each request.',
    how: 'Wall-clock time inside the proxy, averaged, with the 95th percentile beside it.',
    catch: 'Compare it against upstream latency next door — on this traffic it is a rounding '
      + 'error by comparison. If you are chasing slowness, the buffered-response figure is '
      + 'a much bigger lever.',
  },
  'upstream-latency': {
    what: 'How long the model provider took to answer, once we had sent the request.',
    how: 'Wall-clock time waiting on the upstream, averaged, with the 95th percentile.',
    catch: "Not something context-guru controls, and it dwarfs our own added time.",
  },
  expands: {
    what: 'Times we removed something and the model asked for it back, so we had to fetch '
      + 'it — content paid for twice.',
    how: 'Counted per request, with the share of requests affected and the tokens involved.',
    catch: 'Added to the actual cost side and never subtracted from the baseline, so it '
      + 'makes the savings look WORSE rather than better. Any number above zero is shown in '
      + 'red on purpose.',
  },
  reverts: {
    what: 'Times a component made a request bigger instead of smaller and was rolled back.',
    how: 'Counted whenever the never-worse guard fired.',
    catch: 'A revert is the safety net working, not a failure. Its cost is the wasted time '
      + 'of the attempt, which is in the latency figure.',
  },
  passthroughs: {
    what: 'Requests that went through untouched.',
    how: 'Requests where nothing was removed, for any reason.',
    catch: 'Mostly not a miss. The chart below breaks down why — the commonest reason by '
      + 'far is that the request contained no tool output, and every deployed component '
      + 'only rewrites tool output.',
  },
};


/**
 * tile renders one figure, and — when TILE_INFO has an entry for its key — an "i" that
 * discloses what the figure means.
 *
 * The disclosure is a native <details>, which is the same mechanism the denominators and
 * the waterfall already use: focusable, keyboard-operable and screen-reader-announced for
 * free, no JS, and collapsed so the prose costs nothing until it is wanted. It goes
 * through whyBlock's key registry too, so an explanation the reader has opened stays open
 * across a refresh instead of closing under them.
 */
function tile(key, label, value, sub, cls) {
  const info = TILE_INFO[key];
  // The disclosure is the tile's LAST child, not a child of the label row. A <details>
  // inside the label's flex row gets its prose squeezed into whatever width is left beside
  // the label; as the last child of the tile's own column it simply grows downward. It also
  // means the "i" is in the same place on every tile, which is what makes it findable.
  return el('div', { class: 'tile ' + (cls || ''), 'data-testid': 'tile-' + key },
    el('div', { class: 'k', text: label }),
    el('div', { class: 'v', 'data-testid': 'tile-' + key + '-value', text: value }),
    sub ? el('div', { class: 's', text: sub }) : null,
    info ? tileInfo(key, label, info) : null);
}

/**
 * tileInfo is the "i" affordance. The visible glyph is a real <summary>, so it is a
 * button as far as the keyboard and assistive technology are concerned without any
 * role/tabindex being asserted by hand.
 */
function tileInfo(key, label, info) {
  const k = 'tile:' + key;
  return el('details', {
    class: 'tile-why', 'data-why': k, 'data-testid': 'tile-' + key + '-info',
    open: openWhy.has(k) ? 'open' : null,
    ontoggle: (ev) => { if (ev.currentTarget.open) openWhy.add(k); else openWhy.delete(k); },
  },
  el('summary', { 'aria-label': 'What is "' + label + '"?', title: 'What is "' + label + '"?' }, 'i'),
  el('div', { class: 'tile-why-body' },
    el('p', { text: info.what }),
    info.how ? el('p', {}, el('strong', {}, 'How it is worked out: '), info.how) : null,
    info.catch ? el('p', { class: 'tile-why-catch' },
      el('strong', {}, 'Worth knowing: '), info.catch) : null));
}

/** tileGroup renders one labelled band of tiles. */
function tileGroup(label, note, tiles, cls) {
  const frag = document.createDocumentFragment();
  if (label) {
    frag.appendChild(el('div', { class: 'section' },
      el('h2', { text: label }),
      note ? el('span', { class: 'section-note', text: note }) : null));
  }
  frag.appendChild(el('div', { class: 'tiles' + (cls ? ' ' + cls : '') }, ...tiles));
  return frag;
}

/**
 * costCoverage is the share of the window that actually contributed to a dollar figure.
 *
 * Every dollar sum on this page is unrestricted by `token_accounting`, so a row with no
 * provider usage contributes $0 to it — and `costKnown` used to be `complete > 0`, meaning ONE
 * priced request in a window unlocked five tiles presented as totals. On a 40-row window with 8
 * complete rows, 32 rows contributed nothing to a figure labelled "Total dollars avoided".
 *
 * The fix is not to hide the number: a partial total is still the best available figure and
 * hiding it would be its own dishonesty. The fix is that it never renders without saying what
 * share of the window it covers (DESIGN §3.4 rule 1 — a figure carries its denominator, in the
 * same block, not in a tooltip).
 */
function costCoverage(o) {
  const complete = (o.accounting && o.accounting.complete) || 0;
  const requests = o.requests || 0;
  return { complete, requests, pct: requests > 0 ? (100 * complete) / requests : 0 };
}


function renderDistribution(hostSel, map, labels, detail) {
  const host = clear($(hostSel));
  const entries = Object.entries(map || {}).filter(([, v]) => v > 0);
  if (!entries.length) { emptyState(host, 'No requests in this window', detail || ''); return; }
  entries.sort((a, b) => b[1] - a[1]);
  const total = entries.reduce((n, [, v]) => n + v, 0);
  barRows(host, entries.map(([k, v]) => ({
    label: (labels && labels[k]) || (k === '' ? 'trimmed' : k),
    value: v, max: total,
    display: num(v) + '  (' + pct((v / total) * 100, 0) + ')',
  })));
}

function renderWaterfall(o) {
  CGCharts.savingsBars($('#waterfall'), o);
}

function renderLive() {
  const body = clear($('#live-body'));
  // The feed is the raw capture stream: it is not filtered, and with a filter bar right
  // above it that is the sort of disagreement a reader blames on the numbers.
  const note = $('#live-filter-note');
  const active = activeFilters();
  note.hidden = !active.length;
  if (active.length) {
    note.textContent = 'Not filtered — shows traffic outside ' + describeFilters() + ' too.';
  }
  if (!state.live.length) {
    tableMessage(body, 8, 'Waiting for traffic',
      'Requests appear here the moment they are captured.');
    return;
  }
  for (const e of state.live.slice(0, 25)) {
    body.appendChild(el('tr', { class: 'click', onclick: () => openRequest(e.id) },
      el('td', { text: when(e.ts) }),
      el('td', {}, el('span', { class: 'trunc', title: e.session_id, text: e.session_id || '—' })),
      el('td', { text: e.model || '—' }),
      el('td', { class: 'num', text: compact(e.tokens_before) }),
      el('td', { class: 'num', text: compact(e.tokens_after) }),
      el('td', { class: 'num', text: compact(e.tokens_before - e.tokens_after) }),
      el('td', { class: 'num', text: ms(e.cg_latency_ms) }),
      el('td', {}, el('span', { class: 'pill ' + e.token_accounting, text: e.token_accounting }))));
  }
}

async function loadOverview(opts = {}) {
  // The skeleton is for the FIRST paint only. A repaint swaps the numbers in place, so the
  // document does not shrink and the scroll offset is not clamped under a reader.
  const first = !state.overview;
  state.loadingOverview = true;
  if (first) loadingState($('#tiles'), 3);
  const scroll = window.scrollY;
  try {
    // Snapshotted BEFORE the fetch: an event that lands while the request is in flight is about
    // data this response does not contain, so clearing the flag afterwards would drop it.
    const wasDirty = state.dirty;
    // Two reads of the same endpoint. ?lean=1 answers the figures the three savings rows need at
    // full precision without the slow parts of the full read, so the rows paint first; the full
    // read follows and fills in the tools and skills filter row and the plots. The net and total
    // dollars do not change when it lands: both reads apply the same caps (dash/api.go, pinned by
    // TestLeanStatsMatchFullForTheSameWindow).
    const lean = await api('stats', { lean: 1 });
    state.overview = lean;
    state.overviewFull = false;
    // "Updated N ago" is the age of the DATA: a body served from the server's stale cache is older
    // than the moment it arrived (see the Age header read in api()).
    state.loadedAt = Date.now() - ((state.ageMs && state.ageMs.stats) || 0);
    if (state.dirty === wasDirty) {
      state.dirty = false;
      $('#refresh-now').classList.remove('has-new');
    }
    renderSavedLedger(lean, state.keepAliveLedger, true);
    paintFreshness();
    // Not awaited, each on its own failure path: a slow or failed read must never blank the
    // savings above it.
    loadOverviewKeepAlive();
    loadOverviewTools();
    loadOverviewIdentity();
    loadOverviewLimits();
    const o = await api('stats');
    state.overview = o;
    state.overviewFull = true;
    // Past the 30 s freshness window the server is recomputing behind a stale answer, so look again once,
    // shortly, and the page converges on current figures unasked.
    const dataAge = (state.ageMs && state.ageMs.stats) || 0;
    state.loadedAt = Date.now() - dataAge;
    if (dataAge > 45000 && !state.staleRetry) {
      state.staleRetry = setTimeout(() => { state.staleRetry = 0; if (state.view === 'overview') loadOverview(); }, 10000);
    }
    renderSavedLedger(o, state.keepAliveLedger, false);
    if ($('#overview-diagnostics').open) renderDiagnostics();
    else state.diagStale = true;
    if (!first && window.scrollY !== scroll) window.scrollTo({ top: scroll });
  } catch (err) {
    if (aborted(err)) return;
    // An error on a REPAINT must not destroy a page that is still perfectly readable.
    if (!first) { markDirty(); return; }
    errorState($('#tiles'), 'Could not load statistics', err);
  } finally {
    state.loadingOverview = false;
    state.lastTryAt = Date.now();
  }
}

/** The plots are inside a fold that starts closed, so their data (the time series, one of the larger
 *  reads) is fetched only when the reader opens it, and again on a refresh while it stays open. */
function loadScriptOnce(src) {
  if (!loadScriptOnce.p) loadScriptOnce.p = {};
  return loadScriptOnce.p[src] ||= new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = src; s.onload = resolve; s.onerror = () => { loadScriptOnce.p[src] = null; reject(new Error('Could not load ' + src)); };
    document.head.appendChild(s);
  });
}

async function renderDiagnostics() {
  state.diagStale = false;
  // The plots' code is fetched with the fold, not with the page.
  try { await loadScriptOnce('charts.js'); } catch (e) { for (const id of ['#chart-cost', '#waterfall', '#chart-tokens', '#chart-cache', '#chart-latency', '#chart-volume']) errorState($(id), 'Could not load this plot', e); return; }
  const o = state.overview;
  if (!o || !state.overviewFull) { state.diagStale = true; return; }
  try {
    const s = await api('series', { bucket: bucketFor() });
    renderWaterfall(o);
    renderSeries(s.buckets || []);
    state.overviewBuckets = s.buckets || [];
  } catch (err) {
    if (!aborted(err)) for (const id of ['#chart-cost', '#waterfall', '#chart-tokens', '#chart-cache', '#chart-latency', '#chart-volume']) errorState($(id), 'Could not load this plot', err);
  }
}

/**
 * The three savings, one row each: money and tokens, both set against what was spent.
 *
 * Every figure is one the server already computed (GET /api/stats, and the keep-alive ledger
 * for the waiting time); the only arithmetic here is a ratio of two of them. A lever with
 * nothing recorded says so in words instead of printing a zero that could be an absence.
 */
/** The name a reader knows their preset by: the one the proxy runs now (/api/whoami, local proxy
 *  only), with a note while the window's requests mostly ran under another. See presetLabel in
 *  savings.js. The name stays empty until known, and the card title then reads without it. */
function presetName() {
  return presetLabel(account.preset || '', (state.identity && state.identity.presets) || []);
}
async function loadOverviewIdentity() {
  try {
    // The running preset is read again on each refresh, with the history: a preset switch while the
    // page is open must show, and a stale value would name the old preset as the running one.
    const [r, who] = await Promise.all([api('breakdown', { dim: 'preset' }),
      account.hosted ? null : ctl('/api/whoami').catch(() => null)]);
    if (who) account.preset = who.preset || '';
    state.identity = { presets: r.groups || [] };
    if (state.overview) renderSavedLedger(state.overview, state.keepAliveLedger, !state.overviewFull);
  } catch (e) { /* the rows read fine without the name */ }
}

/** Waiting time in the unit that matches its size, so 411 ms never reads as "nothing". */
function waitText(msv) {
  if (msv < 1000) return (msv / 1000).toFixed(2) + ' s';
  if (msv < 60000) return (msv / 1000).toFixed(1) + ' s';
  return dur(msv);
}
/** A share of a whole, with one decimal below 10% so a small saving does not read as 0%. */
function share(v, of) {
  if (!(of > 0)) return '—';
  const p = (100 * v) / of;
  return pct(p, Math.abs(p) < 10 ? 1 : 0);
}
/**
 * infoButton is a round "i" that opens a short explanation under the card. A native <details>,
 * like the tile "i", and it stays open across a refresh through the same openWhy registry.
 */
function infoButton(key, label, paras) {
  const k = 'ov:' + key;
  return el('details', {
    class: 'tile-why ov-info', 'data-why': k, 'data-testid': 'info-' + key,
    open: openWhy.has(k) ? 'open' : null,
    ontoggle: (ev) => { if (ev.currentTarget.open) openWhy.add(k); else openWhy.delete(k); },
  },
  el('summary', { 'aria-label': 'How is "' + label + '" worked out?', title: 'How is "' + label + '" worked out?' }, 'i'),
  el('div', { class: 'tile-why-body' }, paras.map((t) => el('p', { text: t }))));
}
/** One figure: a big number, then "of <whole>" and the percent, on one line under it. */
function ovFigure(tid, label, value, of, pctText, cls) {
  return el('div', { class: 'ov-fig', 'data-testid': tid },
    el('div', { class: 'ov-fig-k', text: label }),
    el('div', { class: 'ov-fig-v ' + (cls || ''), text: value }),
    of ? el('div', { class: 'ov-fig-of' }, el('span', { text: of }),
      pctText ? el('span', { class: 'ov-pct', text: pctText }) : null) : null);
}

/**
 * The Overview's savings: one total, then one card for each part of context-guru.
 *
 * Every figure is one the server already computed (GET /api/stats, and the keep-alive ledger for
 * the measured latency); the arithmetic here is sums and ratios of them (dash/ui/savings.js, tested
 * by dash/savings.test.mjs). Dollars are set against what the same traffic would have cost without
 * context-guru, so the parts' percents add up to the total's. A part with nothing recorded says so
 * in words instead of printing a zero that could be an absence.
 */
function renderSavedLedger(o, ka, partial) {
  const host = clear($('#tiles'));
  const priced = ((o.accounting && o.accounting.complete) || 0) > 0;
  const t = overviewTotals(o);
  const parts = t.parts;
  const mp = o.manual_pings || {};
  // Tokens. The preset's gross is every token it removed, counted on every request that would have
  // carried it. The filter's is every declaration token left out. Both were never sent, so the
  // whole is the input billed plus both.
  const input = (o.cache_read || 0) + (o.cache_write || 0) + (o.fresh_input || 0);
  const presetTok = Math.max(0, (o.tokens_before || 0) - (o.tokens_after || 0));
  const filterTok = o.decl_filter_reads || 0;
  const inputWithout = input + presetTok + filterTok;
  // The cache does not remove tokens: it keeps them as cheap reads instead of a full rewrite. The
  // ledger holds dollars only, so this is the dollar figure divided by the per-token price gap, with
  // the input price recovered from the provider-cache line (cache_saved = reads x 0.9 x input price)
  // and the write premium taken as 1.15x input (1.25 write less 0.10 read). Shown with "≈".
  const providerSaved = o.cache_saved_usd || 0;
  const inPrice = o.cache_read > 0 && providerSaved > 0 ? providerSaved / (0.9 * o.cache_read) : 0;
  // Both halves are write-minus-read: the automatic credit is stored that way, and the manual net
  // (the avoided write less the ping, which is a read) is the same difference.
  const cacheGross = (o.keepalive_saved_usd || 0) + (mp.saved_usd || 0);
  const cacheTok = inPrice > 0 && cacheGross > 0 ? cacheGross / (1.15 * inPrice) : null;
  // Writes prevented and the waiting time count REAL requests that read a cache that would have
  // expired. A manual ping is not a request anyone waited on, so it is shown beside, as "up to".
  const w = writesPrevented(o);
  const lat = ka && ka.latency;
  const latKnown = !!(lat && lat.known && lat.per_miss_ms > 0);
  const waited = latKnown && w.requests > 0 ? lat.per_miss_ms * w.requests : null;
  // The lean first read carries the money of the cache and the preset, but not the filter or the
  // input-token columns. Until the full read lands, nothing that needs them is drawn.
  const ready = priced && !partial;

  // ── The total ──
  const totalTok = presetTok + filterTok;
  // A Claude subscription does not bill per token: every saving is in tokens (/api/stats `tokens`),
  // with the 5-hour share as an estimate below the total. null keeps the dollar view.
  const sub = state.sub ? subscriptionHeadline(state.sub.limits, o, state.sub.win, Date.now(), compact, pageFiltered(), state.sub.fit) : null;
  const unmeasured = o.tokens_unmeasured_requests || 0;
  const heroMain = sub
    ? el('div', { class: 'ov-hero-money' },
      el('div', { class: 'ov-hero-v ov-hero-sub', 'data-testid': 'saved-total-subscription', text: sub.main }),
      el('div', { class: 'ov-hero-of', text: 'tokens in this period, by kind: wr cache write, wr1h 1-hour cache write, rd cache read, in fresh input' }),
      sub.pings ? el('div', { class: 'ov-hero-spent', 'data-testid': 'saved-total-pings', text: sub.pings }) : null,
      sub.estimate ? el('div', { class: 'ov-hero-spent', 'data-testid': 'saved-total-5h-estimate', text: sub.estimate }) : null,
      unmeasured > 0 ? el('div', { class: 'ov-hero-spent', 'data-testid': 'saved-total-unmeasured',
        text: num(unmeasured) + (unmeasured === 1 ? ' request' : ' requests') + ' not measured in tokens.' }) : null)
    : el('div', { class: 'ov-hero-money' },
      el('div', { class: 'ov-hero-v' + (t.saved < 0 ? ' neg' : ''), 'data-testid': 'saved-total-usd', text: usd(t.saved) }),
      el('div', { class: 'ov-hero-of' }, 'saved, out of ', el('strong', { text: usd(t.without) }),
        ' without context-guru'),
      el('div', { class: 'ov-hero-pct', 'data-testid': 'saved-total-pct', text: share(t.saved, t.without) + ' saved' }),
      el('div', { class: 'ov-hero-spent', text: 'You spent ' + usd(t.spent) + ' instead.' }));
  const subInfo = [
    'On a Claude subscription every saving is in tokens, not dollars. A subscription does not bill per token.',
    'The total is the sum of the parts below, kind by kind. wr is cache write (wr1h: the 1-hour cache), rd is cache read, in is fresh input.',
    'Kinds are never added into one number: Anthropic does not say how the limit weights them. "cost" is what a saving made you pay, for example the cache reads after a keep-alive rescue.',
    'Keep-alive pings are on a line of their own, not in the total: a ping is not your agent\'s traffic.',
    'Requests from before the token count, or that could not be priced, are not measured. That is not "saved nothing". "saved 0 tokens" is a measured zero.',
    'The 5-hour share is an estimate for the current 5-hour window: the share of the limit used, times what context-guru saved, divided by what you spent, at API prices.',
    'With enough history, the estimate uses learned 5-hour weights instead: how much of the limit one token of each kind used, learned from the share the provider reported after each request. It is then the saved tokens, less the tokens a saving cost and the keep-alive pings, each kind times its weight.',
    'Anthropic does not publish how the limit weights token kinds, and it reports whole percents only. That is why it is an estimate. It shows after $0.50 of spend and 2% of the limit used in the window.',
    'The share saved can never be more than 100% less the share used: Anthropic stops you at 100%. When the estimate reaches that cap, the line says that without context-guru you would have hit the 5-hour limit.',
    'The estimate is for your whole account. It is hidden while a filter (a session, a time range, a model...) narrows this page.',
  ];
  const hero = el('section', { class: 'ov-hero', 'data-testid': 'saved-total' },
    el('div', { class: 'ov-hero-head' },
      el('h3', { text: 'Total saved by context-guru' }),
      infoButton('total', 'Total saved', sub ? subInfo : [
        'Total saved is the sum of the parts below: the cache, your preset, the tools and skills filter, and tool deferral.',
        'Each part is already net of what context-guru spent to make it: our own model calls and the cache pings.',
        'The cache part is the most it could have saved: another session can keep the same cache warm. The preset and filter parts are estimates.',
        '"Without context-guru" is what you spent plus what was saved. The percent is the saved share of that.',
        'The provider’s own cache discount is not counted. You get it with or without context-guru.',
      ])),
    !ready
      ? el('p', { class: 'ov-none', text: partial ? 'Loading…' : 'Nothing priced yet in this period.' })
      : el('div', { class: 'ov-hero-body' },
        heroMain,
        el('div', { class: 'ov-hero-tok' },
          ovFigure('saved-total-tokens', 'Input tokens removed', compact(totalTok),
            'of ' + compact(inputWithout) + ' tokens', share(totalTok, inputWithout)))));
  host.appendChild(hero);

  // ── One card per part ──
  const money = (tid, v, ran) => !ran ? el('p', { class: 'ov-none', text: 'Nothing saved yet in this period.' })
    : !priced ? el('p', { class: 'ov-none', text: 'Not priced yet.' })
      // The "of" and the percent wait for the full read: the whole is not known before it.
      : ovFigure(tid, 'Money saved', usd(v), ready ? 'of ' + usd(t.without) : 'Loading…', ready ? share(v, t.without) : '', v < 0 ? 'neg' : 'good');
  const card = (id, tid, title, href, what, kids, info) => el('section', { class: 'ov-card', 'data-lever': id, 'data-testid': tid },
    el('div', { class: 'ov-card-head' },
      el('h3', {}, el('a', { href, title: 'Open the tab for ' + title }, title)), info || null),
    el('p', { class: 'ov-what', text: what }),
    el('div', { class: 'ov-figs' }, kids));
  // On a subscription each card shows tokens (tokens.by_feature, full read only) in place of money.
  const ft = sub ? featureTokens(o) : null;
  const tokens = (tid, flow, ran) => !ran ? el('p', { class: 'ov-none', text: 'Nothing saved yet in this period.' })
    : partial ? el('p', { class: 'ov-none', text: 'Loading…' })
      : ovFigure(tid, 'Tokens saved', tokenFigure(flow, o, compact).text, '', '', 'ov-fig-tok');
  const saving = (tid, v, flow, ran) => sub ? tokens(tid.replace(/-usd$/, '-tok'), flow, ran) : money(tid, v, ran);
  const grid = el('div', { class: 'ov-cards' });

  // Cache: the automatic pings (and the cache reorder), plus your manual pings.
  const cacheUSD = parts.cache + parts.manual;
  const cacheRan = o.keepalive_pings > 0 || !!o.keepalive_net_usd || parts.reorder !== 0 || (mp.count || 0) > 0;
  const tokRow = (label, flow) => hasTokens(flow) ? el('li', {}, el('span', { text: label }), el('strong', { text: tokenLine(flow, compact) })) : null;
  const pings = sub ? pingLine(o, compact) : '';
  const subRows = sub ? [
    tokRow('Keep-alive rescue', ft.keepalive),
    pings ? el('li', { 'data-testid': 'saved-cache-pings' }, el('span', { text: PING_LABEL }), el('strong', { text: pings })) : null,
    tokRow('Cache reorder', ft.cachesplit),
    tokRow('1-hour cache', ft.ttl),
  ].filter(Boolean) : [];
  // No empty list when every row is empty.
  const brk = sub ? (subRows.length ? el('ul', { class: 'ov-split', 'data-testid': 'saved-cache-split' }, subRows) : null)
    : el('ul', { class: 'ov-split', 'data-testid': 'saved-cache-split' },
    el('li', {}, el('span', { text: 'Automatic pings' }), el('strong', { text: usd(parts.keepalive) })),
    el('li', { 'data-testid': 'saved-manual' }, el('span', { text: 'Manual pings' + (mp.count ? ' (' + num(mp.count) + ')' : '') + (mp.count ? ' · estimate' : '') }),
      el('strong', { text: usd(parts.manual) })),
    parts.reorder !== 0 ? el('li', {}, el('span', { text: 'Cache reorder' }), el('strong', { text: usd(parts.reorder) })) : null);
  // The strategy's name, as the plugin calls it, beside "Cache" the way the preset's name sits beside
  // "Preset". Read from the signed-in account's own settings; not guessed for a manager looking at
  // everyone, nor for a local proxy, whose page does not see the plugin's strategy file.
  const tn = account && account.tenant;
  const sName = tn && tn.role !== 'manager' ? strategyName((tn.effective_config || {}).cache) : '';
  grid.appendChild(card('cache', 'saved-cache', 'Cache' + (sName ? ' (' + sName + ')' : ''), '#/keepalive',
    'Keeps your prompt cache warm while you think, so the next turn reads it instead of writing it again.',
    [
      saving('saved-cache-usd', cacheUSD, ft && ft.cache, cacheRan),
      cacheRan && (sub ? !partial : priced) ? brk : null,
      cacheRan && ready && !sub ? ovFigure('saved-cache-tokens', 'Tokens read instead of rewritten',
        cacheTok === null ? '—' : '≈ ' + compact(cacheTok), 'of ' + compact(inputWithout) + ' tokens',
        cacheTok === null ? '' : share(cacheTok, inputWithout)) : null,
      cacheRan ? el('div', { class: 'ov-fig', 'data-testid': 'saved-cache-latency' },
        el('div', { class: 'ov-fig-k' }, 'Waiting time saved ', infoButton('latency', 'Waiting time saved', [
          'A request that must write the cache again is slower than one that reads it.',
          'We measure the gap on your own traffic in this period: the average time of large requests (100k+ tokens) that read the cache, against those that wrote it.'
            + (latKnown ? ' Here: ' + waitText(lat.miss_mean_ms) + ' for a write, ' + waitText(lat.hit_mean_ms) + ' for a read, so ' + waitText(lat.per_miss_ms) + ' saved each time.' : ''),
          'Waiting time saved = that gap × the cache writes prevented: real requests that read a cache that would otherwise have expired.',
          'Manual pings are not counted here. A ping is not a request you waited on.',
          'We need at least 20 requests of each kind to measure it. With fewer, it shows "—".',
        ])),
        el('div', { class: 'ov-fig-v', text: waited === null ? '—' : waitText(waited) }),
        el('div', { class: 'ov-fig-of', text: latKnown ? waitText(lat.per_miss_ms) + ' per write prevented' : !ka ? 'Measuring…' : 'not enough requests to measure yet' })) : null,
      cacheRan ? ovFigure('saved-cache-writes', 'Cache writes prevented', num(w.requests),
        'requests that read a warm cache' + (w.manual ? ' · up to ' + num(w.manual) + ' more from manual pings that found it warm' : ''), '') : null,
    ],
    infoButton('cache', 'Cache', sub ? [
      'Tokens saved: the cache writes context-guru prevented, by kind, and "cost" for what that made you pay. It is the keep-alive rescue, the cache reorder and the 1-hour cache together.',
      'Keep-alive rescue: requests that read a cache a ping kept warm, in place of writing it again. Its cost is the cache reads.',
      'Keep-alive pings: what the pings themselves paid. A ping is not your agent\'s traffic, so it is on its own line and not in the total.',
      'This is the most it could have saved. Another session can keep the same cache warm. The Cache strategy tab says if it pays off.',
    ] : [
      'Automatic pings: context-guru sends a small request before the cache expires. Saved = the cache writes it prevented, less the cost of the pings.',
      'Manual pings: the same, when you press Ping. This figure is an estimate. It is the write the ping avoided, taken from the cache lifetime and the size of the prefix. No request read it back. The other figures in this total are measured.',
      'This is the most it could have saved. Another session can keep the same cache warm. The Cache strategy tab says if it pays off.',
    ])));

  // Your preset.
  const named = presetName();
  grid.appendChild(card('compaction', 'saved-compaction', 'Preset' + (named ? ' (' + named + ')' : ''), '#/components',
    'Removes old tool output and repeated text before it reaches the model.',
    [
      saving('saved-compaction-usd', parts.compaction, ft && ft.compaction, o.requests > 0),
      o.requests > 0 && !partial ? ovFigure('saved-compaction-tokens', 'Tokens removed', compact(presetTok),
        'of ' + compact(inputWithout) + ' tokens', share(presetTok, inputWithout)) : null,
      capNote(o, usd) ? el('p', { class: 'ov-none ov-small', 'data-testid': 'replay-capped', text: capNote(o, sub ? null : usd) }) : null,
      o.estimated_requests > 0 ? el('p', { class: 'ov-none ov-small', 'data-testid': 'estimated-requests',
        text: num(o.estimated_requests) + ' of ' + num(o.requests) + ' requests are not counted above: the first '
          + 'request of a thread, or the first after a restart that lost its state. The proxy cannot tell if the cache was already warm.'
          + (sub ? '' : ' Estimated saving, not in any total: ' + estRange(o.estimated_saved_low_usd || 0, o.estimated_saved_usd || 0) + '.') }) : null,
    ]));

  // Tools and skills filter.
  const filterRan = o.decl_filter_requests > 0;
  grid.appendChild(card('filter', 'saved-filter', 'Tools and skills filter', '#/tools',
    'Leaves tool and skill definitions the model does not need out of the prompt.',
    partial && !filterRan ? [el('p', { class: 'ov-none', text: 'Loading…' })] : [
      saving('saved-filter-usd', parts.filter, ft && ft.filter, filterRan),
      filterRan ? ovFigure('saved-filter-tokens', 'Tokens removed', compact(filterTok),
        'of ' + compact(inputWithout) + ' tokens', share(filterTok, inputWithout)) : null,
    ]));

  // Tool deferral (ultrahigh). Drawn only on an account that deferred: no card for 0.
  if (parts.defer !== 0 || (ft && hasTokens(ft.defer))) {
    grid.appendChild(card('tooldefer', 'saved-defer', 'Tool deferral', '#/tools',
      // In tokens the figure is net of the names list only: the searches are requests of their own
      // and have no request-token figure here (#988 review).
      'Keeps tool definitions out of the prompt until the model searches for them. '
        + (sub ? 'Net of the names list.' : 'Net of the names list and the searches.') + ' Not counted in Your preset.',
      [saving('saved-defer-usd', parts.defer, ft && ft.defer, true)]));
  }
  host.appendChild(grid);
}

/** Keep-alive's waiting-time figure comes from the keep-alive ledger. Best-effort and separate. */
/** The keep-alive ledger is read by both the Overview and the Keep-alive tab. Share one request for a
 *  few seconds, so opening the tab right after the Overview does not ask the server for it twice. */
function keepAliveLedger() {
  const now = Date.now();
  // Keyed by the whole query (tenant, window, filters): a change of scope must not get the old scope's ledger.
  const key = qs({});
  if (!state.kaLedgerP || now - state.kaLedgerAt > 15000 || state.kaLedgerKey !== key) {
    state.kaLedgerAt = now;
    state.kaLedgerKey = key;
    state.kaLedgerP = api('keepalive').catch((e) => { state.kaLedgerP = null; throw e; });
  }
  return state.kaLedgerP;
}
/**
 * The Claude subscription (GET /api/me/limits), and the open 5-hour window's own stats for the
 * estimate under the headline. On a subscription every saving shows in tokens. Any failure, or `{}`
 * (an API key, Bedrock, Vertex), leaves state.sub null: the dollar view.
 * A manager's Overview spans other accounts, so it keeps the dollar view (readsLimits).
 */
async function loadOverviewLimits() {
  // Each call takes a number. An older answer that lands after a newer call must not overwrite it.
  const seq = (state.subSeq = (state.subSeq || 0) + 1);
  const done = (sub) => {
    if (seq !== state.subSeq) return;
    state.sub = sub;
    if (state.overview) renderSavedLedger(state.overview, state.keepAliveLedger, !state.overviewFull);
  };
  if (!readsLimits(account && account.tenant)) { done(null); return; }
  let sub = null;
  try {
    const limits = await meLimits(0);
    if (isSubscription(limits, Date.now())) {
      sub = { limits, win: null };
      // The estimate needs an open 5-hour window and its spend and saving. Without them the tokens still show.
      const f = liveFiveHour(limits, Date.now());
      if (f) {
        try { sub.win = await ctl('/api/stats?lean=1&since=' + fiveHourStartMs(f)); } catch (e) { /* tokens only */ }
        // The learned 5-hour weights (#1047). Any failure keeps the estimate at API prices.
        try { sub.fit = await ctl('/api/me/fivehour-weights?since=' + fiveHourStartMs(f)); } catch (e) { /* API prices */ }
      }
    }
  } catch (e) { /* no subscription signal: the dollar view */ }
  done(sub);
}

async function loadOverviewKeepAlive() {
  try {
    state.keepAliveLedger = await keepAliveLedger();
    if (state.overview) renderSavedLedger(state.overview, state.keepAliveLedger, !state.overviewFull);
  } catch (e) { /* the savings are already on screen without the waiting time */ }
}

/**
 * Tools and skills worth removing, ranked by what they cost to carry, with the command that
 * does it. Reuses the Inventory tab's own GET /api/tools; nothing about caching lives here.
 */
async function loadOverviewTools() {
  const panel = $('#inventory-panel');
  if (!panel) return;
  try {
    const rep = await api('tools');
    if (!rep.coverage || !rep.coverage.captured) { panel.hidden = true; return; }
    panel.hidden = false;
    const host = clear($('#recommendations'));
    const servers = (rep.servers || []).filter((s) => s.tools_used === 0 && s.unused_usd > 0)
      .map((s) => ({ kind: 'MCP server', name: s.server, usd: s.unused_usd, tools: s.tools, sessions: s.sessions_declared }));
    const skills = ((rep.skills && rep.skills.skills) || []).filter((t) => neverCalled(t) && t.unused_usd > 0)
      .map((t) => ({ kind: 'Skill', name: t.name, usd: t.unused_usd, sessions: t.sessions_declared }));
    const rows = [...servers, ...skills].sort((a, b) => b.usd - a.usd).slice(0, 5);
    if (!rows.length) {
      host.appendChild(el('p', { class: 'empty-line', text: 'Nothing to remove. Every tool server and skill you added was used at least once in this window.' }));
      return;
    }
    const list = el('ol', { class: 'reco-list' });
    for (const r of rows) {
      list.appendChild(el('li', { class: 'reco-row' },
        el('span', { class: 'reco-kind', text: r.kind }),
        el('span', { class: 'reco-name', text: r.name }),
        el('span', { class: 'reco-note', text: (r.tools ? r.tools + ' tools, ' : '') + 'never used in ' + num(r.sessions) + ' sessions' }),
        el('span', { class: 'reco-usd', text: usd(r.usd), title: 'Estimated cost of carrying it in this window' })));
    }
    host.appendChild(list);
    host.appendChild(el('p', { class: 'note reco-note-foot', text: 'Dollars are what carrying each one cost in the period selected above. The Tools and skills filter card is a separate, automatic saving; removing these is up to you.' }));
    const cmd = (c, why) => el('div', { class: 'cmd-line' }, el('code', { text: c }), copyButton(c), el('span', { class: 'cmd-why', text: why }));
    host.appendChild(el('div', { class: 'cmd-box' },
      el('p', { text: 'In Claude Code, run:' }),
      cmd('/context-guru:remove-mcp-and-skills', 'removes the ones you pick'),
      cmd('/context-guru:insights tools', 'shows the details for each one')));
  } catch (e) {
    if (!aborted(e)) panel.hidden = true; // best-effort: Overview must not depend on this
  }
}

function bucketFor() {
  const [since, until] = rangeMs();
  if (since === 0) return 3600000;
  const span = (until || state.nowMs || Date.now()) - since;
  if (span <= 3600000) return 60000;
  if (span <= 86400000) return 300000;
  return 3600000;
}

function renderSeries(buckets) {
  if (!buckets.length) {
    for (const id of ['#chart-cost', '#chart-tokens', '#chart-cache', '#chart-latency', '#chart-volume']) {
      emptyState($(id), 'No data in this window', 'Send traffic through the proxy, or widen the time range.');
    }
    return;
  }
  // The five plots live in charts.js (CGCharts): one idea each, labelled on the plot, exact figures on hover.
  CGCharts.netSaving($('#chart-cost'), buckets);
  CGCharts.columns($('#chart-tokens'), buckets, {
    key: 'saved_unique', sum: ['saved_unique'], color: SERIES[0], fmt: compact, empty: 'No tokens were removed in this window',
    label: 'Tokens removed per period',
    rows: (bins) => (i) => [['Tokens removed', compact(bins[i].saved_unique), SERIES[0]]],
  });
  CGCharts.cacheShare($('#chart-cache'), buckets);
  const lat = clear($('#chart-latency'));
  const a = el('div'), b = el('div');
  lat.append(el('p', { class: 'plot-sub', text: 'Added by context-guru' }), a, el('p', { class: 'plot-sub', text: 'Taken by the provider' }), b);
  CGCharts.trend(a, buckets, { key: 'cg_latency_ms_avg', color: SERIES[0], fmt: ms, name: 'Added by context-guru', label: 'Time context-guru added to each request' });
  CGCharts.trend(b, buckets, { key: 'upstream_ms_avg', color: SERIES[1], fmt: ms, name: 'Provider round trip', label: 'Time the provider took per request' });
  CGCharts.columns($('#chart-volume'), buckets, {
    key: 'requests', sum: ['requests', 'cache_misses', 'expands'], part: 'cache_misses', color: SERIES[0], partColor: SERIES[1],
    fmt: num, label: 'Requests per period, and how many missed the cache',
    rows: (bins) => (i) => [['Requests', num(bins[i].requests), SERIES[0]], ['Missed the cache', num(bins[i].cache_misses), SERIES[1]], ['Text restored on request', num(bins[i].expands)]],
  });
}

Object.assign(loaders, { overview: loadOverview });
