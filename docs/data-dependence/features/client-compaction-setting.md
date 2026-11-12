---
name: **Client compaction setting** (Claude Code #948; Bob and Codex #963)
kind: metadata
off: ✅ Works. Nothing is kept: the proxy holds the setting in memory.
metadata_only: 🟡 Works, metadata
everything: 🟡 Works, metadata
order: 60
---

The user can move the point where Claude Code compacts its own transcript. The savings figures and the summarize trigger must use that point, or they are wrong for that user. The local proxy reads a few short setting values once, at start-up, from its env and from `settings.json` and `.claude.json` (Claude Code), `~/.bob/settings.json` (Bob) and `config.toml` (Codex), and holds them in memory. For Bob it also uses the client's own default threshold. For Bob, the local proxy also reads the model list that passes through it (`/inference/v1/model/info`). It keeps one window number for each model id, in memory only, at most 512 entries, replaced on each fetch, and never written anywhere. It writes the resulting C (a number) and its source (`configured` or `client_default`) on each request row (`compaction_point`, `compaction_point_source`). No transcript text, no paths, no other settings. A hosted proxy reads and captures nothing. Decided by: David Amid, #948, #963.
