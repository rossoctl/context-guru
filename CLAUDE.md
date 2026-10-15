# CLAUDE.md

Guidance for working in `context-guru` (repo dir `lab-context-engineering`), a Rossoctl
platform component.

## What this repo is

A single **Go** core (`components`) that reduces the token cost of LLM agent traffic,
operating on bifrost's provider-agnostic chat schema. It ships as a proxy binary
(`cmd/context-guru-proxy`), an importable library (`components`, `apply`, `schema`,
`config`, `expand`, `store`), a bifrost `LLMPlugin` adapter (`adapters/bifrost`), and
eval-containers wiring. Its lineage is the Python `winnow` prototype, the behavioral
reference — port its *logic*, re-implement its transport in Go.

## Hard boundaries

- **No sidecar-plugin code lives here.** That plugin is built in its own
  downstream repo and depends on this repo. Keep the public API (`components`,
  `apply`, `schema`, `config`) clean and importable; never reach into another repo.
- **Fail open, always.** Any component error/panic reverts that component only; the
  original request is always forwarded as a valid fallback. Every lossy Offload must be
  reversible (a `<<cg:HASH>>` marker + the stashed original in the Store).
- **Metadata first.** A component or feature works from metadata only (hashes, counts, sizes,
  ids, flags). Depending on stored customer data (transcript text, tool output, bodies,
  summaries) is a deliberate exception. Every PR that adds or changes a feature states its data
  dependence and what it does at each storage level (off, metadata only, everything), and updates
  [docs/data-dependence.md](docs/data-dependence.md) — the reason in plain words, not a code
  reference. If the PR changes a feature's data dependence in either direction, say so at the top
  of the PR, and have the person in that row's "Decided by" confirm it before merge.
  `components/all/datadep_test.go` fails when a registered component has no row.

## Conventions

- Go 1.26, module `github.com/rossoctl/context-guru`. The build is pure Go — `make build` sets
  `CGO_ENABLED=0`. cgo is needed only for `go test -race` and for `-tags cg_skeleton` (tree-sitter),
  which is why the Makefile still exports `CGO_ENABLED=1` for the test targets.
- Match the surrounding code's style; keep packages small and single-purpose.
- **Commits: DCO sign-off is mandatory** — `git commit -s`. Author as the repo owner.
  AI attribution uses `Assisted-By:` — never `Co-Authored-By`, never a "Generated with"
  line. Conventional-commit titles.
- Observability follows OpenTelemetry **GenAI semantic conventions** (`gen_ai.*`).

## Layout

See [docs/design.md](docs/design.md) for package layout.
