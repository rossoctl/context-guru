# Install the Pi package

> **Experimental.** It is tested with fake keys in a sandbox. A real model answer through the proxy,
> keep-alive pings and Sign in with ChatGPT are not tested live yet.

The [Pi](https://pi.dev) package routes Pi's `anthropic` and `openai` providers through a local
context-guru proxy. It adds no API key.

## You need

- Pi 1.5.0 or later. Pi 1.5.0 needs Node.js 22.19 or later (the `engines` field of its `package.json`).
- `python3`.
- macOS or Linux.

## Install

1. Clone this repository:
   ```sh
   git clone https://github.com/rossoctl/context-guru.git
   ```
2. Install the package into Pi:
   ```sh
   pi install ./context-guru/pi-package
   ```
3. Start Pi and run:
   ```text
   /context-guru setup
   ```
   Setup asks one question and changes nothing until you answer yes. On yes, it installs the proxy
   as a user service that starts at login. Then it points Pi's `anthropic` and `openai` providers at
   the proxy, in `~/.pi/agent/models.json`.
4. Select the model again with `/model`, or restart Pi.

## Check, undo and recover

- Check it with `/context-guru status`.
- Undo it with `/context-guru uninstall`.
- If Pi cannot talk after setup, run `~/.local/state/context-guru-pi/context-guru-reset`.

## Keep-alive uses your quota

The default cache strategy, `auto`, sends small keep-alive requests while you are idle. They use your
own quota. To turn them off, run `/context-guru cache-strategy none`.

## More

- Details and limits: [pi-package/README.md](https://github.com/rossoctl/context-guru/blob/main/pi-package/README.md).
- How it differs from the Claude Code plugin: [Pi package and Claude Code plugin compared](../reference/pi-vs-claude-code.md).
