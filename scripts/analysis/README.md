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
before trusting any regex here — formats can and do change):

| | Start | Finish |
|---|---|---|
| Background Bash (`run_in_background`) | tool_result text: `running in background with ID: X.` | `<task-notification><task-id>X</task-id>` |
| `Monitor` | tool_result text: `Monitor started (task X, expires in <dur>...)` — the duration is parsed and tracked as an absolute expiry | `<task-notification><task-id>X</task-id>`, OR the stated expiry elapsing (checked against the gap-start call's own timestamp) |
| Async agent (`Agent` tool, `run_in_background: true`) | tool_result text: `Async agent launched successfully... agentId: X` | **`<agent-message from="X">`** (a subagent hand-back notice) — **not** a `<task-notification>`; this is the one place a background task's finish signal differs by how it started, found by checking real transcript text rather than assuming all three use the same finish shape |
| Any of the above | — | A `TaskStop(task_id=X)` tool call, matched structurally on the tool name and its JSON `task_id` field, not by regex |
| A teammate wait (`SendMessage` tool_use) | matched structurally on the tool name and its JSON `to` field (not by regex) | `<teammate-message teammate_id="X">` or `<cross-session-message from-name="X">`, matched by regex since these only ever appear as plain text |

A background task with no stated expiry (a backgrounded Bash command, or an
async agent) stays open until an explicit finish signal; one with a stated
expiry (a `Monitor`) is also treated as closed once that expiry has passed,
even with no notification yet — the tool's own description says a Monitor
may expire silently in rare cases, and treating "open forever" as the
default for an expiring task is exactly the bug issue #424 flagged in its
own rough detector (see Validation).

## Learning a ping budget per tool (issue #424 section 4)

With `--projects-dir`, for `main` and `team_thread` threads (not plain
`subagent`, per the task), the report also includes:

- **Per-tool wait stats**, sorted by call count: `call_count` (every
  `tool_use` block anywhere, not just a response's last one — a plain
  usage-frequency count), `wait_count` (the narrower population below),
  median/p75/p90 wait and the share over 5 minutes (over **every**
  consecutive call pair whose response's **last** `tool_use` named this
  tool — no `GAP_THRESHOLD_MIN` floor here, since the distribution's own
  shape, including the short waits, is what gets learned), and `best_n`
  (the N with the lowest simulated cost, but computed only over this
  tool's waits that *are* at least `GAP_THRESHOLD_MIN` — a sub-5-minute
  wait never needed a ping to begin with).
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

The DB has no thread id (that is issue #423's job), so three things —
the mislabel check, the OBSERVED column's real-ping lookup (`DBIndex`), and
nothing else — all match a transcript call to a DB row the same way: same
`session_id`, same `cache_read` token count, and the nearest **approximate
response time** within `MATCH_WINDOW_SEC` (15 seconds).

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
  rule and the same real `keepalive_pings` as transcript mode now (not the
  `cache_miss_reason` label, which depends on the ttl_expiry/prefix_change
  distinction issue #423 is still fixing — a production number built on
  that label would silently inherit whatever #423 has not yet corrected).
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
