# context-guru for Pi

This Pi package routes [Pi](https://pi.dev)'s Anthropic and OpenAI models through a local context-guru proxy.
The proxy keeps the prompt cache warm while you are idle, and with a preset it carries less context.

Status: **first version.** It routes two of Pi's built-in providers:

| Pi provider | Wire API | Proxy URL | Forwards to |
|---|---|---|---|
| `anthropic` | Anthropic Messages | `http://127.0.0.1:<port>/anthropic` | `api.anthropic.com` |
| `openai` | OpenAI Responses | `http://127.0.0.1:<port>/openai/v1` | `api.openai.com` |

`openai-codex` (the legacy ChatGPT Plus/Pro login, which talks to `chatgpt.com/backend-api`) is not
routed: the proxy has no route for that backend. See [Limits](#limits).

## Install

Pi installs a package from a local path. Clone this repository, then:

```sh
pi install /path/to/context-guru/pi-package
```

Start Pi and run:

```text
/context-guru setup
```

Setup shows one question and changes nothing until you answer yes. On yes it:

1. Downloads the latest `context-guru-proxy` release and checks it against the release's SHA-256
   list. A `context-guru-proxy` already on `PATH` is used instead.
2. Starts the proxy as a user service: a LaunchAgent on macOS, `systemd --user` on Linux. The
   service starts at login and restarts after a crash. No Pi session starts or checks it.
3. Sets `providers.anthropic.baseUrl` and `providers.openai.baseUrl` in `~/.pi/agent/models.json`
   to the proxy URLs in the table above. Your models and login stay as they are. On Anthropic, an API
   key and a Claude Pro/Max login both work. On OpenAI, an API key works; for Sign in with ChatGPT
   see [Limits](#limits).
4. Copies a reset command out of the package, to `~/.local/state/context-guru-pi/context-guru-reset`.

The proxy starts first and the route is written second, so a failed start leaves Pi unrouted.

After setup, re-select the model with `/model`, or restart Pi.

**An install from the Anthropic-only version.** Run `/context-guru setup` again. It asks the same one
question, now naming both providers, and adds the OpenAI route. The Anthropic route and its record
stay as they are. Until you do, `/context-guru status` reports that a provider is not routed.

**A port someone else took.** If another program now holds the port the proxy used, setup picks a
free port and moves both routes to it. It does not refuse because of its own old URL.

## An existing baseUrl (a company gateway)

If `models.json` already sends `anthropic` or `openai` to another endpoint, setup asks one question
with three answers. Only a provider with its own `baseUrl` is affected. The other provider is routed
as usual.

| Answer | Route | Use it when |
|---|---|---|
| **Chain** (recommended) | Pi -> proxy -> your endpoint | Usually right. The endpoint still gets every request, with the same path, headers and key |
| **Replace** | Pi -> proxy -> the provider's public API | You want to stop using that endpoint. This usually breaks its auth |
| **Don't change anything** | As before | Nothing is written |

- Chain keeps the provider entry as it is (`headers`, `apiKey` and the rest). Only `baseUrl` changes.
- The proxy forwards the client's headers, so the endpoint's key and custom headers still arrive.
  The proxy removes only its own `x-context-guru-*` headers, `Cookie` and hop-by-hop headers.
- The proxy appends `/v1/messages` (Anthropic) or `/v1/responses` (OpenAI) to its upstream. So an
  OpenAI `baseUrl` of `https://gw.example/openai/v1` becomes the upstream `https://gw.example/openai`.
- The upstream goes on the service command (`--anthropic-upstream`, `--openai-upstream`) and into the
  install record. `preset`, `cache-strategy`, `update` and a new `setup` keep it.
- `/context-guru status` shows each route, for example `anthropic -> proxy -> https://gw.example/anthropic`.
- Uninstall and the reset command put your old `baseUrl` back.

From a shell, `pi_plugin.py setup --plan` prints `consent_question_chain` with `confirm_command_chain`,
and `consent_question_replace` with `confirm_command_replace`. Setup with a conflict and no
`--on-conflict chain|replace` refuses and changes nothing.

## Commands

| Command | What it does |
|---|---|
| `/context-guru status` | Proxy up or down, the port, the preset, the cache strategy, the dashboard link |
| `/context-guru preset [name]` | `off`, `conservative`, `medium`, `high`, `xhigh`, `ultrahigh`. Restarts the proxy after a yes |
| `/context-guru cache-strategy [name]` | `auto`, `1-ping`, `2-ping`, `none`. Restarts the proxy after a yes |
| `/context-guru update` | Checks for a newer proxy release and installs it after a yes |
| `/context-guru uninstall` | Restores `models.json`, stops and removes the service, deletes the downloaded binary |

`/context-guru` alone runs `status`. Every command that writes or restarts asks for a yes first. In print
mode (`pi -p`) there is no dialog, so these commands change nothing.

The presets and the cache strategies are the same as in the Claude Code plugin. See
[docs/reference/presets.md](../docs/reference/presets.md).

The default preset is `off`: the proxy trims nothing. The default cache strategy is `auto`: the proxy
sends small keep-alive requests while you are idle, which uses some of your own quota. Choose `none`
to turn the pings off.

One cache strategy drives both providers. What each one spends:

| Strategy | Anthropic | OpenAI |
|---|---|---|
| `none` | No pings | No pings |
| `auto` | 5-minute cache: first ping after 240 s idle, at most 2. 1-hour cache: 1 ping at 59 minutes | GPT-5.6 and later: a ping after 28 minutes idle, at most 2 |
| `1-ping` | At most 1 ping per idle gap | At most 1 ping per idle gap, after 28 minutes |
| `2-ping` | At most 2 pings per idle gap, on both cache lifetimes | At most 2 pings per idle gap, after 28 minutes |

A ping goes to the provider with your own credential, so it uses your API quota (or your
subscription's limits). The proxy pings only a session with a prefix of 80,000 tokens or more, and
never a ping that would cost more than $0.50. It does not ping OpenAI models before GPT-5.6: their
cache has no lifetime the proxy can plan for.

## What the extension does in a session

The extension makes **no network call** when a session starts or when Pi sends a request.

- **Session id.** It adds the header `x-context-guru-session: <Pi session id>` to requests that go to
  the proxy, on both providers. The proxy then keeps one session across a compaction, instead of two.
- **One owner of cache pings.** While the cache strategy is not `none`, it stops Pi's own cache
  warming for routed models. The proxy's keep-alive does that work, so the cache is not pinged twice.
  With `none`, Pi's own warming runs as usual. Pi 1.5.0 warms no built-in OpenAI model, so on OpenAI
  the proxy's keep-alive is the only one.
- **Sign in with ChatGPT on `openai`.** Pi leaves out four request fields that ChatGPT tokens reject
  (`prompt_cache_retention`, `prompt_cache_options`, `max_output_tokens`, `temperature`). Pi does
  that only when the model's URL is `https://api.openai.com/v1`, which a routed model no longer has.
  So for a routed `openai` model with a stored ChatGPT login, the extension removes the same four
  fields. An API key request is not changed. A chained `openai` provider is not changed either: Pi
  sent those fields to that endpoint before setup.
- **Session signals.** When a session ends or compacts, it tells the proxy. It sends only the session
  id, the event and the reason. The proxy then stops pinging a cache that no later request will read.
  The signal is sent after the fact and never delays Pi.

## Data

The package sends the proxy only metadata: the session id, and the session end and compaction
events. The proxy stores what its preset and storage level say. See
[docs/data-dependence.md](../docs/data-dependence.md).

## If Pi cannot talk

If every request fails, run the reset command from a normal shell:

```sh
~/.local/state/context-guru-pi/context-guru-reset
```

It removes the route from `models.json` first, so Pi works again at once. Then it stops the service.
It removes only what context-guru wrote: a `baseUrl` that you changed after setup stays as it is.

## Files

| Path | Owner |
|---|---|
| `~/.pi/agent/models.json` | Yours. Setup changes only `providers.anthropic.baseUrl` and `providers.openai.baseUrl`. Uninstall puts back each old value from that provider's own record |
| `~/.local/state/context-guru-pi/` | context-guru: install record, routing record, proxy config, log, dashboard database, binary. `models.json.before-context-guru` is a copy for you only; uninstall uses the routing record |
| `~/Library/LaunchAgents/io.rossoctl.context-guru-pi.plist` | context-guru (macOS) |
| `~/.config/systemd/user/context-guru-pi.service` | context-guru (Linux) |

`PI_CODING_AGENT_DIR` and `XDG_STATE_HOME` move these paths, as they do for Pi.

## Limits

- **Two providers:** `anthropic` and `openai`. Other providers go direct, as before.
- **`openai-codex` is not routed.** It sends a ChatGPT login to `chatgpt.com/backend-api` with
  Pi's `openai-codex-responses` API. The proxy has no route for that backend.
- **Sign in with ChatGPT on `openai` is not tested** against the real service. It needs the extension
  (see above). With extensions off (`pi --no-extensions`), use an API key, or run uninstall.
- **OpenAI classifier calls are not served.** Pi's `openai` provider also has a Decisions
  classifier (`/v1/decisions`). The proxy serves only `/v1/responses` and `/v1/chat/completions`, so a
  classifier call on a routed `openai` provider gets a 404.
- **One proxy for all Pi sessions on the machine,** because `models.json` is per user, not per project.
- **Chain needs a URL the proxy can reproduce.** An OpenAI `baseUrl` must end in `/v1`. A URL with a
  query, a user name or password, or one that is not `http(s)` cannot be chained. Setup then offers
  only replace or no change. See [An existing baseUrl](#an-existing-baseurl-a-company-gateway).
- **A `baseUrl` on one model** (`models.<id>.baseUrl` in `models.json`) wins over the provider's.
  Setup does not read or change it, so that model goes direct, as before.

## Development

```sh
python3 -m unittest discover -s scripts      # controller and models.json route
node --test tests/*.test.ts                   # extension helpers (Node 22.18+)
```
