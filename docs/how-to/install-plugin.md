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

**Do this first on any deployment where sessions run under auto mode or a restrictive permission
policy.** It used to be a convenience — the install would stop partway, with the proxy running and the
routing key unwritten. Since the install moved into `install.sh --route`, the consequence is different
and larger, and it was measured in a real non-interactive session rather than reasoned about:

`/context-guru:install` opens by running `install.sh --route --plan` in a `` !`` `` block, whose output
*is* the skill's input. If that command is denied, **the skill's instructions never reach the model at
all** — there is no install to stop partway, because nothing was ever read. In the measured run the
model correctly reported that it was blocked and asked for approval, and then improvised a suggestion
from the plugin's description alone: it proposed `--attach <the corporate gateway>`, which is not what
`--attach` does (that flag points at a proxy that already exists, not at a gateway to sit in front of).
That is precisely the interpretation this design exists to remove, and without the rule the design is
not in effect.

With the rule in place, the same session read the plan and presented the chain/replace/abort choice
generated from it, naming the existing gateway and what happens to it, and wrote nothing while waiting
for an answer.

Add the rule before you start, via `/permissions`, or in a settings file directly:

1. Run `/permissions` and select **add a new rule**.
2. Paste the rule below **alone** — not as a JSON object — replacing `you` with your own username
   (or the absolute path `/context-guru:install` prints, if you are unsure):

   ```
   Bash(/Users/you/.claude/plugins/cache/context-guru/**)
   ```

3. When asked **"Where should this rule be saved?"**, pick **Project settings (local)** —
   `.claude/settings.local.json`, gitignored, this repo only:

   ```
   ❯ 1. Project settings (local)
   ```

Use the absolute path — `~` is not expanded in permission rules. One rule covers every command the
plugin runs.

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

### Reproducing this flow in a sandbox (for people changing it)

Two traps, both of which cost a run:

**1. The project's `env` block applies to the session you are testing with.** To exercise the
conflict path you put an existing `ANTHROPIC_BASE_URL` in the test project's
`.claude/settings.local.json` — and Claude Code then uses that value for the test session's own API
calls. A placeholder like `https://gw.example/v1` makes the session unable to reach any API at all
(`ENOTFOUND`), and the run tells you nothing about the install. Use a **real, reachable** endpoint as
the pre-existing one; a corporate gateway is both realistic and functional.

**2. The plugin has to be registered, not just present.** Copying it under `~/.claude/plugins/` is not
enough. A sandbox `HOME` needs three files:

- `.claude/plugins/known_marketplaces.json` — the marketplace, `installLocation` pointing at the repo
  root (the `.claude-plugin/marketplace.json` there names `./context-guru-plugin` as the source);
- `.claude/plugins/installed_plugins.json` — `{"version": 2, "plugins": {"context-guru@context-guru":
  [{"scope": "user", "installPath": "<repo>/context-guru-plugin", ...}]}}`. Point `installPath` at a
  worktree to test that branch's code directly;
- `.claude/settings.json` — `enabledPlugins: {"context-guru@context-guru": true}`, plus
  `pluginConfigs` for the port, and the permission rule above if you want the `` !`` `` block to run.

Also pin `CONTEXT_GURU_STATE` and put a fake `context-guru-proxy` earlier on `PATH` — one that serves
real HTTP on the port, or the health check cannot pass and you will only ever test the failure path.
Run against a gateway that is **not** context-guru, so the measurement is not routed through the thing
being measured.

### Step 2 asks you to pick a scope — this is what it means

**Project scope is the default, and the script now enforces it.** `settings.py add` refuses to write
the machine-wide `~/.claude/settings.json` unless it is explicitly passed `--user-scope`, which the
install skill adds only after you have asked for `--global` and confirmed it. The reason is the
blast radius, not tidiness: a project-scope install that goes wrong costs you that project, while
the same mistake machine-wide takes out every Claude Code session you have — including the ones you
would use to fix it, and every project unrelated to context-guru. Routing per repo also means a
machine-wide base URL of your own keeps working everywhere else, because `env` blocks merge per key
with the most specific file winning.


`/plugin install` offers three, and they decide who gets **the plugin**:

| Option | Written to | Who gets it |
|---|---|---|
| **Install for you** (user scope) | `~/.claude/settings.json` | you, in **every project** on this machine |
| Install for all collaborators (project scope) | `<repo>/.claude/settings.json` | **everyone who clones the repo** — this file is committed |
| Install for you, in this repo only (local scope) | `<repo>/.claude/settings.local.json` | you, this repo only — gitignored |

**Local scope is the recommended pick, especially the first time.** One repo, gitignored, nothing
left in your user configuration afterwards, and no committed file for anyone else to inherit. If
you decide you want it everywhere, move to user scope later — there is no migration step, `/plugin
install` again with a different scope answer is the whole of it.

**User scope** is what the rest of this page assumes once you are past evaluating: install once,
then decide routing per repo. The trade is that both hooks and ~225 always-on tokens apply to every
session you start anywhere. That is why the hooks self-gate on `ANTHROPIC_BASE_URL` naming their own
port — in a project you never routed they exit immediately and print nothing.

**Project scope commits a proxy plugin to a shared repository.** Everyone who clones then gets a
`SessionStart` hook that launches a local proxy and a `UserPromptSubmit` hook that runs before every
prompt. That is a team decision, not a personal one; do not pick it on someone else's behalf.

**This is not the same question as the routing scope**, which `/context-guru:install` asks separately
and which decides *which sessions go through the proxy* ([table below](#which-file-the-routing-goes-in)).
They are independent: a local-scope plugin still routes only the repo you run the install skill in,
and a user-scope plugin does not route anything until you ask it to.

### `/plugin configure` — five options, all with working defaults

You can open it, press **Save configuration**, and change nothing. Only one of these usually needs
setting, and only in one situation.

| Option | Default | Change it when |
|---|---|---|
| **Proxy port** | `8787` | something already holds 8787. Deliberately not 4000, which collides with litellm |
| **Preset** | `off` | you want context editing at all. `off` is passthrough: no components, so nothing is dropped, no marker is written, no tool is injected and no model is called — a property of an empty pipeline rather than a promise about a full one. `house` and `codesmart` add the offloaders; `housellm` adds a compaction-model pass that spends on its own. Whether *anything* is spent under the default is decided by the cache strategy below, not here |
| **Cache strategy** | `5-min-ping` | you do not want keep-alive: this default holds the cache warm across idle gaps by pinging just under the provider's 5-minute TTL, and that **spends a little of your own quota** while nobody is at the keyboard. Under the default preset it is the *only* thing this plugin does. It targets a measured cost - idle cache misses were 23.6% of all spend over the measured window - but whether it nets positive on your traffic is what `keepalive_net_usd` reports, in `/stats`' own `savings` block (`--dashboard` required) — no need to open the dashboard for this one number. Not something to assume. `none` turns it off; `/context-guru:cache-strategy-picker` names each strategy and what it costs |
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

**And on a hosted agent, check whether the trial can show you anything before doing any of it.** Under
the default preset (`off`) the only mechanism running is the `5-min-ping` cache strategy — see
[What it does to your requests](#what-it-does-to-your-requests) below — and it only has something
to show once a session sits idle past the 5-minute cache TTL. A pod that is cycled quickly, or that never
goes idle, will measure nothing no matter how long you leave it. `/context-guru:status` says so
explicitly; believe it rather than waiting for numbers to appear.

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

The default preset is `off`: an empty pipeline. Nothing in the request body is touched — no
content dropped, no summarising, no `<<cg:HASH>>` markers, no extra tool added, no model call on
the request path. You can check that claim in one line of `config/config.go`.

Under the default preset the only thing the plugin actually does is keep-alive: the `5-min-ping`
cache strategy (`/plugin configure` → Cache strategy, above) pings just under the provider's
5-minute cache TTL so an idle session's prompt cache does not expire between turns. That **spends
your own quota**, and `/context-guru:cache-strategy-picker` is where you name it or turn it off
(`none`).

**When it will save you nothing, which a first run often is:** a fresh session has no idle gap to
keep warm yet, so the first request of any session reads zero — measured, 1,105 of 1,127 session
starts. `/context-guru:status` explains this rather than leaving you to wait for a number that
isn't coming.

Other presets (`cache`, `house`, `codesmart`, `housellm`) add components that also edit the request
body — see [docs/reference/presets.md](../reference/presets.md) if you opt into one of those.

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

- `/context-guru:status` — is it routed, is it up, and what has it saved. Reads `/stats`, including
  its `savings.{current,live,all}` block (with `--dashboard` on) — the same reconciled
  `keepalive_net_usd`/`total_saved_usd` the dashboard shows, without opening it.
  `/context-guru:status --stats` skips the narration and prints the raw `/stats` JSON verbatim.
- Dashboard: `http://127.0.0.1:8787/dashboard/`. The four billed token tiers are where the cache
  effect shows: tokens moving out of the premium cache-**creation** tier into the discounted
  cache-**read** tier. Its database lives in `~/.local/state/context-guru/`, deliberately not in
  your repository — the proxy's own default would write `./context-guru-dashboard.db` into whatever
  directory it started in.
- Note `/stats` reports `acted: 0` and `saved_tokens: 0` under the default preset — those count
  components that removed content, and none are running. The billed tiers and the keep-alive block
  are the signals that move.
- `/context-guru:uninstall` — removes the one settings key (with a backup) and stops the proxy.

## Status line

`/context-guru:statusline` puts this session's own running savings in your terminal's status
line, so you see them on every turn instead of asking `/context-guru:status`:

```
████····  100/200.0k 50% | $0.03/12k saved of $0.41/187k | proxy: 3ms · upstream: 340ms | ◇ github 1% remove
```

Four segments render by default, each independently — a segment whose numbers are not available
just does not print, rather than showing a zero:

- **The context bar** (`████···· 100/200.0k 50%`) — tokens used against the model's real context
  window, coloured green under 50%, yellow under 70%, red above. Read straight off Claude Code's
  own statusLine payload (`context_window.total_input_tokens` / `.context_window_size` /
  `.used_percentage`), so it costs no extra network call.
- **The savings-vs-cost figure** (`$0.03/12k saved of $0.41/187k`) — this session has saved $0.03
  and 12k tokens so far, out of $0.41 and 187k tokens this session has spent in total. Both halves
  are scoped to THIS session — the savings figure is the proxy's own `/api/stats?session=<id>`
  (the same totals `/context-guru:status` shows, but filtered to one session rather than the
  proxy's whole retained window), and the total is read straight off Claude Code's own statusLine
  payload (`cost.total_cost_usd`, `context_window`). Hidden entirely before this session has spent
  anything at all — before that, there is nothing to divide by, not a broken feature; a real
  `$0.00/0 saved` prints once there is a real total to compare it to. Same list-price caveat as
  `/context-guru:status` applies to the money side.
- **The latency split** (`proxy: 3ms · upstream: 340ms`) — ContextGuru's own added latency next to
  what the upstream provider took, each labelled so the two cannot be misread for one another.
  Both are `/api/stats`' own averages (`cg_latency_ms_avg` / `upstream_ms_avg`); nothing here is
  derived.
- **The unused-tool hint** (`◇ github 1% remove`) — the MCP server or skill THIS session used the
  least, checked against every MCP server configured in `~/.claude/settings.json` and every skill
  this plugin ships: `remove` at 1% or under of this session's tool/skill uses, `move` (to project
  scope, rather than global) at 20% or under, nothing at all above that. Session-scoped, not a
  system-prompt enumeration — nothing exposes what a given session's prompt actually loaded, so
  this checks what the transcript's own tail shows was actually called.

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
- **`ka ≤2miss $0.07`** — the cache misses the idle keep-alive pings prevented this window, and
  the NET money that saved (credit minus what the pings themselves cost). Shown only when that net
  is positive — a net loss, or an account with no keep-alive figures recorded at all, prints
  nothing rather than a zero.
  ```bash
  "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add --file ~/.claude/settings.json \
    --statusline "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/statusline.py\" --keepalive"
  ```
  (Both flags can be passed together.) Drop the flag and re-run the same command to turn an
  extra back off.

**It only reads.** The status line renders on nearly every keystroke; if it could also *arm*
anything, a display hook would double as an unbounded traffic generator billed to you. Sending a
keep-alive ping is a separate, explicit action, whether or not its counter is shown here:

`/context-guru:cache-strategy-picker` reports which named cache strategy is in effect and switches
between them. Idle keep-alive is the strategy named `5-min-ping`, and **it is now the install
default** — so the proxy will spend your own credential on idle turns, between sessions, to keep the
cache warm. That is what holds the cache across a gap, and it is bounded (at most 2 pings per idle
span, only on prefixes over 20k tokens, capped at $0.25 a ping) but it is not free. `split` is the
strategy that sends no pings at all, and switching to it is one word.

One nuance worth having straight: once keep-alive is on, `cache cold` in the status line (once
you have turned that segment on) no longer means the provider's own cached entry is actually
cold. The countdown is Claude Code's own view of *its own* last request; it has no way to see a
ping the proxy sent while you were not typing. Treat `ka ≤Nmiss $X.XX` and the next turn's latency
as the ground truth once keep-alive is running, not the countdown by itself.

## The escape hatch: when Claude Code cannot fix it for you

`/context-guru:uninstall` is a skill, so it needs a working session. The failure this plugin can
cause takes that away: every request goes through a local proxy, so a dead port, a stale routing
key or a credential the upstream rejects makes *every* API call fail — and the agent that would
undo the routing cannot reach a model to be asked. That is not hypothetical; it is the incident
this section exists because of.

So the install writes a hatch you can run yourself, and it lives **outside the plugin** — a
recovery tool inside the thing that broke is gone the moment you `/plugin uninstall`, refresh the
marketplace, or the plugin cache is wiped:

```bash
~/.local/state/context-guru/context-guru-reset          # always here
context-guru-reset                                      # on PATH too, when ~/.local/bin exists
```

It is POSIX `sh`. No Claude, no network, no proxy, no Python, no Go binary, no plugin code.

```bash
context-guru-reset --dry-run    # show what it would change, write nothing
context-guru-reset              # show the plan, ask, then restore
context-guru-reset --yes        # no prompt, for a script
```

What it does:

1. reads its record of every settings file the install edited — kept in
   `~/.local/state/context-guru/reset-manifest.tsv`;
2. copies each of those into `~/.local/state/context-guru/prereset/`, so running the hatch is itself
   reversible. Deliberately **not** beside the settings file: these are complete copies of a file that
   can hold a credential, and the old location dropped a new family of them inside your project's git
   working tree, where `git status` noticing them was the only thing between that and a committed
   secret;
3. restores each one from a copy taken **before the first edit**, held in
   `~/.local/state/context-guru/originals/` — or deletes the file, when the install is the reason
   it exists;
4. verifies no routing key is left, and reports anything it could not fix.

Why a separate copy when every edit already writes a `*.context-guru-backup-*` beside the file:
those are capped at ten and every add *and* remove writes one, so on a machine that has installed
and uninstalled a few times the backup holding your pre-context-guru state is the first to be
deleted. The copy under `originals/` is written once, with `O_EXCL`, and never pruned.

It never signals a process, never touches the network, and never edits a file it has no record of
editing. Any proxy still running exits on its own idle timeout.

### What it prints, and what it will not print

The plan shows a diff before it asks, because a restore reverts the whole file — including permission
grants Claude Code appended as you approved tools. That output is filtered, and the filter is an
**allowlist** for `"key": value` lines: values are printed only for keys known to be safe (the
routing keys, `model`, `theme`, `permissions` and friends), and anything else — including a
credential key nobody has thought of yet — is replaced with `<value not shown>`. Permission grants
stay visible because that is what the diff is for, and the routing keys keep scheme, host and first
path segment so you can still see which port you were pointed at.

It is an allowlist rather than a denylist of credential-ish names because the earlier denylist leaked
four times, in four shapes nobody had listed: `sk_live_`, a secret in a URL path, `?auth=`, and
`apiKeyHelper` with an object value. The same filter runs over every place this script echoes
content — the diff, the verify pass, the no-record grep, and the exported-variable lines.

### It restores routing, not credentials

Worth being blunt about, because the incident that prompted the hatch was **not** a routing fault.
Both `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` were set, the gateway rejected whichever one
won, and the export was a single line in a shell rc. No amount of settings surgery reaches that.

So the hatch ends with an environment report: whether both credential variables are set, whether
`ANTHROPIC_BASE_URL` is exported in your *shell* (where no settings file can override it), and the
`file:line` of every `ANTHROPIC_*` assignment in your shell startup files. It prints the locations
and never the values — a recovery tool that echoes a live key into your terminal buffer is not one.
Fixing those lines is yours to do; the hatch will not edit them.

### If there is no record

A hand-edited settings file, or a wiped state directory, and the hatch has nothing to restore
from. It says so, prints the three files worth checking, greps each for the keys, and lists the
timestamped backups beside them — rather than reporting "nothing to do" to somebody whose sessions
are down. Exit status is 3: finished, with something left for a human.

## Upgrading

There are two different things to upgrade, and they move independently.

**The plugin** (skills, hooks, scripts) updates the way any Claude Code marketplace plugin does.
Claude Code checks the marketplace in the background after a session starts and, if something
changed, prompts you to run `/reload-plugins` — or check on demand:

```
/plugin marketplace update rossoctl/context-guru
/reload-plugins
```

Update detection is driven entirely by the `version` field in the plugin's `plugin.json` — an
unchanged version means Claude Code never notices there is anything new, regardless of how much
code actually changed. To keep that from going stale, this repo's release workflow bumps the
plugin's version to match every proxy release automatically (a PR, not a silent push), so a proxy
release is always also something your installed plugin gets prompted to update to.

**The proxy binary** is the other half, and it upgrades separately: `/context-guru:install`
reports what is installed and does not replace it. To move to a newer release, run the installer
with `CONTEXT_GURU_UPGRADE=1`, or pin one with `CONTEXT_GURU_VERSION=vX.Y.Z`.

## Troubleshooting

**Every request fails or hangs, and `/context-guru:uninstall` cannot run.** Run this:

```bash
~/.local/state/context-guru/context-guru-reset
```

That is the whole fix. It needs no working Claude session — which is the point, because when routing
is what broke, the skill that would undo it cannot reach a model either. It restores every settings
file this plugin edited, prints what it changed, and ends by naming anything a file restore cannot
fix (a credential variable, an `ANTHROPIC_BASE_URL` exported in your shell). Add `--dry-run` first if
you want the plan without the change. Full detail: [the escape
hatch](#the-escape-hatch-when-claude-code-cannot-fix-it-for-you).

**"Nothing happened after `/context-guru:install`."** The setting applies to a **new** session;
the one you ran it in already has its environment. Start a new session.

**A prompt hangs with no output at all.** That is a dead proxy on a routed project — the failure
has no error message of its own. The `UserPromptSubmit` hook should catch it and say so; if it
cannot, start the proxy by hand
(`context-guru-proxy --listen 127.0.0.1:8787 --preset cache`) or remove
`env.ANTHROPIC_BASE_URL` from `.claude/settings.local.json` to get working immediately.

**Upgrading the plugin or the proxy binary.** See [Upgrading](#upgrading) above.

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
