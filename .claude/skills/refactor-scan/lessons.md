# Lessons from review

The reviewers of each run found these mistakes in the fixes. Put this file in every fix
agent's brief. Add a lesson when a later run's review finds a new kind of mistake.

### The fix can repeat the bug

- **Centralize the decision, not the constant.** #849 moved the multiples into one constant, but 6
  callers still filled a blank rate themselves, at different times. When the rate became per
  model (#855), each fill site had to change, and review found them one at a time. One function
  that answers the question for a given input is the fix. A shared number is not.
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

- **Fail open covers moved code too.** Accountant or booking code on the request path, or in a
  handler's defer, runs under a `recover` that logs and drops. On `main`, the dashboard path in `capture.finish`
  (`proxy/dashcapture.go`) calls `accCompute` with no recover of its own. `accRun`, `accFigure`
  and `finishAccountant` have one.
- **A cost or saving formula belongs in the accountant.** Move it there, release a tag, and call it.
  Do not leave a second formula in a host or a library package.
- **A comment that gives a design reason is not a copy.** In #1535, `config/strategy.go` wrote its
  keep-alive numbers out on purpose ("a later change to a default must not silently re-tune a
  strategy an install uses"). The fix made them read the constants, which reverses that choice.
  Read the reason before you merge two values. Pin them with a test instead.
- **"One rule" can be two questions.** In #1521, "billed at the 1h tier" was coded two ways. The
  mixed write (a 1h head plus a 5m tail) is the designed head-TTL shape. "Every token" and "any
  token" answer different questions: whether the whole entry lives 1h, and whether the head lives
  1h. Name two rules. Do not force one, and test the designed shape.
- **A host refactor of a total can drop a term.** #1507's `f.DeltaUSD = fig.CountedUSD()`
  overwrote the defer-spend subtraction made just before it. When you replace a sum with a library
  call, list every term the old total had, and test a case with all of them.
- **Two numbers from different removals make an unbounded ratio.** #1536's RequestFactor divided
  gross bytes, including discarded changes, by net tokens. Pair the numerator and the denominator
  on the same set, and bound the result like its sister value.
- **Model inputs keep their trained values.** A move to the accountant must not change a feature
  the model was trained on, unless training used the new value. Check the training script (#1508:
  CostUSD moved to the stored bill, which training used; the gbs stop feature stayed `tool_use`
  only).
- **Bit-equal money needs per-product rounding.** On arm64, `usd += r*n` fuses into one
  multiply-add. Round each product with an explicit `float64()` when a figure must equal a stored
  one (accountant #125, context-guru #1128).
- **A constant above 2^31 breaks 32-bit builds.** Accountant #129's `1 << 31` did not fit a 32-bit
  int. Build with `GOARCH=386` when you add a bound.

### Cost

- **The hot path must not get slower.** PR #813's prefilter passed every body with `\u`. Go's
  `encoding/json` escapes `<`, `>` and `&` (backslash, `u`, then 4 hex digits), so most tool
  outputs held a `\u` and paid the full parse, on a count that was about 22% of the rewrite path's CPU. PR #822
  decoded and re-encoded a message of up to several hundred KB on every request. Add a benchmark,
  and give the before and after figures in the PR.

### Tests

- **A test that passes on the old code proves nothing.** Run each new test against origin/main, or
  with the fix disabled, and report the failure. PR #822's first-block test passed under the old
  rule until its fixture was changed.
- **A test that checks source text is not a test.** Drive a real request through the test harness.
- **Run the full non-race suite of each touched package**, and of its direct dependents if an
  exported API changed. Use `-race` only where [CLAUDE.md, Working on a task](../../../CLAUDE.md#working-on-a-task)
  allows it: not on `proxy` or `dash` while the interim rule holds. Run `go vet` and `gofmt -l` at
  the final head, with no output filter.
- **Find the name of a failing test before you call it flaky.** Keep the full output in a file. Then
  run the same test on an unmodified origin/main.
- **A change to dash SQL needs the proxy tests too.** Proxy tests drive `/api/cachestate`, so #1521
  broke a proxy test that the dash tests did not see. Run the packages that read the endpoint.
- **A test that builds the state by hand can skip the real order.** In #1536, a test set the
  accountant record by hand and passed. The real job path read it before the job was joined, so the
  feature never ran. Test through the real path (start, goroutine, join).

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
  server. Two agents answered an older message (in the 2026-11-26 run, several times), and one
  reported "done" for work it had listed as a follow-up.
- **Repeat open items.** When you send a second message, list every item still open. An agent may
  act on only the first message.
- **Rebase after main moves.** A rebase moves the head, so a PR with a real conflict needs one more
  review round at the new head.
- **Two fix agents can add the same new helper.** #1507 and #1508 each added a pricer adapter.
  Name one owner for each new shared helper, and stack the other PR on it.
- **A stacked PR after its base squash-merges.** `approval-still-valid.sh` says CHANGED, because
  it measures against main's old merge base. Compare the PR's own sorted `-U0` +/- lines at the
  approved head (against its stacked base) and at the new head (against main), as RELEASING
  "Stacked PRs" says (#1508).
- **Agents stop mid-edit on a gateway error.** Two agents stopped on an API 502 with uncommitted
  work. Check the worktree's `git status` before you resume them.
