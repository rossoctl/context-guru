---
name: cache-campaign
description: Build a cache plan from your own history, compare it with what you do today, and switch it on in shadow or live mode. Manager login needed; an older proxy gets a labelled example.
argument-hint: "[build [--builder auto] [--session-end] | show ID [details] | mode ID off|shadow|active]"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign), Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign details), Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign show:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign $ARGUMENTS
```

`build --builder auto` picks this tenant's own keep-alive plan, shrunk toward the fleet's and rebuilt daily while it runs
(`--session-end` if the SessionEnd hook is installed); the default builder trains a predictor. `build` waits up to 80 s for the result,
then says how to check later. Add `details` (for example `show ID details`) to see the shipped plan next to the fleet's and every strategy compared.

Only the reads (no argument, `details`, `show`) are pre-approved: `build` and `mode` change what the proxy does, so Claude Code asks first.
The routes are manager-only: the user sets `CG_DASH_COOKIE` to the dashboard login cookie. Dollars
compare strategies on past traffic; shadow numbers are estimates, never billed savings.
