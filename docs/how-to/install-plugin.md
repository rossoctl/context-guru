# Install as a Claude Code plugin

No toolchain, reversible, and the routing decision is per repo.

```
/plugin marketplace add rossoctl/context-guru
/plugin install context-guru@context-guru
/reload-plugins
/context-guru:install
```

The first two are once per machine. The last is once per repo, and it is the one that decides which
sessions get routed.

**`/reload-plugins` is not optional, and skipping it looks like a broken plugin.** `/plugin install`
tells you to run it, and until you do, this session has no `/context-guru:*` skills — so the next
line answers `Unknown command: /context-guru:install` on a perfectly good install. Starting a fresh
session works too; reloading is just quicker.

### Step 2 asks you to pick a scope — this is what it means

`/plugin install` offers three, and they decide who gets **the plugin**:

| Option | Written to | Who gets it |
|---|---|---|
| **Install for you** (user scope) | `~/.claude/settings.json` | you, in **every project** on this machine |
| Install for all collaborators (project scope) | `<repo>/.claude/settings.json` | **everyone who clones the repo** — this file is committed |
| Install for you, in this repo only (local scope) | `<repo>/.claude/settings.local.json` | you, this repo only — gitignored |

**User scope** is what the rest of this page assumes: install once, then decide routing per repo.
The trade is that both hooks and ~225 always-on tokens apply to every session you start anywhere.
That is why the hooks self-gate on `ANTHROPIC_BASE_URL` naming their own port — in a project you
never routed they exit immediately and print nothing.

**Project scope commits a proxy plugin to a shared repository.** Everyone who clones then gets a
`SessionStart` hook that launches a local proxy and a `UserPromptSubmit` hook that runs before every
prompt. That is a team decision, not a personal one; do not pick it on someone else's behalf.

**Local scope** is the cleanest way to evaluate: one repo, gitignored, nothing left in your user
configuration afterwards.

**This is not the same question as the routing scope**, which `/context-guru:install` asks separately
and which decides *which sessions go through the proxy* ([table below](#which-file-the-routing-goes-in)).
They are independent: a local-scope plugin still routes only the repo you run the install skill in,
and a user-scope plugin does not route anything until you ask it to.

### `/plugin configure` — four options, all with working defaults

You can open it, press **Save configuration**, and change nothing. Only one of these usually needs
setting, and only in one situation.

| Option | Default | Change it when |
|---|---|---|
| **Proxy port** | `8787` | something already holds 8787. Deliberately not 4000, which collides with litellm |
| **Preset** | `cache` | you want more than the prompt-cache split. `cache` drops no content, adds no tools and makes no model calls |
| **Idle exit** | `24h` | rarely. The floor is `max(2 × store.ttl_seconds, 1h)`; below it the proxy refuses to start rather than silently discarding cache state |
| **Upstream base URL** | *(empty)* | **something else is already the gateway** — see below |

**Upstream base URL is the one that matters on a hosted agent.** Empty means "forward straight to
`api.anthropic.com`". On a platform that supplies its own local gateway — a coding-agent pod, a
managed workspace, a corporate proxy — that is wrong twice over: the gateway holds the credential,
and it may rewrite model names. One pod maps `claude/haiku…` to the real model id, so a proxy that
bypasses it sends model names the API has never heard of and **every** request fails.

Set it to whatever `ANTHROPIC_BASE_URL` already contains, and the proxy chains behind that gateway
instead of replacing it: your `Authorization` / `x-api-key` passes straight through, so the gateway
keeps authenticating and keeps translating models.

Setting it here is slightly better than letting `/context-guru:install` detect it. `start-proxy.sh`
reads this option directly, so the `SessionStart` hook chains correctly in every session without
depending on the settings `env` block having been written. The install skill writes
`ANTHROPIC_UPSTREAM` there as well, and the two agree.

### If a permission prompt or an auto-mode denial stops the install

Starting the proxy routes this session's API traffic through a locally-installed binary, and Claude
Code treats that as worth asking about. In interactive use you get a prompt to approve; under auto
mode the classifier may deny it outright, with something like:

```
Denied by auto mode classifier ∙ [Traffic Redirection] ... a local proxy intercepting all Claude
API traffic before forwarding to an unverified upstream
```

**That is a reasonable objection, not a bug to route around.** Installing a plugin by name is not the
same as consenting to have your API traffic intercepted, and those are genuinely two decisions.

Three ways through, in the order worth trying:

1. **Approve it when asked.** One approval, and the install finishes.
2. **Run the two commands yourself** with the `!` prefix in Claude Code, which makes the consent
   yours rather than the agent's. The skill prints them if it is blocked.
3. **Add a permission rule** if you would rather not be asked each time. Rules match by command
   prefix, so name the script:

   ```json
   {"permissions": {"allow": ["Bash(~/.claude/plugins/cache/context-guru/**)"]}}
   ```

   Adjust the path to what your install actually reports. The plugin cannot grant this to itself, by
   design — a plugin that could approve its own traffic interception would be worth distrusting.

## You do not need an API key

Setting `ANTHROPIC_BASE_URL` **without** a credential variable leaves your claude.ai login
alone: a Pro or Max subscription keeps working, with your usage limits and billing unchanged.
You can evaluate context-guru on your own sessions with no API key at all.

One honest caveat: on subscription billing the saving lands in **usage limits**, not dollars. The
cost figures on `/context-guru:status` and the dashboard are list-price estimates, and they will
not match a subscriber's bill — because a subscriber does not get one per request.

Do not let the installer add `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN`. It will not, and
that is deliberate: a credential variable is exactly what would move you off subscription
billing and onto metered API billing.

## What gets installed where

| Thing | Scope | How often |
|---|---|---|
| Plugin and its skills | your user settings | once per machine |
| Proxy binary | `~/.local/bin` | once per machine |
| **Routing — the `env` block** | **this project by default**, `--global` opt-in | **once per repo** |
| The proxy process | started on demand, exits when idle | automatic |

Only the routing decision is per-repo, and deliberately: it is the one with blast radius.

## Which file the routing goes in

Settings precedence runs managed → `--settings` → `.claude/settings.local.json` →
`.claude/settings.json` → `~/.claude/settings.json`, so user scope is the *lowest*.

| Write target | Reaches | If the proxy is down |
|---|---|---|
| `.claude/settings.local.json` | you, this repo, gitignored | one repo — **the default** |
| `.claude/settings.json` | everyone who clones the repo | one repo, whole team |
| `~/.claude/settings.json` (`--global`) | every project on the machine | **every Claude Code session you have** |

The default is project-local because a global base URL pointing at `localhost` means a dead
proxy breaks Claude Code everywhere, including repos you never meant to experiment in. `env`
blocks merge **per key** across scopes, so a user-scope install is not clobbered by a repo that
ships its own `env` block, and a `--global` install needs no per-repo caveat.

## What it does to your requests

The default preset is `cache`: [`cachesplit`](../components/cachesplit.md) and nothing else.

- No content dropped, no summarising, no `<<cg:HASH>>` markers.
- No extra tool added to your requests, and no model calls.
- One oversized system block is split into two adjacent text blocks whose concatenation is
  byte-identical, so the model sees exactly the prompt your agent sent. The cache breakpoint
  moves onto the half that does not churn.

You can check that claim in one line of `config/config.go`. That is the point of the preset.

**When it will save you nothing, which a first run often is.** All three of these are silent — the
numbers are simply zero:

| Condition | Why |
|---|---|
| **You are not in a git repository** | Claude Code emits no environment snapshot, so there is no volatile tail to split. This is the common case for a casual trial, and `/context-guru:status` checks for it. |
| Your system prompt is under ~1,024 tokens | Below `minSplitTokens` the split is refused: the extra breakpoint slot costs more optionality than it recovers. |
| A non-Anthropic backend (vLLM, llm-d) | They match an implicit longest prefix and stop at the divergence by themselves. |

And even in the good case, be calibrated about the size: the headline **−34.1%** figure comes from
a benchmark harness running tasks back-to-back inside the provider's 5-minute cache TTL. On this
project's own interactive traffic the measured figure is **$0.0298 across 1,127 sessions** — because
Claude Code captures the environment snapshot once per session, and 1,105 of 1,127 session starts
found the previous prefix already expired. The mechanism needs a second session inside five
minutes; humans mostly do not work that way.

**Anthropic-family only.** `cachesplit` is a no-op against implicit prefix-cache backends
(vLLM, llm-d), which stop at the divergence by themselves.

## Lifecycle

The plugin installs a `SessionStart` hook that starts the proxy if it is not already running.
Three things about it worth knowing:

- **It runs in every project, and does nothing in almost all of them.** The hook exits
  immediately unless `ANTHROPIC_BASE_URL` names its port — so it acts only where you configured
  routing. Remove the env key by hand and the hook stops firing on its own.
- **It is synchronous**, so the proxy is answering `/healthz` before your session's first
  request goes out. It also fires on `clear`, `compact`, `resume` and `fork`, and is idempotent:
  it never starts a second proxy.
- **The proxy exits by itself** after no requests and no keep-alive ping pending. **24h is the
  value the plugin passes**, not the flag's default — `--idle-exit` defaults to `0`, meaning never,
  because a gateway must not self-terminate. Liveness probes (`/healthz`, `/metrics`) deliberately
  do not count as activity, so a monitoring loop cannot silently keep the proxy alive forever; an
  open dashboard tab does count, because somebody is watching.

A committed `.claude/settings.json` containing a hook prompts for trust when someone clones the
repo. That is correct behaviour, but it means "clone and go" is really "clone, approve, go".

## Then

- `/context-guru:status` — is it routed, is it up, and what has it saved. Reads `/stats`.
- Dashboard: `http://127.0.0.1:8787/dashboard/`. The four billed token tiers are where the cache
  effect shows: tokens moving out of the premium cache-**creation** tier into the discounted
  cache-**read** tier. Its database lives in `~/.local/state/context-guru/`, deliberately not in
  your repository — the proxy's own default would write `./context-guru-dashboard.db` into whatever
  directory it started in.
- Note `/stats` reports `acted: 0` and `saved_tokens: 0` even on a turn where the split worked:
  `acted` counts components that removed content, and this one relocates a cache breakpoint. The
  signals that do move are `components.cachesplit.verdict` and the billed tiers.
- `/context-guru:uninstall` — removes the one settings key (with a backup) and stops the proxy.

## Troubleshooting

**"Nothing happened after `/context-guru:install`."** The setting applies to a **new** session;
the one you ran it in already has its environment. Start a new session.

**A prompt hangs with no output at all.** That is a dead proxy on a routed project — the failure
has no error message of its own. The `UserPromptSubmit` hook should catch it and say so; if it
cannot, start the proxy by hand
(`context-guru-proxy --listen 127.0.0.1:8787 --preset cache`) or remove
`env.ANTHROPIC_BASE_URL` from `.claude/settings.local.json` to get working immediately.

**Upgrading.** `/context-guru:install` reports what is installed and does not replace it. To move to
a newer release, run the installer with `CONTEXT_GURU_UPGRADE=1`, or pin one with
`CONTEXT_GURU_VERSION=vX.Y.Z`.

**Requests fail with a connection error.** The proxy is not running and this project is routed.
`/context-guru:status` will say so; the log is `${TMPDIR:-/tmp}/context-guru-proxy-<port>.log`.
To get working again immediately, `/context-guru:uninstall`.

**"The proxy binary is not on PATH."** The installer puts it in `~/.local/bin`. Add that to your
`PATH` — the session hook runs with your normal environment and cannot find it otherwise.

**Port 8787 is taken.** Change it in the plugin's configuration. Do not use 4000; litellm
defaults to it, and the installer's default avoids the collision on purpose.

**`--idle-exit` is refused at startup.** A threshold below roughly 5h34m (2× the store's default
entry lifetime) is rejected, because exiting clears in-memory cache state — including frozen
decisions, whose loss re-bills a whole prefix as cache creation. Raise the threshold, or raise
`store.ttl_seconds` if the short lifetime is deliberate.
