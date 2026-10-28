# Where cg's saving differs from the expected figure

**Status:** report, generated from commit `d8a3c4ae` of branch `docs/context-operations-spec`.
**Reproduce:** `go test ./dash/ -run TestCasesAgreeWithCurrentLogic -v` from the repo root (needs
the `go.work` that links the root module to `counterfactual/`).

## 1. What this report compares

context-guru (**cg**) edits LLM requests: it shortens tool outputs, removes unused tool
declarations, replaces old messages with a summary. Each edit makes the request cheaper, and cg
reports how much money that saved. The report is built by `dash.Event.Price` in `dash/event.go`.

The `counterfactual` library (the "spec" in this report) is a proposal to compute the same saving by
one shared rule. Its rule is the **counterfactual**: *if this change had never been made, how would
the provider have billed these tokens on this request?* Where the spec and cg disagree, one of them
is wrong, or they answer different questions. This report lists the disagreements.

**How the two sides were produced**

- **Expected figure** (the spec). Each case is a sequence of requests of one conversation. I wrote the
  expected tokens by hand from `examples.md`, then checked them: the billed bill minus the
  no-change bill must equal the sum of tokens saved minus tokens caused (conservation), and every
  dollar total must match the document it came from. `Compute`, the library function that would
  produce these figures, is **not implemented yet**. So "expected" means "what the spec's rules give
  when applied by hand", and it is a specification, not a measured result.
- **cg's figure.** The test calls the real `dash.Event.Price` for every request of every case and
  reads `BaselineCostUSD`, `Components[i].SavedUSD`, `KeepAliveSavedUSD` and `CachesplitSavedUSD`.
  The only code I wrote on that side is the adapter `Play` in `dash/counterfactual_play_test.go`, which
  turns a case into a `dash.Event`. One assumption in it matters and is repeated below: the first
  request of a change counts as a "unique" removal, and later requests as "replays".

Of nine cases, **four agree** with cg to the micro-dollar and **five differ**
(case 3 only on its per-component line: its request totals agree). The cases that
agree are listed in section 8 so the reader knows what was not found wrong.

## 2. Terms

| Term | Meaning |
|---|---|
| Tier | The rate a token is billed at: **input** (fresh), **cache write** (1.25 times input on Anthropic), **cache read** (0.1 times input). |
| Hot / cold | Hot: the provider still holds the cached prefix, so it reads it. Cold: the entry expired (5 minutes), so the prefix is written again. |
| Prefix, breakpoint | The provider caches the start of the prompt (tools, then system, then messages), but only up to a `cache_control` breakpoint. |
| B\* | The end of the prefix the provider would have read on this request if no edit had happened. |
| Saved / caused | Tokens the provider did not bill because of an edit, and tokens it billed because of an edit. |
| Break | An edit changes the prompt inside the cached prefix, so the provider can no longer read from the first changed position onward and writes it again. It is a **cost** of the edit. |
| µ$ | Micro-dollar, $0.000001. |

**Rates used in every case** (per million tokens): input $3.00, cache write $3.75, cache read $0.30,
output $15.00. Case 5 adds a second model with its own rates, given there. These are test rates, not
production rates.

**The two pricing rules compared.** The spec prices a removed token at the tier it would have had at
its *position*: before B\* it would have been read, after B\* and up to the last breakpoint it would
have been written. cg prices it by *novelty*, in `baselineDeltaUSD` (`dash/event.go:1020`): the first
time a token is removed it is priced as a **write**, and on later requests it is priced at the tier
this request actually paid, by `repeatRate` (`dash/event.go:1081`). That works when the removed
content is new, which is the normal case. It does not look at where the content sits relative to the
cache, which is what cases 2 and 3 expose.

## 3. Summary

| # | Case | Spec total | cg total | Where the problem is | Section |
|---|---|---:|---:|---|---|
| 1 | Summary that breaks the cache | +$0.01675 | +$0.02883 | cg: the cache break is billed but not charged to the change | 4 |
| 2 | Edit inside the cached prefix | −$0.05960 (this change) | +$0.07481 | cg: prices a first removal as a write, no break. Also depends on an open spec question | 5 |
| 3 | Added tool declaration | totals agree | 0 for that item | cg: an addition is clamped to zero. Partly my adapter | 6 |
| 4 | Model change | −$0.03680 | $0 | cg has no concept of a setting. Spec rates are undecided | 7.1 |
| 5 | Effort change | −$0.11040 | $0 | cg has no concept of a setting | 7.2 |

---

## 4. Case 1: a summary that costs money first, then pays back

*Case id `rewrite+spend/summary/break-then-payback/5m`, from `examples.md` example 4.*

### The situation

A summarizer replaces three old messages (10,000 tokens) with a 500-token summary. The call that
writes the summary costs money before it saves anything. The prompt is built from three parts:
**4,000 tokens of tools, a 4,000-token system prompt**, and the messages. The system prompt and the
tools do not change, and they matter for one figure below: the provider reads up to the end of
`system`, which is 8,000 tokens. The client's prompt on request A is 21,500 tokens; the forwarded one
is 12,000. Four requests follow in time:

| Request | When | Cache | What happens |
|---|---|---|---|
| 0 | minute 0 | cold | Caches 21,000 tokens. |
| A | +1 minute | hot | The summary is applied. It sits at the start of the messages, so every message changes. |
| B | +2 minutes | hot | The summary is cached now. |
| C | +17 minutes | cold | The cache expired. |

### How the expected figures were computed

*The spec's rule, step by step, per request.*

**Request A.** The provider can read only up to the end of `system` (4,000 tools + 4,000 system = 8,000
tokens), because the first message has changed. It writes the other 4,000 tokens: the 500-token summary, the 3,000-token tail and a new
500-token message. Without the summary the provider would have read all 21,000 cached tokens and
written the 500 new ones.

| Item | Calculation | µ$ |
|---|---|---:|
| No-change bill (baseline) | 21,000 × $0.30 + 500 × $3.75 | 8,175 |
| Bill with the summary | 8,000 × $0.30 + 4,000 × $3.75 | 17,400 |
| Summarizer call | 10,000 × $1.00 + 500 × $5.00 (summarizer rates) | −12,500 |
| Saving of the removed tokens | 9,500 × $0.30 (they would have been read) | +2,850 |
| Cache break | 3,500 × ($3.75 − $0.30): summary (500) and tail (3,000) written, not read | −12,075 |
| **Request A, net** | 2,850 − 12,075 − 12,500 | **−21,725** |

Check: baseline − billed bill = 8,175 − 17,400 = −9,225 = 2,850 − 12,075. The call is separate.

**Requests B and C** need no break. B: 9,500 × $0.30 = +2,850. C is cold, so the 9,500 tokens would
have been written: 9,500 × $3.75 = +35,625.

**Thread total:** −21,725 + 2,850 + 35,625 = **+16,750 µ$ ($0.01675)**.

### What cg computed

| Request | Expected µ$ | cg µ$ |
|---|---:|---:|
| 0 | 0 | 0 |
| A | −21,725 | **−9,650** |
| B | 2,850 | 2,850 |
| C | 35,625 | 35,625 |
| **Total** | **16,750** | **28,825** |

cg is higher by 12,075 µ$ on request A, **72% above the spec on the thread total**. B and C agree.

On request A, cg computes: bill with the summary 17,400 + the removed 9,500 tokens priced at the read
rate (`uniqueRate` returns `CacheRead` for a summarizer on a hot cache, `dash/event.go:811-819`) =
20,250 as "the bill without the summary". The saving is 20,250 − 17,400 = 2,850, and after the
12,500 call it is −9,650.

### Where the problem is

**In cg.** `baselineDeltaUSD` (`dash/event.go:1041-1043`) builds the no-change bill by adding only the
*removed tokens* to the billed cost. The no-change bill therefore comes out as 20,250 µ$, where the
real one is 8,175 µ$. The 12,075 µ$ gap is exactly the break: the summary forced the provider to
write 3,500 tokens that it would otherwise have read, and that extra cost is already inside the
billed 17,400. cg counts the cost in the bill and never attributes it to the summary, so the
summary looks cheaper than it is.

cg does have a debit for this elsewhere. `chargeRowCosts` and `causedWriteUSD`
(`dash/compactepisode.go:797-813`, `:1012`) charge the whole cache write of the compaction turn to
the summary, as a stated upper bound. I did **not** run that code, because `Event.Price` is the
function that produces the per-request figure. By my reading it would charge 4,000 × $3.75 = 15,000
µ$ here, against the spec's 12,075 µ$. The spec counts only the tokens that were read before and are
written now (3,500), and not the 500 new tokens that were written anyway. So even the episode view
would not match the spec exactly, but it would be much closer. **Unverified**: which of the two views
a user sees for this case.

---

## 5. Case 2: an edit inside the cached prefix

*Case id `rewrite/coref-break-inside-prefix/hot/5m`, from `examples.md` examples 6 and 7.*

### The situation

On turn 7 of a conversation, an edit called `coref` reduces message 5, a 20,000-token tool output, to
a 50-token marker. Turn 6 had cached everything through message 11, so message 5 sits **inside the
cached prefix**. The client did not change anything. The edit changes the prompt at message 5, so
the provider can no longer read from there onward.

The client has two breakpoints only: the end of `system` and the end of the body. So the provider
reads `tools` and `system` (15,300 tokens) and writes everything after them again.

Four other edits are still active from earlier turns (the tools removal, the added expand tool,
`mask`, `cmdfilter`), and their figures are in the totals.

### How the expected figures were computed

| Item | Calculation | µ$ |
|---|---|---:|
| Client body, if nothing had been edited | 73,000 read + 2,000 written (the new 2,000 tokens) | 29,400 |
| Bill with all edits | 15,300 read + 21,010 written | 83,377.5 |
| Saving of `coref` | 19,950 × $0.30 (the grep output would have been read) | +5,985 |
| Break | 19,010 × ($3.75 − $0.30): messages 0 to 4, the marker and messages 6 to 11 move from read to write | −65,584.5 |
| `coref`, net | | **−59,599.5** |

The 19,010 tokens are 11,060 + 50 + 7,900. The 2,000 new tokens are written in both bills, so they
cancel. Request 7 as a whole, with the other four edits: 29,400 − 83,377.5 = **−53,977.5 µ$**.

At this saving rate `coref` needs about 10 more hot turns after this one (59,599.5 ÷ 5,985) to repay its net cost.

### What cg computed

| | Expected µ$ | cg µ$ |
|---|---:|---:|
| `coref` | −59,599 | **+74,813** |
| Request 7 as a whole | −53,977 | **+80,434** |
| Thread total (turns 1 to 7) | 33,524 | 167,935 |

cg says `coref` **saved** $0.0748 on this turn. The spec says it **cost** $0.0596.

cg computes the saving as 19,950 tokens × $3.75, the write rate: `uniqueRate` returns
`CacheWrite` for any component that is not a summarizer (`dash/event.go:819`), because the first
removal of content is assumed to be new content that would have been written. It adds no break.

### Where the problem is

**Two places, and one of them is open.**

1. **In cg, a wrong assumption for this edit.** `coref` removes content that is already cached. The
   write-rate assumption in `uniqueRate` and `baselineDeltaUSD` (`dash/event.go:811-819`, `:1041`)
   is right for `cmdfilter` on a brand-new message, and wrong here. `Event.Price` has no term for the
   break at all. The 74,813 figure is 19,950 × $3.75, and the real effect is the opposite sign.

   cg also tries to keep this situation from happening. The comment at `components/component.go:473-479`
   says that, when `CacheAware` is set, offloaders must restrict their mutations to the uncached
   tail. `coref` and `extract_llm` are the two components that **choose** to break the prefix on
   purpose, when the batch repays it: `components/offload/prefix_econ.go:14-20` states the rule
   (`S × T > 11.5 × W`) and the 11.5 factor. So cg *knows* the break costs 11.5 read-equivalents per
   token, in the decision to act. It does not carry that cost into the saving it reports after.
   **Unverified**: whether the live pipeline would ever choose this edit on this prompt. If it would
   not, this case cannot happen in production, and the difference is theoretical.

2. **In the spec, an open question.** The spec's rule for B\* (design §5.1) snaps to the client's
   own breakpoints. I tried reading it literally for this case: the client's only breakpoints are the
   end of `system` and the last message, so the literal B\* is the end of `system`, not message 11.
   Then `coref`'s 19,950 tokens come after B\* and are priced as writes, which is **cg's answer**
   (+74,813) with no break. The same literal reading also contradicts `examples.md` example 2. I
   worked this out by hand and did not run it. So this case's expected figure depends on a decision
   the spec has not made. It is listed in the case's `open` field.

### A related, smaller difference: the added tool

Turn 7, and turns 1 to 5, also show `expand`, a 300-token tool that cg's pipeline adds to every
request. The spec books it as a cost of 300 tokens (−$0.00009 per hot turn, −$0.001125 on the first).
cg books **0** for it. See case 3.

---

## 6. Case 3: an added tool declaration

*Case id `rewrite/four-operations/hot/5m`, from `examples.md` example 6.*

### The situation

On turn 5, four edits are active: `tool-prune` removes three tool declarations (−8,000 tokens),
`mask` reduces one message (−3,940), `cmdfilter` reduces the new message (−7,100), and `expand`
**adds** a 300-token tool (+300). The client body is 71,000 tokens and the forwarded one 52,260.

### How the expected figures were computed

Turn 5 is hot, and turn 4 left messages 0 to 8 cached.

| Edit | Tokens | Tier | µ$ |
|---|---:|---|---:|
| `tool-prune` | −8,000 | read (tools come first) | +2,400 |
| `mask` | −3,940 | read | +1,182 |
| `cmdfilter` | −7,100 | write (message 9 is new) | +26,625 |
| `expand` | **+300** | read: it is before B\*, so a cost | **−90** |
| **Net** | | | **+30,117** |

Check: baseline 63,000 read + 8,000 written = 48,900; billed 51,360 read + 900 written = 18,783;
48,900 − 18,783 = 30,117.

### What cg computed

The request total is **30,117, the same as the spec**. Only the line for `expand` differs: the spec
books −90 µ$, cg books 0. The same happens on turns 1 to 4 (−1,125 then −90 each) and on turn 7 of
case 2. The thread total is unaffected.

### Where the problem is

**In cg, as a design gap, and partly in my test adapter.**

- `Report.Saved()` (`components/component.go:967-972`) returns 0 when a component's tokens *grow*.
  `Event.Saved()` (`dash/event.go:474`) does the same per request. `ComponentSavedUSD` clamps a
  negative gross to zero (`dash/event.go:766-769`). So cg has no way to show an *addition* as a
  cost for one component.
- The request total still agrees, because `Event.Saved()` works on the whole request, where the
  added 300 tokens net against the removed ones.
- My adapter `Play` passes `max(delta, 0)` for `expand`, the same way. **I did not check whether the
  live pipeline reports `expand` as a component at all.** If it does not, then cg has no row for it
  and the difference is real but lives in "no row", not in "a row of zero". The case's
  `differences.json` entry says this.

This is the weakest of the five differences. The money is small and the totals agree.

---

## 7. Cases 4 and 5: a change of setting

Both start from the same request 4: a conversation of 40,000 cached tokens (`tools` 5,000,
`system` 3,000, `messages` 32,000) on `large-model` at `effort` high. Request 5 adds one 1,000-token
message and changes **one setting**. The bodies of the two requests are identical up to the new
message: a setting is not content.

The spec does **not** price the change itself, because the alternative is not observable (what would
the old setting have written?). It prices the one thing that is measurable: the cache break the
change causes. cg has no concept of a setting, so it prices nothing.

### 7.1 Case 4: a model change

*Case id `setting/model-change/break`, from `examples.md` example 5a.*

**Situation.** A router moves request 5 to `small-model`. The provider keeps a separate cache for each
model, so the new model starts cold: request 5 reads **0** where request 4 left 40,000 cached, and it
writes all 41,000 tokens. The setting sits at `tools[0]`, the start of the prompt, so the break covers
the whole 40,000-token prefix.

**Rates.** `small-model`: input $0.80, cache write $1.00, cache read $0.08 per million tokens.

**Expected figure.** The moved tokens are priced at the rates of the model that serves the request
after the change (the proposed rule, not yet decided, see below):

| Item | Calculation | µ$ |
|---|---|---:|
| Break | 40,000 × ($1.00 − $0.08) | −36,800 |
| Check: baseline bill | 40,000 × $0.08 + 1,000 × $1.00 | 4,200 |
| Check: billed | 41,000 × $1.00 | 41,000 |

4,200 − 41,000 = −36,800. At the large model's rates the break would be 40,000 × ($3.75 − $0.30) =
−138,000 instead. The choice matters by a factor of 3.75, and it is **open question 10** of the
design. Also, −36,800 compares the bill against a baseline that never existed. Against "stayed on
the large model" the net would be −25,250, which mixes the break with the difference in rates.

**What cg computed.** 0. cg's baseline for this request is 41,000, the same as the bill.
`AttributeCache` (`dash/event.go:1096-1100`) sends a model it has not seen to `cold_start`, and its
comment says a first use of a model "must never be reported as a bust". `baselineDeltaUSD` adds only
removed tokens, and nothing was removed.

**Where the problem is.** In neither place: **cg is not wrong, it answers a different question.**
cg measures what its *own edits* saved. A router that changes the model is not a cg edit, and cg
deliberately does not treat a model change as a cache failure. The difference is real only if the
product needs to show the user that a model change cost them a cache. The case is marked
`notCovered` for that reason. Open: which rates price the break.

### 7.2 Case 5: an effort change

*Case id `setting/effort-change/break-from-messages`, from `examples.md` example 5b.*

**Situation.** A router lowers `effort` from high to low on the same `large-model`. Where the provider
renders `effort` into the prompt depends on the model (`cached-prefix.md`). Here it is a model that
renders it ahead of the messages only, so `tools` and `system` stay valid and `messages` do not. The
provider reads 8,000 tokens and writes the other 33,000.

**Why the spec needs a position.** The two bodies are identical up to the new message, so a
comparison of body hashes finds nothing. The setting therefore carries a position, `messages[0]`
(`SettingChange.At`). Without it the library has no way to tell where the break starts.

**Expected figure.**

| Item | Calculation | µ$ |
|---|---|---:|
| Break | 32,000 × ($3.75 − $0.30): messages 0 to 3 move from read to write | −110,400 |
| Check: baseline bill | 40,000 × $0.30 + 1,000 × $3.75 | 15,750 |
| Check: billed | 8,000 × $0.30 + 33,000 × $3.75 | 126,150 |

15,750 − 126,150 = −110,400.

**What cg computed.** 0, for the same reason as case 4. cg does not read `effort` at all: it appears
only as a filter parameter in `dash/api.go:1188`.

**Where the problem is.** As in case 4, cg does not model it. **Unverified in this case:** that a
real model renders effort ahead of the messages only. I took that from `cached-prefix.md`, which says
it varies by model, and I did not test a real model. Public documentation establishes only that a change to thinking
parameters breaks the cache, not where `effort` is rendered, so this premise is an assumption.

---

## 8. Cases that agree with cg

These four match cg to the micro-dollar, so they are **not** evidence of a problem. They show that,
for a plain removal of new or cached-and-read content, the two rules give the same answer.

| Case | Thread total µ$ | What it covers |
|---|---:|---|
| Single rewrite, hot (example 1) | 26,250 | A shortened new message, priced as a write |
| Same rewrite replayed hot, hot, cold (example 2) | 54,600 | The same 7,000 tokens as write, read, write |
| Tool removal with three spans (example 3) | 62,400 | The removal is before B\*: read when hot, write when cold |
| Keep-alive ping and the request it rescues (example 8) | 31,455 | A ping costs 3,045 µ$ and the rescue saves 34,500 |

## 9. Limits of this report

- **The expected figures are by hand.** `Compute` does not exist. If a figure here is wrong, the case
  file is wrong, and the spec's rule should be re-read. Every case passes the conservation check and
  its document total, which catches arithmetic errors but not a wrong rule.
- **The tests use a flat 4 bytes per token.** The library converts every size with the request's own
  ratio, so cg's model-specific factor (1.69 for Sonnet and Opus, `internal/tokens/billedfactor.go`) is
  not an input. Real ratios differ from 4, and no case tests another ratio yet.
- **The adapter `Play` is mine.** It feeds `dash.Event.Price` the "unique versus replayed" split that
  the real pipeline computes in `FromTrace`. A different split would change cg's figure in cases 1
  and 2.
- **The B\* rule is undecided** (section 5), and it decides whether case 2 is a defect in cg or a
  question about the spec.
- **No production traffic was used.** Whether any of these situations occurs, and how often, is not
  known from this report.

## 10. Independent review by four models

Four models on the LiteLLM proxy each received this report and checked cases 1 to 5 from general
knowledge and Anthropic's public prompt-caching documentation. They did not see `design.md` or any
repo file. Each recomputed the figures and judged who is right in principle.

Model substitutions: `aws/gpt-6-astra` was refused by the proxy (403, no access for the key), so
`azure/gpt-6.1-sol` answered in its place. `aws/kimi-k2.5` is not on the proxy, so
`azure/FW-Kimi-K3` answered. Each model ran once. The verdicts are opinions, not measurements.

| Case | Opus 5.5 | Sonnet 5.5 | GPT-6.1-sol | Kimi K3 | Consensus |
|---|---|---|---|---|---|
| 1. Summary that breaks the cache | SPEC-RIGHT (high) | SPEC-RIGHT (high) | SPEC-RIGHT (high) | SPEC-RIGHT (high) | **Spec right, 4 of 4.** cg never charges the 3,500-token read-to-write break to the summary. |
| 2. Edit inside the cached prefix | SPEC-RIGHT (high) | SPEC-RIGHT (med-high) | SPEC-RIGHT (med) | SPEC-RIGHT (med) | **Spec right, 4 of 4.** Cached content would have been read, so a write-rate price has the wrong sign. |
| 3. Added tool declaration | SPEC-RIGHT (med) | SPEC-RIGHT (med) | SPEC-RIGHT (med) | DIFFERENT-QUESTION (med) | **Spec right, 3 of 4.** All agree the totals match and cg's clamp only hides a real cost. |
| 4. Model change | DIFFERENT-QUESTION (med) | DIFFERENT-QUESTION (high) | DIFFERENT-QUESTION (high) | DIFFERENT-QUESTION (high) | **Different question, 4 of 4.** A router's switch is not a cg edit. |
| 5. Effort change | DIFFERENT-QUESTION (med) | DIFFERENT-QUESTION (med) | UNDECIDABLE (med) | DIFFERENT-QUESTION (med) | **Different question, 3 of 4.** The cache break is plausible but the premise is unverified. |

**Arithmetic.** All four confirmed every expected figure in sections 4 to 7 (16,750, −59,599.5,
−53,977.5, 30,117, −36,800, −110,400) and cg's figures (28,825 and 74,812.5 on the changed lines).
No model found an error in a total.

**B\* (section 5, open question).** All four said the same: B\* is neither the client's literal
breakpoints nor an unbounded "longest cached prefix". It is the end of the longest cached prefix
that the request's own breakpoints could actually have hit: written earlier, not expired, and
within the provider's lookback of about 20 blocks. For case 2 that makes B\* message 11, which
supports the spec's expected figure and makes the literal reading in section 5 item 2 the wrong one.

**Errors found in this report** (not in the totals):

1. Case 1 (section 4) says the tail is 3,500 tokens. All four say it is **3,000**: 500 summary +
   3,000 tail + 500 new = 4,000. The 3,500 in the break is summary plus tail.
2. Case 2 says `coref` needs "about 11 more hot turns". Three models say **about 10** more, because
   this turn's 5,985 is already netted in (59,599.5 ÷ 5,985 ≈ 10). Eleven counts this turn too.
3. Case 4 (three models): the spec's −36,800 prices the break at the small model's rates, which
   compares against a baseline that never existed. Against "stayed on the large model" the net is
   −25,250, and that mixes the break with the rate difference. This sharpens open question 10.
4. Section 1 counts case 3 among the five that differ, though its request totals agree. Two models
   flagged the inconsistency. Only the per-component line differs.
5. Case 5 (GPT-6.1-sol, Sonnet): the claim that `effort` renders ahead of the messages is not
   established by public documentation. The documented case is a change to thinking parameters.
