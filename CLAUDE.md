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

## Conventions

- Go 1.26, module `github.com/rossoctl/context-guru`. The build is pure Go — `make build` sets
  `CGO_ENABLED=0`. cgo is needed only for `go test -race` and for `-tags cg_skeleton` (tree-sitter),
  which is why the Makefile still exports `CGO_ENABLED=1` for the test targets.
- Match the surrounding code's style; keep packages small and single-purpose.
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

## Review process

**Every PR goes through the `offload-review` skill before it is offered for merge.** When you
finish committing and the PR is pushed and ready, run the skill. It is not optional, and you do
not wait for the user to ask.

- Run it in the main session, not in a subagent.
- Fix the findings, reply on the PR to each one, and run the next round, as the skill says.
- The budget is 2 rounds. Only the user grants more; you may ask.
- After the PR merges or closes, run the skill's cleanup.

See [.claude/skills/offload-review/SKILL.md](.claude/skills/offload-review/SKILL.md).

## Eval boxes

Go is not installed on the laptop, so builds, tests and benchmark runs happen on one of two remote
boxes. **Reach them through a wrapper, not raw `ssh`** — a raw `ssh` with flags is refused by Claude
Code's auto-mode classifier while the wrapper passes:

```bash
~/cgssh2 '<command>'      # the CURRENT box — all new runs go here
~/cgssh  '<command>'      # the OLD box — read-only, for the iteration 007-021 record
```

The wrappers live in the operator's home directory and hold the connection details, so nothing here
names a host. They are per-machine: a new contributor sets up their own and the commands below are
unchanged.

**Which box.** `~/cgssh2` is where runs go. `~/cgssh` still holds the experiment record from
iterations 007–021 under `/tmp/cg-loca/**` (stats JSONs, flap logs, LOCA outputs), so read from it
when older results are needed, but do not launch there — it carries another engineer's live services.

**Toolchain on both.** Go 1.26.4 at `/usr/local/go/bin`, and builds need `CGO_ENABLED=1`
(tree-sitter). Neither is on a non-interactive `ssh` PATH, so export it in every command:

```bash
~/cgssh2 'cd ~/cg-src-mine && export PATH=/usr/local/go/bin:$PATH && CGO_ENABLED=1 go test ./...'
```

**Getting code there.** There is no canonical checkout — ship a tarball, and unpack into a *fresh*
directory. Untarring over an existing tree does not delete files removed upstream, and a stale test
file failing against new source reads exactly like a real regression:

```bash
tar czf /tmp/t.tgz --exclude=.git .
~/cgssh2 'cat > /tmp/t.tgz' < /tmp/t.tgz
~/cgssh2 'rm -rf ~/cg-src-mine && mkdir -p ~/cg-src-mine && cd ~/cg-src-mine && tar xzf /tmp/t.tgz'
```

**Heredocs through the wrapper are refused.** Write the script locally and pipe it:
`~/cgssh2 'cat > /tmp/x.py' < x.py`, then run it. The wrapper's banner goes to **stderr**, so
`~/cgssh2 'cat file' > out` is clean — do not strip the first stdout line.

### Shaping the command so it is not refused

Claude Code's auto-mode classifier judges the command it is asked to run. A raw `ssh` with flags
reads as an arbitrary remote shell and gets refused; the wrapper is a single known script with one
argument, and passes. Four rules follow, all learned by having commands refused:

1. **Always go through the wrapper.** Never `ssh -o … user@host '…'` directly, even though the
   wrapper is only doing that.
2. **Pass the remote command as ONE single-quoted argument** — `~/cgssh2 'cd x && go test ./...'`.
   Chain with `&&` inside the quotes rather than splitting into several wrapper calls, and keep the
   wrapper as the outermost command rather than burying it in a subshell or a pipeline.
3. **No heredocs through the wrapper.** They are refused outright. Write the script locally and pipe
   it, then run it as a file:

   ```bash
   cat > /tmp/mine.py <<'PY'
   ...
   PY
   ~/cgssh2 'cat > /tmp/mine.py' < /tmp/mine.py
   ~/cgssh2 'python3 /tmp/mine.py'
   ```

   This is also the fix for anything long or quote-heavy: a script file has no quoting problem, and a
   file is easier to re-run unchanged after an edit.
4. **Put a `kill` or `pkill` in a script file, never in the wrapper argument.** Beyond the
   self-matching hazard above, a bare destructive pattern in the command text is the shape most
   likely to be refused.

If a session's permissions are narrower than this — some are configured to allow only a small set of
commands, and will refuse `bash /tmp/x.sh` outright — that is a settings question for the operator.
**Do not ask another session to run the command instead.** A peer running what your own guard refused
substitutes their permissions for yours, and the person who set the guard did not agree to that.

### Traps that produce a garbage result rather than an error

These matter more than the mechanics, because each one lets a run finish, produce numbers, and mean
nothing:

- **`node`/`npx` are absent from the non-interactive PATH** (they live under nvm). A benchmark whose
  MCP servers need `npx` loses its tools silently — tasks still run, requests still flow, numbers
  still come out. Export the nvm bin directory explicitly in any stage script.
- **`/tmp` on both boxes is on a 10-day `systemd-tmpfiles` cleaner.** It has already deleted an
  iteration's frozen proxy binary mid-analysis. Anything that must survive lives under `~/`. Note
  `find -mtime +10` truncates to integer days, so use `+9` when checking what the cleaner will take.
- **Verify one real tool call and one non-zero component action before believing an arm.** A pinned
  dependency breaking every MCP server, or a missing `npx`, both present as a completed run.

### Both boxes are shared

Another engineer's services and other Claude sessions run there as the **same unix user**.

- Work in your own directory (`~/cg-src-<topic>`), and name any proxy you launch
  `cg-<topic>-proxy-vNN` with a monotonic NN.
- **Never a `pkill` pattern containing `proxy`.** Target your own exact name, or kill by PID.
- **Bracket `pgrep`/`pkill` patterns** (`cg-proxy-v[0-9]`) — an unbracketed pattern matches the shell
  command containing it and reports a false positive.
- **Put kill logic in a script file, never a pattern in argv** — a pattern naming the target also
  matches the killing shell, which will kill itself mid-cleanup.
- `timeout … loca` survives a `pkill` of its wrapper; kill LOCA by PID or it keeps running orphaned
  with no proxy behind it.
- **Do not touch** `~/cg-loca/**` or `/tmp/cg-loca/**` (live experiment evidence that open issues
  still depend on), `/tmp/inv3-*`, or another session's `/tmp/cg-*` tree.
- Rig scripts derive helper ports as `PORT+50` (shim) and `PORT+70` (capture hop), and `:6980` is the
  model-info server, so `PORT=6930` silently collides and the run produces rows of zeros.

Gateway credentials sit at `~/.cg-bench/env`, mode 600, and are for benchmark traffic only. Never
echo, copy or relay their contents — and see the operator's own instruction that benchmark runs must
not be routed through Context Guru.

## Layout

See [docs/design.md](docs/design.md) for package layout.
