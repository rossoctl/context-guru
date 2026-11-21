---
name: Install a beta (`/context-guru:install-beta`, `install-beta.sh`)
kind: none
off: 🟡 Works, no data
metadata_only: 🟡 Works, no data
everything: 🟡 Works, no data
order: 300
---

It reads release tags, version numbers, ports, process ids and the update answer. It reads no stored customer data and sends none to the proxy. It writes one setting: it sets the update answer to `never` when it was `always` (#1254), and puts the old answer back if the install fails. The level changes nothing.
