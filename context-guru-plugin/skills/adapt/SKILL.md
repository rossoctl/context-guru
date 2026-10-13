---
name: adapt
description: Replay your own past sessions under each setting and recommend the cheapest - `cache` (keep-alive strategy), `compaction` (tail-compaction config) or `tools` (unused MCP servers, skills and tools). Shows current vs default vs best with saved, spent and net dollars, then asks before changing anything. Use when asked which setting would save money, to tune or adapt Context Guru to how they work, or to apply a recommended change.
argument-hint: "[cache|compaction|tools] [details] [--days N]  |  apply <kind> <option>@<basis>"
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py adapt:*)
---

Only the read is pre-approved. Applying is the separate command `cg.py apply`, so Claude Code also asks the user
for permission to run it: that second prompt is expected, do not work around it.

**If the arguments start with `apply`** (the user typed the apply themselves, which is their yes), run this
and print its output in one code block:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" $ARGUMENTS
```

**Otherwise,** tell the user in one line that the analysis replays their history and can take a few minutes.
Then run this with a Bash timeout of 420000 ms and print its output exactly as it is in one code block:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" adapt $ARGUMENTS
```

If the output has no `Apply it?` line, stop there. Add nothing.

If it has one, the user must see the screen before being asked, so the order is fixed:
first send the output as your message, then ask ONE yes/no question with AskUserQuestion. Never ask before
the output is printed. The question is the `Change:` line plus the first line's dollar figure, for example
"Apply: cache strategy 5-min-ping -> adaptive? (about $12.40 more saved over the last 30 days)", with the options
"Yes, apply it" and "No, leave it".

- Only on "Yes, apply it": the `Apply it?` line ends with `/context-guru:adapt apply ...`. Run
  `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py"` followed by every word after `/context-guru:adapt` on that line,
  unchanged (for example `... cg.py" apply tools remove-sufficient@8c21e0d4 --days 40`), then print its output
  in one code block.
- On anything else (No, Esc, a different answer): say "Nothing changed." and stop. Do not apply.
- Never apply because the user asked for the report, and never apply a different option than the one shown.
  If the user picks another option from the table, ask the same yes/no question for that option first and
  apply it as `cg.py apply <kind> <option>@<basis>`, with the basis from the shown line.

If the apply output starts with `Not applied:`, print it and stop; do not retry.
Do not turn dollars into tokens, and do not add advice the output does not contain.
