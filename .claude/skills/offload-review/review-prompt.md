# Review a context-guru pull request

You review one pull request in the `context-guru` repo. Another reviewer reviews the same PR
at the same time.

- **Round 1:** review on your own. Do not read other reviews or comments on the PR until you
  post your own. Two independent first views find more defects than one.
- **Later rounds:** read the whole PR conversation first, as "This review" says.

## What to do

1. Read `CLAUDE.md` and `docs/design.md` for the rules of this repo.
2. Read the full diff and the code around each change.
3. For each finding you believe, try to prove it: exact lines, a reasoning trace, or a test.
4. Write your review to `.review.md` in the repo root, in the format below.
5. Post it on the PR with the command given under "This review". Post it only one time.

## Do not

- Do not change, commit, or push code. Your only file is `.review.md`.
- Do not approve or request changes through GitHub. Post a comment review only.
- Do not run Go builds or tests unless "This review" says that live tests are approved.
  Go is not installed on this machine.

## Rules

- **Fail open.** Any component error must revert that component only, and the original request
  must still go out. Every lossy change must be reversible.
- Report real defects: wrong behavior, data loss, a broken fail-open path, a missing test for
  new behavior, a security problem.
- Leave out style nitpicks. If a small point is worth noting, put it under "Non-blocking".
- Put all real findings in this round. Rounds are limited.

## Format of `.review.md`

Write in ASD-STE100 style: short sentences, simple words, active voice. Keep the first two
lines exactly as "This review" gives them: the marker and the identity table.

```
<marker line>
<identity table>

Verdict: approve | blocking

## Findings
1. **<file>:<line> — <one-line claim>**
   - What fails: <inputs or state → wrong result>
   - Proof: <lines, trace, or test and its output>
   - Fix: <short proposal>

## Non-blocking
- ...

## What I checked
- ...
```

Use `Verdict: blocking` when there is at least one finding. Use `Verdict: approve` when there
is none. Put the same verdict in the marker line and the table.
