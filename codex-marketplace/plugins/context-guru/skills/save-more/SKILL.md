---
name: context-guru-save-more
description: Replay your own past sessions under every shipped config and every keep-alive strategy, recommend the cheapest, and apply it after a yes.
---

Say in one line that this replays the user's history and can take a few minutes. Then run this from this skill
directory and print its output exactly as it is in one code block:

```bash
python3 ../../scripts/cg.py save-more
```

Append `config`, `cache` or `settings` if the user named one (`settings` lists client settings that cost money; it replays nothing), `--custom` to also search custom combinations, `details` for more.
In the output, read `/context-guru:save-more` as this skill.

If the output has no `Apply it?` line, stop. Otherwise, after printing it, ask ONE question and wait: with two
`Apply it?` lines, "Apply both recommended changes? (both / only config / only cache / neither)"; with one, its
`Recommended:` line followed by "Apply it? (yes/no)". For each change the user chose, run
`python3 ../../scripts/cg.py` followed by every word after `/context-guru:save-more` on its `Apply it?` line,
unchanged, with `sandbox_permissions="require_escalated"` (it changes the proxy's configuration), and print the
output. On anything else say "Nothing changed." and stop. Never apply without a yes.
A "Settings that cost you money" card has its own `Apply it?` lines (`apply settings <check-id>`): ask one yes/no question per check, quoting its `Fix:`, `Risk:` and `Cost:` lines, then run `python3 ../../scripts/cg.py apply settings <check-id>` the same way.
If the user typed `apply <kind> <token>` or `revert <kind>` themselves, that is their yes: run
`python3 ../../scripts/cg.py apply <kind> <token>` (or `revert <kind>`), also with `sandbox_permissions="require_escalated"`.
If an output is a single `Run this instead ...` line, run that command the same way.

If the user asks for a row the screen does not recommend, ask first and quote the screen's own verdict; never
invent a figure. On yes run the words after `/context-guru:save-more` on the `Any row` line with `<option>`
replaced by the row's id. If an apply output ends with an `Apply anyway ... --force` line, nothing went live: ask
a SECOND, separate yes/no question that says so, and only on yes run that line's words, `--force` included.
Never add `--force` yourself. To undo, run the `Undo:` line the same way.

If loopback access is blocked, retry the read-only command with `sandbox_permissions="require_escalated"`.
If it still cannot run, say proxy state is unverified rather than guessing.
