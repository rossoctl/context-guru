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

## Conventions

- Go 1.26, module `github.com/rossoctl/context-guru`. The build is pure Go — `make build` sets
  `CGO_ENABLED=0`. cgo is needed only for `go test -race` and for `-tags cg_skeleton` (tree-sitter),
  which is why the Makefile still exports `CGO_ENABLED=1` for the test targets.
- Match the surrounding code's style; keep packages small and single-purpose.
- **Commits: DCO sign-off is mandatory** — `git commit -s`. Author as the repo owner.
  AI attribution uses `Assisted-By:` — never `Co-Authored-By`, never a "Generated with"
  line. Conventional-commit titles.
- Observability follows OpenTelemetry **GenAI semantic conventions** (`gen_ai.*`).

## Eval boxes

Go is not installed on the developer laptops. Builds and tests run on a shared Linux box, through
`~/cgssh2 '<command>'`.

- **Run remote builds and tests in the foreground.** Use a long Bash timeout (up to 10 minutes).
  Split a slower suite into several foreground calls.
- **Never start a remote run detached and then end the turn to wait for it.** A job on the box is
  not a task the local harness tracks, so nothing wakes an idle agent when it finishes.
- **Commit a WIP before any long run.**
- **`go` is not on the non-interactive PATH.** Use `/usr/local/go/bin/go`. `cgo` is needed for
  `-race` (`CGO_ENABLED=1`).
- **Sync with tar, not rsync:** `tar czf - --exclude .git . | ~/cgssh2 "tar xzf - -C ~/<branch-dir>"`.
- **The box is shared** with other engineers and sessions, as the same Unix user. Kill only by
  pid. Never `pkill` or `killall`. Never assume the box is idle.
- **Two tests fail on the box itself**, not on your change: `TestInstallRefusesAnUnverifiedDownload`
  and `TestInstallReportsPATHFromTheSourceFallbackToo` (the box has a root-owned
  `/usr/local/bin/context-guru-proxy`). Before you believe a failure is yours, run the same test on
  an unmodified `origin/main` on the box.

## Layout

See [docs/design.md](docs/design.md) for package layout.
