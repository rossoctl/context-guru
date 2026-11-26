---
name: Pi one-command installer (`install-pi.sh`)
kind: none
off: 🟡 Works, no data
metadata_only: 🟡 Works, no data
everything: 🟡 Works, no data
order: 310
---

It reads version numbers (Node.js, Pi, the latest Pi on npm), the keys of Pi's `models.json` and `settings.json`, and the Anthropic key that the user types. It reads no stored customer data and sends none to the proxy. It sends one 1-token model request ("hi") with the user's key to the user's gateway or to the Anthropic API, to check the key before it changes anything. It stores the key only in the Keychain (macOS), in the secret store, or in a file that only the user can read (Linux); `models.json` gets only a command that prints it. It records which keys it changed, with their old values, so uninstall puts them back. The level changes nothing.
