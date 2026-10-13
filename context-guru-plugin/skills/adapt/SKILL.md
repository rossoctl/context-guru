---
name: adapt
description: Replay your own past sessions under each setting and recommend the cheapest - `cache` (keep-alive strategy), `compaction` (tail-compaction config) or `tools` (unused MCP servers, skills and tools). Shows current vs default vs best with saved, spent and net dollars, then asks before changing anything. Use when asked which setting would save money, to tune or adapt Context Guru to how they work, or to apply a recommended change.
argument-hint: "[cache|compaction|tools] [details] [--days N]"
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py:*)
---

Tell the user in one line that the analysis replays their history and can take a few minutes. Then run this with a Bash timeout of 420000 ms and print its output exactly as it is in one code block:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" adapt $ARGUMENTS
```

If the output has no `Apply it?` line, stop there. Add nothing.

If it has one, the user must see the screen before being asked, so the order is fixed:
first send the output as your message, then ask ONE yes/no question with AskUserQuestion. Never ask before
the output is printed. The question is the `Change:` line plus the first line's dollar figure, for example
"Apply: cache strategy 5-min-ping -> adaptive? (about $12.40 more saved over the last 30 days)", with the options
"Yes, apply it" and "No, leave it".

- Only on "Yes, apply it": run the exact command after `run:` on the `Apply it?` line, as
  `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" adapt <kind> apply <option>@<basis>` (keep any `--days N` it shows), and print its output in one code block.
- On anything else (No, Esc, a different answer): say "Nothing changed." and stop. Do not apply.
- Never apply because the user asked for the report, and never apply a different option than the one shown.
  If the user picks another option from the table, ask the same yes/no question for that option first and
  apply it as `adapt <kind> apply <option>` (no `@basis`; the command reads the current report for it).

If the apply output starts with `Not applied:`, print it and stop; do not retry.
Do not turn dollars into tokens, and do not add advice the output does not contain.
