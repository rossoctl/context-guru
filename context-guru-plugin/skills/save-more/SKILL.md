---
name: save-more
description: Replay your own past sessions under every config Context Guru ships (each preset) and every keep-alive strategy, show what each would have saved, recommend the best, and apply it after a yes. `config` or `cache` for one of them; `--custom` also searches custom combinations of components. Use when asked how to save more, which preset or cache strategy is cheapest for them, or to apply or undo such a change.
argument-hint: "[config|cache] [details] [--custom] [--days N]  |  apply <config|cache> <option>@<basis>  |  revert <config|cache>"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" save-more:*), Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" record-run save-more)
---

Run this now, with a Bash timeout of 420000 ms (it replays the user's history and can take a few minutes), and print its output exactly as it is in one code block:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" save-more $ARGUMENTS
```

(Exception: if the arguments start with `apply` or `revert`, the user typed the change themselves, which is their
yes: run `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" $ARGUMENTS` instead and print its output.)

Only if that read printed a report (it did not start with an error line such as `The proxy`, `This proxy`,
`No Context Guru proxy`, `Usage:` or `Run this instead`), record the run for the status line, once:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" record-run save-more
```

Skip that for a failed or refused run, and never run it for an apply or a revert. Say nothing about it.

Only the read and that run record are pre-approved. A change is the separate command `cg.py apply` or `cg.py revert`, so Claude Code
also asks the user for permission to run it: that second prompt is expected, do not work around it.

If an output is a single `Run this instead ...` line, run that exact command and print its output.

Then ask ONE question with AskUserQuestion, after the output is printed, never before:

- Two `Apply it?` lines (config and cache): the question is "Apply both recommended changes?" plus the two
  `Recommended:` lines; options "Apply both", "Only config", "Only cache", "Neither".
- One `Apply it?` line: the question is its `Recommended:` line, verbatim (it may say that applying drops some of
  the user's own settings; that must be said before they answer), plus the card's weekly figure, e.g.
  "Apply: preset conservative -> medium? (about $1.49 a week more)"; options "Yes, apply it", "No, leave it".
- No `Apply it?` line: stop. Add nothing.

For each change the user chose, run `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py"` followed by every word after
`/context-guru:save-more` on its `Apply it?` line, unchanged, and print the output. On anything else say
"Nothing changed." and stop.

If the user asks for another row of a table, ask first and quote that screen's own verdict, e.g. "The proxy
does not recommend high (it says: ...). Apply it anyway?"; never invent a figure. On yes run the `Any row` line
(or the `Apply it?` line) with `<option>` replaced by the row's id from the `option` column.

If an apply output ends with an `Apply anyway ... --force` line, the proxy's own check failed and nothing went
live. Ask a SECOND, separate yes/no question that says so ("The go-live check failed. Switch it on anyway?").
Only on yes run the words after `/context-guru:save-more` on that line, `--force` included. Never add `--force`
yourself. If an apply output starts with `Not applied:`, print it and stop; do not retry.
To undo, run the `Undo:` line the same way. After an apply or a revert, print its output and stop; ask nothing more. Do not turn dollars into tokens or add advice the output does not contain.
