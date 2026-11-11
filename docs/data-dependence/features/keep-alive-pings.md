---
name: **Keep-alive pings**
kind: content. The host keeps the credential.
off: ❌ **Should be off. Today it is still on**, and keeps the last body. The proxy binary also keeps the credential.
metadata_only: ❌ Off
everything: ✅ Works
order: 70
---

To keep the prompt cache warm, the proxy sends the exact same prompt again with the user's credential. Without the text, it cannot ping. The Ping button (`POST /api/me/cache/ping`) uses the same body and key. With keep-alive off, a request that carries the header `x-context-guru-ping: 1` is still held, for the button only: no ping is sent for it, and the body and key are released 300 seconds after the last request on a 5-minute cache, or 59 minutes after it on a 1-hour cache (a press does not extend it), and one account holds at most 16 such threads. Without the header nothing is held. The install adds the header and tells the user. An older install (from before `--no-ping-header`) gets it at its next session start, with the same notice. Any other install without the header is never changed. It gets a one-time note instead. The cache-state answer also carries the time of the next planned ping (a number, metadata), so the mod can hide the button while a ping fires before the cache expires.
