---
name: OpenCode one-command installer (`install-opencode.sh`)
kind: none
off: 🟡 Works, no data
metadata_only: 🟡 Works, no data
everything: 🟡 Works, no data
order: 320
---

It reads version numbers (OpenCode) and the `plugin` lists of OpenCode's `opencode.json` and `tui.json`. It reads no stored customer data and sends none to the proxy. It asks for no key and stores none: the plugin uses OpenCode's own login. When the user says yes to the test prompt, OpenCode sends one short prompt with the user's own login. The installer shows the last 200 characters of OpenCode's answer and reads the proxy's up or down answer. It stores neither. Then it stops that check's proxy and deletes its state folder. Its record holds the path of the plugin copy, which config files it created, whether each one had a `plugin` list, and whether it installed OpenCode. It does not record the entries it replaced. Uninstall removes every entry of this plugin, also one that the user added by hand. The level changes nothing.
