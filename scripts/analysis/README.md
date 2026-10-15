# wait_causes.py

Measures why a thread's keep-alive matters: wait causes, gap distributions,
cache-miss cost, and a ping-budget simulation. Built for
[issue #424](https://github.com/rossoctl/context-guru/issues/424).

Python 3 standard library only. Always run it with `python3 -I` (isolated
mode — skips the user site directory and `PYTHONPATH`, so a stray file next
to the data cannot get imported).

## Modes

**Transcript mode** (`--projects-dir`, optionally with `--db`): reads Claude
Code session transcripts. Each transcript file is one thread. Threads come
in three kinds:

- `main` — the session's own file.
- `subagent` — a plain Task-tool subagent, `<session>/subagents/*.jsonl`.
- `team_thread` — an in-process agent-team teammate (a committer/reviewer/
  etc. spawned by the AgentTeam skill), told apart from a plain subagent by
  its `.meta.json`'s `taskKind == "in_process_teammate"`. These are
  long-lived peer sessions, not bounded tasks, which is why their gaps run
  much longer than a plain subagent's — do not confuse this (a *thread
  kind*) with the `teammate` *wake cause* below (a thread of any kind woken
  by a `<teammate-message>`; a `main` thread very often has this cause too).

This gives exact per-thread gaps and the exact signal that ended each one
(teammate message, cross-session message, background task, human text, a
skill load or other system notice, the agent's own tool result, or a retry
after a failed call).

**DB-only mode** (`--db` only, no `--projects-dir`): this is how the script
must run against the production proxy, which has no transcripts. There is
no thread id and no wake cause here. The script says so and groups rows by
`(session_id, model)` as an approximate thread proxy, then reports gap
distributions, hit/miss counts and observed/simulated cost (using the DB's
own `keepalive_pings` and the real `cache_read`/`cache_write` hit rule, not
the `cache_miss_reason` label — see Known approximations), and failure
stats.

## Usage

```bash
# One project, with its dashboard DB for prices, the mislabel check, the
# real-ping lookup, and the failure cross-check:
python3 -I scripts/analysis/wait_causes.py \
  --projects-dir ~/.claude/projects/-Users-you-git-your-repo \
  --db ~/.local/state/context-guru/dashboard-8789.db

# Several projects (including Claude Code worktree variants of the same
# project — the script folds "<project>--claude-worktrees-<topic>" dirs back
# into "<project>" automatically) and several DBs:
python3 -I scripts/analysis/wait_causes.py \
  --projects-dir ~/.claude/projects/-Users-you-git-your-repo \
  --projects-dir ~/.claude/projects/-Users-you-git-your-repo--claude-worktrees-some-topic \
  --projects-dir ~/.claude/projects/-Users-you-git-other-repo \
  --db ~/.local/state/context-guru/dashboard-8789.db \
  --db ~/.local/state/context-guru/dashboard-8788.db \
  --json > report.json

# Production proxy: DB only, no transcripts.
python3 -I scripts/analysis/wait_causes.py --db /path/to/dashboard.db

# Built-in self-check on tiny synthetic transcripts and a tiny synthetic DB
# (known gaps, known hits/misses/errors, known causes, known DB matches) —
# run this after touching the cost, hit/miss, pricing, or matching logic.
python3 -I scripts/analysis/wait_causes.py --self-test
```

By default it prints a human-readable summary; `--json` prints the full
machine-readable report instead.

## What it reports

With `--db`, the report opens with **which price priced which model**
(`price_sources`): for each model string actually seen, its USD/MTok base
input price and whether that came from `db_fit` (fitted on that DB's own
billed `cost_usd`) or `hardcoded_fallback` (no DB, or the DB never billed
that model). See "Prices" below for how the fit works.

Per project, per thread kind (`main` / `subagent` / `team_thread`), per wake
cause:

- **Gap distribution**: count, median, p75, p90, max, in minutes, for gaps
  of at least 4.67 minutes (the keep-alive ping interval from issue #424 —
  below that, nothing would have needed a ping anyway).
- **Hits vs misses vs errors**: how many of those gaps resolved as a cache
  hit, a cache miss, or an upstream error, using `Call.is_cache_hit`
  (`cache_read > 0` AND `cache_creation` under half of `cache_read` — NOT
  simply `cache_read == 0`; a prompt that comes back with some `cache_read`
  but a `cache_creation` that dwarfs it is still a miss in every way that
  matters for cost) and `Call.is_error` (an `isApiErrorMessage` response,
  which is neither a hit nor a miss — it bought nothing and cost nothing in
  cache terms, so counting it as a miss, as an earlier version of this
  script did via `len(gs) - hits`, overstated the miss count). `hits +
  misses + errors` always equals the gap count. Report this breakdown so a
  reader can see whether "gap over the threshold means a miss" actually
  holds on that traffic — it is usually true but not universal.
- **OBSERVED cost**: what this traffic actually cost, in units of `x` (the
  gap-ending call's own base input price, from `price_sources`) and in USD.
  This is real pings already sent (pulled from the DB's `keepalive_pings`,
  where the DB covers the session — `observed_pings_total` reports how
  many) plus a miss premium if the call was still a miss (zero for an
  error). **This is a measurement of what happened, not a simulation** — it
  can be well below the simulated N=0 cost on a gap where real pinging
  already helped.
- **SIMULATED ping-budget**: average cost per gap, in `x` units, for a
  hypothetical, uniform budget of N = 0 to 12 pings, using the formula from
  the issue's technical notes (a ping every 4.67 min; N pings keep the cache
  warm for `4.67N + 5` minutes; a gap that ends inside that window costs
  `0.1 * min(N, floor(gap / 4.67))`, otherwise `0.1*N + 1.15`). The `min(N,
  ...)` matters at the window's edge: without it, N=0 could be charged for a
  ping it never sends (a 4.8-minute gap resolving inside the 5-minute
  no-ping window still has `floor(4.8/4.67) == 1`). **N=0 is 1.15x for
  every gap over 5 minutes, by construction** — it is the "no mitigation at
  all" baseline every other N is measured against, not a restatement of the
  OBSERVED column above. Reports the best N and its cost.
- **Cold starts**: a thread's first call, counted and costed separately —
  not a "wait", so excluded from gap distributions.

With `--db`, also:

- **Mislabel check**: how many DB `prefix_change` rows are really a
  per-thread TTL expiry or cold start, matched to a transcript event by
  session id, matching `cache_read` token count, and nearest approximate
  response time within `MATCH_WINDOW_SEC` (15s — see "Matching a DB row to
  a transcript call" below). Reports `mislabeled_full_cost_usd`: the
  mislabeled rows' full recorded `cost_usd` (what the dashboard calls
  `prefix_change_cost_usd`), not just their miss premium over a hit.
- **Failures**: non-200 upstream rows, their hang time (median/max, and by
  status code), and how many cache misses follow a run of consecutive
  failures on the same session.

In transcript mode, also, per thread kind:

- **`waiting_for` x `wake_cause` cross-tab**: `wake_cause` (above) is known
  only once a wait ENDS; `waiting_for` is known the moment it STARTS, from
  the response that ends the turn before the gap (see "waiting_for" below).
  This is the table that says how well `waiting_for` predicts `wake_cause`
  — which is the question that matters, because **the keep-alive's ping
  budget can only ever be set from `waiting_for`** (it has to decide before
  the wait ends).
- **Cost by `waiting_for`**: the same OBSERVED / SIMULATED columns `wake_cause`
  gets, grouped by `waiting_for` instead — this is the table a ping-budget
  policy is actually built from.
- **`own_tool` breakdown**: how many `own_tool` gaps started with each tool
  name (`Bash`, `Monitor`, `SendMessage`, ...). A response can call more
  than one tool, so this can sum to more than the `own_tool` gap count — it
  counts tool CALLS, not gaps.

## `waiting_for`

Read from the response that ends the turn **before** the gap (the gap's
`start` call), merging content across every transcript entry that shares
its `message_id` — a response's tool_use block can land in a different
split entry than its usage, so a naive "first entry only" read misses it.

- **A failed call (`isApiErrorMessage`) means `retry`** — same rule as
  `wake_cause`'s `retry`, since both read `Call.is_error` on the same call.
- **A `tool_use` in that response means `own_tool`**, with the tool name
  kept (see the breakdown). The exception: `AskUserQuestion` and
  `ExitPlanMode` are themselves a `tool_use`, but they wait on the human —
  so a response with either of these means `human`, not `own_tool`, even if
  it also carries other tool calls.
- **A response with no `tool_use` (`end_turn`)** means `background` if a
  background task is open, `teammate` if a teammate wait is open
  (`background` wins if somehow both are), and `human` otherwise.

### Open background tasks and open teammate waits (`waiting_on`)

Tracked per thread (`ThreadWaitState`), scanning entries in file order, from
the actual shapes found in both local projects' transcripts (grep for them
before trusting any regex here — formats do vary more than they look):

| | Start | Finish |
|---|---|---|
| Background Bash (`run_in_background`) | tool_result text: `running in background with ID: X.` | A `<task-notification>` naming `X`, EXCEPT a `Monitor`'s own mid-watch event notices (see below) |
| `Monitor` | tool_result text: `Monitor started (task X, <detail>)`, where `<detail>` is one of 3 real shapes — `expires in <dur> unless the source ends first; ...` (a timed watch), `timeout <N>ms` (also timed, in milliseconds), or `persistent — runs until TaskStop or session end` (no expiry at all) | The stated expiry elapsing (checked against the gap-start call's own timestamp), OR a `<task-notification>` whose `<event>` text says `[Monitor expired after ...]` specifically — **not just any `<task-notification>` naming X**: a Monitor fires one of these on every matching event while it keeps running (49 of 70 in both local projects were mid-watch, not final), and treating each one as a finish closed the task on its FIRST event, not its last |
| Async agent (`Agent` tool, `run_in_background: true`) | tool_result text: `Async agent launched successfully... agentId: X` | **`<agent-message from="X">`** (a subagent hand-back notice) — **not** a `<task-notification>`; this is one of two places a background task's finish signal differs by how it started |
| A background Skill (`Skill` tool, forked execution) | tool_result text: `... launched (forked execution, running in the background)` — but the id itself is NOT in this text at all; it is the entry's own structured `toolUseResult.agentId` field | A `<task-notification>` or an `<agent-message from="X">`, same as an async agent |
| Any of the above | — | A `TaskStop(task_id=X)` tool call, matched structurally on the tool name and its JSON `task_id` field, not by regex |
| A teammate wait (`SendMessage` tool_use) | matched structurally on the tool name and its JSON `to` field (not by regex) | `<teammate-message teammate_id="X">` or `<cross-session-message from-name="X">`, matched by regex since these only ever appear as plain text |

A background task with no stated expiry (a backgrounded Bash command, an
async agent, or a background Skill) stays open until an explicit finish
signal; one with a stated expiry (a `Monitor`) is also treated as closed
once that expiry has passed, even with no notification yet — the tool's
own description says a Monitor may expire silently in rare cases, and
treating "open forever" as the default for an expiring task is exactly the
bug issue #424 flagged in its own rough detector (see Validation).

**`waiting_for` is computed the moment the call that starts the wait
finishes, not when the gap is finally built.** The first version of this
script computed it lazily — only when the NEXT call was flushed — by which
point every "in-between" entry (the very teammate reply or task
notification that is about to END the gap) had already had its side
effects applied to the tracked state. That reliably made a genuinely open
background task or teammate wait look closed before it was ever checked.
Fixed by snapshotting `waiting_for` immediately after each call is
finalized, before any later entry can mutate the tracker; see the review's
3 real examples (one per leak) in `wait_causes.py`'s self-test and in
`iter_calls_and_gaps`'s own comments.

**A SendMessage's own answer, and AskUserQuestion/ExitPlanMode's, both
travel back as a bare `tool_result` block** — that is how the Messages API
returns a tool's result, regardless of who (a human, for Ask/ExitPlan)
produced it. `wake_cause`'s `tool_result` is right for every OTHER tool,
but wrong for these two: `classify_wake_cause` now applies the same
exception `waiting_for` already has, so AskUserQuestion/ExitPlanMode's
human answer is `wake_cause=human`, never `tool_result`.

**A teammate wait that never gets a reply stays open for the rest of the
thread's visible transcript** — there is no analogous expiry rule to a
Monitor's. On both local projects combined: of 1,474 teammate waits, 129
(9%) never saw a reply within the thread's own transcript (context-guru:
91 of 276, **33%**; forever: 38 of 1198, 3%); of the ones that DID close,
the median close time is a few minutes, but the tail is long — on
context-guru, the **p90 of closed waits is ~3.1-3.3 hours** (it moves
slightly run to run, since both projects' transcripts are live and still
being appended to), with a handful of outliers in the thousands of
minutes, almost certainly sessions that resumed days later rather than a
single continuous wait. Reported per project as `teammate_wait_durations`,
split into closed and never-closed (the latter is a lower bound, measured
only to the thread's last call, not a real close).

**This is reported as a finding, not fixed, and issue #424 needs to act
on it**: a third of context-guru's teammate waits never close within the
visible transcript, and even the ones that do can run for hours — so
#424 must define an assumed timeout for an open teammate wait (there is
currently no rule at all, unlike a `Monitor`'s stated expiry), or the
keep-alive has no way to ever stop treating "teammate" as the active
`waiting_for` value for a thread that is realistically done waiting.

## Learning a ping budget per tool (issue #424 section 4)

With `--projects-dir`, for `main` and `team_thread` threads (not plain
`subagent`, per the task), the report also includes:

- **Per-tool wait stats**, sorted by call count. `call_count` is every
  `tool_use` block anywhere, not just a response's last one — a plain
  usage-frequency count. **The PRIMARY columns are over waits of
  `GAP_THRESHOLD_MIN` (4.67 min) or more**: `waits_ge_threshold` (count),
  `median_wait_min`/`p75_wait_min`/`p90_wait_min`, `hits`/`misses` among
  them (the same `Call.is_cache_hit` rule every other table uses), and
  `best_n` — because **the keep-alive only ever acts after that much idle
  time**, so a tool's typical sub-minute wait (true for almost every tool
  except `AskUserQuestion`/`ExitPlanMode` and the rare long-running task)
  tells the policy nothing; only the rare long wait does. `wait_count_all`,
  `median_wait_min_all_waits` and `share_over_5min_all_waits` are kept as
  SECONDARY columns, over every wait of any length, mostly useful to see
  how rare a long wait is for that tool at all. `best_n` is `"n/a"` when a
  tool has fewer than `MIN_TOOL_WAITS_FOR_OWN_BUDGET` (20) qualifying
  waits — a best N computed from 0 or 1 waits is not a real signal (the
  earlier version of this script computed one anyway, e.g. from a single
  `Write` wait).

  **This table's `waits_ge_threshold[tool]` must equal
  `own_tool_breakdown[tool]`** (reported alongside it) for every tool
  except `AskUserQuestion`/`ExitPlanMode` (which `own_tool_breakdown`
  excludes — they wait on the human, not the tool) — both come from the
  same gaps, keyed the same way (the response's LAST `tool_use`), at the
  same threshold. `--self-test` asserts this exactly. An earlier version
  of `own_tool_breakdown` counted EVERY `tool_use` block in a multi-tool
  response instead of just the last one, which could inflate a tool's
  count past what the per-tool table showed for it (e.g. `Write` showing
  4 in one table and 1 in the other, for the same traffic).
- **A policy comparison**: four ping-budget policies' average cost per
  gap, learned on the **first half of the project's sessions by time**
  and evaluated on the second half (so this measures generalization, not
  fit — the issue's own instruction):
  1. **Hierarchical** (issue #424's own proposal): `AskUserQuestion` and
     `ExitPlanMode` always use the `human` group's budget; otherwise a
     tool with at least `MIN_TOOL_WAITS_FOR_OWN_BUDGET` (20) training
     waits of `GAP_THRESHOLD_MIN`+ uses its own learned budget; otherwise
     a tool-having-too-little-data falls back to the `own_tool` group's
     budget; a gap with no tool at all uses its own `waiting_for` group's
     budget; a default of 2 when there is no data at all. Capped at
     `MAX_PING_BUDGET` (11 — "never go above the break-even point")
     throughout.
  2. **A single global best N** — one N, learned over every training gap
     pooled together, applied to every test gap.
  3. **`waiting_for`-only** — the same per-group budgets the hierarchical
     policy uses as its fallback, but with no per-tool step at all.
  4. **Today's fixed 2 pings.**

  **A test set below `MIN_MEANINGFUL_TEST_GAPS` (50 gaps) is flagged with
  a CAVEAT line** rather than reported as a result — `forever`'s
  main-thread time-ordered split has only 14 test gaps, far too few to
  call any policy a win or a tie there.

  **A repeated-split check** (`repeated_split_policy_comparison`, 5
  RANDOM session-level splits — a different split rule from the headline
  time-ordered one, used only to check whether the headline result is a
  property of the method or an artifact of that one split's particular
  boundary) reports each policy's mean cost and its spread (min/max)
  across the 5 splits. See Validation for what it shows.
- **Unused tools**: declared built-in tools (the DB's
  `tool_declarations`, `kind='tool'`) that were never called (the DB's
  `tool_uses`) for this project's sessions. A transcript carries no
  declared-tools list of its own — checked directly: Claude Code's
  session JSONL records the rendered conversation, not the raw request
  body a `tools` array would live in — so **this needs `--db`**; without
  one, the report says so and lists only the tools that *were* called,
  from the transcripts. The DB's own `tool_uses` table can under-cover a
  project (it may only have rows for a handful of sessions out of many),
  so a tool the TRANSCRIPTS show as called is never reported unused just
  because this particular DB missed that session — the two "called" sets
  are unioned before subtracting from "declared".

## Prices

`MODEL_BASE_INPUT_PRICE_PER_TOKEN` is a **fallback only**, used when no
`--db` is given or the DB has no billed rows for a model. When a DB is
given, `fit_model_prices()` fits a fresh per-model price straight from what
that DB actually billed:

```
cost_usd = price * (fresh_input + 0.1*cache_read + 1.25*cache_write + 5*output_tokens)
```

solved per model by least squares through the origin
(`price = sum(x*y) / sum(x*x)`). This is a correction from the script's
first version, which used Anthropic's published **list** prices — those
overstated the actual billed rate on this gateway by up to 3.8x
(`claude-opus-5-5`: $15/MTok listed vs **$3.91/MTok** actually billed). The
hardcoded fallback table now holds that fit's numbers (from both local
DBs), not list prices, with a note on any model it has no direct data for.
`PriceTable.price_and_source()` always prefers a DB fit over the fallback
table when one exists for that model, and matches a transcript's full model
id (which can be a Bedrock ARN, e.g.
`anthropic.claude-haiku-4-5-20251001-v1:0`) by substring, checking longer
table keys first so `claude-opus-5-5` is never mistaken for a match on the
shorter `claude-opus-5`.

## Matching a DB row to a transcript call

The DB has no thread id (that is issue #423's job), so the mislabel check
matches a transcript call to the single real DB row it produced
(`DBIndex.match()`): same `session_id`, same `cache_read` token count, and
the nearest **approximate response time** within `MATCH_WINDOW_SEC` (15
seconds).

The DB's `ts` is the request's **start**; the transcript only has the
response's **arrival** time. `approx_response_ts()` closes most of that gap
by adding the DB row's own `upstream_ms`: on the validation session
(`fix-summarizer`), matching on raw `ts` alone needs a loose 60-second
window and still misses about 4% of calls (2817/2824, counting every
thread); adding `upstream_ms` tightens the window to 15 seconds and raises
the match rate to 99.75% (2817/2824 within 15s too, with a median residual
delta of 0.13 seconds) — the handful of unmatched calls are almost
certainly ones a DB row's own hang or retry still displaces by more than a
few seconds.

## Reading real keep-alive pings (`DBIndex.pings_for`, `db_only_report`)

The OBSERVED column's real-ping count and cost — and `ping_rows`,
`ping_orphans` in the report — read ping rows (`requests.keepalive = 1`)
**directly**, matching each one to the gap it falls inside by timestamp:
same thread_id when the DB has one (issue #423, not populated on either
local DB at the time of writing), else the best available fallback,
`(session_id, model)`.

**This replaced an earlier version that read `keepalive_pings` off the
NEXT real row instead** — a column the Go proxy writes once, on whichever
real request a ping run ends with. That misses every ping whose session
never sent another real request: a production cross-check (PR #425,
comment on the eval box) found 351 real ping rows in 75 sessions where
the old method surfaced only 28. Reading ping rows directly, and pricing
each one from its own `cost_usd` rather than a `0.1x` estimate, fixes
both: `ping_rows_summary()` reports every ping row in the DB regardless of
whether it falls in a measured gap, and `DBIndex.ping_orphans()` /
`db_only_report`'s own matching report the ones that do not — "pings for
sessions that did not return" — separately, with their own count and
cost, instead of just disappearing from the total.

A ping row with `max_tokens = 0` is a summary-substitute call (issue
\#458): still a ping slot, counted the same way, priced from its own
`cost_usd` rather than the `0.1x` read-only-ping estimate (a
summary-substitute call does real work, not just a read). Reported
separately as `substitute_ping_rows` / `substitute_ping_cost_usd_total`.
If a substitute ping is ALSO logged as a component action in
`extraction_calls` (with a strategy ending `/keepalive_substitute_billed`
or `/keepalive_substitute`), that row must be excluded from any "LLM
spend" total built from `extraction_calls`, since the same call's cost is
already counted once here, from `requests.cost_usd`
(`is_keepalive_substitute_strategy()` — this script does not read
`extraction_calls` itself today, so nothing currently needs to call it,
but anything that someday sums LLM spend across both tables must).

## Known approximations (read before trusting a number)

- **Gap timing uses response-arrival timestamps**, not request-start
  timestamps, because that is all a transcript has. For a normal call this
  differs by seconds; for a call that hung for minutes before failing (the
  502s), the *following* gap's start point is a few minutes later than the
  true idle start. This is why failed calls get their own `retry` cause
  instead of being folded into the gap they distort, and why the ending
  call of such a gap is counted as an error, never a miss (see "Hits vs
  misses vs errors" above).
- **The mislabel check and the OBSERVED column's real-ping lookup both run
  over the whole DB**, not filtered to one thread, because the DB has no
  thread id. A DB row is attributed to whichever transcript event matches
  closest (see "Matching a DB row to a transcript call"); two different
  threads' events landing within the same 15-second window on the same
  session, with the same `cache_read`, could in principle be confused —
  unlikely given how tightly the matched deltas cluster, but not provably
  impossible.
- **"Misses following a failure" counts once per run of consecutive
  failing rows** on a session (several 5xx rows in a row are usually one
  underlying hang, retried fast), checked against whichever thread's row
  happens to come next for that session — this can overcount versus a
  hand-filtered, main-thread-only count, since any thread's traffic on the
  same session can land in between.
- **DB-only mode's `(session_id, model)` grouping is not a thread.** Two
  different subagents on the same model look like one thread; a main
  thread that changes model mid-session looks like several. Treat its gap
  counts as directional, not exact. It does, however, use the same hit
  rule as transcript mode now (not the `cache_miss_reason` label, which
  depends on the ttl_expiry/prefix_change distinction issue #423 is still
  fixing — a production number built on that label would silently inherit
  whatever #423 has not yet corrected).
- **Reading real pings by `(session_id, model)` (no thread id yet) can
  attribute a ping sent for one thread to a different thread's gap**, on a
  session with more than one thread sharing a model — the same
  approximation as everywhere else in this script that falls back to
  `(session_id, model)`. This also means the SAME ping row can appear
  attributed under more than one report view (for example, both the
  `wake_cause` and `waiting_for` tables, which group the same gaps two
  ways) — that is expected, not a double real cost, since both are
  reading the one real ping row's one real `cost_usd`.
- **The `system` wake cause is a catch-all** for an `isMeta` entry that
  carries no recognized marker (a skill load, a one-line nudge like "your
  previous response had no visible output", etc.) — it is deliberately
  broad rather than enumerating every meta-entry shape, since the one thing
  that matters here is that none of them is a `human`.

## Validation

**Hit/miss counts**, cross-checked against an independent count (same
"gap > 5.5 min, deduped by `message.id`" rule, same hit definition) on both
local projects — exact match:

| Project | Thread kind | Misses | Hits |
|---|---|---|---|
| forever | main | 68 | 25 |
| forever | subagent + team_thread | 290 | 6 |
| context-guru | main | 346 | 75 |
| context-guru | subagent + team_thread | 154 | 5 |

**Price fit**, cross-checked against an independent fit of the same formula
on the same DB — exact match: `claude-opus-5-5` $3.91/MTok, `claude-sonnet-5`
$2.00/MTok, `claude-opus-5` $5.00/MTok, `claude-haiku-4-5` $1.00/MTok.

**DB match rate**, cross-checked against an independent match using
`ts + upstream_ms` — same result: 99.8% (vs. 96.2% on raw `ts` alone).

**Self-test** (`--self-test`): synthetic transcripts for a `main` thread
(cold start, a `human` miss, a `tool_result` hit, an upstream error
followed by a `retry` miss), a plain `subagent` thread (a `background`
hit), a `team_thread` (a `teammate` wake via `<teammate-message>`, a
second via `<cross-session-message>`, and a `system` wake via an
unmarked `isMeta` entry), and a dedicated `waiting_for` session covering
all five values (`own_tool` via a plain tool, `human` via the
`AskUserQuestion` exception, `background` via a `Monitor`-opened task
still inside its stated expiry, `teammate` via an unanswered
`SendMessage`, and `retry` reusing the main thread's upstream-error case)
— plus a tiny synthetic sqlite DB exercising the price fit, the
response-time match (`DBIndex`, `mislabel_check`), and
`db_only_report`'s hit rule and real-ping lookup against deliberately
mislabeled `cache_miss_reason` rows. Asserts exact numbers throughout. Run
it after touching anything in the cost, hit/miss, pricing, matching,
wake-cause, or waiting_for logic.

**`waiting_for` x `wake_cause`**, on main threads across both local
projects (gaps over 4.67 min; the same slice issue #424's own rough
cross-tab uses, as a sanity check — not a target to force a match
against, since the issue's own text calls that cross-tab "rough" and
flags its background-task detector as unreliable):

| waiting_for | Predicted cause's share | Total | Median gap | Issue's rough pass |
|---|---|---|---|---|
| `own_tool` | `tool_result`: 110/113 (97%) | 113 | 6.1 min | 91/95 (96%) |
| Turn ended, nothing open (`human`) | `human`: 194/324 (60%) | 324 | 13.9 min | 112/137 (82%) |
| Turn ended, teammate open (`teammate`) | `teammate`: 19/96 (20%) | 96 | 23.4 min | 31/61 (51%) |
| Turn ended, background open (`background`) | `background`: 19/184 (10%) | 184 | 12.0 min | 44/274 (16%) |
| Previous call failed (`retry`) | `retry`: 11/11 (100%) | 11 | 29.4 min | n/a |

`own_tool` matches the issue's rough pass closely — a `tool_use` really is
almost always answered by its own `tool_result`. The other three rows are
real signal in the same direction (each predicts its own cause more often
than chance) but weaker here than the issue's rough numbers, in both
directions on different axes: this script's `background` detector is
*stricter* than the issue's own rough one (it actually closes a task on
`TaskStop`/expiry/`<agent-message>`, which the issue says its rough
detector did not do), so a smaller fraction of gaps show "background
open" at all here (184 vs. 274) — and of those, a smaller fraction
resolve by a background wake (10% vs. 16%), which is plausible on a
*correctly* strict detector: a human message or teammate reply arriving
while a background task happens to still be open does not become less
likely just because the task is open. The `teammate` row shows the same
pattern (96 vs. 61 total, 20% vs. 51% precision). This is reported as-is
rather than tuned to match, per the open question in issue #424 itself
("is it enough to know a wait is pending, or do we also need to know who
it waits on"): a thread that sent one `SendMessage` and is also mid-way
through other independent work may have several real things it could
still be "waiting on" at once, and `waiting_for` as specified here can
only report the first-checked one (background beats teammate beats
human), not the full set.

**Full-session comparison**, against the known session
`6c456ae0-469f-44a1-92c1-f6ea03192715` (`fix-summarizer`, context-guru
project, Oct 6-8), cross-checked against the issue's own rough pass (which
used opus DB rows as a proxy for the main thread, since it had no thread id
either):

- Main-thread gaps by cause (exact, this script): 11 background / 29 human /
  25 teammate / 1 system / 7 tool_result / 1 retry. The issue's rough pass:
  10 / 25 / 21 / (not tracked) / 22 / (no retry bucket). Totals are close;
  the shift from `tool_result` to `human`/`teammate` is expected, because
  the rough pass attributed every opus DB row with no thread id to "the
  main thread", which also swept in some subagent/fork calls this script
  correctly separates into their own threads (several of them
  `team_thread`s, not `main`).
- Mislabel check: 68/71 `prefix_change` rows reclassified, full cost $96.35.
  The issue's rough pass: 46/65, $70.27. Same conclusion (most
  `prefix_change` rows on this session are mislabeled TTL expiries), higher
  count here both because this script checks every thread's events (not
  just main) and because the tighter response-time match (see "Matching a
  DB row to a transcript call") now finds more of them.
- Upstream 502s: median hang 305 seconds (5.1 min), matching the issue's
  "5.3-6.0 min" range.

**Per-tool policy comparison** (`--self-test`): built and evaluated
directly on hand-constructed `Gap`/`ToolWait` objects (no file round-trip
needed, since `simulate_policies`/`split_sessions_by_time` only look at
`.ts`/`.gap_min`/`.waiting_for`/`.tool_use_names`). 25 `Bash` waits at 6
minutes and 25 `human` waits at 20 minutes as training data (hand-derived
best N: 1 for `Bash`, 4 for the pooled `human` group and for a single
global N), evaluated on one 6-minute `Bash` test gap and one 20-minute
`human` test gap: the hierarchical, global-N, and `waiting_for`-only
policies all land on 0.25x/gap here (expected — with only one tool
feeding the `own_tool` group, its group budget and the tool's own budget
coincide), while today's fixed 2 pings costs 0.725x/gap on the same two
gaps — a clear, exactly-asserted case where a learned budget beats the
fixed default. The `unused_tools_report` DB-undercoverage fix (a tool the
transcript shows as called must never show as unused just because the
DB's own `tool_uses` table missed that session) is asserted directly
too.

On real data (both local projects' `main` and `team_thread` threads), the
DB-undercoverage case is not hypothetical: before the fix, `forever`'s
"unused tools" list included `SendMessage` and `AskUserQuestion` — tools
the transcripts show hundreds of calls to — because the local dashboard
DBs happen to cover only a handful of `forever`'s sessions.

**Cross-tab leak investigation.** A review of the context-guru main-thread
`waiting_for` x `wake_cause` cross-tab found 3 leaks (cells that should be
rare but weren't); each was traced to a specific real session, timestamp,
and transcript entry before fixing (see the comments at each fix site in
`wait_causes.py`, and the dedicated regression session in `--self-test`):

| Leak | Before | After | Root cause |
|---|---|---|---|
| `waiting_for=human` woken by `tool_result` | 15 | 0 | `AskUserQuestion`/`ExitPlanMode`'s human answer travels back as a bare `tool_result` — `wake_cause` needs the same exception `waiting_for` already has. |
| `waiting_for=human` woken by `background` | 29 | 0 | Mostly the ordering bug below; the residual cases were a Monitor's `persistent`/`timeout <ms>` detail forms (unhandled regex), a Monitor's own mid-watch event wrongly treated as a finish, and a background Skill launch's id (`toolUseResult.agentId`, not in the text at all). |
| `waiting_for=human` woken by `teammate` | 45 | 11 | Mostly the ordering bug below. All 11 residual cases were checked by hand against every `SendMessage` sent earlier in that session: none had an outstanding, unanswered send at the time the reply arrived — each is a teammate (or another Claude session, several literally named after other work this script's own PR went through) volunteering a follow-up with no corresponding ask, which a transcript cannot predict. |

The dominant root cause (most of the drop in all 3 rows): `waiting_for`
was computed lazily — only when the NEXT call was flushed — by which point
every entry between the two calls, including the very teammate reply or
task notification about to END the gap, had already been applied to the
tracked open-tasks/open-teammates state. That reliably made a background
task or teammate wait that was genuinely open at the START of the gap look
already closed. Fixed by snapshotting `waiting_for` the moment each call
finishes, before anything later can mutate the tracker.

**Policy comparison, stated plainly**: `forever`'s time-ordered test
split has only 14 gaps — far too few to call its policy comparison a
result in any direction; it proves nothing and is reported with a
CAVEAT line, not a tie or a win.

**On `context-guru` (463 test gaps, the only split large enough to mean
anything), the 3 learned policies cluster closely — hierarchical
0.565x/gap, `waiting_for`-only 0.556x/gap, global best N=5 0.544x/gap —
and all 3 beat today's fixed 2 pings (0.580x/gap) by roughly 3-6%. The
best of the three is the single global N, not the per-tool hierarchical
policy: this data does not show that per-tool learning beats one global
N.** A 5-random-session-split check (a different, non-time-ordered split
rule, used only for this robustness check) confirms the ranking is not
an artifact of one split: across 5 splits, global best N has the lowest
mean on both projects (context-guru: 0.590x vs. hierarchical's 0.614x
and `waiting_for`-only's 0.613x; `forever`: 0.517x vs. 0.629x for both —
`forever`'s spread is wide on every policy, 0.35-0.88x, reflecting how
few gaps even a random half of its sessions has).

**Why**: inspecting which test gaps cost the most under the hierarchical
policy relative to global-N shows one mechanism dominating. `Bash`
qualifies for its own learned budget (52 training waits), and that
budget is tuned to `Bash`'s own typical wait, which is almost always
under a minute — so its learned N is small (N=2 on this split). A handful
of `Bash` waits in the TEST set run 15-18 minutes (a rare outlier for
that specific tool), and N=2's window (14.3 min) misses them, costing
1.35x each. The pooled global N=5 (learned by pooling every cause
together, so it has to also cover the longer waits from `human` and
`background`) has a wider window (28.4 min) that happens to catch these
same outliers. The same pattern repeats for the `teammate` group: its
own learned N=3 (window 19 min) misses several 20-28 minute test gaps
that the global N=5's wider window catches. In short: fitting a smaller,
cause-specific N to a cause's typical (short) behavior makes it
systematically worse on that SAME cause's rare long tail than one
coarser number learned across everything — the training population for
`Bash`/`teammate` individually is dominated by short waits, so the
learned per-cause N optimizes for the common case at the expense of the
rare expensive one, while the global N, forced to also fit the
inherently-longer `human`/`background` populations, ends up accidentally
well-suited to those same long tails.

**Ping-row counting, before and after the fix.** On both local DBs
(the same 25 sessions that cover any real pings): 360 real ping rows,
read directly. Of those, DB-only mode's `(session_id, model)` grouping
attributes 305 to a measured gap and reports 55 as pings for sessions
that did not return ($2.83 of the $25.53 total). The earlier version —
reading `keepalive_pings` off the next real row — would have reported
whatever that column's own count happened to add up to on this data,
with no way to see the orphaned 55 at all. Transcript mode's exact
per-thread gaps attribute more precisely still: 256 pings fall inside a
measured gap, 104 do not.

## `turn_gate.py`: would a "no ping before turn K" gate lose value?

A second, smaller script, built on `wait_causes.py`'s parser, DB reader and
cost model. It answers one question: by the TURN a pause falls on (1-based
call index within its thread — turn 1 is the thread's first call), what did
the current keep-alive gates ping and not ping, and would refusing to ping
before turn 5 or turn 10 win or lose money?

```bash
# Transcript mode, with a DB for prices and the real-ping attribution:
python3 -I scripts/analysis/turn_gate.py \
  --projects-dir ~/.claude/projects/-Users-you-git-your-repo \
  --db ~/.local/state/context-guru/dashboard-8789.db

# Production proxy: DB only, no transcripts (approximate thread proxy, same
# grouping choice as wait_causes.py's db-only mode: by `thread_id` where the
# DB has that column, else by (session_id, model)).
python3 -I scripts/analysis/turn_gate.py --db /path/to/dashboard.db

# Override the keep-alive gates (defaults are issue #444's shipped values):
python3 -I scripts/analysis/turn_gate.py --db /path/to/dashboard.db \
  --pings 2 --prefix-floor 80000 --idle-sec 270 --ping-ceiling-usd 0.50

python3 -I scripts/analysis/turn_gate.py --self-test
```

A pause is simulated (costed and priced) only if it passes all 3 of the
keeper's live gates: the prefix is at least `--prefix-floor` tokens, the
pause has already run `--idle-sec` before a ping would fire at all, and one
ping's own USD cost does not exceed `--ping-ceiling-usd`. A pause that fails
any gate is still counted (`pauses`), just with zero simulated ping cost and
zero avoided-miss value — so the report also shows how much traffic never
reaches a ping decision at all, separate from how much reaches one and is
then declined.

Reports, by turn bucket (`1`, `2-4`, `5-9`, `10+`):

- `pauses` / `eligible_pauses`: every qualifying pause, and how many of
  those the 3 gates would actually have let a ping fire for.
- `ends` / `eligible_ends`: a thread's LAST call, once it has sat idle 20+
  minutes — a thread the keeper is very likely still pinging for nothing,
  since it is not coming back. No miss is ever avoided here; it is pure
  waste when it fires.
- `ping_cost_x` / `avoided_x` / `net_x` and the same 3 in USD: the
  simulated ping spend, the miss-premium value it would have avoided (net
  of the ping spend itself), and the difference, same `sim_cost_x` cost
  model as `wait_causes.py`.
- `real_pings` / `real_ping_cost_usd`: REAL keep-alive ping rows
  (`keepalive = 1`) attributed to the pause they actually fell inside,
  priced from their own real `cost_usd` — not simulated, and read the same
  way `wait_causes.py`'s `DBIndex` does (directly, by timestamp range, not
  from a later row's `keepalive_pings` column). Orphan pings (a session
  that never returned) are reported separately, same as `wait_causes.py`.
- `gate_vs_no_gate`: for a gate at turn 5 and at turn 10, the net change
  (x and USD) from refusing to ping every turn below that cutoff, against
  today's "ping from turn 1" baseline. **Positive means the gate would have
  saved money** (today's early pinging is a net loss on that traffic);
  negative means it would give up value pinging currently captures.

In DB-only mode, pings (`keepalive = 1`) and failed/non-200 rows are
excluded from the turn sequence before indexing, same as `wait_causes.py`'s
own DB-only mode — a ping never had its own hit/miss and must not become a
turn boundary.
