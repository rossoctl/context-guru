---
name: dashboard
description: Open context-guru's dashboard in the browser on the two pages for deciding what to remove yourself — History (every installed skill you never used, across all your Claude Code history including before context-guru was installed) and Cleanup (what carrying each unused capability has really cost since install, with a command builder). Use when the user wants to see the dashboard, look at the data themselves, deep-dive, browse their usage, or decide for themselves what to remove. Accepts --no-open to print the URLs without opening a browser.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/dashboard.sh), Bash(${CLAUDE_PLUGIN_ROOT}/scripts/dashboard.sh --no-open)
---

# context-guru dashboard

One command:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/dashboard.sh"
```

**If invoked with `--no-open`**, add `--no-open`. It prints the two URLs and opens nothing — for an
SSH session, a headless machine, or a user who would rather click the links.

The script resolves this install's port itself and checks the proxy answers before opening
anything. Do not substitute a port of your own, and in particular never 8787: ports are allocated
per project, so a guessed one is another install's proxy and every figure on it would be that
install's spend shown under this user's name.

## What to tell the user

Relay the script's output. If it exits non-zero, say what it printed — not installed, or proxy not
running — and the command it gave. Do not open a URL yourself as a workaround.

When it succeeds, say in a sentence or two what each page is for, because they answer different
questions over different spans and are easy to read as one:

- **History** — every installed skill you have never used, in any project, across all your
  history, including the months before context-guru existed. It has **no money** in it, and that is
  deliberate: Claude Code's transcripts record what a request spent in total, never which skills it
  carried, so the cost of an unused skill before install is not recoverable. It shows *what* is
  unused, not what it cost.
- **Cleanup** — what carrying each unused capability has really cost, priced from traffic this
  proxy measured. That means only the time **since** context-guru was installed, and only projects
  that have been used through it. This is the only page with a dollar figure.

Read together: History says what you never touched; Cleanup says what that has cost you since you
started measuring.

## Removing things

**Neither page removes anything.** On Cleanup, ticking items builds the command that would remove
them; the user has to **copy** it and run it, or tell you which ones to remove. Say this, because
someone who ticks a box and sees nothing happen will think the page is broken.

If the user then asks you to remove something, treat it as you would any other change to their
setup: show the exact command first and run it only when they confirm. A plugin's skills can only
be removed together, so removing one skill from a plugin means disabling the whole plugin — say so
before running it.
