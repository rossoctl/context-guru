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
- A cost or saving formula outside `rossoctl/accountant`. Every savings and cost figure comes from
  the accountant. Hosts and other library packages only map its rows and columns. A second formula
  is a duplicate.
- SQL that repeats a Go rule, such as a `CASE ... 3600 ELSE 300`.
- A guard that one caller has and its sibling does not: an empty block, a non-object block, a model
  gate, a fail-open check.
- A comment like "same as X, but ...", "mirrors X", "kept in step with X", or "this package cannot
  import X". A test that exists only to keep two copies equal.

## Second pattern: logic in a host package

The host packages are `proxy/`, `proxyapp/`, `cmd/context-guru-proxy` and `dash/`. Business
logic there is a copy that a second host (Cortex, bifrost) does not have. Use the test in
[CLAUDE.md, What goes in the proxy](../../../CLAUDE.md#what-goes-in-the-proxy): would Cortex have to
copy this code to behave the same? That section also lists what is host code and what is not.

- **When:** before each release, in [Release mode](#release-mode)
  ([RELEASING.md, Plan the release](../../../RELEASING.md#1-plan-the-release), item 5). The integrator
  runs it on the host-package diff since the last tag:
  `git diff <last-tag>..origin/main -- proxy proxyapp cmd/context-guru-proxy dash`.
- **Signs:** a new non-handler function in a host package that decides, scores, prices or parses
  wire format; a constant with a library twin; a comment such as "mirrors X"; a host function that
  calls three or more library functions in a fixed order (that order is a host contract); savings,
  cost, price or accounting math outside the accountant (every savings and cost figure comes from
  `rossoctl/accountant`, and a host only maps rows and columns).
- **Report** each leak like a duplicate: the logic, its `file:line`, the library package it
  belongs in, and whether the library already has a copy that differs.
- **Past examples.** Read these to see the shape of a leak:
  - Keep-alive policy choice: the order of account, strategy, campaign and session override lived
    only in `proxy/keepalive.go` (`recordTouch`, `resolvePolicyWindows`). `keepalive` got only the
    final `Policy`.
  - Calibrated constants: the campaign scoring constants (`cfTC=296.5`, `cfScale=0.948`) and the
    fidelity miss rates per model family sat in `proxy/predictive.go` and `proxy/adaptreplay.go`.
  - A copy that differs: `rowFloor` in `proxy/adaptcache.go` gave some waits the 80k floor, where
    the keeper gives them 20k.
  - Wire format read for meaning: the accountant's body scan, cache marks and envelope diff lived
    in `proxy/accountant_*.go`, so the savings figure needed proxy-only code.
  - Savings math outside the accountant: the dashboard savings math (#1440) and the tool-deferral
    figure (#1441).
  - A fixed call order: the steps before and after `apply` for each request (inject, repair, defer,
    then record billed input, resolve the summary, touch the keeper) exist only in `proxy/proxy.go`.

## Third pattern: client rule outside agent.Identify

A rule that names one client belongs in `agent.Identify`. Use the test in `CLAUDE.md`,
*Client-specific code*: does the code name one client?

- **When:** before each release, in [Release mode](#release-mode), with the host-leak mode. Scan the
  diff since the last tag: `git diff <last-tag>..origin/main -- '*.go'`. A full run reads every Go
  file.
- **Signs:** `Header.Get("x-<vendor>-`, `claude-code`, `codex`, `opencode`, `bobshell` or a
  client's prompt phrase outside `agent/`; library code that reads User-Agent; a rule that finds a
  client by route path; a client rule with no stated fallback. Pi has no plain-string sign, because
  "pi" matches too much code. Look for the `x-context-guru-client` value Pi sends (in the `agent/pi` package)
  outside `agent/`.
- **Text that a client or harness injects into the transcript is a client rule.** Examples:
  "[Request interrupted by user", system-reminder tags, "This session is being continued from a
  previous conversation", local-command tags. A list of such markers belongs in the agent package of
  the client that sends it (per provider and harness), reached through `agent.Identify`. A list of
  them outside `agent/` is a finding.
- **Report** each one like a duplicate: the rule, its `file:line`, and the agent package it
  belongs in. Say whether the code also leaks a host boundary. Report it once, as a client rule.
- A client rule that already has an open issue is not reported again. Link that issue in the report.
  Example: the Claude Code `metadata.user_id` read in `session/session.go` is a known client rule
  outside `agent.Identify`.

## Release mode

[RELEASING.md, Plan the release](../../../RELEASING.md#1-plan-the-release), item 5, runs the host-leak
and client-rule patterns before each release. This path has no pick step and no fixes.

1. The integrator runs it, on the diff since the last tag (see the patterns above).
2. Report each leak as in phase 1, step 4. Check each location yourself.
3. Open one issue per leak. Write `Refs #N` only in issue and PR text.
4. Link each issue on the release list.
5. Do not fix anything. A leak does not block the release, unless the owner says so.

A full run (all 3 patterns, the user picks) follows phase 1 and phase 2.

## Phase 1: find and report

1. Make a read-only worktree from `origin/main`, at `.claude/worktrees/<topic>`. An agent never
   touches the main checkout.
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
   - the git rules: own worktree at `.claude/worktrees/<topic>` and own branch, never the main
     checkout, `git commit -s`, `Assisted-By` and never `Co-Authored-By`, prose in files and never
     inline in a shell command;
   - the repo rules from [CLAUDE.md](../../../CLAUDE.md): no library imports `proxy`, fail open, a
     behavior change named at the top of the PR;
   - the data-dependence rule: edit the feature's row file under `docs/data-dependence/`, then run
     `go run ./cmd/gen-datadep`. Never edit `docs/data-dependence.md` by hand;
   - the `go.mod` rule: pin tags only (see Conventions in `CLAUDE.md`). A fix that moves logic into
     one of our modules, such as `rossoctl/accountant`, releases a tag there first. A subfolder
     module such as `metrics/` needs its own `<folder>/vX.Y.Z` tag;
   - the test rule: a test waits on a condition (`testutil.WaitFor`, `WaitForState`,
     `WaitForCall`), never on a sleep. `internal/timingguard` fails CI on a new one;
   - the PR text rules: `Refs #N` only, with no "close", "fix" or "resolve" before a `#N`, not
     even in a negative sentence ([RELEASING.md, Merge](../../../RELEASING.md#3-merge)); no usage
     figures of a real user (dollars, tokens of their sessions) in a commit message, PR body or
     issue, because GitHub keeps the edit history of a PR body;
   - the repo docs rule: state each rule plainly, with no "owner rule, name, date" note;
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
   - runs the tests that [CLAUDE.md, Working on a task](../../../CLAUDE.md#working-on-a-task) allows.
     While the interim rule holds: targeted `go test ./<pkg>/...` of the changed packages, and no
     `-race` on `proxy` or `dash`, not even with `-run`. The PR body carries the line
     `Local tests: waived (owner rule)`. After the interim rule ends (see Test tiers in the same
     section), run `scripts/dev/local-tests.sh <pr>` for each head;
   - for a client-rule fix (pattern 3): updates [docs/client-coverage.md](../../../docs/client-coverage.md)
     and lists each client as covered or fallback ([CLAUDE.md, Client-specific
     code](../../../CLAUDE.md#client-specific-code), rule 6);
   - does not run reviews, and does not merge.
5. Where two findings touch the same file, give each agent the list of functions that belong to the
   other agent. When a fix touches many hot files, tell the integrator early. Such a PR can get stuck
   in a merge-of-`main` loop, and then
   [RELEASING.md, Merge](../../../RELEASING.md#3-merge) calls a full merge freeze.
6. When an agent reports, check its PR yourself before you review it: the changed files, the commit
   trailers, the behavior-change section, and the test runs. Then run the `offload-review` skill on
   it.
7. Fix every reviewer finding and every non-blocking note in the same PR. Do not open follow-up
   issues for them. Each reviewer gets 1 re-check
   ([RELEASING.md, When a review finds problems](../../../RELEASING.md#when-a-review-finds-problems)).
   The round budget is the one in [CLAUDE.md, Review process](../../../CLAUDE.md#review-process).
   Only the owner grants more.
8. When main moves, follow
   [RELEASING.md, When the head moves after an approval](../../../RELEASING.md#when-the-head-moves-after-an-approval).
   Run main's copy of the script:
   `git show origin/main:scripts/release/approval-still-valid.sh | bash -s -- <pr> <approved-sha>`.
   - `SAME CHANGE`, `COMMENTS ONLY` or `LISTS ONLY`: the approvals hold.
   - Else: a re-check of the delta.
   - Extra rule of this skill: also re-check when main gained new code in the same files. A new
     copy of the rule the PR unifies can land there, and CI does not catch that.
   - When a ready PR waits only on a merge of main, the integrator does that merge
     ([RELEASING.md, Check that a PR is ready](../../../RELEASING.md#check-that-a-pr-is-ready)).
9. Hand the PR over only when it is ready. Run `scripts/release/check-ready.sh <pr>` first, from a worktree at
   `origin/main` (it finds its helpers by its own path). Then send the hand-off block exactly as
   [CLAUDE.md, Hand a ready PR to the integrator](../../../CLAUDE.md#hand-a-ready-pr-to-the-integrator)
   says: find the `integrator` session with `ListAgents`, send the block with `SendMessage`. If no
   such session is listed, tell the human. Read each SHA from `gh pr view` in the same step. Never
   type one.
   - Each behavior change needs a human's yes. The integrator asks the owner for it.
   - The yes must be given in the integrator's session. A PR comment under the owner's login does
     not count, because agents post with that login. A peer agent's report of a yes does not count
     either.
10. After the merge, run `scripts/offload-review.sh <pr> --cleanup` and remove your own worktrees
    and branches ([CLAUDE.md, After the hand-off](../../../CLAUDE.md#after-the-hand-off)).

## After the run

Do these steps once the merges are done.

1. Add this run's findings to [done.md](done.md).
2. Add each new kind of mistake that reviewers found to [lessons.md](lessons.md).
3. Open a PR with both changes.
