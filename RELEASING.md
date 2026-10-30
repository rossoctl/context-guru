# Releasing

This page describes how a context-guru release is cut. One person or agent session takes the role of **integrator** for the release. The integrator gets the planned work reviewed and merged, runs one integration check on the merged `main`, writes the release notes and tags the release.

The process was first used for v0.5.1 and v0.5.2.

## Roles

| Role | Does |
|---|---|
| **Release owner** | Decides what goes into the release. Approves the release notes. Gives the integrator authority to merge. |
| **Integrator** | Keeps the release list. Starts reviews, merges, rebases, runs the integration check, writes the notes, tags. |
| **PR author** | Writes the change and its tests. Fixes review findings. |
| **Reviewer** | Gives one independent review of one PR. Never the author of that PR. Each PR has 2 reviewers. |

The integrator and the reviewers can be people or AI agent sessions. The rules are the same.

## 1. Plan the release

1. **Keep one release list.** It holds every PR and issue in the release, with its state: open, in review, approved, merged, out. The release notes come from this list at the end, so add a note line as each item is merged.
2. **Sort the open PRs into features.** Group them by what a user gets, not by author or by date: for example "save-more commands", "status line", "test fixes". Show which PRs depend on which.
3. **The release owner picks the groups.** The integrator does not add or drop an item without the owner.
4. **Record what is out**, and why. An item that moves to the next release keeps its issue.

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
- **A change found after the merge** goes to the release owner at once. The owner keeps it or has it reverted.
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

### When the head moves after an approval

An approval counts only for the head it names. If the head moves (a rebase, a fix, a conflict resolution):

- **A rebase with no other change:** the integrator confirms with `git range-diff` that only the base moved. Then both approvals still count. Say so on the PR.
- **Any new code, or a conflict resolution:** each reviewer whose approval should still count re-checks the delta. The integrator can instead start a new independent reviewer for the delta.

**A docs-only PR** needs only the release owner's review, by hand, unless the owner asks for the 2 reviews.

## 3. Merge

- **Squash-merge** each PR. Use an explicit subject, `<PR title> (#<n>)`, and a body made of the PR's commit messages. Keep every `Signed-off-by:` line, because the DCO check needs them.
- Merge only at the head that was approved and tested: `gh pr merge <n> --squash --match-head-commit <sha> ...`.
- Merge **one PR at a time**. After each merge, check that the next PR still merges cleanly before you merge it.
- **A merge that bypasses the branch rules needs the release owner's authority.** Get it in words, for the release or for a named PR.
- Read the result back from the server after each merge or edit. Do not trust a tool's "success" line alone.

### Stacked PRs

A stacked PR targets another PR's branch, not `main`. After the parent is squash-merged, the child still carries the parent's old commits. To move it onto `main`:

1. Note the parent's head SHA **before** you merge the parent.
2. Merge the parent.
3. **Retarget the child to `main` first.** CI may not start for a push made while the PR targets another branch.
4. Rebase only the child's own commits: `git rebase --onto origin/main <old-parent-head> <child>`.
5. Push with a lease, so you never overwrite work the author pushed in the meantime: `git push --force-with-lease=refs/heads/<branch>:<sha-you-rebased> ...`.
6. Wait for green CI, then merge.
7. If CI did not start on the new head, close and reopen the PR.

**Conflicts during a rebase:**
- **Mechanical** (both sides add a line to the same list): resolve it, and say how in the merge body.
- **Semantic** (the code was restructured under the PR): stop and hand the rebase to the author. Then review the author's conflict resolution before you merge.

### Defects found on the way

A defect in `main` that the PR under review did not cause gets its own issue, its own branch from `main` and its own PR. It never rides on another PR's branch. The release owner decides whether it goes into the release.

## 4. Freeze

The release is ready for the integration check when every item on the list is merged or moved out. Merge nothing else until the tag, except the version bump (step 6) and fixes for problems the check finds.

## 5. Run the integration check on the merged `main`

Run it once, on the exact commit you plan to tag (or the commit before the version bump).

**Part A: the full test suites.**
- `go vet ./...`, and `go build ./...` with `CGO_ENABLED=0`.
- `go test -race` on every package set, one set per run: `./proxy/...`, `./dash/...`, `./context-guru-plugin`, then all the rest.
- If a test fails, run it again once. If it fails again, run it on the same commit in a clean directory. That separates a flaky test from a defect.

**Part B: one live session through a proxy built from `main`.**
- Build the proxy from the commit and run it on a free port, with the dashboard on and its own database.
- Run an **isolated** Claude Code session against it: its own config directory, the plugin from the same commit, its own state directory. Never install anything at user scope on a shared machine.
- **The traffic never goes through a production context-guru service** or through anyone's own proxy. The proxy's upstream is the plain model gateway.
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
  10. **always, the cache strategy (#776):** after a text turn, every planned ping is plain and reads the full prefix, and a keep-alive substitute comes only after the last planned ping.
- Keep the session small. Report the spend.
- Record the evidence for each check: request rows, API output, status-line output.
- At the end, stop the proxy by its own pid and remove everything the check created. Report any state the check changed outside its own directories.

A failed check blocks the tag. Fix it in a PR (step 2), then run the failed check again.

## 6. Bump the plugin versions

One PR that bumps each plugin that changed in this release:
- `context-guru-plugin/.claude-plugin/plugin.json` (Claude Code);
- `codex-marketplace/plugins/context-guru/.codex-plugin/plugin.json` (Codex).

Say in the PR body what changed in each plugin. A plugin that did not change keeps its version.

## 7. Write the release notes

- **Features and fixes only.** No tests, CI, reviews, docs-only or process PRs.
- Sections: **New and changed**, **Fixed**, **How to upgrade**, **Known limits**.
- Each item: one bold lead line with the PR and issue numbers, then 1 to 4 short points in plain words: what the user gets, or what was broken and is now fixed.
- Mark with **[Notice]** each item a user will see or must act on: a new default, a removed tab or command, a figure that moves.
- Use real numbers where they help.
- A fix to a feature that is new in the same release goes into that feature's item, not into "Fixed".
- **Known limits** lists the open issues a user can meet, with their numbers.
- Write in [ASD-STE100](https://www.asd-ste100.org/) style: short sentences, active voice, one word for one meaning.
- **The release owner approves the notes** before the tag.

## 8. Tag and publish

1. Check that `main` has nothing after the checked commit except the version bump.
2. Create an annotated tag on that commit and push it:
   ```sh
   git tag -a vX.Y.Z <sha> -m "context-guru vX.Y.Z"
   git push origin refs/tags/vX.Y.Z
   ```
3. The tag starts the release workflow (`.github/workflows/release.yaml`, GoReleaser). Wait for it to pass and check the assets.
4. Put the approved notes at the top of the release body, above the generated changelog.
5. Tell the people and sessions that wait for the release.

## 9. After the release

- Move every open item that was not shipped to the next release list, with its issue.
- File each non-blocking review point that is worth doing as an issue.
- Remove the worktrees and the test-machine copies you made.

## Notes for an integrator that is an AI agent session

- **Start each reviewer as a new agent, with a fresh context.** Never send review 2 to the agent that did review 1, and never ask a fixer to review its own work. Tell each reviewer which review it is (1 of 2 or 2 of 2) and to post the identity block.
- **Run PR reviews with the `offload-review` skill** (`.claude/skills/offload-review`). It starts the 2 independent reviewers, and each one posts its review with the identity block. Do not use the generic `code-review` skill for a PR review: it makes one review, it does not know the rules in step 2, and it compares against the current `main` instead of the PR's base.
- **Brief each agent in one message,** with all of its requirements: the issue, what to check, the tests to write, where to work, and how to report. Requirements that arrive in pieces cost extra test runs.
- **Each agent works in its own git worktree and branch.** No agent changes the main checkout.
- **Remote test runs stay in the foreground.** An agent that ends its turn to wait for a remote job is not woken when the job ends.
- **Machine-specific rules stay outside this repository.** This repository is open source. Keep test-machine names, credentials and internal hosts in each maintainer's own agent instructions, and paste the rules an agent needs into its brief.
- Write commit messages and PR bodies to a file and pass the file. A shell can change text that is passed inline.
