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

If one of your commits has no sign-off, do not rewrite the branch. Push a follow-up commit that
signs it off. Use the same name and email as the author of the unsigned commit, for the declaration
and for the follow-up commit itself. The DCO app compares them without regard to case, and it
rejects the commit with no message if they differ.

```sh
git commit -s --allow-empty \
  -m "chore: DCO remediation for commit <short sha>" \
  -m "I, Your Name <you@example.com>, hereby add my Signed-off-by to this commit: <full sha>"
```

- The declaration needs the full 40-character SHA. The short SHA in the subject is for people only.
- The DCO app accepts it for a commit you wrote. It does not accept it for a commit someone else wrote.
- `scripts/release/check-ready.sh` applies the same rule, so a remediated commit does not block the merge.
- The setting takes effect only after `.github/dco.yml` is on the default branch.

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

## Pull requests

Use conventional-commit titles (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`).
Keep PRs focused. CI (lint, test, build, Trivy) must be green.

How a PR is reviewed and merged into a release, and how a release is cut, is in
[RELEASING.md](RELEASING.md).

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not open public issues for vulnerabilities.
