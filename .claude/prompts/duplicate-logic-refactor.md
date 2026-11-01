# Prompt: find and fix duplicate logic

Paste the prompt below into a new Claude Code session at the repo root. It runs in two phases. In
phase 1 the session reports and changes nothing. Phase 2 starts only when you say which findings to
fix.

This file comes from the 2026-11-01 run: 12 numbered findings and 1 smaller one, 11 PRs, and 2
issues for discussion. The lessons below come from
that run's reviews. Update this file when a later run teaches something new.

---

## The prompt

```text
Your task: find places in this repo where the SAME concern is solved more than once, and report
them to me. Do not fix anything until I say which ones to fix.

## The pattern we hunt

One rule about how a client or a provider behaves is coded in several places. Each copy handles
the variation that its author met first. The gaps do not show up as errors. They show up as silent
cache misses, lost reuse, numbers that disagree between pages, or a provider that one copy forgets.

Past examples. Read the PRs to see the shape of a good fix:
- "Is this the same message as last turn?" was answered 4 ways. Each copy stripped cache_control,
  but none treated a string content and a one-text-block list as the same. Fix: one canonical form
  in schema/canon.go (#783, PR #787).
- "Where does a cache breakpoint go, and is there a free slot?" was answered 3 ways. Fix: one
  placer in apply/breakpoint.go (#784, PR #786; and #795, PR #796).
- The 2026-11-01 run found 12 more. The list is in "Already done" below. Do not report them again
  unless a new copy appeared.

## Where to look

- Functions that normalize, hash, compare or fingerprint messages, bodies, tools or spans.
- More than one place that counts or places cache_control or cachePoint, or that knows a provider
  limit: 4 breakpoints, the 20-block lookback, TTL rules, the minimum cacheable prefix.
- More than one place that parses the same client format: Claude Code's deferred-tools text, the
  skills listing, system reminders, tool_result shapes (string or block array), and the Anthropic,
  OpenAI chat and Responses dialects.
- Lists of providers or model families written out by hand (a `switch` on provider names, or
  `strings.Contains(model, "sonnet")`).
- Prices, multipliers, lifetimes and ping times written as literals (0.1, 1.25, 2.0, 300, 3600,
  280, 240, 3360, len/4).
- SQL that repeats a Go rule (a `CASE ... 3600 ELSE 300`).
- A guard that one caller has and its sibling does not: empty block, non-object block, a model gate,
  a fail-open check.
- A comment like "same as X, but ...", "mirrors X", "kept in step with X", or "this package cannot
  import X". A test that exists only to keep two copies equal.

## How to work in phase 1

1. Make a read-only worktree from origin/main. Do not touch the main checkout.
2. Grep for the signs above. Use 2 or 3 Explore subagents for the wide sweeps, each with a narrow
   scope. Give them the pattern and the examples, not your guesses.
3. Read the code of every location yourself before you report it. A subagent's claim is a lead,
   not a fact. Mark each point as checked by you or checked by a subagent only.
4. For each cluster, report:
   - the concern, in one line;
   - every location, as file:line and function name;
   - how the copies DIFFER: what one handles and another misses, with the code difference;
   - the effect: a wrong value, a miss, or none yet.
5. Split the report into:
   - "duplicates with a confirmed gap": a copy gives a wrong result today;
   - "duplicates with no confirmed wrong result yet".
6. Mark the clusters that change money, prices, ping times or matching. Those are behavior changes.
   A human developer must decide them. I may want an issue for the code owner instead of a fix.
7. Stop and wait for me.

## Phase 2, when I say which to fix

- Open the issues I ask for (for discussion items, assign the person I name).
- Start one general-purpose subagent per finding. Each gets its own worktree and branch from
  origin/main, opens one issue and one PR, and runs the Go tests where my global CLAUDE.md says. It does not run reviews
  and does not merge.
- Write one shared brief file for all agents, plus one short brief per finding. Agents read the
  files. The shared brief holds the git rules, the repo rules, the writing style, and the test-machine
  rules from my global CLAUDE.md. Never commit those rules to this repo.
- Where two findings touch the same file, give each agent a list of functions that belong to the
  other agent.
- When an agent reports, check its PR yourself (files, trailers, behavior-change section) before
  you run the offload-review skill on it in the main session.
- Fix every reviewer finding and every non-blocking note in the same PR. Do not open follow-up
  issues for them.

Follow the lessons in .claude/prompts/duplicate-logic-refactor.md, "Lessons from review". Read
the file from origin/main.
```

---

## Lessons from review

The reviewers of the 2026-11-01 run found these. Put them in every fix agent's brief.

### The fix can repeat the bug

- **A rule added in one reader only is a new duplicate.** PR #819 hid stale rows in the narrow SQL
  path. At 250 or more sessions the report reads a different path, so the bug came back at
  production size. Write the rule once, in Go, where the facts are built, and call it from every
  reader.
- **A size or a count can switch the code path.** Look for thresholds like `declScanMinSessions`.
  Each test must force every path.
- **A shared helper must keep the strictest guard of all its callers.** List each caller's guards
  before you merge them. PR #815 dropped the GPT-5.6 model gate that the keeper had, so gpt-4o
  read as a 30-minute cache.
- **A shared helper can widen what a caller accepts.** PR #807 moved `claudeModel` to `IsClaude`.
  That also accepted any id with "anthropic" in it, a change nobody named. Pin the old answer for
  ids at the edge, in a test.
- **Check that each caller has the same need** before you merge two copies. Some differ on
  purpose: requested tier against billed tier, or a cheap hash on the hot path against a full
  canonical form. Keep those apart, and say why in a comment.
- **A compatibility fallback for stored values must cover every form the new rule merges.** Find
  out if stored keys or hashes change, and if there is a version field.

### Cost

- **The hot path must not get slower.** PR #813's prefilter passed every body with `\u`. Go's
  `encoding/json` escapes `<`, `>` and `&` (backslash, `u`, then 4 hex digits), so most tool
  outputs held a `\u` and paid the full parse, on a count that was 22% of CPU. PR #822
  decoded and re-encoded a message of up to several hundred KB on every request. Add a benchmark,
  and give the before and after figures in the PR.

### Tests

- **A test that passes on the old code proves nothing.** Run each new test against origin/main, or
  with the fix disabled, and report the failure. PR #822's first-block test passed under the old
  rule until its fixture was changed.
- **A test that checks source text is not a test.** Drive a real request through the test harness.
- **Run the full suite of each touched package**, and of its direct dependents if an exported API
  changed. Run `go vet` and `gofmt -l` at the final head, with no output filter.
- **Find the name of a failing test before you call it flaky.** Keep the full output in a file. Then
  run the same test on an unmodified origin/main.

### The PR text

- **The behavior-change section must match the code exactly.** Reviewers blocked when a PR said
  "Anthropic values do not change" and one did. They also flagged a PR that claimed a change the
  write path can never produce.
- **Name every behavior change at the top:** who it affects, the old and new behavior, and how to
  turn it off. A refactor that adds a provider, a model or a form to a list is a behavior change.
- **A finding can be wrong, or need no code change.** Finding 12 ended as comments only (#800).
  Finding 9's expected effect did not exist, because the importer had no caller. Report that with
  the evidence. Do not invent work.

### Working with agents

- **Messages cross.** Before you act on an agent's report, read the PR head and body from the
  server. Two agents answered an older message, and one reported "done" for work it had listed as a
  follow-up.
- **Repeat open items.** When you send a second message, list every item still open. An agent may
  act on only the first message.
- **Rebase after main moves.** A rebase moves the head, so a PR with a real conflict needs one more
  review round at the new head.

---

## Already done (2026-11-01 run)

Rows are findings that were reported and got an issue or a PR. A row is not proof that the fix
merged. If a PR in this table closes without a merge, delete its row, so the next run reports that
duplicate again.

| # | Concern | Result |
|---|---|---|
| 1 | Providers that accept cache_control (Mantle was missing) | #809, PR #811 |
| 2 | Wire dialect decided in about 5 places | #817, PR #819 |
| 3 | tool_result text and string-or-blocks content | #802, PR #803 |
| 4 | Model family checks; none knew `fable` | #806, PR #807 |
| 5 | Keep-alive ping time defined twice (56 vs 59 min, 240 vs 280 s) | #808, PR #810; the values wait for a decision in #808 |
| 6 | Cache lifetime (5m / 1h / OpenAI 30m) in about 7 places | #814, PR #815 |
| 7 | Breakpoint cap and count | #812, PR #813 |
| 8 | Skills listing and deferred-tools parsing (and #788) | #821, PR #822 |
| 9 | Provider usage parsing | #816, PR #818 |
| 10 | Cache price multiples in about 8 places | #797, for discussion |
| 11 | The bytes/4 token estimate in 5 copies | #798, for discussion |
| 12 | Per-message digests outside the canonical form | #799, PR #800 (comments only) |
| - | Bifrost session id ignored metadata | #804, PR #805 |
