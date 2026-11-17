# Contributing

Thanks for your interest in `lab-context-engineering`, a component of the
[Rossoctl](https://github.com/rossoctl/rossoctl) platform.

## Developer Certificate of Origin (DCO)

All commits must be signed off under the [DCO](https://developercertificate.org/):

```sh
git commit -s -m "feat: ..."
```

This adds a `Signed-off-by:` trailer certifying you wrote or have the right to submit the
patch. PRs with unsigned commits will not be merged.

When a commit was assisted by an AI agent, attribute it with an `Assisted-By:` trailer —
do **not** use `Co-Authored-By` and do not add "Generated with" lines.

## Development

```sh
make fmt      # format + go mod tidy
make lint     # go vet + gofmt check
make test     # go test -race ./...
make build    # build ./bin/lab-cx
```

- Go 1.25+.
- Keep packages small and single-purpose; the engine core must stay transport-agnostic.
- Every reduction must be reversible and fail-open. Add tests for both the happy path and
  the fault-injection (fail-open) path.

## Waiting in tests

A test must not wait for a fixed time. A fixed wait fails on a slow CI runner.

- Wait for a condition: `testutil.WaitFor(t, what, cond)` or `testutil.WaitForState`.
- Wait for a request to a fake upstream: `testutil.NewUpstream(...).WaitForCall(...)`.
- Do not use `time.Sleep`, a loop with a `time.Now().Add(...)` deadline, or a `select` on `time.After(...)` as a test timeout.
- A real case (an injected fake delay, or a sleep that is the thing under test) takes a line comment, on the line or the line above: `//timing:ok <reason>`. The reason must not be empty.

The test `internal/timingguard` fails when a test file gains one of these patterns. It also fails when a count falls below its baseline, so the baseline only shrinks. Regenerate the baseline with:

    go test ./internal/timingguard -run TestTestTiming -update

## Pull requests

Use conventional-commit titles (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`).
Keep PRs focused. CI (lint, test, build, Trivy) must be green.

How a PR is reviewed and merged into a release, and how a release is cut, is in
[RELEASING.md](RELEASING.md).

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not open public issues for vulnerabilities.
