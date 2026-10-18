# Commands

Every Claude Code plugin command, in one place. Each command opens with one line that answers its question, then a
small picture; add `details` for the tables.

| Command | The question | Example first line |
|---|---|---|
| `/context-guru:status` | Is it working, how am I doing, what next? | Saved $41.20 in 7 days (4.6% of what the same work would have cost). |
| `/context-guru:insights` | Savings, cache and tools, one after another | |
| `/context-guru:insights savings` | How much did it save? | Saved $41.20 in 7 days, net of Context Guru's own spend (4.6% of the $898.05 the same work would have cost). |
| `/context-guru:insights cache` | Is my cache about to cost me? | Cache is warm for 2:41 more. If it lapses, the next message costs $1.91 extra. |
| `/context-guru:insights tools` | What do I carry and never use? | 6 things you carry were never used: $37.70 in 7 days, about $161.57 a month. |
| `/context-guru:session` | What happened in this session? | In the last 4 hours of this session (9h20m long), cache misses cost you about $4.20 of the $13.50 billed (2 misses in 31 messages). |
| `/context-guru:doctor` | Is it set up right? | All 7 checks pass. |
| `/context-guru:save-more` | Which config and keep-alive strategy would have cost least on my history? | So far: your current config saved $9.40 in 30 days (observed). |
| `/context-guru:remove-mcp-and-skills` | Which MCP servers and skills do I never use? | So far: the 2 items you already removed saved $4.20 (measured, since 2026-09-15). |
| `/context-guru:cache-campaign` | Would a keep-alive campaign built from a tenant's history beat what it does today? (managers) | |

## Save more, from your own history

`/context-guru:save-more` replays your past sessions under every config Context Guru ships and every keep-alive
strategy, says in plain words what your current setting saved so far and what the best change would have saved, and
applies it only after you say yes. `/context-guru:remove-mcp-and-skills` does the same for MCP servers and skills
you carry and never use (built-in tools are never listed).

| Command | Does |
|---|---|
| `/context-guru:save-more` | config and cache together, one question at the end |
| `/context-guru:save-more config` | every shipped preset; `--custom` also searches component combinations |
| `/context-guru:save-more cache` | every keep-alive strategy, with a day-by-hour view |
| `/context-guru:remove-mcp-and-skills` | unused MCP servers, MCP tools and skills, and what earlier removals saved |
| `... revert <config\|cache\|tools>` | undo the last change made there |

```
╭─ Save more: cache ──────────────────────────────────────────────────────────────────────────────╮
│ So far: your current keep-alive strategy saved $31.10 in 30 days (observed).                    │
│ Change: cache strategy 5-min-ping -> adaptive. Over the last 30 days it would have saved about  │
│ $2.89 a week (95% CI $0.72 to $4.89).                                                           │
│ Before -> after: $300.00 billed on these sessions, $287.00 with the change.                     │
╰─────────────────────────────────────────────────────────────────────────────────────────────────╯
switch to adaptive keep-alive to save about $12.40 more over the last 30 days.
  best vs default +$12.40 (95% CI $3.10 to $21.00)  ·  best vs current +$12.40 (95% CI $3.10 to
  $21.00)

      option                            saved  spent     net
  ★   adaptive    Adaptive keep-alive  $51.20  $7.70  $43.50  ████████████
  ▸◦  5-min-ping  Ping every 5 min     $40.10  $9.00  $31.10  █████████░░░
```

(Illustrative numbers.) Codex: `$context-guru-save-more`, `$context-guru-remove-mcp-and-skills`. Reference:
[Save more](save-more.md).

Five more views draw in the terminal (`watch`, `timeline`, `chart`, `heatmap`, `fill`). They are on
[Terminal views](terminal-views.md).

Each command has its own page: see the [plugin overview](index.md).
