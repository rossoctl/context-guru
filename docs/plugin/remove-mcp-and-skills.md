# Remove MCP servers and skills

**Question: which MCP servers and skills do I carry and never use, and what does removing them save?**

`/context-guru:remove-mcp-and-skills` lists the MCP servers, MCP tools and skills that were declared on your requests and never used, prices what carrying them cost, and shows what the ones you already removed have **measurably** saved since. Built-in tools (Read, Bash, Edit and the like) are never listed. It asks one question and filters them only on a yes.

```text
╭─ Remove MCP servers and skills ─────────────────────────────────────────────────────────────────╮
│ So far: the 2 items you already removed saved $4.50 (measured, since 2026-09-15).               │
│ Change: filter mcp__playwright, mcp__figma, skill__brand-voice and 3 more. Over the last 30     │
│ days it would have saved about $7.33 a week (range $6.10 to $8.40).                             │
╰─────────────────────────────────────────────────────────────────────────────────────────────────╯
9 declared items were never used; filtering the 6 with enough evidence would have saved $31.40.

      option                                                     saved  spent     net
      remove-all         Filter all 9 unused items              $33.90     $0  $33.90  ████████████
  ★   remove-sufficient  Filter the 6 unused items seen in 5+…  $31.40     $0  $31.40  ███████████░
  ▸◦  keep               Keep everything                            $0     $0      $0  ░░░░░░░░░░░░
  ★ recommended   ▸ yours now   ◦ the default
  saved: what it avoided paying · spent: what it paid itself · net = saved - spent, on your past
  sessions

Never used in this window (8 of 9):
  MCP server  playwright             $18.40  ██████████
  MCP server  figma                   $6.20  ███░░░░░░░
  skill       brand-voice             $3.10  ██░░░░░░░░
  MCP tool    sentry__list_releases   $2.20  █░░░░░░░░░
  skill       slides                    n/a  ░░░░░░░░░░
  skill       pdf-export              $1.10  █░░░░░░░░░
  MCP tool    sentry__get_event       $0.80  ░░░░░░░░░░
  skill       changelog               $0.40  ░░░░░░░░░░

  counterfactual · unused declarations priced on the requests that carried them ·
  2026-09-08..2026-10-08 · 64 sessions, 5,210 requests
  ! Unused means unused in this window, not that it will never be needed.
  ! Filtering changes the cached prefix, so the next request re-writes it once; that cost is not
  included.

Recommended: filter mcp__playwright, mcp__figma, skill__brand-voice and 3 more (takes effect now).
Why: declared in 5+ sessions over 7+ days and never called
Apply it? Say yes, or run: /context-guru:remove-mcp-and-skills apply tools remove-sufficient@8c21e0d4

Question: Remove these 6 items? Over the last 30 days they would have saved about $7.33 a week (pays back in about 2 weeks).
```

- The card: what earlier removals saved so far (measured, not estimated), and what the recommendation would have saved per week over the same window, with its range.
- The recommendation covers only items seen in enough sessions over enough days; `remove-all` is shown for comparison.
- `details` lists every item and each earlier removal with its measured saving.

The question is exactly the last line: "Remove these N items? Over the last D days they would have saved about $X a week", with "(pays back in about W weeks)" when the one-off cache rewrite takes more than a week to earn back. It is hindsight, not a promise. On yes the proxy filters them from the next request on:

![the question in Claude Code](img/cc-remove-1-question-dark.svg#only-dark)
![the question in Claude Code](img/cc-remove-1-question-light.svg#only-light)

![the permission prompt for the change](img/cc-remove-2-permission-dark.svg#only-dark)
![the permission prompt for the change](img/cc-remove-2-permission-light.svg#only-light)

```text
Applied: 6 unused items filtered (playwright, figma, brand-voice and 3 more) (takes effect now).
Undo: /context-guru:remove-mcp-and-skills apply tools restore@5d0e17aa
```

To undo: `/context-guru:remove-mcp-and-skills revert tools`, or the `Undo:` line. Removing an item changes the cached prefix once (one cache write on the next request); the report counts that as the option's spend. Removing an MCP server or skill on the client side (`claude mcp remove ...`, `claude plugin disable ...`) saves the same and needs no proxy.

Codex: `$context-guru-remove-mcp-and-skills`. The report format is the one on [Save more](save-more.md#the-report-format).
