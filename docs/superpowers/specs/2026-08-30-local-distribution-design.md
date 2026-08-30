# Local distribution: get context-guru running on a stranger's Claude Code in one command

## Problem

Someone finds the repo, wants to know whether it saves them money on their own Claude
Code sessions, and today has to: install Go 1.26, install a C toolchain, build with the
right tags, run a binary, work out `ANTHROPIC_BASE_URL`, and trust that routing their
coding agent through an unknown local proxy will not break it. Most evaluators stop at
step one.

The value we want them to feel first is **KV-cache**, not the offloaders. That decides
almost everything below, because cache work requires being on the wire — so this proposal
is about making *the proxy* trivial to install, not about avoiding it.

Target: **two commands, no toolchain, no guide, reversible.**

```
/plugin marketplace add rossoctl/context-guru
/context-guru:install
```

## What we ship

Five pieces. Each is independently useful; the order is the order they unblock each other.

| # | Piece | Why |
|---|---|---|
| 1 | Pure-Go release binaries + Homebrew tap | Removes the toolchain gate entirely |
| 2 | `--idle-exit` self-terminating proxy | Nothing left running on the machine |
| 3 | `cache` preset | The KV-cache-only pitch, fully lossless |
| 4 | A plugin: install skill + `scripts/` + `SessionStart` hook | One command, and an agent that can make the judgment calls |
| 5 | Gateway conformance fixes | Routing must not break their agent |

### 1. Pure-Go binaries

`docs/get-started/quickstart-proxy.md` tells every evaluator to set `CGO_ENABLED=1` and
install a C toolchain. Reading the dependency tree, that looks unnecessary for the default
build:

| Dependency | cgo? |
|---|---|
| `tiktoken-go/tokenizer` | **No.** `internal/tokens/tokens.go` says it outright: *"o200k_base is embedded in the binary (pure-Go, offline, no CGO)"* |
| `modernc.org/sqlite` (dashboard) | **No** — pure Go by design |
| `tree-sitter/go-tree-sitter` | Yes — but gated behind `//go:build cg_skeleton`, with `internal/treesitter/stub.go` for `!cg_skeleton` |

So without `-tags cg_skeleton` there should be no cgo dependency at all.

**Gate A — verify before anything else** (30 seconds, and it decides the shape of all of
piece 1):

```sh
CGO_ENABLED=0 go build -o /tmp/cg ./cmd/context-guru-proxy
```

If it passes we get a **static single binary**: a plain `GOOS`/`GOARCH` matrix in one CI
job, no C cross-toolchains, no zig, no libc coupling, ~20 lines of GoReleaser. If it
fails, cross-compiling four platforms needs per-platform runners and the binaries carry
libc requirements — still doable, several times the work.

`skeleton` is the only casualty, and it costs nothing here: it is not in `codesmart` (the
default) and not in the cache story at all. Ship pure-Go binaries for everyone; a
`-skeleton` variant can follow per-platform, or not at all initially.

**Distribution, ranked by friction:**

1. **Homebrew tap** — `brew install rossoctl/tap/context-guru`. One command, **no Gatekeeper
   dialog**, plus updates and clean uninstall. The primary path for macOS and Linux.
2. `go install` — one command, but puts the toolchain back.
3. `curl … | sh` — one command, but macOS quarantines an unsigned download ("cannot be
   verified"). Fixable with `xattr -d com.apple.quarantine` in the installer, though
   `curl | sh` reads badly to this repo's audience. Notarization needs a paid Apple account.
4. Plugin `bin/` — committing per-platform binaries to git is ugly, and `bin/` is barred
   from claude.ai org distribution.

Decision: **tap first, `curl | sh` as fallback, `go install` documented for Go users.**

### 2. `--idle-exit` — the proxy cleans itself up

Lifecycle is tied to sessions (piece 4 starts it), so nothing should outlive use. The
machinery already exists in `cmd/context-guru-proxy/main.go`: SIGINT/SIGTERM handling,
`srv.Shutdown`, and `armShutdown` to release the dashboard's SSE connections. An idle
watchdog just triggers the same path — atomic last-request timestamp, ticker, exit 0.

Three constraints, all load-bearing:

- **Off by default.** A gateway or eval-containers deployment must never self-terminate.
  `--idle-exit=24h` is opt-in and set only by the installer.
- **The threshold has a floor: `store.ttl_seconds`.** Exit wipes the in-memory Store —
  rewind stashes, frozen decisions, `cg:len:`. A frozen decision dying mid-session is the
  11.5× cache-write bug that `FrozenLost` exists to detect. The default TTL is 10000s
  (≈2.8h), so 24h is safely past it and nothing live is lost — but a *short* threshold (10
  or 30 minutes) would be actively destructive. **Enforce the floor in config validation,
  not in a doc comment.**
- **The keepalive inverts the meaning of idle.** If pings are the point, the proxy has to be
  alive precisely when there is no client traffic — idle is when it works (X=280s → ping). A
  naive watchdog kills the feature it ships. The idle clock must also reset on keepalive
  activity, and exit only when no session has a pending ping schedule. 24h ≫ the keepalive
  window (K=2 ≈ 10 min) so they coexist, but that has to be explicit rather than lucky.

Self-kill alone is a footgun; self-kill **plus** `SessionStart` resurrection is coherent.
They ship together or not at all. Longer term a SQLite-backed store makes self-kill free at
any threshold — `store` already has the interface seam for it.

### 3. A `cache` preset

`pipeline: [cachesplit]` and nothing else. This is the funnel's default.

Why it is the right first taste, and not a watered-down one:

- **Fully lossless.** No content dropped, no `<<cg:HASH>>` markers, no `context_guru_expand`
  tool injected, no LLM calls. The loudest objection to a context proxy — *you are editing
  my agent's context* — does not apply, and that is verifiable by reading one config line.
- **It is the best-evidenced component we have**: −34.1% cost, 0% → 96.7% cache hit in an
  isolated A/B, and it already ships in every preset.
- **The win is visible in the dashboard's four billed token tiers**, which come from the
  provider's usage block.

`safe` (`format` → `cachesplit`) is close but still rewrites JSON; `cache` makes the claim
exact. Note `cachesplit` and `cacheinject` are both no-ops on implicit prefix-cache backends
(vLLM, llm-d) — `apply/prefixsplit.go` says so — so the funnel's pitch is Anthropic-family
specific and the docs should say that plainly.

### 4. The plugin: skill + `scripts/` + hook

A plugin **cannot** set `ANTHROPIC_BASE_URL` — plugin `settings.json` accepts only `agent`
and `subagentStatusLine`. So the plugin is not the transport; it is the **installer and
operator surface**, and it closes the loop by having Claude do the file edit.

```
context-guru-plugin/
  .claude-plugin/plugin.json      # userConfig: preset, port, scope
  skills/install/SKILL.md         # /context-guru:install  — drives scripts/, makes the judgment calls
  skills/status/SKILL.md          # /context-guru:status   — reads /stats, explains the numbers
  skills/uninstall/SKILL.md       # /context-guru:uninstall — removes the env key, stops the proxy
  scripts/install.sh              # brew → curl fallback; idempotent; prints machine-readable result
  scripts/settings.py             # merge/remove ONE key in a settings.json, with backup
  scripts/start-proxy.sh          # healthz probe, start only if absent, fixed port
  hooks/hooks.json                # SessionStart → start-proxy.sh
```

**Why a skill rather than a shell script alone.** A skill does not reduce permission
prompts, it *concentrates* them — an eight-step guide becomes eight prompts unless the skill
runs one command. The real reason for the skill is the part a script does badly:

`init` must merge into an existing `~/.claude/settings.json` that already holds the user's
theme, model, permission rules, and possibly their own `ANTHROPIC_BASE_URL`. That is
judgment work — read it, back it up, add exactly one key, detect a conflicting base URL
already present, choose a scope, verify, and report. An agent handles that; `jq` gymnastics
against an unknown file does not. Every maintainer's own settings file is the proof: at
least one of ours already sets a base URL plus a benchmark override that must survive.

So: **`scripts/` for the deterministic steps, the skill for the judgment and the verify.**

**Scope choice, which the skill must ask about.** Precedence is managed → `--settings` →
`.claude/settings.local.json` → `.claude/settings.json` → `~/.claude/settings.json`, so user
scope is the *lowest*.

| Write target | Reaches | Blast radius if the proxy is down |
|---|---|---|
| `.claude/settings.local.json` | you, this repo, gitignored | one repo — **the install default** |
| `.claude/settings.json` | everyone who clones the repo | one repo, whole team — team adoption |
| `~/.claude/settings.json` | you, every project on the machine | **every Claude Code session you have** |

A global base URL pointing at `localhost` means a dead proxy breaks Claude Code everywhere,
including repos the evaluator never meant to experiment in. For a trial aimed at strangers
that blast radius is the main risk in this proposal — bigger than the build. Hence: default
to project-local, `--global` as an explicit opt-in, and `--idle-exit` paired with the
`SessionStart` hook so "is it running?" stops being the user's problem.

**Why `SessionStart` and not a launchd/systemd service.** Session-scoped lifetime leaves
nothing running on the machine and needs no privileged install. Mechanics check out:
`SessionStart` supports `type: "command"`, hooks are awaited unless `async: true` is set, and
the default timeout is 600s — so a hook that starts the proxy and polls `/healthz` before
returning closes the race with the first API request. Four details:

- **Never set `async: true`** on this hook; it reintroduces the race.
- **`SessionStart` also fires on `clear`, `compact`, `resume` and `fork`**, not just
  `startup`. The starter must be idempotent — probe `/healthz`, start only if absent.
  Compaction alone re-fires it repeatedly in a long session.
- **Fixed port.** The env block and the hook have to agree, and you cannot negotiate a
  dynamic port after the env value is already written. Configurable at install time, written
  to both places. Not 4000 — that collides with litellm.
- **Trust gate.** A hook in a committed `.claude/settings.json` that launches a binary
  prompts on clone. Correct behaviour, but it means "clone and go" is really "clone, approve,
  go", and the README should say so rather than surprise people.

Note the plugin also gives us `SessionEnd` and `PreCompact`/`PostCompact` as explicit
signals, which is strictly better than `proxy/agentcompaction.go` inferring compaction from a
string match against a specific Claude Code build. Out of scope here; worth recording.

### 5. Gateway conformance

This funnel puts us on the wire, so a broken trial is a lost adopter. Checked against the
gateway protocol reference; each needs a test, and #1 and #2 are the ones that can make the
demo look *negative*:

1. **Do not buffer SSE.** *"A gateway that buffers complete responses before relaying them
   stalls the client"*, and Claude Code aborts a stream silent for 300s. We have
   `sse_buffered` / `sse_buffered_pct` counters, so we do buffer some responses to inspect
   for an expand call. Under the `cache` preset nothing injects the expand tool, so this
   should be inert — verify it against a real session rather than assume.
2. **A rejected `cache_control` disables prompt caching for the rest of the conversation.**
   Claude Code retries and turns the capability off. So a breakpoint-budget mistake is not an
   error the user sees — it silently switches off the thing we are selling. This is the
   strongest argument for shipping `cache` (no `cacheinject`) rather than a placement preset.
3. **The attribution block is stripped positionally.** Claude Code prepends it as the first
   `system` block and the API strips it only if the array arrives unchanged. `cachesplit`
   reshapes that array. Either prove the interaction is safe (our split has a
   `minSplitTokens` floor of 1024, so it should never touch the small attribution block) or
   ship `CLAUDE_CODE_ATTRIBUTION_HEADER=0` in the install.
4. **Forward error bodies byte-unmodified.** Claude Code's capability-rejection retry matches
   on the upstream's error *wording*; wrapping errors breaks the recovery path.
5. **Serve `/v1/messages/count_tokens`.** Absent, Claude Code counts context by issuing
   inference requests. Cheap to add.

Confirmed already correct: `copyHeaders` forwards `anthropic-beta` (which carries the OAuth
capability — stripping it is a `401`), and auth headers are dropped only when a real API key
is configured.

## Subscription auth works, and it is the headline

Setting `ANTHROPIC_BASE_URL` **without** a credential variable keeps the user's claude.ai
login active: their Pro/Max usage limits and billing continue to apply. Combined with the
above, an evaluator can trial context-guru **with no API key at all.**

That is the single most important adoption fact about this project and it appears nowhere in
our docs. One caveat to state honestly in the docs: on subscription billing the saving lands
in usage limits rather than dollars, so `/stats` cost figures are list-price estimates that
do not match a subscriber's bill.

## Non-goals

- **Not a replacement for the proxy.** This is packaging for the proxy, and the proxy stays
  the only place cache work happens.
- **No offloaders in the funnel default.** They are a separate story (see the plugin-transport
  analysis, PR #129) and lossy-by-design, which is the wrong first impression for a cache pitch.
- **No measurement-only mode.** A plugin that merely reports what cache expiry costs was
  considered and rejected: without the ability to ping, it diagnoses a problem it cannot fix.
- **Not the DAM integration.** DAM is harness-plural with its own gateway; a Claude Code
  plugin covers one harness. Separate proposal.

## Open questions for reviewers

1. **Gate A** — does `CGO_ENABLED=0` build? Everything in piece 1 scales off this answer.
2. **Does an `env` block merge per-key across settings files, or is the whole object
   replaced by the highest-precedence file?** The docs say *lists* merge and call `env` "an
   ordinary key". If it is replaced wholesale, a user-scope install silently stops working in
   any repo that ships its own `env` block — i.e. exactly the most-configured repos. Needs a
   10-minute test.
3. **Default scope: project-local or global?** Local is safer, global is the better demo. The
   proposal picks local; happy to be overruled.
4. **Idle-exit default: 24h?** And should the floor be `max(2 × store.ttl_seconds, 1h)`?
5. **Homebrew tap: `rossoctl/homebrew-tap` as a new repo, and who owns release signing?**

## Staging

- **Stage 0** — Gate A + the `env` merge test. Half a day. Decides the rest.
- **Stage 1** — GoReleaser + tap + `cache` preset + fix the CGO claim in the quickstart. The
  funnel already works here for anyone willing to run two commands by hand.
- **Stage 2** — `--idle-exit` + the plugin (skill, scripts, `SessionStart` hook).
- **Stage 3** — conformance items 1–5 with tests.

Stage 1 is shippable alone and is most of the adoption win; stages 2–3 make it pleasant and
safe.
