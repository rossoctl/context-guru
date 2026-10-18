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
3. **Read the result.**
   - Exit 0: read both `.claude/worktrees/review-pr<N>-<backend>-r{1,2}/.review.md`.
   - Exit 1: read that reviewer's `review.log`, report it to the user, and stop.
   - Exit 1 with "already has both round-N reviews": you did not push since that round.
   - Exit 3: the round budget (2) is used. Tell the user that the PR is problematic. Stop.
4. **If both verdicts are `approve`**, tell the user that the PR is ready to merge. Stop.
5. **Fix the findings** in your own worktree. Check each finding first: fix the real ones; for
   one you reject, reply on the PR and say why. Commit with `git commit -s`, then push.
6. **Run step 2 again** for round 2. Each reviewer checks its own earlier findings.
7. After round 2, report both verdicts to the user. Do not start round 3.

## Rules

- Do not edit the reviewer worktrees. They belong to the reviewers.
- Round 1 is independent: the reviewers do not read the PR conversation. From round 2 on, each
  reviewer reads all of it, so it sees your replies and the other reviewer's findings.
- Reply on the PR to every finding before the next round: fixed (with the commit), or rejected
  (with the reason and proof). The reviewers read these replies.
- Write PR replies in ASD-STE100 style, with `--body-file` from a quoted heredoc.

## Add a backend

Create `scripts/offload-review/backends/<name>.sh` with three functions. Each one runs inside
the reviewer's worktree:

- `backend_label`: one line for the identity table, with the tool and version.
- `backend_has_session`: exit 0 when this worktree has an earlier session to resume.
- `backend_run <prompt-file> <resume:0|1>`: run one review with every tool approved. The tool
  must post the review itself, as `review-prompt.md` says.

The tool must keep sessions per folder, or `backend_has_session` and `backend_run` must store
their own session id in the worktree, for example in `.review-session`.
