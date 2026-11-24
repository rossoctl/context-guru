# Install the Pi package

> **Experimental.** It is tested with real Anthropic requests through a company gateway on macOS. Keep-alive
> pings and Sign in with ChatGPT are not tested live yet.

The [Pi](https://pi.dev) package routes Pi's `anthropic` and `openai` providers through a local
context-guru proxy. The package adds no API key. The one-command installer below can store your Anthropic key
in the Keychain (macOS) or the secret store (Linux) for Pi.

## You need

- Pi 1.5.0 or later. Pi 1.5.0 needs Node.js 22.19 or later (the `engines` field of its `package.json`).
- `python3`.
- macOS or Linux.

## Install with one command (recommended)

Run this in a terminal, on macOS or Linux:

```sh
curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-pi.sh | bash
```

It asks before each change:

1. **Pi:** install it if it is missing, or update it if it is older than the latest release.
2. **The package:** it installs the release's Pi package, checked by SHA-256, at one fixed path. It removes
   another copy of the package first.
3. **The connection:** it keeps Pi's current Anthropic connection, or asks for a gateway URL (or none, for
   the Anthropic API), the auth type, the model id and the key. The key is not shown on the screen. It is
   checked with one 1-token request before anything changes, then stored in the Keychain or the secret store.
   `models.json` gets only a command that reads it.
4. **context-guru:** it shows the setup question and turns context-guru on after your yes.
5. **A check:** the status, and one test prompt if you want it.

To remove everything it added (context-guru, the package, the stored key, and Pi if it installed Pi), run:

```sh
curl -fsSL https://github.com/rossoctl/context-guru/releases/latest/download/install-pi.sh | bash -s uninstall
```

npm installs Pi from `https://registry.npmjs.org/`. Set `CG_PI_NPM_REGISTRY` to use another registry.
When npm cannot read the registry and Pi is already installed, the installer keeps that Pi and goes on.

The installer names the folders it uses: Pi's settings (`~/.pi/agent`, or `$PI_CODING_AGENT_DIR`) and
context-guru's data. When `PI_CODING_AGENT_DIR` is set, for example in a terminal left from a test, it asks
"Use these folders?" before any change. The default is no: then open a new terminal and run it again.

## Install by hand

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
