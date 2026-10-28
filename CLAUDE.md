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
- **Keep the core library separate from the proxy.** Logic and decisions live in library
  packages. The proxy (`proxy/`, `proxyapp/`, `cmd/context-guru-proxy`) is only one host of that library,
  like the bifrost adapter and the Cortex plugin. A library package never imports `proxy`.
  If a feature must work outside the proxy, move its logic into a library package. Then keep
  only the host-specific parts (transport, credentials, proxy-only config) in `proxy/`.
  External calls go through the host's `dispatch.Dispatcher`: the core builds the request, the
  host resolves the destination and credential and sends it. Credentials and their masking are
  each host's own business. See `docs/superpowers/specs/2026-10-13-dispatch-design.md`.
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
- **No behavior change for an edge case without a human developer's permission.** A fix
  that changes what the proxy or the library does for users or for other cases (a default, a
  time limit, what gets cached, offloaded or shown) needs a human developer's yes before it
  merges. This holds even when the change looks like a side effect of a narrow fix. Name the
  change at the top of the PR: who it affects, the old and the new behavior, and how to turn it
  off. An agent never decides this, and neither does a peer agent's report of a decision. See
  [RELEASING.md](RELEASING.md#behavior-changes-need-a-humans-permission).

## Conventions

- Go 1.26, module `github.com/rossoctl/context-guru`. The build is pure Go — `make build` sets
  `CGO_ENABLED=0`. cgo is needed only for `go test -race` and for `-tags cg_skeleton` (tree-sitter),
  which is why the Makefile still exports `CGO_ENABLED=1` for the test targets.
- Match the surrounding code's style; keep packages small and single-purpose.
- **Commits: DCO sign-off is mandatory** — `git commit -s`. Author as the repo owner.
  AI attribution uses `Assisted-By:` — never `Co-Authored-By`, never a "Generated with"
  line. Conventional-commit titles.
- Observability: every exported metric is `cg_*` / `cg_tenant_*`. OpenTelemetry **GenAI
  semantic conventions** (`gen_ai.*`) are the intended direction and are not yet emitted.

## How to write for me: ASD-STE100 style

Applies to every project and every message I must read: chat replies, issues, PRs, PR
reviews, commit messages, docs.

Write in the style of ASD-STE100 (Simplified Technical English). Less prose, less jargon.

- **Short sentences.** One idea per sentence. About 20 words at most in an instruction, 25 in a
  description.
- **Simple, common words.** Use one word for one meaning, and keep it the same all the way through.
  Prefer "use" to "leverage", "start" to "initiate", "help" to "facilitate".
- **Active voice.** Say who does what: "The sweep removes 2 outputs", not "2 outputs are removed".
- **No jargon without need.** If a technical term is necessary, say what it means the first time.
- **Lead with the result.** Then the facts, as numbered or bulleted points with real numbers.
- **Lists over paragraphs.** Bold the key fact in a point when it helps a fast reader.
- **No filler.** No hedging, no restating the question, no closing recap.

**Issues and PRs especially.** The description must be easy to read for someone who did not do
the work. Use short sections such as *Summary*, *What we found*, *Why it happens*, *Proposal* (or
*What changed* / *How it was tested* for a PR), *Open questions*. If deeper technical detail is
needed, put it in a separate *Technical notes* section at the end, so the main text stays simple.

## Review process

**Every PR goes through the `offload-review` skill before it is offered for merge.** When you
finish committing and the PR is pushed and ready, run the skill. It is not optional, and you do
not wait for the user to ask.

- Run it in the main session, not in a subagent.
- Fix the findings, reply on the PR to each one, and run the next round, as the skill says.
- The budget is 2 rounds. Only the user grants more; you may ask.
- After the PR merges or closes, run the skill's cleanup.

See [.claude/skills/offload-review/SKILL.md](.claude/skills/offload-review/SKILL.md).

## Layout

See [docs/design.md](docs/design.md) for package layout.
