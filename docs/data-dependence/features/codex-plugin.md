---
name: Codex plugin (`codex-marketplace/plugins/context-guru`: opt-in profile, launcher, hook, `status`, `doctor`)
kind: none
off: 🟡 Works, no data
metadata_only: 🟡 Works, no data
everything: 🟡 Works, no data
order: 330
---

It reads Codex's `config.toml` for the active provider's base URL, bearer token and auth flag, and the proxy's health and `/stats` numbers. It writes a Codex profile file, a launcher script, a proxy config file and its own install record. It reads no transcript, prompt or tool output, and sends none to the proxy. The profile file and the install record hold the provider's bearer token, if the user set one in `config.toml`. They stay on the user's machine, in the user's own folders, mode 0600. The hook reads only the environment flag that the launcher sets. It never reads the session text. `doctor` reads file names, the proxy's up or down answer and that flag. To list the cache strategies, `configure` runs the local proxy binary once, with no network and no user data. The level changes nothing in the plugin itself.
