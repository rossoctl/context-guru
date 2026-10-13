---
name: context-guru-adapt
description: Replay your own past sessions under each setting and recommend the cheapest cache keep-alive strategy, compaction config, or unused tools to drop; asks before applying.
---

Say in one line that this replays the user's history and can take a few minutes. Then run this from this
skill directory and print its output exactly as it is in one code block:

```bash
python3 ../../scripts/cg.py adapt
```

Append the subcommand the user named (`cache`, `compaction` or `tools`), and `details` when they ask for more.
In the output, read `/context-guru:adapt` as this skill.

If the output has no `Apply it?` line, stop. If it has one, print the output first, then ask the user ONE yes/no
question: the `Change:` line followed by "Apply it? (yes/no)", and wait for the answer. Only on yes, run
`python3 ../../scripts/cg.py` followed by every word after `/context-guru:adapt` on the `Apply it?` line, unchanged
(for example `python3 ../../scripts/cg.py apply tools remove-sufficient@8c21e0d4 --days 40`),
with `sandbox_permissions="require_escalated"` (it changes the proxy's configuration), and print its output.
On anything else say "Nothing changed." and stop. Never apply without a yes.
If the user typed `apply <kind> <token>` or `revert <kind>` themselves, that is their yes: run
`python3 ../../scripts/cg.py apply <kind> <token>` (or `revert <kind>`), also with `sandbox_permissions="require_escalated"`.
If an output is a single `Run this instead ...` line, run that command the same way.

If the user asks for a row the screen does not recommend, ask first and quote the screen's own verdict
("The proxy does not recommend it: <first line>. Apply it anyway?"); never invent a dollar figure. On yes run the
words after `/context-guru:adapt` on the `Any row` line, with `<option>` replaced by the row's id from the `option` column.
If an apply output ends with an `Apply anyway ... --force` line, the proxy's own check failed and nothing went live:
ask a SECOND, separate yes/no question that says so, and only on yes run the words after `/context-guru:adapt` on
that line, `--force` included. Never add `--force` yourself. To undo, run the `Undo:` line the same way.

If loopback access is blocked, retry the read-only command with `sandbox_permissions="require_escalated"`.
If it still cannot run, say proxy state is unverified rather than guessing.
