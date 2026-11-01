# Lessons from review

The reviewers of the 2026-11-01 run found these mistakes in the fixes. Put this file in every fix
agent's brief. Add a lesson when a later run's review finds a new kind of mistake.

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
  outputs held a `\u` and paid the full parse, on a count that was about 22% of the rewrite path's CPU. PR #822
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
