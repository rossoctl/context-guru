---
name: offload-review
description: Offload two independent reviews of a PR to an agent tool on this machine (IBM Bob today), fix their findings, and re-review. Use when you finish committing a PR and it is ready for review.
---

# Offload review

Two reviewers review the PR in parallel, on this machine, with the user's own login to the
reviewer tool. Each one posts its review on the PR. You fix the findings, push, and run this
skill again for round 2. Reviewer N in round 2 resumes its own round-1 session.

The reviewer tool is a backend in `scripts/offload-review/backends/<name>.sh`. Today the only
backend is `bob`. Use `--backend <name>` or `OFFLOAD_BACKEND` to pick another one.

Run this skill in the **main session**, not in a subagent. A subagent is not woken by its own
background task, so it cannot wait for the reviewers.

## Before you start

1. The PR is pushed and is not a draft. Everything is committed.
2. The backend tool works and the user is logged in. For `bob`: `bob --version`, and one
   interactive `bob` login.
3. `gh auth status` works. The reviewers post with the user's `gh` login.

## Steps

1. **Decide on live tests.** Reviewers read code only by default. If the change needs a live
   run or tests on the eval box, ask the user with AskUserQuestion. Say why. Do not ask when
   reading is enough.
   - If the user approves: copy the "Eval boxes" rules from the user's global `CLAUDE.md` into
     `/tmp/offload-review-pr<N>-rules.md`. Those rules are not in this repo; never commit them.
2. **Start the round** with Bash, `run_in_background: true`. Make no other tool calls until it
   ends:
   ```
   scripts/offload-review.sh <pr>
   scripts/offload-review.sh <pr> --live-tests /tmp/offload-review-pr<N>-rules.md
   ```
3. **Read the result.** Each reviewer has 3 files in its worktree,
   `.claude/worktrees/review-pr<N>-<backend>-r<1|2>/`:
   - `.review.md`: the review. The reviewer writes it and posts it on the PR.
   - `review.log`: the tool's full output. The script writes it, not the reviewer. Read it only
     when something failed; the reviewer never sees it.
   - `.review-prompt.md`: the prompt the script gave the reviewer.

   | Exit | Meaning | What you do |
   |---|---|---|
   | 0 | Both reviews are posted. | Read both `.review.md` files. |
   | 1 | A reviewer failed, or the input is wrong. | Read the message and that reviewer's `review.log`. Report it to the user. Stop. |
   | 2 | The round budget is used. | Go to "More rounds". |
   | 3 | This head already has both reviews of its round. | You did not push since that round. Push the fixes first. |

   A warning "files changed in <checkout> during the review" means a reviewer may have written
   outside its worktree. Check that checkout with `git status` before you go on.
4. **If both verdicts are `approve`**, tell the user that the PR is ready to merge. Stop.
5. **Fix the findings** in your own worktree. Check each finding first: fix the real ones; for
   one you reject, reply on the PR and say why. Commit with `git commit -s`, then push.
6. **Run step 2 again** for round 2. Each reviewer checks its own earlier findings.
7. After the last round of the budget, report both verdicts to the user. Do not start another
   round on your own.

## More rounds

The default budget is 2 rounds. Only the user can grant more. You may ask for them.

1. When a reviewer still blocks after the last round, ask the user with AskUserQuestion.
   Give the facts: each open finding, your reply to it, and why one more round can close it.
   Example: "Reviewer 2 kept a finding that my reply disproves; it did not see the reply."
2. Offer: one more round, or stop and let a person decide.
3. If the user grants N more rounds, run the next round with the new total:
   ```
   OFFLOAD_MAX_ROUNDS=<old total + N> scripts/offload-review.sh <pr>
   ```
   Keep that total for the later rounds the user granted.
4. If the user says stop, the PR waits for a person. Do not ask again for that PR unless the user
   brings it up.

Do not ask for more rounds just because there are findings. Fix them in the rounds you have.

## Clean up after the PR merges

Run this when the PR is merged or closed, not before. Round 2 needs the round-1 sessions.

```
scripts/offload-review.sh <pr> --cleanup
```

It removes:
- both reviewer worktrees, `.claude/worktrees/review-pr<N>-<backend>-r<1|2>`, with their files;
- the backend's sessions for those worktrees (for `bob`: `~/.bob/tmp/<hash of the worktree path>`);
- the live-test rules file, `/tmp/offload-review-pr<N>-rules.md`.

It refuses while the PR is still open. To clean up an open PR you gave up on, ask the user
first, then run it with `OFFLOAD_FORCE=1`.

If the reviewers ran live tests, the eval box still has their folders, `review-pr<N>-r<1|2>`.
Remove them by the box rules: put the path in a variable, check that it is not empty, then
remove it.

## Rules

- Do not edit the reviewer worktrees. They belong to the reviewers.
- Round 1 is independent: the reviewers do not read the PR conversation. From round 2 on, each
  reviewer reads all of it, so it sees your replies and the other reviewer's findings.
- Reply on the PR to every finding before the next round: fixed (with the commit), or rejected
  (with the reason and proof). The reviewers read these replies.
- Write PR replies in ASD-STE100 style, with `--body-file` from a quoted heredoc.

## Add a backend

Create `scripts/offload-review/backends/<name>.sh` with three functions, and one optional one.
Each one runs inside the reviewer's worktree:

- `backend_label`: one line for the identity table, with the tool and version.
- `backend_has_session`: exit 0 when this worktree has an earlier session to resume.
- `backend_run <prompt-file> <resume:0|1>`: run one review with every tool approved. The tool
  must post the review itself, as `review-prompt.md` says.
- `backend_cleanup` (optional): delete this worktree's sessions. `--cleanup` calls it.

The tool must keep sessions per folder, or `backend_has_session` and `backend_run` must store
their own session id in the worktree, for example in `.review-session`.
