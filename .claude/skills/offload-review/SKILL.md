---
name: offload-review
description: Offload two independent reviews of a PR to agent tools on this machine, fix their findings, and re-review. Use for every PR review in this repo - when asked to review a PR (yours or another person's), and when you finish committing a PR. Use it instead of the generic code-review skill.
---

# Offload review

Two reviewers review the PR in parallel, on this machine, with the user's own login to the
reviewer tool. Each one posts its review on the PR. You fix the findings, push, and run this
skill again for round 2. Reviewer N in round 2 resumes its own round-1 session.

The reviewer tool is a backend in `scripts/offload-review/backends/<name>.sh`:

- `glm` (the default): GLM through Claude Code, pointed at an internal gateway.
- `bob`: IBM Bob Shell.

Use `--backend <name>` or `OFFLOAD_BACKEND` to pick one.

Run this skill in the **main session**, not in a subagent. A subagent is not woken by its own
background task, so it cannot wait for the reviewers.

## When to use

- **Every PR review in this repo uses this skill.** This includes "review PR <N>" for a PR that
  another person wrote.
- **Do not use the generic `code-review` skill for a PR review.** It makes one review, not two
  independent ones. It does not know this repo's rules. It also compares against the current
  `main`, not against the PR's base, so a branch that is behind `main` gets findings in files the
  PR does not change.
- `code-review` is still correct for a quick check of your own diff before you commit.
- **A PR by another person:** run the rounds, then report both verdicts to the user. Do not fix
  the findings (step 5) unless the user asks you to.

## Before you start

1. The PR is pushed and is not a draft. Everything is committed.
2. The backend tool works and the user is logged in.
   - For `glm`: `claude --version`, and `GLM_KEY_FILE` names the user's JSON key file. The file
     holds the key, the gateway URL and the model id. It is not in this repo. Never print, copy or
     log it. The backend reads the key at call time through `apiKeyHelper`, so the key never
     enters the reviewer's environment.
   - For `bob`: `bob --version`, and one interactive `bob` login.
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
   | 3 | This head already has both reviews of its round. | You did not push since that round. Push the fixes first. If the user asks for a round on the same head, see "Same head". |

   A warning "files changed in <checkout> during the review" means a reviewer may have written
   outside its worktree. Check that checkout with `git status` before you go on.
4. **If both verdicts are `approve`**, tell the user that the PR is ready to merge. Stop.
5. **Fix the findings** in your own worktree. Check each finding first: fix the real ones; for
   one you reject, reply on the PR and say why. Commit with `git commit -s`, then push.
6. **Run step 2 again** for round 2. Each reviewer checks its own earlier findings.
7. After the last round of the budget, report both verdicts to the user. Do not start another
   round on your own.

## Same head

Run a round on a head that already has both reviews only when the user asks for it. Two cases:

- The reviewers could not run their tests (for example, the eval box was down), and now they can.
- The author answered a finding with a reply and proof, with no code change.

Before the round, post on the PR what is new: the replies and any test results. Then run:

```
scripts/offload-review.sh <pr> --same-head [--live-tests /tmp/offload-review-pr<N>-rules.md]
```

Each reviewer resumes its own session and is told that the head did not move. The round counts
toward the budget, so the user may need to grant more rounds (see "More rounds").

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
- the backend's sessions for those worktrees (for `glm`: the `.glm-cc/` folder in each worktree; for `bob`: `~/.bob/tmp/<hash of the worktree path>`);
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
