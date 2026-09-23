# Community

context-guru is a [Rossoctl](https://github.com/rossoctl) platform
component, licensed under **Apache-2.0**.

## Contributing

!!! note "Sign-off is mandatory"
    Every commit must be signed off under the
    [DCO](https://developercertificate.org/):

    ```sh
    git commit -s -m "feat: ..."
    ```

    This adds a `Signed-off-by:` trailer. PRs with unsigned commits will not be
    merged.

- **AI attribution** — when a commit was assisted by an AI agent, attribute it
  with an `Assisted-By:` trailer. Do **not** use `Co-Authored-By`, and do not add
  "Generated with" lines.
- **Conventional-commit titles** — `feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`. Keep PRs focused; CI (lint, test, build, Trivy) must be
  green.
- **Every reduction must be reversible and fail-open** — add tests for both the
  happy path and the fault-injection (fail-open) path.

## Running the eval harness locally

Want to check a change against real benchmark traffic before opening a PR? [Setup & example
run](setup.md) walks through building the binary/image and driving a SWE-bench task through
context-guru as the eval-containers gateway, including the `sweep.py` matrix runner for
comparing configs across many tasks.

## Governance & policies

| Document | |
|---|---|
| [CONTRIBUTING.md](https://github.com/rossoctl/context-guru/blob/main/CONTRIBUTING.md) | How to contribute, DCO, PR norms. |
| [GOVERNANCE.md](https://github.com/rossoctl/context-guru/blob/main/GOVERNANCE.md) | Project governance model. |
| [MAINTAINERS.md](https://github.com/rossoctl/context-guru/blob/main/MAINTAINERS.md) | Current maintainers. |
| [SECURITY.md](https://github.com/rossoctl/context-guru/blob/main/SECURITY.md) | Reporting security issues (do not open public issues for vulnerabilities). |
| [CODE_OF_CONDUCT.md](https://github.com/rossoctl/context-guru/blob/main/CODE_OF_CONDUCT.md) | Community standards. |
| [LICENSE](https://github.com/rossoctl/context-guru/blob/main/LICENSE) | Apache-2.0. |
