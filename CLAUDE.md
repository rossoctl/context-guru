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
  The checklist is in [What goes in the proxy](#what-goes-in-the-proxy).
  The rule for code that names one client is in [Client-specific code](#client-specific-code).
- **Fail open, always.** Any component error/panic reverts that component only; the
  original request is always forwarded as a valid fallback. Every lossy Offload must be
  reversible (a `<<cg:HASH>>` marker + the stashed original in the Store).
- **Metadata first.** A component or feature works from metadata only (hashes, counts, sizes,
  ids, flags). Depending on stored customer data (transcript text, tool output, bodies,
  summaries) is a deliberate exception. Every PR that adds or changes a feature states its data
  dependence and what it does at each storage level (off, metadata only, everything). It adds or
  edits that feature's row file under `docs/data-dependence/` (`components/<name>.md` or
  `features/<slug>.md`), then runs `go run ./cmd/gen-datadep` to rebuild the generated page
  [docs/data-dependence.md](docs/data-dependence.md). Never edit that page by hand, and on a
  conflict in it, run the generator again. Write the reason in plain words, not a code
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
- **Keep-alive never shortens the cache strategy.** A request sent in place of a ping may replace
  one only when it renews the same cache entry. See
  [docs/design.md](docs/design.md#keep-alive-the-cache-strategy-comes-first).

## What goes in the proxy

The host packages are `proxy/`, `proxyapp/`, `cmd/context-guru-proxy` and `dash/`. Cortex,
bifrost and the proxy must give the same behaviour from the same config. Use this test: **if
Cortex would have to copy the code to behave the same, the code is business logic.** Business
logic goes in a library package.

**Business logic. Never put it in a host package:**

- A decision on what to trim, offload, expand, cache, ping, defer, filter or compact.
- A policy choice: which strategy, override or campaign wins, and in what order they apply.
- Thresholds, defaults, bounds and calibrated constants, for example a prefix floor, an idle
  limit or a miss rate.
- Cost, price, savings and accounting math.
- Reading a provider wire format for meaning: usage, cache tokens, stop reasons, tool calls,
  cache marks, server-held history.
- Session, thread and trace identity.
- The order of the steps around a request: what runs before `apply`, and what each host must
  record after the response.
- Edits to the config document, for example adding a tool to `toolfilter.remove`.

**Host code. Keep it in the host package:**

- HTTP handlers that only decode the request, call a library function and encode the result.
- Byte forwarding, header copying, retries and time-outs on the transport.
- Credentials, auth modes and masking.
- Flags, env vars, file paths and other process wiring.
- Tenant login, rate limits, mail.
- Prometheus registration and dashboard display.

**Rules for a change in a host package:**

1. Before you add a function to a host package, ask the Cortex test. If the answer is yes, put
   the function in a library package and call it from the host.
2. Do not copy a library rule into the host "for now". Call the library function. If it does
   not fit, change the library function.
3. A constant that a library package already has is not defined again in a host package.
4. A library type that only `internal/` holds cannot reach Cortex. When a public host needs it,
   move it out of `internal/`.
5. A comment such as "mirrors X" or "keep in sync with Y" in a host package is a defect. Fix it
   by calling X.
6. A PR that adds logic to a host package states why it cannot live in a library package. The
   reviewer checks this.

`offload-review` checks each PR for a new leak, and `refactor-scan` checks each release.

## Client-specific code

Clients with agent rules (Claude Code, Codex, OpenCode, Pi, Bob) differ in headers, session ids and
prompts. Other names that the dashboard shows as labels (cursor, cline, aider, gemini-cli) have no
rules. Use this test: **if a rule names one client, it lives in `agent.Identify`, and nowhere else.**

**Client rules. Keep them in `agent.Identify` and the agent packages:**

- A header, User-Agent value, tool name, marker or prompt phrase that only one client sends.
- A client's session id, subagent id, compaction phrase or label.
- The choice of which client sent a request.

**Not client rules. Keep them where they are:**

- A rule that holds for every client, such as "an `end_turn` with nothing open".
- A signal that a host sets for all clients, such as `x-context-guru-session`.
- The fallback when no client is found. It is the generic rule.

**Rules for a client rule:**

1. Find the client by its headers and User-Agent, not by the route path. A path may break a tie.
   One exception: an agent that owns a route, such as Bob's gateway.
2. Library code takes the `agent.Client` value. It never reads User-Agent or a vendor header again.
3. Every client rule states its fallback: no headers, another client, or a host without the
   signal (Cortex, the bifrost adapter). The fallback is never worse than the generic result.
4. A new client signal cites the client's source and version. Never guess a header or a phrase.
5. Code in `proxy/` or `internal/` follows [What goes in the proxy](#what-goes-in-the-proxy). In
   addition, it must not depend on the proxy's route paths.
6. A PR that adds a client rule lists each supported client as covered or fallback, and updates
   [docs/client-coverage.md](docs/client-coverage.md).

The code is in `agent/identify.go` and the table is in `docs/client-coverage.md`. `offload-review` checks each PR for a client rule outside `agent.Identify`.
`refactor-scan` checks each release. `agent/all/coverage_test.go` fails when a registered agent has
no row in the coverage table.

## Conventions

- Go 1.26, module `github.com/rossoctl/context-guru`. The build is pure Go — `make build` sets
  `CGO_ENABLED=0`. cgo is needed only for `go test -race` and for `-tags cg_skeleton` (tree-sitter),
  which is why the Makefile still exports `CGO_ENABLED=1` for the test targets.
- Match the surrounding code's style; keep packages small and single-purpose.
- **Tests wait on a condition, not on time.** In `*_test.go`, use `testutil.WaitFor`,
  `WaitForState` or `(Upstream).WaitForCall` instead of `time.Sleep`, a fixed-deadline poll loop
  or a `time.After` timeout. `internal/timingguard` fails CI on a new one. A real case (an injected
  fake delay) carries `//timing:ok <reason>`. See [CONTRIBUTING.md](CONTRIBUTING.md).
- **Commits: DCO sign-off is mandatory** — `git commit -s`. Author as the repo owner.
  AI attribution uses `Assisted-By:` — never `Co-Authored-By`, never a "Generated with"
  line. Conventional-commit titles.
- Observability: every exported metric is `cg_*` / `cg_tenant_*`, except the common set the proxies
  push to the central collector: those are `llm_*` (metrics spec, #886). OpenTelemetry **GenAI
  semantic conventions** (`gen_ai.*`) are the intended direction and are not yet emitted.

## Branches, worktrees and force-push

Owner rule (David Amid, 2026-11-12): "subagents should work on their own worktree and own
branch and then they can do any force push they want, no?"

- Every agent works in its own worktree, at `.claude/worktrees/<topic>`, on its own branch. It
  never touches the main checkout.
- On its own branch, with no other writer, an agent may force-push.
- Use the explicit lease: `--force-with-lease=refs/heads/<branch>:<expected-sha>`. The bare flag
  fails when the remote is a URL.
- After a force-push on a PR that already has approvals, follow
  [When the head moves after an approval](RELEASING.md#when-the-head-moves-after-an-approval).
  An approval names one commit. Run a new `offload-review` round only when that section asks for one.
- Never force-push a branch that another person or session writes. Merge `origin/main` into it,
  without force.
- If a tool or a permission check blocks a push, stop and report. Do not route around it.

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

## Working on a task

Owner rule (David Amid, 2026-11-24): the integrator holder can change, so no rule here names
the person. The team knows who holds the role at any time.

- **The main session coordinates.** Subagents do the work. Each subagent uses its own worktree
  and branch, as in [Branches, worktrees and force-push](#branches-worktrees-and-force-push).
- **The main session runs `offload-review`.** See [Review process](#review-process).

### Interim rule (in force until the 3-tier test plan lands)

Owner decisions (David Amid): 2026-11-24 "in the meantime ask everyone to stop their heavy tests and
u coordinate it once flaky is done. so u run all tests every few prs till we get the process right.
rn everything is in livelock". 2026-11-25 "yes" (write it into CLAUDE.md, and make `check-ready.sh`
accept the waiver).

- **Do NOT run `scripts/dev/local-tests.sh` or `scripts/release/part-a.sh` on the shared laptop.**
- **Run only targeted tests** of the packages your PR changes: `go test ./<pkg>/...`. Do not use
  `-race` on the big suites `proxy` and `dash`. The node and python suites of a plugin you change
  are fine.
- **Hand off with this line** in the PR body or in a PR comment: `Local tests: waived (owner rule)`.
  `scripts/release/check-ready.sh` accepts it in place of the local-tests marker.
- **The integrator runs the heavy CI on main** (`scripts/release/heavy-ci.sh`) every few merges and
  before each cut. The integrator also runs Part A before each cut. A failure goes back to the PR
  that caused it.

The rest of this section, about `local-tests.sh`, applies after the interim rule ends.

### Timing tests

- **A timing test asserts a wall-clock bound or races a rate limit.** It starts with
  `testutil.RequireTiming(t)` (Go) or uses `timingOnly` from `opencode-plugin/test/helpers.mjs`
  (node). It runs only with `CG_TIMING_TESTS=1`.
- **`scripts/dev/local-tests.sh` skips the timing tests.** It runs them only when the PR adds or
  changes one (`scripts/dev/timing-tests-needed.sh` decides, from the diff). The integrator runs
  them in heavy CI and in Part A. That covers the Go tests and the OpenCode node tests (the step
  `node-timing`).
- **Gate a test only if its result depends on time or load.** Do not gate a test that uses a fake
  clock, or a long timeout as a safety net.

### A PR is ready when

- It has 2 approvals at its final head.
- CI is green. A PR runs only the fast jobs (lint-build, purego, trivy).
- The heavy tests passed on the laptop at the head (after the interim rule ends; until then, see the
  interim rule above: `Local tests: waived (owner rule)`). Run `scripts/dev/local-tests.sh <pr>` in the
  PR's worktree. It posts one comment with `<!-- local-tests sha=<head> result=pass -->`. A new
  commit, even comment-only, needs a new run. It takes 70 to 90 minutes: run it detached.
  A laptop failure of a test listed in `scripts/release/pending-timing-fixes.txt` does not block the
  PR, as long as the PR does not change that test's package. Check the list by hand.
- It merges cleanly on `main`.
- Every behavior change is named at the top of the PR and has a human developer's yes. See
  [Behavior changes need a human's permission](RELEASING.md#behavior-changes-need-a-humans-permission).
- The row in [docs/data-dependence.md](docs/data-dependence.md) is updated.
- It adds no binary and no LFS file.
- Exception: a docs-only PR needs only the release owner's review, by hand, unless the owner asks
  for the 2 reviews. See
  [RELEASING.md](RELEASING.md#when-the-head-moves-after-an-approval), where the rule stands.

For the details, see [Check that a PR is ready](RELEASING.md#check-that-a-pr-is-ready).

### Hand a ready PR to the integrator

1. Find the session named `integrator` with `ListAgents`.
2. Send one message to it with `SendMessage`. Use this block:

   ```text
   PR: <link>
   Head SHA: <sha>
   Approvals: round <n>, backend <name>, head <sha> (one line for each reviewer)
   CI: <result>
   Behavior change: <none, or the change and where the human's yes is recorded>
   Data dependence: <none, or the row and what changed>
   Binaries: <none>
   Squash notes: keep the Signed-off-by lines
   ```

3. If no session named `integrator` is listed, do not merge the PR yourself. Tell the human:
   "PR #N is ready. Please ask the integrator holder on Slack to review and merge it." Add the
   same block.

### After the hand-off

- After the merge, the PR-owning session runs `scripts/offload-review.sh <pr> --cleanup`. The
  session also removes its own worktrees and branches.
- When the integrator asks for "all done", the PR-owning session answers with its PR list. See
  [What "all done" contains](RELEASING.md#4-freeze).

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
