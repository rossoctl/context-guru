# wait_causes.py

Measures why a thread's keep-alive matters: wait causes, gap distributions,
cache-miss cost, and a ping-budget simulation. Built for
[issue #424](https://github.com/rossoctl/context-guru/issues/424).

Python 3 standard library only. Always run it with `python3 -I` (isolated
mode — skips the user site directory and `PYTHONPATH`, so a stray file next
to the data cannot get imported).

## Modes

**Transcript mode** (`--projects-dir`, optionally with `--db`): reads Claude
Code session transcripts. Each transcript file is one thread — the main
session file, plus `<session>/subagents/*.jsonl` for each subagent. This
gives exact per-thread gaps and the exact signal that ended each one
(teammate message, background task, human text, the agent's own tool
result, or a retry after a failed call).

**DB-only mode** (`--db` only, no `--projects-dir`): this is how the script
must run against the production proxy, which has no transcripts. There is
no thread id and no wake cause here. The script says so and groups rows by
`(session_id, model)` as an approximate thread proxy, then reports gap
distributions, miss cost, the ping simulation (cause = unknown), and
failure stats.

## Usage

```bash
# One project, with its dashboard DB for the mislabel/failure cross-check:
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
```

By default it prints a human-readable summary; `--json` prints the full
machine-readable report instead.

## What it reports

Per project, per thread kind (`main` / `subagent`), per wake cause:

- **Gap distribution**: count, median, p75, p90, max, in minutes, for gaps
  of at least 4.67 minutes (the keep-alive ping interval from issue #424 —
  below that, nothing would have needed a ping anyway).
- **Miss cost today (0 pings)**: average cost per gap in units of `x` (the
  gap-ending call's own base input price), and the total in USD, using a
  small hardcoded per-model price table (see `MODEL_BASE_INPUT_PRICE_PER_TOKEN`
  in the script — not pulled from a live pricing source, said plainly in the
  code).
- **Ping-budget simulation**: average cost per gap, in `x` units, for N = 0
  to 12 pings, using the formula from the issue's technical notes (a ping
  every 4.67 min; N pings keep the cache warm for `4.67N + 5` minutes; a gap
  that ends inside that window costs `0.1 * floor(gap / 4.67)`, otherwise
  `0.1*N + 1.15`). Reports the best N and its cost.
- **Cold starts**: a thread's first call, counted and costed separately —
  not a "wait", so excluded from gap distributions.

With `--db`, also:

- **Mislabel check**: how many DB `prefix_change` rows are really a
  per-thread TTL expiry or cold start, matched to a transcript event by
  session id, matching `cache_read` token count, and nearest timestamp
  within 60 seconds (the DB's `ts` is the request's *start*; the transcript
  only has the response's *arrival* time, so some skew is expected — see
  `MISLABEL_MATCH_WINDOW_SEC` in the script).
- **Failures**: non-200 upstream rows, their hang time (median/max, and by
  status code), and how many cache misses follow a run of consecutive
  failures on the same session.

## Known approximations (read before trusting a number)

- **Gap timing uses response-arrival timestamps**, not request-start
  timestamps, because that is all a transcript has. For a normal call this
  differs by seconds; for a call that hung for minutes before failing (the
  502s), the *following* gap's start point is a few minutes later than the
  true idle start. This is why failed calls get their own `retry` cause
  instead of being folded into the gap they distort.
- **The mislabel check and the failure stats run over the whole DB**, not
  filtered to one thread, because the DB has no thread id (that is issue
  #423's job). A DB row is attributed to whichever transcript event matches
  closest; two different threads' events landing within the same 60-second
  window could in principle be confused. In practice the matched deltas on
  the validation session cluster tightly under 65 seconds and then jump, so
  the window is a clean cutoff there.
- **"Misses following a failure" counts once per run of consecutive
  failing rows** on a session (several 5xx rows in a row are usually one
  underlying hang, retried fast), checked against whichever thread's row
  happens to come next for that session — this can overcount versus a
  hand-filtered, main-thread-only count, since any thread's traffic on the
  same session can land in between.
- **DB-only mode's `(session_id, model)` grouping is not a thread.** Two
  different subagents on the same model look like one thread; a main
  thread that changes model mid-session looks like several. Treat its gap
  counts as directional, not exact.
- **Model prices are hardcoded**, not derived from the DB's own
  `cost_usd`/`cache_write` columns. A future pass could derive them live
  where the DB covers the model.

## Validation

Run against the known session `6c456ae0-469f-44a1-92c1-f6ea03192715`
(`fix-summarizer`, context-guru project, Oct 6-8) and cross-checked against
the issue's own rough pass (which used opus DB rows as a proxy for the main
thread, since it had no thread id either):

- Main-thread gaps by cause (exact, this script): 11 background / 29 human /
  24 teammate / 7 tool_result / 1 retry. The issue's rough pass: 10 / 25 / 21
  / 22 (no retry bucket). Totals are close (72 vs 78); the shift from
  tool_result to human/teammate is expected, because the rough pass
  attributed every opus DB row with no thread id to "the main thread",
  which also swept in some subagent/fork calls that this script correctly
  separates out as their own threads.
- Mislabel check: 61/70 `prefix_change` rows reclassified, $82.41. The
  issue's rough pass: 46/65, $70.27. Same conclusion (most `prefix_change`
  rows on this session are mislabeled TTL expiries), higher count here
  because this script checks every thread's events, not just the main one.
- Upstream 502s: median hang 305 seconds (5.1 min), matching the issue's
  "5.3-6.0 min" range.
