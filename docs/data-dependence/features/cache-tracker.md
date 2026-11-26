---
name: Cache tracker
kind: metadata
off: 🟡 Works, metadata
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 170
---

It must know how much of the conversation the provider already cached, so components never edit that part. After a restart it asks the dashboard (when on) for the time each thread's cached prefix was last refreshed — by its own last request or a keep-alive ping that read or wrote the cache and got a 2xx reply, under either session id the conversation uses (the row stores the content-derived alias id, a hash like the session id itself); timestamps and ids, metadata only — and treats a thread refreshed inside the request's cache lifetime as cached, not as new, until that thread's first request. Without the dashboard — and for a tenant running observe mode, whose rows carry no session id — it uses the proxy's start time instead.
