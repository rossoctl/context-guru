---
name: OpenCode one-command installer (`install-opencode.sh`)
kind: none
off: 🟡 Works, no data
metadata_only: 🟡 Works, no data
everything: 🟡 Works, no data
order: 320
---

It reads version numbers (OpenCode) and the `plugin` lists of OpenCode's `opencode.json` and `tui.json`. It reads no stored customer data and sends none to the proxy. It asks for no key and stores none: the plugin uses OpenCode's own login. When the user says yes to the test prompt, OpenCode sends one short prompt with the user's own login, and the installer reads only the proxy's up or down answer. It records which config files it created and which entries it removed, so uninstall removes only what it added. The level changes nothing.
