---
name: Bob plugin (`bob-plugin/context-guru`: routing block, hooks, `status`, `doctor`, status bar)
kind: metadata
off: 🟡 Works, but `status` and `doctor` cannot see Bob traffic, and the status bar shows `—`
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 230
---

It routes Bob with one block in the shell startup file and adds two hooks to Bob's settings. The hooks read only the session id, the event name and the `SessionStart` source. They never read the prompt, and they send nothing for events that carry model text. `status` and `doctor` read only the time of the newest Bob request from the dashboard. The status bar reads the newest Bob session id and its saved-token count, and that session's cache times, prefix size and context window. All of these are counts, times and ids. With the dashboard off, those reads find nothing: `status` and `doctor` cannot see Bob traffic, and the status bar shows `saved —` and `cache —`, never `saved 0`. Everything else still works. The level changes nothing in the plugin.
