# Validating the cache-aware trigger end to end

One live run that shows the whole chain works: a session reaches the fill threshold, its cache goes
near-expiry, `summarize` fires, the chat continues, and the Components tab's episode figures agree
with what the database says happened.

This is a **functional validation of the proxy**, so the traffic must go **through** Context Guru —
it is not a benchmark measurement of an agent, and the "benchmarks bypass Guru" rule does not apply.
Nothing here is compared against a non-Guru baseline.

## Why this needs a plan at all

Three conditions have to be true on the same turn, and two of them are timing:

| Condition | Controlled by |
|---|---|
| billed input ≥ `0.9 × window` | how much transcript has accumulated |
| the window is exactly known | the model id resolving in a price list or LiteLLM |
| cache phase is `pre_expiry` — `0 < TTL − idle ≤ 60s` | **the gap between two turns** |

The third is the whole reason this is hard to catch by accident and easy to force on purpose: with a
5-minute TTL, a turn arriving **240–300s** after the previous one lands in the window. So the test
is a sequence of `sleep`s.

## Making the fill threshold cheap to reach

Reaching 0.9 of a real 1M window costs ~900k input tokens per turn. Instead, **pin a small window
in an operator price list**, which is the first link of the resolver chain and reports its answers
as exact (`modelinfo.Table.WindowExact`), so `CtxWindowExact` is true and the fill gate engages:

```yaml
# /tmp/cg-validate/prices.yaml
cache_read_frac: 0.1
cache_write_frac: 1.25
models:
  - match: "aws/claude-sonnet-5"
    in: 3.0
    out: 15.0
    window: 30000        # THE TEST LEVER: 0.9 of this is 27,000, not 900,000
    note: "validation run only — the real window is 1,000,000"
```

`0.9 × 30,000 = 27,000` billed tokens to fire, and the episode span is `0.10 × 30,000 = 3,000`
tokens of transcript growth. That turns a $25 run into well under $1 and a multi-hour run into
about fifteen minutes.

This tests the trigger's **arithmetic and plumbing**, which is what has been wrong twice. It does
not test behaviour at a genuine 1M fill; see "What this run does not prove".

## Setup

An isolated proxy — its own port and its own database, so nothing lands in the shared dashboard and
no other session's traffic lands in ours:

```bash
mkdir -p /tmp/cg-validate
cat > /tmp/cg-validate/config.yaml <<'YAML'
components:
  summarize:
    keep_last: 3
    min_tokens: 500
    resummarize_tokens: 2000
    # The shipped defaults, written out so the run is self-describing:
    trigger:
      min_request_frac: 0.9
      cache_state: pre_expiry
      pre_expiry_seconds: 60
YAML

MODEL_PRICES=/tmp/cg-validate/prices.yaml \
CHEAP_MODEL=aws/claude-haiku-4-5 \
/tmp/cg-purego \
  --listen 127.0.0.1:4111 \
  --config /tmp/cg-validate/config.yaml \
  --dashboard --dashboard-db /tmp/cg-validate/dash.db \
  --anthropic-upstream "$ANTHROPIC_BENCHMARK_BASE_URL" &
```

Confirm the window resolved exactly before spending anything — if this line is absent the run
cannot fire and there is no point continuing:

```bash
grep "model price list loaded" <proxy log>   # expect entries=1
```

Client turns go to `127.0.0.1:4111` with a stable session header, which is what keys the
checkpoint, the TTL record and the billed-input figure:

```bash
curl -sS http://127.0.0.1:4111/anthropic/v1/messages \
  -H 'content-type: application/json' \
  -H "x-api-key: $KEY" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'x-context-guru-session: validate-1' \
  -d @turn.json > /dev/null
```

### Seeding from a real session instead

If you would rather start from traffic that genuinely reached 0.9 rather than a synthetic
build-up, pull a recorded transcript and replay it as the first turn:

```bash
curl -sS "$GURU/api/sessions/<session-id>/transcript" > /tmp/cg-validate/seed.json
```

That reproduces a real near-full transcript in one request instead of paying to generate one, and
it makes the fixture representative rather than invented. It replaces phase A only; phases B-D are
unchanged.

## The run

Each turn re-sends the whole transcript, so `Billed` per request is the prompt size and the episode
span is transcript **growth** from t0.

| Phase | Action | Gap before it | Expected |
|---|---|---|---|
| **A** build-up | 4-6 turns, each pasting ~4-6k tokens of filler so the transcript grows fast | ~5s (stay warm) | no summary. Gate `cache_state_declined_warm` on every turn once past 27k, which is itself a result: it proves the size gate opened and the cache gate held |
| **B** fire | one turn | **250s** | `summarize` acts. `fresh_summary` in its events, and the request body upstream is `[msg0, summary, last-3]` |
| **C** cold turn | one turn | **310s** (past the TTL) | `cache_miss_reason = ttl_expiry`, `cache_read = 0`, `cache_write > 0`. This is the turn that earns **cold credit** |
| **D** close | 3-5 turns, small | ~5s | each replays the checkpoint (`reused_checkpoint`, zero model calls) and earns **read credit**; the span closes when growth reaches 3,000 |

Phase A's gate is worth reading rather than skipping past: `cache_state_declined_warm` while the
transcript keeps growing is the design working, and it is also the thing that makes the coverage
number on the panel non-zero.

## Verification — hand-compute, then compare

The panel agreeing with itself proves nothing. Recompute each figure from the raw rows and require
exact agreement.

```bash
SPAN=/tmp/cg-validate/dash.db
curl -sS 'http://127.0.0.1:4111/api/components/compaction-episodes' > /tmp/cg-validate/api.json
```

**1. The trigger fired for the stated reason, once.**

```sql
SELECT r.id, r.ts, r.cache_miss_reason, r.cache_read, r.cache_write,
       c.events, c.gates, c.saved_usd
FROM requests r JOIN request_components c ON c.request_id = r.id
WHERE c.component = 'summarize' ORDER BY r.ts;
```

Expect exactly one row whose `events` contains `fresh_summary`; every later row carries
`reused_checkpoint` and `saved_usd > 0`. If a later row also carries `fresh_summary`, the
checkpoint is rolling forward more often than intended — record it, it is a real finding about
`resummarize_tokens`.

**2. The cache really went cold.** The phase-C row must show `ttl_expiry` with `cache_read = 0`.
If it shows `hit`, the sleep was too short or a keep-alive ping refreshed the entry — check for
`keepalive = 1` rows and disable the keeper for the run if so. Without a genuine cold turn there is
no cold credit to check.

**3. The span closed where it should.** From the API, `start_billed` and `end_billed`; require
`end_billed − start_billed ≥ 3000` and that the immediately preceding turn was below it.

**4. Cold credit is the sum it claims to be.**

```sql
SELECT ROUND(SUM(c.saved_usd), 8) FROM requests r
JOIN request_components c ON c.request_id = r.id
WHERE c.component = 'summarize' AND r.cache_miss_reason = 'ttl_expiry'
  AND r.ts BETWEEN <start_ts> AND <end_ts>;
```

Must equal `cold_credit_usd`. Repeat with `= 'hit'` for `read_credit_usd`, and with
`= 'prefix_change'` — that one must appear in **no** column of the API response.

**5. The invalidation debit is t0's own write.** `cache_write` on the `fresh_summary` row × the
5-minute write rate (`in × 1.25` from the price list) must equal `invalidation_debit_usd`.

**6. Net is the arithmetic.** `cold + read + other − invalidation − summarizer = net_usd`.

**7. Coverage counted the conversation.** `coverage.conversations = 1` and `with_episode = 1`. Then
run a **second** session that reaches 27k with no long gap at all: it must land in `no_episode` with
a non-zero `no_episode_cold_usd`, and produce no episode.

## Negative controls — the part that makes the run mean something

A trigger that fires on everything would pass every check above. Each of these must **not** fire,
with the named gate present:

| Control | Expected gate |
|---|---|
| a warm turn (5s gap) above the fill | `cache_state_declined_warm` |
| a pre-expiry turn on a small transcript | `below_request_trigger` |
| a pre-expiry turn above the fill on a model **absent** from the price list, with `MODEL_INFO=off` so only the substring table answers | `window_not_exact` |
| a session's **first** turn (no previous billed figure) | `window_not_exact` |

The third is the specific defect this branch fixed on the window side, and the fourth is the one it
fixed on the units side. Both are cheap to run and both would have caught a regression that
arithmetic review did not.

## Pass criteria

1. Exactly one `fresh_summary`, in the pre-expiry window, above the fill.
2. At least one genuine `ttl_expiry` turn inside the span.
3. All six recomputations in the Verification section agree to 8 decimal places.
4. All four negative controls decline, each with its named gate.
5. The second session appears in `no_episode` with a non-zero opportunity figure.
6. `mkdocs`-documented behaviour matches what the panel shows — in particular that `recorded` and
   `inferred` are separate rows.

## Cost

About 250-350k input tokens across both sessions on a sonnet-class model, most of it cache reads:
well under $1. Wall time ~15 minutes, dominated by two sleeps of 250s and 310s. The summarizer's
own calls are on `CHEAP_MODEL` and are cents.

## What this run does not prove

**The firing rate.** One forced episode says the mechanism works; it says nothing about how often
the conditions co-occur on real traffic, which is the number that decides whether the default earns
its place. That needs the query over historical `IdleMs` per conversation, and it is unaffected by
this run.

**Behaviour at a real 1M fill.** The pinned 30,000 window exercises the arithmetic, not the
provider's behaviour near its actual limit. If you want that too, drop the `window:` override and
repeat phases B-D on a session that genuinely reached 900k — same script, ~$25, several hours.

**That 0.9 and 60s are the right numbers.** Neither is measured. This validates the mechanism
against its specification, not the specification against reality.
