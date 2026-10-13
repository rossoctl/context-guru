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
  dependence and its behaviour with storage off, and updates
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

## Eval boxes

Go is not installed on the developer laptops. Builds and tests run on a shared Linux box, through
`~/cgssh2 '<command>'`.

- **Run remote builds and tests in the foreground by default.** Use a long Bash timeout (up to
  10 minutes) and one package set per call. `./proxy/...` takes about 7.5 minutes and `./dash/...`
  about 6 minutes with `-race`, so run them as separate calls.
- **A subagent runs remote work in the foreground only.** It is not woken by its own
  `run_in_background` task once it ends its turn, so it must never end the turn to wait.
- **A detached remote run is for main sessions only,** and only when the run takes more than
  10 minutes. Start ONE local poller with `run_in_background: true` that waits for the run's done
  marker, with a timeout inside it. Then make no tool calls until its notification arrives. A job
  on the box by itself is not a task the local harness tracks, so without the poller nothing
  wakes you.
- **Write the done marker on ANY exit,** with the exit code:
  `trap 'echo "DONE rc=$?" >> ~/<run>.out' EXIT`. A marker written only at the end of a `set -e`
  script is never written when a test fails, so a failure looks like a timeout.
- **Never spin-wait.** Do not call `true`, `echo` or `sleep` to pass time. Each call is a paid model
  request on the whole context.
- **Never re-sync code to the box while a run is in progress there.** It mixes two versions of the
  code within one test run.
- **Commit a WIP before any long run.**
- **`go` is not on the non-interactive PATH.** Use `/usr/local/go/bin/go`. `cgo` is needed for
  `-race` (`CGO_ENABLED=1`).
- **Sync with tar, not rsync, into an EMPTY directory:**
  `~/cgssh2 "rm -rf ~/<branch-dir> && mkdir -p ~/<branch-dir>"`, then
  `tar czf - --exclude .git . | ~/cgssh2 "tar xzf - -C ~/<branch-dir>"`. `tar` adds and overwrites
  files but never deletes one. A file you removed locally (a test, for example) stays on the box
  and keeps running against the old behaviour.
- **The box is shared** with other engineers and sessions, as the same Unix user. Kill only by
  pid. Never `pkill` or `killall`. Never assume the box is idle.
- **Two tests fail on the box itself**, not on your change: `TestInstallRefusesAnUnverifiedDownload`
  and `TestInstallReportsPATHFromTheSourceFallbackToo` (the box has a root-owned
  `/usr/local/bin/context-guru-proxy`). Before you believe a failure is yours, run the same test on
  an unmodified `origin/main` on the box.

## Layout

See [docs/design.md](docs/design.md) for package layout.
