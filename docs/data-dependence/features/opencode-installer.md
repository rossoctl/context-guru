---
name: OpenCode one-command installer (`install-opencode.sh`)
kind: none
off: 🟡 Works, no data
metadata_only: 🟡 Works, no data
everything: 🟡 Works, no data
order: 320
---

It reads version numbers (OpenCode, and the latest OpenCode on npm), the `plugin` lists of OpenCode's `opencode.json` and `tui.json`, and the provider names in `opencode.json`. It reads which providers have a login, from the names that `opencode auth list` prints. It reads which project state folders have the "turned off" marker, and the project folder path from each one's `install.json`, to name them. It reads no stored customer data and sends none to the proxy. On the direct path it never reads, asks for, or stores a key: OpenCode's own login does. On the gateway path it asks for the gateway key, sends it only to that gateway (one model-list request), and writes it only to OpenCode's own key file. It never shows, logs or records the key. For a gateway, the installer writes the base URL and the model names that the user typed. When the user says yes to the test prompt, OpenCode sends one short prompt with the user's own login. The installer shows the last 200 characters of OpenCode's answer, and reads the proxy's up or down answer and its request count. It stores none of them. Then it stops that check's proxy and deletes its state folder. Its record holds the path of the plugin copy, which config files it created, whether each one had a `plugin` list, whether it installed OpenCode, the gateway settings it wrote with their values from before, and the provider ID of a login it started. It does not record the entries it replaced. Uninstall removes every entry of this plugin, also one that the user added by hand. The level changes nothing.
