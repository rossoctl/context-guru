# context-guru for Pi

This Pi package routes [Pi](https://pi.dev)'s Anthropic models through a local context-guru proxy.
The proxy keeps the prompt cache warm while you are idle, and with a preset it carries less context.

Status: **first version, Anthropic only.** OpenAI support comes in a follow-up change.

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
3. Sets `providers.anthropic.baseUrl` in `~/.pi/agent/models.json` to
   `http://127.0.0.1:<port>/anthropic`. Your models and login stay as they are, so an API key and a
   Claude Pro/Max login both work.
4. Copies a reset command out of the package, to `~/.local/state/context-guru-pi/context-guru-reset`.

The proxy starts first and the route is written second, so a failed start leaves Pi unrouted.

After setup, re-select the model with `/model`, or restart Pi.

## Commands

| Command | What it does |
|---|---|
| `/context-guru status` | Proxy up or down, the port, the preset, the cache strategy, the dashboard link |
| `/context-guru preset [name]` | `off`, `conservative`, `medium`, `high`, `xhigh`, `ultrahigh`. Restarts the proxy after a yes |
| `/context-guru cache-strategy [name]` | `auto`, `1-ping`, `2-ping`, `none`. Restarts the proxy after a yes |
| `/context-guru update` | Checks for a newer proxy release and installs it after a yes |
| `/context-guru uninstall` | Restores `models.json`, stops and removes the service, deletes the downloaded binary |

The presets and the cache strategies are the same as in the Claude Code plugin. See
[docs/reference/presets.md](../docs/reference/presets.md).

The default preset is `off`: the proxy trims nothing. The default cache strategy is `auto`: the proxy
sends small keep-alive requests while you are idle, which uses some of your own quota. Choose `none`
to turn the pings off.

## What the extension does in a session

The extension makes **no network call** when a session starts or when Pi sends a request.

- **Session id.** It adds the header `x-context-guru-session: <Pi session id>` to requests that go to
  the proxy. The proxy then keeps one session across a compaction, instead of two.
- **One owner of cache pings.** While the cache strategy is not `none`, it stops Pi's own cache
  warming for routed models. The proxy's keep-alive does that work, so the cache is not pinged twice.
  With `none`, Pi's own warming runs as usual.
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
| `~/.pi/agent/models.json` | Yours. Setup changes only `providers.anthropic.baseUrl`, and uninstall puts back the old value |
| `~/.local/state/context-guru-pi/` | context-guru: install record, routing record, proxy config, log, dashboard database, binary |
| `~/Library/LaunchAgents/io.rossoctl.context-guru-pi.plist` | context-guru (macOS) |
| `~/.config/systemd/user/context-guru-pi.service` | context-guru (Linux) |

`PI_CODING_AGENT_DIR` and `XDG_STATE_HOME` move these paths, as they do for Pi.

## Limits

- **Anthropic only** in this version.
- **One proxy for all Pi sessions on the machine,** because `models.json` is per user, not per project.
- If `models.json` already sends Anthropic somewhere else (a company gateway, for example), setup
  refuses and changes nothing.

## Development

```sh
python3 -m unittest discover -s scripts      # controller and models.json route
node --test tests/*.test.ts                   # extension helpers (Node 22.18+)
```
