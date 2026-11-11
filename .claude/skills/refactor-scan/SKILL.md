---
name: refactor-scan
description: Find places in this repo where one concern is solved more than once (duplicate logic), report them, and fix only the ones the user picks, with one agent and one PR each. Use when asked to find duplicate logic, hunt for refactors, or run a refactor scan.
---

# Refactor scan

You find duplicate logic in this repo, report it, and then fix only what the user picks. The work
has two phases. In phase 1 you report and change nothing. Phase 2 starts only when the user says
which findings to fix.

Read these two files first. They are part of this skill:
- [lessons.md](lessons.md): the mistakes that reviewers found in earlier fixes. Every fix agent's
  brief includes this file.
- [done.md](done.md): findings from earlier runs. Do not report them again.

Run this skill in the main session. Phase 2 starts agents and runs the `offload-review` skill, and
only the main session can wait for both.

## The pattern you hunt

One rule about how a client or a provider behaves is coded in several places. Each copy handles
the variation that its author met first. The gaps do not show up as errors. They show up as silent
cache misses, lost reuse, numbers that disagree between pages, or a provider that one copy forgets.

Past examples. Read these PRs to see the shape of a good fix:
- "Is this the same message as last turn?" had 4 answers. Each copy stripped `cache_control`, but
  none treated a string content and a one-text-block list as the same. The fix is one canonical
  form in `schema/canon.go` (#783, PR #787).
- "Where does a cache breakpoint go, and is there a free slot?" had 3 answers. The fix is one
  placer in `apply/breakpoint.go` (#784, PR #786; #795, PR #796).

## Where to look

- Functions that normalize, hash, compare or fingerprint messages, bodies, tools or spans.
- More than one place that counts or places `cache_control` or `cachePoint`, or that knows a
  provider limit: 4 breakpoints, the 20-block lookback, TTL rules, the minimum cacheable prefix.
- More than one place that parses the same client format: Claude Code's deferred-tools text, the
  skills listing, system reminders, tool_result shapes (string or block array), and the Anthropic,
  OpenAI chat and Responses dialects.
- Lists of providers or model families written out by hand: a `switch` on provider names, or
  `strings.Contains(model, "sonnet")`.
- Prices, multipliers, lifetimes and ping times written as literals: 0.1, 1.25, 2.0, 300, 3600,
  280, 240, 3360, `len/4`.
- SQL that repeats a Go rule, such as a `CASE ... 3600 ELSE 300`.
- A guard that one caller has and its sibling does not: an empty block, a non-object block, a model
  gate, a fail-open check.
- A comment like "same as X, but ...", "mirrors X", "kept in step with X", or "this package cannot
  import X". A test that exists only to keep two copies equal.

## Phase 1: find and report

1. Make a read-only worktree from `origin/main`. Do not touch the main checkout.
2. Grep for the signs above. For the wide sweeps, use 2 or 3 Explore subagents, each with a narrow
   scope. Give them the pattern and the examples, not your guesses.
3. Read the code of every location yourself before you report it. A subagent's claim is a lead,
   not a fact. Mark each point as checked by you, or checked by a subagent only.
4. For each cluster, report:
   - the concern, in one line;
   - every location, as `file:line` and function name;
   - how the copies DIFFER: what one handles and another misses, with the code difference;
   - the effect: a wrong value, a miss, or none yet.
5. Split the report into two groups:
   - duplicates with a confirmed gap: a copy gives a wrong result today;
   - duplicates with no confirmed wrong result yet.
6. Mark each cluster that changes money, prices, ping times or matching. That is a behavior change,
   and a human developer must decide it. The user may want an issue for the code owner, not a fix.
7. Stop and wait for the user.

## Phase 2: fix what the user picks

1. Open the issues the user asks for. For a discussion item, assign the person the user names.
2. Write one shared brief file for all agents, in `/tmp/<run>/common.md`. It holds:
   - the git rules: own worktree and branch, `git commit -s`, `Assisted-By` and never
     `Co-Authored-By`, prose in files and never inline in a shell command;
   - the repo rules from `CLAUDE.md`: no library imports `proxy`, fail open, a behavior change
     named at the top of the PR, data dependence stated;
   - the writing style from `CLAUDE.md`;
   - the test-machine rules from the user's global `CLAUDE.md`. Never commit those rules to this
     repo;
   - [lessons.md](lessons.md).
3. Write one short brief per finding: the concern, every location, how the copies differ, and the
   fix shape.
4. Start one general-purpose subagent per finding. Each agent:
   - works in its own worktree and branch from `origin/main`;
   - confirms the finding first, and stops with the evidence if it is wrong;
   - opens one issue and one PR;
   - runs the Go tests where the user's global `CLAUDE.md` says;
   - does not run reviews, and does not merge.
5. Where two findings touch the same file, give each agent the list of functions that belong to the
   other agent.
6. When an agent reports, check its PR yourself before you review it: the changed files, the commit
   trailers, the behavior-change section, and the test runs. Then run the `offload-review` skill on
   it.
7. Fix every reviewer finding and every non-blocking note in the same PR. Do not open follow-up
   issues for them.
8. When main moves, rebase each PR. Compare the PR's own `+`/`-` lines before and after the rebase.
   If they are the same, the approvals still hold. If they differ, run one more review round.
   Also run one more round if main gained new code in the same files: a new copy of the rule
   the PR unifies can land there, and CI does not catch that.
9. Send each PR to the release integrator only when it has green CI and 2 approvals at its final
   head. The user's yes for each behavior change must be in the integrator's own session.

## After the run

1. Add this run's findings to [done.md](done.md).
2. Add each new kind of mistake that reviewers found to [lessons.md](lessons.md).
3. Open a PR with both changes.
