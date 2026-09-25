---
name: insights
description: The full cost report on this machine's Claude Code setup, measured from your own traffic - what your configuration is costing you, ranked by money, each finding with the command that fixes it. Covers unused MCP servers/tools/skills, idle time and cache misses, which components earned their place and which switched-off ones would have paid, and a configuration audit of context-guru itself. Use when the user asks what context-guru recommends, what is wasting money or tokens, how to save money, what to turn on, whether their setup is optimal, for a health check or checkup of their setup, or asks for insights. Accepts --json for the raw document.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/insights.py)
---

# context-guru insights

One command. Everything below is presentation.

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/insights.py" all
```

**If invoked with `--json`**, run `"${CLAUDE_PLUGIN_ROOT}/scripts/insights.py" all --json` and print
its output verbatim — no narration. That is for somebody who wants the document, not a reading of it.

The script resolves the configured port itself, off disk, and makes only GETs. It never writes a
settings file, never arms a strategy and never sends a ping. **Every fix in its output is printed,
not applied** — offer them, run one only when the user says so.

## What this is, and why it is not `/doctor`

Say this once, briefly, if the user asks how it differs — do not lecture anyone who did not.

`/doctor` asks *is the tool installed correctly*: version, path, ripgrep, auto-update, invalid
settings. Every finding is about Claude Code itself, it is a snapshot, and nothing in it has a
price. This asks *what is your configuration costing you*, and answers from the traffic you already
sent. Four things follow from that and they are the whole differentiator:

1. **Money, from your own bill.** Every figure is priced at the tier each request was really
   billed — cache read for a hit, the premium cache-write rate for the turn that built the prefix,
   fresh input where there was no cache. Not a blended average, which would be an order of
   magnitude out on a cold start.
2. **Counterfactuals.** What a setting you do *not* have on would have saved you over *your*
   window. `/doctor` has no equivalent because it has no history.
3. **Evidence and coverage on every claim.** n sessions, the span they cover, and how many sessions
   could not answer at all. A finding that rests on one afternoon says so.
4. **Refusal instead of a number.** Where the sample cannot carry a figure, this reports the refusal
   and the reason. A confident number from thin data is worse than no number, and that is the one
   rule this whole surface is built around.

Two things it borrows from `/doctor` deliberately, because they are good: the per-check shape
(label, verdict, one-line fix) and the terminal "nothing to fix" state.

## Read the two gating facts first

None is a cost finding and each makes everything after it meaningless if wrong.

- **`port=(none)`, with `unavailable=port` and the `no-install` finding** — nothing on disk routes
  this directory: no settings file in scope names a port, and the plugin's own record has no entry
  that covers it. There is no report to give, only the install command. Say that, and do not reach
  for a port yourself: ports are allocated per project from 8787 upwards, so "the default" is
  whichever project installed first, and every figure read off it would be that project's spend
  under this user's name. `port_source` says where the port DID come from when there is one — a
  settings file, or the install record — and is worth quoting whenever the user is surprised by a
  number, because it distinguishes "your project's proxy" from "the machine-wide one".
- **`proxy_up=false`** — nothing is running. History on disk is intact; the process is not. The
  `proxy-down` finding carries the command.
- **`session_routed_through_us=false` with a `session_base_url`** — this session's traffic is going
  somewhere else, so the numbers describe *earlier* traffic, not what the user is doing now. Say so
  before quoting any of them.
- **`unavailable=dashboard`** — the proxy answers but records nothing. Then there is no report, only
  the fix. Do not present a blank section as a clean bill of health; that is the one misreading this
  command must never invite.

## Then lead with the ranked findings, not the facts

`finding.N.*` is already ranked by money — low end of a range first, so a wide interval cannot
outrank a smaller certain saving on its optimistic end. Present them in that order. For each:

```
<title>
  <evidence>
  Fix: <fix>            (<basis>)
```

**`basis` is not decoration and must never be dropped.** It is the field that stops a forecast being
read as a bill, and each value means something different:

| `basis` | What you may say |
|---|---|
| `measured over the window` | it happened; this is what it cost |
| `projected from what carrying it has cost` | a forecast, from real history. "would save", never "saved" |
| `90% interval over a window like this one` | quote **both ends**. Never average them |
| `measured size of the problem, NOT a projected saving` | there is waste here; nobody measured what fixing it saves |
| `measured in tokens; unpriced, so no dollar figure` | quote the tokens. Unpriced is not free |
| `refused: insufficient observation` | the refusal IS the answer. Do not substitute a number |
| `deliberately withheld: insufficient observation` | same, per item |
| `configuration, not a measurement` | a setting is wrong; no money attached |
| `no measurement yet` | too early. Not a clean bill of health |

`usd_window` is what it cost or would have over the measured window. `usd_month` is that scaled to
30 days and is **absent on a window under two days** — when it is absent, do not compute it
yourself. `usd_lo`/`usd_hi` come as a pair; a finding carrying them has no point estimate on purpose.

## The configuration audit

`preset`, `cache_strategy`, `pipeline`, `idle_exit`, `upstream`, `options_file` are what is
configured; `running_fingerprint` is what the running proxy was *started* with. Where the
`config-pending` finding appears, the two differ and **the findings about the configured value
describe the next session, not the history you are reading**. Say that rather than telling somebody
to change a setting they already changed.

Give the recommended configuration as one block at the end, only where a finding actually argues
for it, and name what each change buys:

```
/plugin configure context-guru
  preset:         <name>   — <which components that adds, and the one finding that asked for it>
  cache strategy: <name>   — <what it spends, and the range it is expected to return>
```

**Say the spend out loud before recommending `5-min-ping`.** It uses the user's own credential while
nobody is at the keyboard, on every session routed through the proxy. It is the default because
holding a cache warm across a gap is what people install this for, not because it is free.

## Honesty rules that survive a friendly summary

- **On a Pro/Max subscription the saving lands in usage limits, not dollars.** The bill does not
  change. Say so rather than quoting a dollar figure at a subscriber as money back.
- **These totals are process-wide, not per-project.** Several projects routed through one proxy are
  all in here.
- **A fresh install shows almost nothing and that is correct.** The cache effect appears from a
  session's second turn; measured, 1,105 of 1,127 session starts are cold.
- **A freshly started proxy may have no price list yet**, so a first run right after a restart can
  report `decl_priced=false` and no dollars where a run a minute later has them. If the dollar
  figures are absent and the token figures are not, that is worth one sentence, not a diagnosis.
- **Do not add `usd_window` figures across findings into one total.** They are different kinds —
  already-spent, would-have-saved, and a range — and the sum is a number with no meaning. Quote the
  largest two or three.

## When there is nothing to report

`verdict=nothing to fix in what could be measured` is a real result and the wording matters: say it
plainly, name what was measured, and name anything in `unavailable=` that was not. Do not go looking
for something to recommend.

## Going deeper

Three focused commands share this data and read one part of it closely. Point at the one that
matches what the user actually asked, rather than re-running `all`:

- `/context-guru:insights-capabilities` — the MCP servers, tools and skills you carry and never call
- `/context-guru:insights-idle` — idle gaps, the cache misses they cause, and what keep-alive is worth
- `/context-guru:insights-components` — which components earned their place, and what is off
