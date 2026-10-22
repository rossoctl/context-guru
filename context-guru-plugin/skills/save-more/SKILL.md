---
name: save-more
description: Replay your own past sessions under every config Context Guru ships (each preset) and every keep-alive strategy, show what each would have saved, recommend the best, and apply it after a yes. It also lists client settings that cost money (`settings` alone shows only that). `config` or `cache` for one of them; `--custom` also searches custom combinations of components. Use when asked how to save more, which preset or cache strategy is cheapest for them, or to apply or undo such a change.
argument-hint: "[config|cache|settings] [details] [--custom] [--days N]  |  apply <config|cache> <option>@<basis>  |  revert <config|cache>  |  apply|revert settings <check-id>"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" save-more:*), Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" record-run save-more)
---

Run this now, with a Bash timeout of 420000 ms (it replays the user's history and can take a few minutes), and print its output exactly as it is in one code block:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" save-more $ARGUMENTS && python3 "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" record-run save-more
```

Run it exactly as one command: the second half records the run for the status line, and runs only when the
read printed a report (the read exits non-zero on any failure or refusal). Say nothing about it.

(Exception: if the arguments start with `apply` or `revert`, the user typed the change themselves, which is their
yes: run `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" $ARGUMENTS` instead and print its output.)

Only the read and that run record are pre-approved; never record a run for an apply or a revert. A change is the separate command `cg.py apply` or `cg.py revert`, so Claude Code
also asks the user for permission to run it: that second prompt is expected, do not work around it.

If an output is a single `Run this instead ...` line, run that exact command and print its output.

Then ask ONE question with AskUserQuestion, after the output is printed, never before:

- Two `Apply it?` lines (config and cache): the question is "Apply both recommended changes?" plus the two
  `Recommended:` lines; options "Apply both", "Only config", "Only cache", "Neither".
- One `Apply it?` line: the question is its `Recommended:` line, verbatim (it may say that applying drops some of
  the user's own settings; that must be said before they answer), plus the card's weekly figure, e.g.
  "Apply: preset conservative -> medium? Over the last 30 days it would have saved about $1.49 a week."; options
  "Yes, apply it", "No, leave it". Say it in hindsight (would have saved), never as a promise.
- No `Apply it?` line: stop. Add nothing.
- A card "Settings that cost you money" is a separate question, asked AFTER the config and cache question (or alone).
  Its `Apply it?` lines read `apply settings <check-id>`. Ask: "Change this setting?" plus that check's `Fix:` and
  `Risk:` lines, verbatim, and its `Cost:` line in hindsight, never as a promise; options "Yes, change it",
  "No, leave it". One question per check. A check with no `Apply it?` line cannot be changed by Context Guru (managed
  settings, or your environment): print nothing more about it. A cost of "not measured yet" is never zero: say so.

For each change the user chose, run `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py"` followed by every word after
`/context-guru:save-more` on its `Apply it?` line, unchanged, and print the output. On anything else say
"Nothing changed." and stop. A settings change is the same command with the same words, e.g.
`python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" apply settings disable-experimental-betas`; it edits the user's own
settings file after a backup, and the output names the backup and the `Undo:` line.

If the user asks for another row of a table, ask first and quote that screen's own verdict, e.g. "The proxy
does not recommend high (it says: ...). Apply it anyway?"; never invent a figure. On yes run the `Any row` line
(or the `Apply it?` line) with `<option>` replaced by the row's id from the `option` column.

If an apply output ends with an `Apply anyway ... --force` line, the proxy's own check failed and nothing went
live. Ask a SECOND, separate yes/no question that says so ("The go-live check failed. Switch it on anyway?").
Only on yes run the words after `/context-guru:save-more` on that line, `--force` included. Never add `--force`
yourself. If an apply output starts with `Not applied:`, print it and stop; do not retry.
To undo, run the `Undo:` line the same way. After an apply or a revert, print its output and stop; ask nothing more. Do not turn dollars into tokens or add advice the output does not contain.
