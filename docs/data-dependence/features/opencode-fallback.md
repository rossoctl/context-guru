---
name: OpenCode fallback to the provider when the proxy is down (`opencode-plugin`, `fallback`)
kind: none
off: 🟡 Works, no data
metadata_only: 🟡 Works, no data
everything: 🟡 Works, no data
order: 300
---

When the OpenCode plugin cannot reach the local proxy, it sends the request straight to the provider. The same holds at all three storage levels, because the feature never uses stored data. To decide, it reads only the request URL and the error code of a failed connection. It reads the `disabled` marker file and the proxy's health answer, which hold no customer data. The direct request carries the user's own headers and body unchanged and is not read. The log lines hold only the provider host, the proxy port, the error code, and why a restart failed (for example "exited with code 1"). The feature stores nothing: no file, no record, no copy of the request.
