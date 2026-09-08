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

### Recommended first: grant the plugin's scripts once

Not required, and worth doing anyway — on any deployment where sessions run under auto mode or a
restrictive permission policy, the install otherwise stops partway with the proxy running and the
routing key unwritten. Add this rule before you start, via `/permissions` (paste the rule **alone**, not
a JSON object) or in a settings file:

```
Bash(/absolute/path/to/.claude/plugins/cache/context-guru/**)
```

Use the absolute path — `~` is not expanded in permission rules — and take the path from what
`/context-guru:install` prints if you are unsure. One rule covers every command the plugin runs.

**Why it is needed at all is worth understanding rather than pasting past.** Starting the proxy, and
writing the routing key, are the two steps that put your model traffic — and the credential that travels
with it — through a locally installed third-party binary. Claude Code is right to treat "the user
installed a plugin called context-guru" as different consent from "the user approved intercepting their
API traffic". The rule is you saying the second thing explicitly, once. Details and the alternatives are
in [If the install is blocked](#if-the-install-is-blocked).

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

### Hosted agents and managed workspaces

Anywhere the platform supplies its own local gateway — a coding-agent pod, a managed dev workspace, a
corporate proxy injected into the environment — the install takes a slightly different shape. It is a
supported shape, not a workaround, and it is worth recognising because the signal is easy to miss:

```bash
echo "$ANTHROPIC_BASE_URL"     # already set, and not by you
```

If that is set to a local port that is not ours, **the platform's gateway is doing real work.** It holds
the credential, and it may rewrite model names — one platform maps `claude/haiku…` onto the real model
id, so a proxy that forwards straight to `api.anthropic.com` sends model names the API has never heard
of and every request fails. So context-guru goes **in front of** that gateway rather than replacing it:
your `Authorization` / `x-api-key` passes straight through, and the gateway keeps authenticating.

Three things to expect, and none of them are errors:

| What you see | What to do |
|---|---|
| `ANTHROPIC_BASE_URL` set in the environment, no settings file mentions it | chain: set **Upstream base URL** to that URL in `/plugin configure` |
| `on_path=false` from the install | nothing — the skill passes the absolute path itself, and persists it as `CONTEXT_GURU_BIN` so later sessions' hooks find it |
| the proxy start or settings write denied | the [permission rule](#recommended-first-grant-the-plugins-scripts-once) above, or approve once |

The install writes three keys in that case rather than one — routing, the upstream to chain behind, and
the binary path — all in one atomic edit that `/context-guru:uninstall` reverses.

**One thing to check before bothering:** if the workspace's working directory is not a git repository,
the `cache` preset has nothing to act on and the saving is exactly zero. See
[What it does to your requests](#what-it-does-to-your-requests).

### If the install is blocked

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
3. **Add a permission rule** if you would rather not be asked each time.

   With `/permissions`, paste **the rule on its own** — not a JSON object. Pasting the whole snippet
   stores the literal text as one allow entry, which matches nothing and looks like it worked:

   ```
   Bash(/home/you/.claude/plugins/cache/context-guru/**)
   ```

   Use an **absolute** path rather than `~`. Editing a settings file by hand instead, the same rule is:

   ```json
   {"permissions": {"allow": ["Bash(/home/you/.claude/plugins/cache/context-guru/**)"]}}
   ```

   Adjust the path to what your install actually reports. The plugin cannot grant this to itself, by
   design — a plugin that could approve its own traffic interception would be worth distrusting.

   One rule covers every command the plugin runs, but only because they are all invoked *directly* and
   with no environment prefixes. A command like `FOO=1 /path/to/script.sh` begins with `FOO=1`, so no
   rule naming the script can match it.

**Why prefix matching is worth knowing here.** A rule naming this script covers
`.../start-proxy.sh --unrouted --upstream <url>` but **not** `SOMEVAR=1 .../start-proxy.sh`, because
the second command does not begin with the script path. That is why every option the install needs is
passed as an argument: an env-prefixed command is one nobody can approve. If a skill ever prints you a
command with env prefixes in front of the script, a permission rule will not help and you are back to
approving each time.

**A correctly-formed permission rule does clear these gates — confirmed.** With the rule above in place
before the install, both steps that had been denied ran without a prompt and the install completed on a
hosted agent. If you are denied *with* a rule in place, check the rule is a bare `Bash(...)` string with
an absolute path, not a JSON object pasted into `/permissions`.

**Without a rule, expect TWO gates, not one.** Both were observed under auto mode, and both are
correct:

1. **starting the proxy** — *"intercepts and forwards the agent's own Anthropic API traffic"*;
2. **writing the routing key** — *"routes all future API traffic (including the Authorization
   credential …) through a locally-run proxy sourced from a third-party marketplace plugin the user
   only generically installed"*.

The second is the sharper objection and it is worth reading twice, because it is the real decision:
routing means your credential passes through this binary. On a chained install it has to — that is how
the platform gateway keeps authenticating. Checksum verification proves the download matches what the
repo published; it does not make the code trustworthy, and there is no signature anywhere in this path.

So on a hosted agent, an unattended install cannot complete, by design. Either approve both steps,
run both commands yourself with `!`, or add a rule covering the plugin's `scripts/` directory — one rule
covers both, since both are that directory's scripts.

**And on a hosted agent, check whether the trial can show you anything before doing any of it.** The
`cache` preset works by moving a cache breakpoint inside the environment snapshot Claude Code appends
to its system prompt. **Outside a git repository there is no such snapshot**, so `cachesplit` reports
`verdict: skipped` and the saving is exactly zero — a structural zero, not a warm-up. A pod whose
working directory is not a repo will measure nothing no matter how long you leave it. `/context-guru:status`
says so explicitly; believe it rather than waiting for numbers to appear.

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

## Status line

`/context-guru:statusline` puts this session's own running savings in your terminal's status
line, so you see them on every turn instead of asking `/context-guru:status`:

```
$0.03/12k saved of $0.41/187k
```

That is: this session has saved $0.03 and 12k tokens so far, out of $0.41 and 187k tokens this
session has spent in total. Both halves are scoped to THIS session — the savings figure is the
proxy's own `/api/stats?session=<id>` (the same totals `/context-guru:status` shows, but filtered
to one session rather than the proxy's whole retained window), and the total is read straight off
Claude Code's own statusLine payload (`cost.total_cost_usd`, `context_window`), so it costs no
extra network call. Hidden entirely before this session has spent anything at all — before that,
there is nothing to divide by, not a broken feature; a real `$0.00/0 saved` prints once there is a
real total to compare it to. Same list-price caveat as `/context-guru:status` applies to the money
side.

**`cg!`** means the proxy this project routes to is not answering. This is the one thing that
still shows regardless of the toggles below — everything else about the line is silent, including
in every project that is not routed through context-guru at all.

### Extras, off by default

Two more segments exist and are hidden until you turn them on — the toggle is one line, no
restart needed beyond a new session:

- **`cache 4:12` / `cache cold`** — a countdown to the prompt-cache going cold, from Claude
  Code's own client-side cache tracker (the same one behind its `/context` view). `cache –`
  before the first response of a session has anything to track is normal, not a fault.
  ```bash
  "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file ~/.claude/settings.json \
    --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --cache"
  ```
- **`ka 2p`** — how many idle keep-alive pings have fired this window. Hidden while zero even
  once turned on.
  ```bash
  "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file ~/.claude/settings.json \
    --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --keepalive"
  ```
  (Both flags can be passed together.) Drop the flag and re-run the same command to turn an
  extra back off.

**It only reads.** The status line renders on nearly every keystroke; if it could also *arm*
anything, a display hook would double as an unbounded traffic generator billed to you. Sending a
keep-alive ping is a separate, explicit action, whether or not its counter is shown here:

`/context-guru:keepalive` reports whether the mechanism is on, and turns it on or off. Turning it
on means the proxy will spend your own credential on idle turns, between sessions, to keep the
cache warm — worth knowing before you enable it, which is why this asks rather than infers.

One nuance worth having straight: once keep-alive is on, `cache cold` in the status line (once
you have turned that segment on) no longer means the provider's own cached entry is actually
cold. The countdown is Claude Code's own view of *its own* last request; it has no way to see a
ping the proxy sent while you were not typing. Treat `ka Np` and the next turn's latency as the
ground truth once keep-alive is running, not the countdown by itself.

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

**The status line is blank in a routed project.** Blank is its normal state before the first
response of a session — this session has nothing to report yet, so there is no total to show
savings against. Check `/context-guru:status` for the same numbers to confirm it is not simply
early. If it stays blank across a whole session with real traffic, confirm
`/context-guru:statusline` actually installed (`settings.py show` reports `statusline=`), and that
a *new* session was started after installing it — like the routing key, the setting is picked up
live but only in a session that started after it was written.

**`--idle-exit` is refused at startup.** A threshold below roughly 5h34m (2× the store's default
entry lifetime) is rejected, because exiting clears in-memory cache state — including frozen
decisions, whose loss re-bills a whole prefix as cache creation. Raise the threshold, or raise
`store.ttl_seconds` if the short lifetime is deliberate.
