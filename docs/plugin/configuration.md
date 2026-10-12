# Configuration and troubleshooting

## Where things are set

| What | Where |
|---|---|
| Plugin options (port, preset, cache strategy, idle exit, upstream) | `/plugin configure`, or the options listed in [Install as a Claude Code plugin](../how-to/install-plugin.md) |
| Routing for this repo or machine | `/context-guru:install` (scope is asked once) |
| Status line command, layout and refresh | `statusLine` in the same settings file; `/context-guru:statusline` edits it |
| Which proxy the commands talk to | the session's `ANTHROPIC_BASE_URL`; override with `CG_URL=http://127.0.0.1:PORT` or `--port` |
| Prices used for dollars | `MODEL_PRICES` on the proxy |

## Frequently asked questions

**The commands say "No Context Guru proxy is routing this session".**
The session's `ANTHROPIC_BASE_URL` does not point at a local proxy. Right after `/context-guru:install` this is normal until you start a new session. Otherwise run `/context-guru:doctor`.

**Everything shows $0.**
The proxy has no prices loaded. Point `MODEL_PRICES` at your gateway's price file and restart the proxy. `/context-guru:doctor` checks this.

**Saved is small, estimated is large. Is that a bug?**
No. They are different things. Saved is what the proxy already took off the bill. Estimated is spend you could still avoid, such as cache misses from long pauses. With the default preset the proxy does little to your content, so Saved stays small; the estimated block shows where the money goes.

**Why do two numbers on one screen not add up?**
Estimated rows overlap (a long pause is also a cache miss). They are never added. Saved and Estimated are never added either.

**The status line does not show.**
It renders only in a project routed through the proxy, and only after a new session. Check with `/context-guru:doctor`, then `/context-guru:statusline`.

**The status line clock only moves when I send a message.**
`refreshInterval` is missing or your Claude Code is older than the version that honours it. See [Refresh](statusline.md#refresh).

**The status line is too wide, or I want one line.**
`--set-layout=minimal`, or narrow the terminal; the line sheds detail in a fixed order. See [Layouts](statusline.md#layouts).

**What does `cg ✕ proxy offline` mean?**
The proxy refused the connection. Start a new session; the session hook starts it. If every request is failing and `/context-guru:uninstall` cannot run, use the [recovery script](../how-to/plugin-recovery-files.md).

**`/context-guru:cache-strategy-picker` or `preset-picker`: which should I use?**
They are unchanged. The picker commands change what the proxy does; the commands on these pages only report.

**Does the status line or any of these commands send traffic that costs money?**
No. They read the proxy's own data and never send a keep-alive ping.

**Is any of this sent anywhere?**
No. The commands talk to the proxy on your machine and write nothing.

**Where are the old `insights` and `cost-*` commands?**
They moved; see the [table on the overview page](index.md#moved-commands). The old names work for one release.
