---
name: `expand` and the originals
kind: content
off: ❌ Not offered. Removals have no way back.
metadata_only: ❌ Not offered
everything: ✅ Works
order: 10
---

When Context Guru replaces a large output with a short marker, it must keep the original. Otherwise it has nothing to give back. This now includes the agent's own summary request (Claude Code auto-compact and `/compact`). Context Guru used to forward that request untouched, so it kept nothing from it. It now shortens the history like any other turn, so the summary can carry a marker, and a marker needs the stored original to be expanded later. A summary request over 32 MiB is still forwarded untouched and keeps nothing.
