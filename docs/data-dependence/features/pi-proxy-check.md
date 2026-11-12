---
name: Pi proxy check at session start (`pi-package`, `pi_plugin.py restart`)
kind: none
off: 🟡 Works, no data
metadata_only: 🟡 Works, no data
everything: 🟡 Works, no data
order: 240
---

When a Pi session starts, the Pi extension sends one bare `GET /healthz` to the local proxy, with no body. If the proxy does not answer, it restarts the proxy's user service once and checks again. The extension reads the port, the proxy URLs, the cache strategy and the chained upstreams from the install record, and the model URLs Pi knows. `restart` reads the port, the preset, the cache strategy, the chained upstreams and the binary path from the record. Before the restart it runs the binary's `--version`. For a strategy other than `none` it also runs `context-guru-proxy strategies`, which prints the strategy table (names and texts, no stored data), and it writes its own `proxy.yaml` again from the record (only a file with its marker; #1044). Pi keeps no copy of the table (#1062). It reads no stored data and sends no request content.
