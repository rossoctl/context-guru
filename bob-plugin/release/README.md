# context-guru for IBM Bob

**Community tool, not an IBM product. IBM does not support it, and the IBM Bob team has not reviewed
it. Use it at your own risk.**

This package sends IBM Bob's model traffic through a local context-guru proxy on your Mac. The
proxy cuts token cost. Everything it needs is in this folder. The install works offline.

## Requirements

- macOS, on Apple silicon (arm64) or Intel (x86_64).
- IBM Bob, in `/Applications` or `~/Applications`.
- The Command Line Tools, for Python 3. If you do not have them, the installer tells you. Then
  open Terminal, run `xcode-select --install`, and run the installer again.

## Check the download

The release page lists the SHA-256 of the zip. Before you unzip it, open Terminal and run:

```sh
shasum -a 256 ~/Downloads/context-guru-bob-*.zip
```

The long number it prints must be the same as the one on the release page. If it is not, do not
use the file.

## Install

1. Unzip the package.
2. Double-click **`Install context-guru for Bob.command`**. A Terminal window opens. Use only the
   two `.command` files. Do not open the files in `bin/` or `plugin/`.
3. Read the plan. Type `y` and press Return. Any other answer changes nothing.
4. If Bob is open, the installer quits it. Wait for the ✓ table.
5. Open IBM Bob from the Dock. **Bob asks you to log in once. This is expected.**

If a step fails, or you press Ctrl-C, the installer undoes everything and tells you the log path.

If context-guru is already installed, the installer asks: `r` reinstalls (it removes the old
install first and keeps your dashboard history), `q` quits and changes nothing.

## Uninstall

1. Double-click **`Uninstall context-guru for Bob.command`**.
2. Type `y` and press Return.
3. It asks if it must also delete your dashboard history. The default is No: the history stays
   in `~/.local/state/context-guru-bob/`.
4. Open Bob from the Dock and log in once.

The uninstaller works even if you deleted the plugin folder. If a part cannot be removed, it
keeps the plugin and its install files, says what is left, and you run it again.

## What it changes

Only these 5 things. The uninstaller undoes all of them, and your files come back byte for byte.

| # | What | Where |
|---|---|---|
| 1 | A block of 8 lines at the end of your shell file | `~/.zshrc` or `~/.bash_profile` |
| 2 | Two hooks | `~/.bob/settings/settings.json` |
| 3 | One background service (the local proxy, ports 8891–8990) | `~/Library/LaunchAgents/com.rossoctl.context-guru.bob.plist` |
| 4 | The plugin folder | `~/.bob/plugins/context-guru/` |
| 5 | The status bar item in Bob | Bob's extensions |

Plus one data folder, `~/.local/state/context-guru-bob/` (the proxy, logs, dashboard). The
uninstaller deletes what the install put there. It keeps your dashboard history unless you say
to delete it.

If it is missing, the installer also creates Bob's empty workspace folder `~/.bob/playground`.
Bob uses it for a chat with no folder open, but does not create it. Without it, every shell
command in such a chat fails, the context-guru skills too. The folder belongs to Bob, so the
uninstaller keeps it. If you set your own playground folder in Bob, create that folder yourself.

It does not touch Claude Code, your other shell files, system files, or Bob itself. It needs no
`sudo`.

**Bob asks you to log in once after install, and once after uninstall.** Bob keeps its login per
backend address.

The first Bob answer after a login can show 0 tokens in the status bar. The answer is correct;
Bob did not send its token counts for it. The next answer shows them.

## If macOS blocks the file

These files are not signed by Apple. When you download the package, macOS can block a
double-click with "Apple could not verify…". To allow it, do this once for each `.command` file:

1. Double-click the file. Click **Done** (not "Move to Trash").
2. Open **System Settings → Privacy & Security**.
3. Scroll down. Next to the message about the file, click **Open Anyway**.
4. Type your password, then click **Open Anyway** again.

Do not turn off Gatekeeper for the whole Mac. It is not needed.

## Log

`~/Library/Logs/context-guru-bob-install.log`. It holds the steps and their results only. It
never holds your answers, prompts, tokens or file contents.

## Files

- `Install context-guru for Bob.command`, `Uninstall context-guru for Bob.command`
- `bin/darwin-arm64/context-guru-proxy`, `bin/darwin-amd64/context-guru-proxy`
- `plugin/`: the Bob plugin and its status bar (`plugin/statusbar/context-guru-bob.vsix`)
- `SHA256SUMS`: the installer checks every file against it before it changes anything. It
  refuses a file that is not in the list, and any link.
