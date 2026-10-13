---
name: context-guru-adapt
description: Replay your own past sessions under each setting and recommend the cheapest cache keep-alive strategy, compaction config, or unused tools to drop; asks before applying.
---

Say in one line that this replays the user's history and can take up to a minute. Then run this from this
skill directory and print its output exactly as it is in one code block:

```bash
python3 ../../scripts/cg.py adapt
```

Append the subcommand the user named (`cache`, `compaction` or `tools`), and `details` when they ask for more.
In the output, read `/context-guru:adapt` as this skill.

If the output has no `Apply it?` line, stop. If it has one, ask the user ONE yes/no question: the `Change:` line
followed by "Apply it? (yes/no)", and wait for the answer. Only on yes, run
`python3 ../../scripts/cg.py adapt <kind> apply <option>@<basis>` with the exact token from the `Apply it?` line,
with `sandbox_permissions="require_escalated"` (it changes the proxy's configuration), and print its output.
On anything else say "Nothing changed." and stop. Never apply without a yes.

If loopback access is blocked, retry the read-only command with `sandbox_permissions="require_escalated"`.
If it still cannot run, say proxy state is unverified rather than guessing.
