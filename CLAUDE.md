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

- **No AuthBridge / cortex code lives here.** That plugin is built in
  `cortex` and depends on this repo. Keep the public API (`components`,
  `apply`, `schema`, `config`) clean and importable; never reach into another repo.
- **Fail open, always.** Any component error/panic reverts that component only; the
  original request is always forwarded as a valid fallback. Every lossy Offload must be
  reversible (a `<<cg:HASH>>` marker + the stashed original in the Store).

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

`components` (Component/Reformat/Offload + Pipeline + registry) · `components/{reformat,offload,dsl,all}`
· `apply` (wire body ⇄ pipeline, byte-lossless splice) · `schema` (bifrost-schema helpers) ·
`expand` (marker + expand tool loop) · `store` · `session` · `metrics` · `config` ·
`proxy` · `adapters/bifrost` · `cmd/context-guru-proxy` · `internal/{tokens,treesitter,buildinfo}`
· `deploy` · `docs`. See [docs/design.md](docs/design.md).
