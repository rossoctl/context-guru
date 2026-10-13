---
name: chart
description: Dollars saved per day as a bar chart, split by lever, with what is still avoidable drawn apart as an estimate. Pass 14 or 30 as the number of days.
argument-hint: "[14|30]"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/tv.py" chart:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/tv.py" chart $ARGUMENTS
```
Do not turn dollars into tokens or add advice that the output does not contain.
