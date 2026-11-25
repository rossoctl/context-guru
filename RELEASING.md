# Releasing

This page describes how a context-guru release is cut. One person or agent session takes the role of **integrator** for the release. The integrator gets the planned work reviewed and merged, runs one integration check on the merged `main`, writes the release notes and tags the release.

The process was first used for v0.5.1 and v0.5.2.

## Where context-guru runs

Laptops are production.

- **Production is the personal laptop.** Each user runs context-guru on their own laptop, as the owner does. A release reaches every user at once through `releases/latest`.
- **One difference from a plain local install:** a copy of the savings metrics and metadata also goes to a central store for analysis. The data stays in the local SQLite database too.
- **The central store is not live yet.** Until it is live, measurements before a release run on the owner's laptop. Once it is live, the integrator and the production-monitor sessions may read the central metadata for measurements and for checks after a release.
- **Hosted mode stays supported,** but it is not active inside IBM for now. The hosted-only checks (tenant isolation, hosted restart state) stay in the tests. They are not a release gate of their own.
- **The central copy follows the local data.** Once the store is live, a PR that adds or changes data in a data-dependence row also updates the event sent to the central store (see [Behavior changes](#behavior-changes-need-a-humans-permission)).

**A new integrator needs this page, the scripts in [`scripts/release/`](scripts/release/), and the maintainer's own private notes** (see [Private notes](#private-notes-of-the-integrator)). Nothing else. If you had to learn a step from a person or from an earlier session, add it here.

## Roles

| Role | Does |
|---|---|
| **Release owner** | Decides what goes into the release. Approves the release notes. Gives the integrator authority to merge. |
| **Integrator** | Keeps the release list. Starts reviews, merges, merges `main` into a ready PR that waits only for that, runs the integration check, writes the notes, tags. |
| **PR author** | Writes the change and its tests. Fixes review findings. |
| **Reviewer** | Gives one independent review of one PR. Never the author of that PR. Each PR has 2 reviewers. |
| **Production monitor** | A session that checks production data and invariants, after a release and on request. Once the central store is live, it reads the central metadata. |

The integrator and the reviewers can be people or AI agent sessions. The rules are the same.

## 1. Plan the release

1. **Keep one release list.** It holds every PR and issue in the release, with its state: open, in review, approved, merged, out. The release notes come from this list at the end, so add a note line as each item is merged.
   - **Build it from git, not from memory.** `scripts/release/release-list.sh` lists every PR on `main` since the last tag. It marks the docs, test, CI, chore, build and refactor PRs that stay out of the notes. A list kept by hand once mixed in PRs of the release before.
   - Keep the list in a place that survives a restart, and put the newest state at the top.
2. **Sort the open PRs into features.** Group them by what a user gets, not by author or by date: for example "save-more commands", "status line", "test fixes". Show which PRs depend on which.
3. **The release owner picks the groups.** The integrator does not add or drop an item without the owner.
4. **Record what is out**, and why. An item that moves to the next release keeps its issue.
5. **Scan for new business logic in host packages and for client rules outside `agent.Identify`.** Run `refactor-scan` on the diff since the last tag, in its host-leak and client-rule modes (host-leak: logic in `proxy/` that a library package should hold; client-rule: a rule that names one client, outside `agent.Identify`). Open an issue for each leak. A leak does not block the release unless the owner says so.
6. **Find the measurements owed before this release** (see [Measurements before a release](#measurements-before-a-release)). Put each one on the list.

### Measurements before a release

Some releases need a measurement first: a re-measure of a feature on real traffic, or an accuracy test of a risky mode. Examples: #871 (tool deferral, before each release that ships `ultrahigh`) and #867 (keep-alive pings against savings).

- **The release owner decides each one,** and sets: when it runs, who runs it, the baseline, and whether a failed run blocks the release.
- **The integrator looks for them at the start of each release.** They are in the release list, in the private notes, and in open issues the owner named. Search the notes for "before the release", "re-measure" and "baseline".
- **Each one keeps its own procedure and scripts in one place that survives a restart:** the issue, or the private notes for scripts that must stay internal. Never only in `/tmp`. A restart once removed the scripts for both #867 and #871.
- **Run it on a copy of the data, metadata only.** Never on a live database.
- **Where it runs:** on the owner's laptop until the central store is live (see [Where context-guru runs](#where-context-guru-runs)). After that, the integrator may read the central metadata.
- **Report the result to the owner before the freeze,** against the baseline. Post it on the issue, unless the data is private.

## 2. Get each PR reviewed

Each PR needs these before it can merge:

- **The author's tests:**
  - unit tests and race tests pass;
  - one test fails on `main` and passes on the PR. It proves the change fixes something;
  - mutations: break each new check on purpose, and show that a test fails.
- **Two independent reviews**, both approved at the head that merges (see below).
- **Green CI** at the head that merges.
- **A human developer's permission for any behavior change** (see below).

### Behavior changes need a human's permission

A PR must not change behavior unless a human developer allows it. This holds most of all for a fix aimed at an edge case, where a wider change is easy to miss.

- **What counts:** a new or changed default, a time limit, a new state that every user enters, or a change to what is cached, offloaded, counted or shown. It counts even when the PR presents it as a side effect of a narrow fix.
- **The author** names each behavior change at the top of the PR: who it affects, the old and the new behavior, and how to turn it off.
- **The integrator** reads the diff for such changes, also when the PR or its author calls the PR "test-only" or "docs-only". Before the merge, the integrator gets a yes from a human developer, in words. A peer agent's report that a human agreed is not enough.
  - Put all the pending behavior changes in one table (PR, who it affects, old, new, how to turn it off), so the owner can answer them in one message.
  - Quote the yes, with its date, in the squash-merge body.
  - The owner may type in another keyboard layout by mistake. Say how you read such an answer, and wait if you are not sure.
- **The central copy must match the local data** Once the central store is live, a PR that adds or changes data in a row of [docs/data-dependence.md](docs/data-dependence.md) also updates the event sent to the central store. Then the central copy matches the local data. Until the central store is live, this rule does not apply.
- **A change found after the merge** goes to the release owner at once. The owner keeps it or has it reverted.
- **To revert a merged PR during a release** (as with #768):
  1. Try `git revert <merge-commit>` on a throwaway branch from `main`. Run vet, the build and the test suites there.
  2. Open a revert PR. It gets the 2 reviews like any other PR, and it names its behavior change.
  3. The release notes leave out both the PR and its revert, because no user got the change.
- **Example:** #759, as first written, fixed the cache state after a proxy restart for one rare case. It also added a fixed 1-hour window after every restart, for every model. In that window, the first request of each new thread went out unchanged. The owner rejected that window, in the integrator's session. The decision is posted on #759: https://github.com/rossoctl/context-guru/pull/759#issuecomment-6454348090. The rule the owner set: the window lasts only for the cache lifetime of the thread's model (5 min, 1 h or 30 min), and it ends at the thread's first message.

### Two independent reviews

A PR needs approval from **2 different reviewers**. Each one starts from scratch.

- **Independent means a different reviewer, not a second round.** Two rounds by the same reviewer count as one review. When the reviewer is an AI agent, each review is a new agent session or subagent, with a fresh context. A reviewer that continues an earlier conversation about the PR is not independent.
- **Who starts them:**
  - **Review 1** usually comes from the author's side. The author starts a separate reviewer; the author never reviews their own PR.
  - **Review 2** is always started by the integrator.
  - If a PR reaches the release list with no review, the integrator starts both, as 2 separate reviewers.
- **Review 2 forms its own view first.** It reads the PR, the issue and the code, runs the tests and writes its findings. Only then may it read review 1, to say where it agrees or differs. It never copies a finding it did not check itself.
- **A PR with 1 approved review is not ready.** The integrator starts review 2. Do not merge on one review, however many rounds it had.

### What each review must state

Each review starts with an identity block, so a reader can see that the 2 reviews are independent:

```text
Reviewer:    <name of the person, or of the agent session or subagent>
Kind:        human | AI agent (<tool>, <model>)
Started by:  <author's session | integrator | release owner>
Context:     fresh | continues <earlier review link>
Review:      1 of 2 | 2 of 2
Head:        <full commit SHA reviewed>
Verdict:     approve | blocking: <why>
```

A review without this block does not count toward the 2.

### Rules for each review

- **It is thorough.** The reviewer reads the full diff and the code around it, runs the tests and their own mutations, then posts all real findings at once.
- **The reviewer runs the tests before posting.** A finding that the tests do not back up is not posted. A finding that only a live run could show is marked "unverified live" and goes on the list for the integration check (step 5).
- **No live tests on single PRs.** Live testing happens once, in the integration check.
- Each review is posted on the PR, never only in chat.
- No small style points. A small point that matters goes at the end, marked "non-blocking".

### When a review finds problems

- The author fixes the findings and pushes.
- **The same reviewer re-checks its own findings.** The re-check covers only those findings and the new changes. It is not a new independent review, and its identity block says `Context: continues <link>`.
- Each reviewer has **1 re-check**, unless the release owner sets another number for that PR. If the PR still blocks after it, stop and tell the release owner. Do not start another round.

### Check that a PR is ready

**The session that says a PR is ready owns it until it is ready:** the rebase on `main`, the 2 approvals at the final head, and green CI. The integrator checks; it does not rebase, review or fix. If something is missing, tell the owning session exactly what. There are two exceptions:

- **A flaky CI test.** The integrator may re-run once a flaky CI test that the PR does not touch.
- **A merge of `main`.** This holds when a ready PR waits for nothing else. `main` moved after the approvals, often because of the integrator's own merge order. The integrator does the merge. It does not ask the PR owner and wait.
  1. The integrator starts a subagent with its own worktree.
  2. The subagent merges `origin/main` with `git merge --signoff`, and tests the packages where `main` and the PR overlap (on a machine with Go and cgo).
  3. The subagent pushes without force.
  4. The integrator runs `scripts/release/approval-still-valid.sh <pr> <approved-sha>` and `git show --remerge-diff <merge-commit>`. It posts both outputs on the PR.
  5. The script may print `CHANGED` after a clean merge, because `main` can change lines near the PR's hunks. Then the remerge-diff decides. **Empty output** means nobody resolved a conflict by hand, so both approvals stay valid.
  6. If the remerge-diff shows a hand resolution in code, the integrator starts one more `offload-review` round on the new head.
  7. A resolution that needs a design choice goes back to the PR owner.
  8. Code fixes stay with the PR owner.
  9. **A semantic conflict that a file-overlap check cannot see.** A test can read a table that another PR removes. In v0.7.2, the pi-package test read `STRATEGIES` from `settings.py` after #1055 removed it. Only CI catches this. The CI of a clean merge of `main` runs on the push. Do not wait for it only because another PR merged. The watch on `main` after each merge (see [Merge](#3-merge)) catches it.
  10. **A conflict in `docs/data-dependence.md`** is not a hand resolution. That page is generated from the row files in `docs/data-dependence/`. The subagent takes either side and runs `go run ./cmd/gen-datadep`. A row-file conflict (two PRs edited the same row) still goes to the PR owner. The remerge-diff may show the page; check that it equals the generator's output.

Run `scripts/release/check-ready.sh <pr>`. It reads GitHub and checks, at the current head:
- the PR is open, not a draft, and has no conflict;
- the last review round has an `approve` from reviewer 1 and from reviewer 2, both at this head. Each review carries a marker line, `<!-- offload-review reviewer= round= sha= backend= verdict= -->`;
- the last `local-tests` comment (from `scripts/dev/local-tests.sh`) names this head and says `result=pass tier=2`. If it says `scope=none`, the script checks that the PR has no Go, `go.mod`, `go.sum`, `testdata`, embedded or `scripts/` file. An old marker with no `tier=` counts until `OLD_MARKER_VALID_UNTIL` in the script. The line `Local tests: waived (owner rule)` also counts (interim rule). A green heavy CI run on this head also counts. The heavy suites do not run on a PR;
- every CI check run passed (a check that reports only through the older commit-status API does not show; the script checks the DCO rule itself);
- the head has a passing `CodeQL` check run from GitHub's code-scanning app, which the code-scanning rule reads. **A PR opened before the CodeQL workflow (#945) has none.** It needs a new push, or a close and reopen, before it can merge;
- every commit has a `Signed-off-by:` line.

It does not check for a behavior change. Read the diff for that yourself.

A PR owner sends a ready PR to the integrator as [CLAUDE.md](CLAUDE.md#working-on-a-task) says.

### When the head moves after an approval

An approval counts only for the head it names. If the head moves (a rebase, a fix, a conflict resolution):

- **A rebase with no other change:** run `scripts/release/approval-still-valid.sh <pr> <approved-sha>`. It compares the patch of the approved head with the patch of the new head, each against its own base, and prints `git range-diff`. The same patch means only the base moved, so both approvals still count. Post its output on the PR.
- **A commit that changes only comments**: both approvals stay valid. No new review round. The script prints `COMMENTS ONLY` when the new head descends from the approved head, has the same merge base, and every changed file is `.go` or `.sh` with only blank or whole-line comment changes (`//`, `/* */`, `#`). These are not comments: code, a line with code and a trailing comment, string literals, struct tags, `//go:` directives, `// +build`, `#!`, `# shellcheck`, `.md` pages, config. Those need a re-check of the delta. The PR owner posts the script output on the PR.
- **Any new code, or a conflict resolution:** each reviewer whose approval should still count re-checks the delta. The integrator can instead start a new independent reviewer for the delta.

**A docs-only PR** needs only the release owner's review, by hand, unless the owner asks for the 2 reviews.

## 3. Merge

- **Squash-merge** each PR. Use an explicit subject, `<PR title> (#<n>)`, and a body made of the PR's commit messages. Keep every `Signed-off-by:` line, because the DCO check needs them. Keep one `Assisted-By:` line for AI help, and no `Co-Authored-By:` line.
- Merge only at the head that was approved and tested:
  ```sh
  gh pr merge <n> --squash --match-head-commit <sha> --subject "<PR title> (#<n>)" --body-file <file>
  ```
  Write the body to a file first. A shell can change text that is passed inline. If `gh pr merge` fails with a GraphQL error, use `gh api -X PUT repos/<org>/<repo>/pulls/<n>/merge` with `sha`, `merge_method=squash`, `commit_title` and `commit_message`.
- **Never merge past a failed DCO check.** Only the author can sign off a commit. If a merge past it seems needed, stop and ask the release owner for that PR.
- **Write issue numbers in a PR body as `Refs #N` only.** GitHub closes an issue when "close", "fix" or "resolve" (or a form of them) stands before `#N` anywhere in the PR body. A negative sentence such as "Does not close #1303" also closes it. That is how #1303 closed by mistake when #1370 merged.
  - Before each merge, grep the PR body for those words before a `#number`.
  - If one is there, fix the body before the merge. If the issue closed already, reopen it.
- **Refuse a PR whose `go.mod` diff adds a pseudo-version for one of our modules, or for a module that publishes tags.** A pseudo-version pins a commit, not a released tag. Ask the author to release the dependency and pin the tag. A third-party module with no tags keeps its pseudo-version. See the [`go.mod` rule in CLAUDE.md](CLAUDE.md#conventions).
- Merge **one PR at a time**. After each merge, check that the next PR still merges cleanly before you merge it.
- **After every merge, tell the PR owner in the same step**. Treat the merge and the message as one step.
  - Send the session that owns the PR one short message with `SendMessage`, as in [Working on a task](CLAUDE.md#working-on-a-task): `merged as <sha>`. Add what the owner can do next: close the issues it said it would close, merge `main` into a PR stacked on this one, and clean up its review and fix worktrees.
  - For a PR by a person with no session, post the same text as a PR comment.
  - When several PRs of one owner merge in a batch, send one message per owner.
  - **Why:** #1394 merged and its owner session was not told. The session kept waiting to close #1388.
- **A PR stuck in a merge-of-`main` loop gets a FULL merge freeze until it lands.** Freeze every merge, not a list of files. A file-list freeze missed `campaign/replay.go`, and #1331 needed a 7th merge of `main`.
  1. Tell the other sessions about the freeze.
  2. Merge the stuck PR first.
  3. End the freeze.
- **Merge each PR on its own green CI, then watch `main`.** Waiting for each PR to be green wastes wall-clock time. If `main` turns red after a merge, we fix it.
  - Merge a PR when it has its 2 approvals at its head, green CI at its head, and git can merge it cleanly. Do not re-run a PR's CI only because another PR merged first.
  - After each merge, watch the CI of `main`. If it turns red, fix forward with a fix PR. Do not revert a good PR, unless the fix is not clear.
  - **Why:** a serial re-run costs about one full CI run (about 40 minutes) for each pair of PRs. A break between 2 green PRs is rare: about 1 in 30 merges in the v0.7.4 cycle. A fix PR is cheap.
  - **Exception 1:** a merge of `main` that the integrator resolved by hand in code gets its CI first. In v0.7.4, a hand merge lost a SQL placeholder (#1173).
  - **Exception 2:** the tag. `main` must be green at the tagged commit.
  - **Note:** GitHub's merge queue does this automatically. It tests each PR on top of the PRs ahead of it. Turning it on is a repo setting for the owner.
- **A merge that bypasses the branch rules needs the release owner's authority.** Get it in words, for the release or for a named PR.
  - **Standing rule:** when the only rule that blocks a PR is "1 approving GitHub review from someone other than the author", the integrator may merge with `--admin`, with no new yes. The 2 offload reviews post as comments under the login of the person who ran them, often the author, so GitHub does not count them.
  - **Any other failed rule needs the owner's yes for that PR.** Before a bypass, read which rules fail: `gh api repos/<org>/<repo>/rulesets/rule-suites?ref=refs/heads/main` lists recent merges, and `.../rule-suites/<id>` shows each rule's result.
  - **The code-scanning rule (CodeQL).** The `main` ruleset also needs CodeQL results with no errors and no high or critical security alerts. Until 2026-11-08 the repo had no CodeQL workflow, so this rule failed on every PR, and every merge from v0.6.1 to v0.6.7 went past it with `--admin` without anyone seeing it. The owner decided on 2026-11-08: add the CodeQL workflow and fix what it finds, and **merges wait until the rule passes**. After that, the standing rule holds as written, and a code-scanning failure needs the owner's yes for that PR.
  - **`--admin` does not say which rules it bypasses.** Before an `--admin` merge, run `check-ready.sh`: it fails when the head has no passing `CodeQL` check run. After the merge, read the merge's rule results (`.../rule-suites/<id>`). If a rule other than the review rule failed, tell the owner. On 2026-11-08, #943, #937 and #944 merged past the code-scanning rule this way: they were opened before #945, so CodeQL never scanned them. The scans of `main` afterwards found no new alert.
  - **Only alerts in changed lines block a PR.** A PR's CodeQL result covers only the code that PR changes. To see every open alert, read `main`: `gh api "repos/<org>/<repo>/code-scanning/alerts?ref=refs/heads/main&state=open"`. A false positive is dismissed only with the owner's yes, with the reason in the dismissal comment.
- Read the result back from the server after each merge or edit. Do not trust a tool's "success" line alone.

### Stacked PRs

A stacked PR targets another PR's branch, not `main`. After the parent is squash-merged, the child still carries the parent's old commits. To move it onto `main`:

1. Note the parent's head SHA **before** you merge the parent.
2. Merge the parent.
3. **Retarget the child to `main` first.** CI may not start for a push made while the PR targets another branch.
4. Merge `origin/main` into the child. Do not rebase, so there is no force-push. Do not push yet.
5. Push the merge commit, without force.
6. Wait for green CI, then merge.
7. If CI did not start on the new head, close and reopen the PR.

**Conflicts during the merge:**
- **Mechanical** (both sides add a line to the same list): resolve it, and say how in the merge body.
- **Semantic** (the code was restructured under the PR): stop and hand the merge to the author. Then review the author's conflict resolution before you merge.

**A PR that another PR is stacked on** (GitHub's stack feature) cannot merge with `gh pr merge` or `PUT /pulls/<n>/merge`. Use `PUT /repos/<owner>/<repo>/pulls/<n>/merge-async` with `merge_method: squash`, `merge_action: direct_merge`, `sha`, `commit_title` and `commit_message`. It merges that PR only, not the PRs stacked on top.
- **Use it on the bottom PR of the stack only.** On a child, it also merges every open PR below it.
- **Add `bypass_rules: true` only if a rule blocks the merge.** It is the same bypass as `--admin`. The rules in [Merge](#3-merge) apply: the owner's yes for any failed rule other than the review rule, `check-ready.sh` first, and the rule suite read after the merge.
- **A `202` is not a merge.** Read the PR state back from GitHub.

**Check that the child's own change is unchanged:** the added and removed lines of `git diff origin/main...<head>` match the approved change.

This section is about a stacked PR. For a ready PR that waits only for a merge of `main`, the integrator does the merge (see [Check that a PR is ready](#check-that-a-pr-is-ready)). A semantic conflict there still goes to the PR owner.

### Defects found on the way

### Heavy CI on main

There is no merge queue. `main` may be less than 100% green, because the PR owner runs the heavy suites on the laptop. A PR runs only the fast jobs. The PR owner runs the heavy suites on the laptop. We start lean, and we add checks where failures show up.

**Interim rule (2026-11-24, in force until the 3-tier test plan lands):** PR owners do not run the heavy suites on the laptop. The integrator runs them on `main` every few merges and before each cut. See [CLAUDE.md, Interim rule](CLAUDE.md#interim-rule-in-force-until-the-3-tier-test-plan-lands).

The integrator runs the heavy jobs on `main` with `scripts/release/heavy-ci.sh --wait` (it runs `gh workflow run ci.yaml --ref main`). Run it:
- about every 5 merges;
- after a big refactor or a merge that someone resolved by hand;
- before a merge of a PR that waits on a fix of `main`;
- always before a cut.

**Tier 3 runs on a schedule.** Tier 3 is the heavy suites on `main`: the CI jobs `build-test` and `record`, and Part A.
- **Schedule:** the cron `17 */3 * * *` runs the heavy jobs every 3 hours. GitHub runs a cron only from the default branch, so it starts when this change is on `main`.
- **Only when `main` moved:** the first job, `changed-since-green`, compares `main` with the last green heavy run. If they match, the heavy jobs do not run. `scripts/release/changed-since-green.sh` holds the rule.
- **Before each cut:** run `scripts/release/heavy-ci.sh --wait` (it always runs the heavy jobs) and Part A (step 5). A `v*` tag also runs them.
- **Where the result shows:** the run summary lists both jobs. A red scheduled run opens the issue "Heavy CI failed on main", or adds a comment to it when it is open. A green run closes it. Look for that issue first.
- **Slowest tests:** a scheduled run writes the 30 slowest tests to its run summary. For a manual run, set the input `per_test_times`.
- **Part A cost:** `part-a.sh` adds a `time <step> user=.. sys=.. maxrss_kb=..` line per step to `summary.txt`.

**The heavy jobs run the timing tests.** The jobs `build-test` and `record` set `CG_TIMING_TESTS=1`. `build-test` also runs the OpenCode plugin's node tests with it. A timing test asserts a wall-clock bound or races a rate limit. It fails on a busy machine with nothing wrong in the code, so a PR owner does not run it by default. `local-tests.sh` does not run them. Only the integrator runs them. A PR that adds a timing test runs the timing tests too.

**Tier 2 (the PR owner's run).** A PR owner runs `scripts/dev/local-tests.sh`, which runs `scripts/dev/tier2-steps.sh`: the static steps, non-race tests of the packages that the CI job `purego` does not run, `-race` on the changed packages only, and a rerun of only the failed packages. The load is limited (`-p 2`, `GOMAXPROCS=4`). It is weaker than a full run. **Tier 3 is the integrator's heavy CI and Part A, and it catches what tier 2 skips** (a race in a package the PR did not change, the heavy tests of unchanged packages, the `-race` build of `proxy` with `cg_record` when no tagged file changed). Tier 3 has its own rules in a separate change. The tier-2 marker replaces the interim rule above once the owner says so.

A release is cut only on a commit with a green heavy run **and** a passing Part A (step 5). A failure on `main` is fixed forward: a new PR with the fix. Nobody reverts to make it green.

A defect in `main` that the PR under review did not cause gets its own issue, its own branch from `main` and its own PR. It never rides on another PR's branch. The release owner decides whether it goes into the release.

## 4. Freeze

The release is ready for the integration check when all of these hold:
- every item on the list is merged or moved out;
- each session or person that owns PRs on the list has said "all done";
- the owed measurements are reported (step 1);
- **the release owner says start.**

Merge nothing else until the tag, except the version bump (step 6) and fixes for problems the check finds.

**What "all done" contains.** Ask each PR owner for: the list of its PRs, the proof for each (a link to the passed check, or the Part B check it needs), and anything it knows is still open.

**Order after the freeze:** run Part A and Part B (step 5) at the same time. Open the bump PR (step 6) and draft the notes (step 7) while they run. Merge the bump only after both parts pass. Then tag (step 8).

## 5. Run the integration check on the merged `main`

Run it once, on the exact commit you plan to tag (or the commit before the version bump).

**Part A: the full test suites.** Run `scripts/release/part-a.sh <out-dir>` in a clean copy of the commit, on a machine with Go and cgo. The owner's laptop is one such machine. A test machine is not required. It runs:
- `go vet ./...`, and `go build ./...` with `CGO_ENABLED=0`, with and without `-tags cg_record`;
- the check that the default build links no OpenTelemetry, gRPC or protobuf;
- `go test -race` on every package set, one set per run: `./proxy/...`, `./dash/...`, `./context-guru-plugin`, then all the rest. The limits are: proxy 20m, dash 40m, plugin 30m, rest 20m, record 40m;
- the `-tags cg_record` race tests, as CI runs them.

Notes:
- **Part A runs the timing tests.** `part-a.sh` sets `CG_TIMING_TESTS=1` unless the caller sets it, and its step `node-timing` runs the OpenCode node tests with it (node 22 is needed). `scripts/dev/local-tests.sh` sets it itself (see CLAUDE.md, "Timing tests").
- **Part A is tier 3.** It writes `time` lines (CPU seconds and peak memory) per step to `summary.txt`, from `/usr/bin/time`.
- **Part A skips `TestBobReleasePythonSuite` (in `race-rest`).** The owner put the Bob plugin on hold on 2026-11-18. The summary names the skip. Remove the skip in `scripts/release/part-a.sh` when the owner resumes the plugin ([#1221](https://github.com/rossoctl/context-guru/issues/1221)).
- **The run takes about 70 to 90 minutes on a laptop, so run it detached.** Its summary file always ends with `DONE rc=<n>`, also on a failure. Wait for that line; a marker written only on success makes a failure look like a hang.
- **Copy the code into an empty directory.** A copy tool that only adds files leaves a deleted test in place, and it keeps running.
- **Do not copy new code into that directory while a run is in progress.**
- The script runs a failed step once more. "flaky" in the summary means the second run passed. If a step failed twice, run it on the same commit in a clean directory. That separates a flaky test from a defect.
- Known flakes on a loaded machine: `SQLITE_BUSY` in `./dash/...`, and `TestDashEmitterKeepAlivePingCarriesNoWaitCause`.
- **Known flake: `TestFailedLoadAllowsRetry`** (`store/disk_test.go:413`). It fails with `OpenDisk: context deadline exceeded` when the machine is under load. In v0.7.2 it passed on a re-run, and 3 times when run alone. Issue #1075 covers it. Check it with `gh issue view 1075`.
- If a test fails, run it on an unmodified `origin/main` on the same machine before you call it a defect of the release. Some tests fail because of the machine itself.
- **A failure that also fails on an unmodified `origin/main` on the same machine does not block the tag.** Post the proof, both runs, in the release notes draft or on the release issue.

**Part B: one live session through a proxy built from `main`.**
- **Hosted mode** has no gate of its own (see [Where context-guru runs](#where-context-guru-runs)). Its tests still run in Part A.
- **Setup.** A person runs `scripts/release/part-b-setup.sh <settings.json> <scratch-dir> <port> <upstream-var> [<model>]` (in an agent session, with the `!` prefix). `<upstream-var>` names the env-block variable that holds the plain model gateway; which gateway accepts the key differs per maintainer. The script copies the Claude Code env block without the context-guru base URL and header, points it at `http://127.0.0.1:<port>/anthropic` (the proxy's Anthropic route; the root URL gives a 404), sets a cheap model, and writes the gateway URL to `<scratch-dir>/upstream.url`. An AI agent must not copy credentials itself.
- Build the proxy from the commit and run it on a free port, with the dashboard on and its own database: `-listen 127.0.0.1:<port> -anthropic-upstream "$(cat <scratch-dir>/upstream.url)" -dashboard-db <scratch-dir>/dash.db -control-db <scratch-dir>/control.db`.
- Run an **isolated** Claude Code session against it: its own config directory (`CLAUDE_CONFIG_DIR=<scratch-dir>/cc`), the plugin from the same commit, its own state directory (`CONTEXT_GURU_STATE=<scratch-dir>/state`, `XDG_CACHE_HOME=<scratch-dir>/cache`, `CG_URL=http://127.0.0.1:<port>`). Never install anything at user scope on a shared machine.
- **The traffic never goes through a production context-guru service** or through anyone's own proxy. The proxy's upstream is the plain model gateway.
- An AI integrator can give the checks to one subagent. Cap its report at about 6,000 characters, or the report gets cut off.
- Write the list of checks from the release list: one check for each feature or fix a user can see. For example, for v0.5.2:
  1. the main thread keeps its id across `/compact`;
  2. the status line shows the money line for the session and its subagents;
  3. after an update, the status line moves to the new version;
  4. `/exit` stops the keep-alive for that session;
  5. an idle keep-alive ping reads the cache;
  6. each new command runs without an error. Run only commands that read: never `apply`, `revert` or a mode switch;
  7. the insights rows add up to the headline;
  8. the dashboard loads every tab;
  9. a bypassed request gets its token count;
  10. **always, the cache strategy (#776, #867):** the number of planned pings depends on what the thread waits for. Read `waiting_for` of the turn to know the count: a person gets the base plus 1, a tool gets pings until its result comes back (at most 8). Every planned ping for that wait is plain and reads the full prefix. A keep-alive substitute comes only after the LAST planned ping.
- Ask each PR owner for the check of each user-visible change. **The checks of the other clients (Codex, Bob, Pi, OpenCode) are the PR owner's job,** with proof posted on the PR, not part of Part B.
- Keep the session small. Report the spend.
- Record the evidence for each check: request rows, API output, status-line output.
- **Traps that make a check look broken:**
  - Keep-alive is off unless the config turns it on. Its keys go under `cache:`. The general floor `keepalive_min_prefix_tokens` is 80000. Tool waits, waits for background work or a teammate, and subagent threads use `keepalive_tool_wait_min_prefix_tokens`, 20000. A small session (a main thread that waits for a person) needs a lower general floor in its own config. Without it, a manual ping answers `409 not held`.
  - The keep-alive skips the first request of each session after every proxy restart. Restart, then send 2 turns before you test a ping.
  - The model gateway can replay a byte-identical request from its own cache. Put a new nonce in every prompt.
  - For the shape of a turn's last message, read `wake_cause`, not `stop_reason`. `ping_n` is in `request_lifecycle`.
  - **Do not load the plugin with `--plugin-dir` for the ping-test turns.** When loaded this way, the plugin's SessionEnd hook releases the keep-alive entry as soon as `claude -p` exits. The ping then shows `pingable: false` and "no entry". Send the ping-test turns without the plugin.
  - **The #858 gate: a thread gets pings only after it has made one earlier tool call.** Send a tool turn first. Then test pings.
  - `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` removes Claude Code's ToolSearch. Leave it unset for a tool-search check, and send one warm-up call first.
  - The status line prints nothing until a settings file records the routing: `settings.py add --file <work>/.claude/settings.local.json --url ...`, and run it with `CLAUDE_PROJECT_DIR=<work>`.
  - `sqlite3 "file:...?mode=ro"` fails once the proxy has stopped. Copy the database first.
  - `ps -o args=` shows the path the proxy was started with, often relative. A kill guard that matches the absolute path skips the kill.
- **Checks for production on laptops.** A release reaches every laptop at once through `releases/latest`, so add these to the list:
  1. **Upgrade on an existing database.** Start the previous release, let it write a database, then start the new build on a copy of that database. The schema migrations must run and the data must stay.
  2. **Install through the plugin.** Install the proxy from the current `releases/latest` through the Claude Code plugin, and run the plugin from the commit. The update to the new release is checked after the publish (step 8). The checks of the other clients' plugins are the PR owner's job (see above).
  3. **The central push, once it is in use.** Check three things: the central copy holds **metadata only** (read the payload: no transcript text, tool output or bodies); it **reaches the collector**; it **fails open** when the collector is down (the request still goes out and the user sees no error).
- At the end, stop the proxy by its own pid and remove everything the check created. **Remove the credential copies right after the last run;** the rest may stay until the tag. Report any state the check changed outside its own directories.

A failed check blocks the tag. Fix it in a PR (step 2), then run the failed check again.

## 6. Bump the plugin versions

One PR that bumps each plugin that changed in this release:
- `context-guru-plugin/.claude-plugin/plugin.json` (Claude Code);
- `codex-marketplace/plugins/context-guru/.codex-plugin/plugin.json` (Codex);
- `bob-plugin/context-guru/plugin.json` (Bob);
- `pi-package/package.json` (Pi);
- OpenCode has no version file. The bump moves `PINNED_VERSION` in `opencode-plugin/index.js` to the new proxy release, and the same version in `docs/how-to/install-opencode-plugin.md` and `opencode-plugin/README.md`. This is the practice in the last bump PR (#1136).

Say in the PR body what changed in each plugin. A plugin that did not change keeps its version.

The bump PR gets the 2 reviews like any other PR. Its squash message lists the changes per plugin. Merge it after Part A and Part B pass; the tag goes on the bump commit.

## 7. Write the release notes

- **Features and fixes only.** No tests, CI, reviews, docs-only or process PRs.
- Sections: **New and changed**, **Fixed**, **How to upgrade**, **Known limits**.
- Each item: one bold lead line with the PR and issue numbers, then 1 to 4 short points in plain words: what the user gets, or what was broken and is now fixed.
- Mark with **[Notice]** each item a user will see or must act on: a new default, a removed tab or command, a figure that moves.
- Use real numbers where they help.
- A fix to a feature that is new in the same release goes into that feature's item, not into "Fixed".
- **Rollback.** Every release notes page gives the way back: `CONTEXT_GURU_VERSION=<previous tag>`, with the install command from [docs/how-to/install-plugin.md](docs/how-to/install-plugin.md). Name the real previous tag.
- **Known limits** lists the open issues a user can meet, with their numbers. Check that each one is still open.
- An optional section, **What we measured**, gives the results of the measurements before the release (step 1), with their baselines.
- A mode that the owner marks experimental stays "opt-in, not recommended" in the notes.
- Collect each line of work's items from the PR owners in this format before you write.
- Write in [ASD-STE100](https://www.asd-ste100.org/) style: short sentences, active voice, one word for one meaning.
- **The release owner approves the notes** before the tag.
- **Keep the draft in a file that survives a restart,** not in `/tmp`. The release page carries the approved text, then the generated changelog.
- **A correction to a published release needs the owner's yes.** Mark the corrected line, with the date. Example: the v0.6.6 notes gave Opus 70/80; the true figure was 66/80.

## 8. Tag and publish

1. Check that `main` has nothing after the checked commit except the version bump.
2. **Ask the release owner: is this a beta, or does it replace latest?** Wait for the answer. Do not tag before it.
   - **A beta.** Name the tag `vX.Y.Z-beta.N`. GoReleaser marks it as a pre-release by itself (`prerelease: auto`). The owner and others test it on their laptops. They install it with `/context-guru:install-beta` (script: `context-guru-plugin/scripts/install-beta.sh <tag>`). It uses the beta tag's own `install.sh` and sets the update answer to `never` (#1254). Users who do not install a beta keep `latest`.
   - **A beta that the owner makes latest.** The owner may ask for this later. GitHub never makes a pre-release `latest`. So mark the release as not a pre-release: `gh release edit <tag> --prerelease=false --latest`. The tag and the title keep "beta".
   - **Not a beta.** The release replaces latest at once.
3. Create an annotated tag on that commit and push it:
   ```sh
   git tag -a vX.Y.Z <sha> -m "context-guru vX.Y.Z"
   git push origin refs/tags/vX.Y.Z
   ```
   For a beta, use the beta tag name.
4. The tag starts the release workflow (`.github/workflows/release.yaml`, GoReleaser). It takes about 15 minutes. Wait for it to pass, and check the assets (5 for v0.6.7).
5. Put the approved notes at the top of the release body, above the generated changelog. The release title is the tag name.
6. **Check the update through the plugin.** Update from the previous release to the new one, through the plugin. For a beta, install with `CONTEXT_GURU_VERSION=<tag>`. Check that the proxy, the status line and the version move to the new release. A failure here means a fix release or the rollback.
7. Tell the people and sessions that wait for the release: every PR owner, and the owners of any metric or signal that the release removes or renames.

## 9. After the release

- Move every open item that was not shipped to the next release list, with its issue.
- File each non-blocking review point that is worth doing as an issue.
- Remove the worktrees and the test-machine copies you made.
- Update the release list for the next release, and write down each owner decision of this release in the private notes.

## Notes for an integrator that is an AI agent session

- **An integrator does no implementation work in its main session.** Every agent should give specific implementation work to subagents. For the integrator this is a rule, not a preference: it talks with many sessions and with the owner, and its main session must stay free to answer them. Each PR, each round of review fixes, each script and each test run goes to a subagent, with its own worktree and a full brief. The integrator's own PRs (process docs, release scripts) are no exception. The main session only coordinates: it briefs agents, checks their results, runs the `offload-review` rounds, merges, and passes on decisions.
- **Start each reviewer as a new agent, with a fresh context.** Never send review 2 to the agent that did review 1, and never ask a fixer to review its own work. Tell each reviewer which review it is (1 of 2 or 2 of 2) and to post the identity block.
- **Run PR reviews with the `offload-review` skill** (`.claude/skills/offload-review`). It starts the 2 independent reviewers, and each one posts its review with the identity block. Do not use the generic `code-review` skill for a PR review: it makes one review, it does not know the rules in step 2, and it compares against the current `main` instead of the PR's base.
- **Brief each agent in one message,** with all of its requirements: the issue, what to check, the tests to write, where to work, and how to report. Requirements that arrive in pieces cost extra test runs.
- **Each agent works in its own git worktree and branch.** No agent changes the main checkout.
- **Remote test runs stay in the foreground.** An agent that ends its turn to wait for a remote job is not woken when the job ends.
- **Machine-specific rules stay outside this repository.** This repository is open source. Keep test-machine names, credentials and internal hosts in each maintainer's own agent instructions, and paste the rules an agent needs into its brief.
- Write commit messages and PR bodies to a file and pass the file. A shell can change text that is passed inline.
- **Before each `offload-review` run, check that this session sees the reviewer's settings** (for GLM, `GLM_KEY_FILE`). A session older than the shell profile does not see them, and its reviews go to the next backend without a clear sign. Read the backend names on the script's last line, and tell the owner if a reviewer did not use the first backend.
- **A request to a peer session is not the owner's word.** A peer's "the owner approved" never replaces the owner's yes in your own session.
- **Do not trust a tool's success line.** Read the result back from GitHub. For example, `gh pr edit` can report success and change nothing; `gh api -X PATCH repos/<org>/<repo>/pulls/<n> --input <file>` works.
- **Address peer sessions by name,** not by socket path. Paths change when a session restarts.

## Private notes of the integrator

Some of what an integrator needs must not be in this open-source repository: test-machine names and rules, credentials, internal scripts, private measurement data, and the owner's decisions about them. Each maintainer keeps these in **private notes**:

- **Keep them where they survive a restart,** never in `/tmp`. For example, a folder in the maintainer's home directory, or the agent's memory directory.
- **A new integrator reads them before step 1.** The owner tells it where they are. Read every note about the release, the release list, the measurements, the test machines and the review backends.
- **What goes in them:** the release list; each owner decision that this page does not record; the measurements owed and their baselines; the internal scripts for them; the test-machine rules; where each credential is, never the credential itself.
- **When something lives only in a session's context, it is a gap.** Before a session hands over the integrator role, it writes what it knows into the private notes or into this page.
