---
name: insights-capabilities
description: Which MCP servers, MCP tools and skills this machine declares on every request and has never actually invoked since they first appeared, what carrying them has cost, what removing each one would save, and the exact command to remove it. Use when the user asks about unused MCP servers or tools, unused skills, which plugins to uninstall, why their system prompt is so large, what is in their context window, whether an MCP server is worth keeping, or how much their tool declarations cost. Accepts --json.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/insights.py)
---

# What you carry and never call

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/insights.py" capabilities
```

**With `--json`**, add `--json` and print the output verbatim.

A tool declaration is not paid for once. It sits in the cached prefix and **every turn of the
session re-reads it** — measured, 65.2 requests per Claude Code session with tools. So a 4,000-token
MCP server you never call is not a 4,000-token problem, and that multiplier is the whole reason this
report exists.

## Lead with the share, not with a list

`decl_unused_pct` of what every request carries was never invoked, `decl_unused_reads` billed tokens
over `decl_sessions_captured` sessions, priced at `decl_unused_usd`. Give that first, with
`decl_set_tokens` and `system_prompt_tokens` for scale — the system prompt is normally the largest
single region of the prefix, and a composition that omits it reads as complete and is not.

**Say what the percentage excludes, once.** It counts MCP tools and skills only. Claude Code's own
tools (Read, Bash, Grep) are the largest group by weight and removing one breaks the agent, so a
figure that included them would be, in effect, advice to break the agent. That exclusion is why the
number is actionable.

## The per-item findings, and the line you must not cross

A `unused-*` finding is offered **only** for something declared in at least `suggest_min_sessions`
captured sessions spanning at least `suggest_min_days`, and invoked in **none** of them. Both halves
do different work: the session count is about opportunity (a few hundred turns in which the model
chose something else every time), the span is about variety (five sessions in one afternoon are five
sessions on one task, and the tool you did not need today is the one tomorrow needs).

**`suggest_withheld` is the count that did not clear that bar, and it is a feature.** Report it as
"looks unused, not yet observed long enough to say so" and offer no fix for it. A session with no
captured inventory is not a session that used nothing — treating it as one is how absence of
evidence turns into authorisation to break somebody's agent, and it is the single worst thing this
command could do.

For each finding, the `fix` is already the runnable form: a command where one exists, otherwise the
settings fragment and the file it goes in. **Read the `— effect:` clause out loud when it is
there.** Some perfectly valid ways to block a capability save nothing: denying a tool the model can
no longer call still leaves its schema in the prompt, so the prompt does not shrink. A fix that does
not shrink the prompt is not an answer to a cost finding, and the effect clause is where that is
stated.

Never paste `remove_as` at a user as an instruction. It is the string for the server-side filter's
own list (`skill__foo`, `mcp__server__tool`), not something anybody types.

## Per-server rollup

`server.<name>.*` is the per-server view and **the server is the unit of decision** — it is what a
user adds or removes. A server with three tools used between them is used, not "barely used" three
separate, undercounted ways. Read:

- `tools` vs `tools_used` — how much of what it brought is reached at all
- `sessions_declared` vs `sessions_used` — and `calls`
- `tokens_per_request` — its weight on every single request
- `unused_usd` with `priced` — **`priced=false` means no rate for those models, not free**

## Skills

`skills_state` has three values and conflating two of them is a real error:

- `ok` — the listing parsed. `skills_declared` / `skills_invoked` / `skills_listing_tokens` are real.
- `absent` — no session in the window carried a listing.
- `unknown` — a listing **was** present and could not be parsed. This is **not** "you declare no
  skills". Say the parse failed; `skills_unknown_sessions` counts them.

The listing is one indivisible block of prose in a system message, so it is waste only in sessions
that invoked **nothing** in it — which is exactly what `skills_listing_unused_usd` measures.
Removing one skill from a listing of ninety does not shrink the prompt by a ninetieth in any session
that used another skill. Do not imply otherwise, and do not divide the listing cost per skill.

## Credit what they already did

`self-removed-credit` is a finding with nothing to fix, and it is worth presenting rather than
skipping: a capability that stopped being declared partway through the window is a reduction the
user made themselves, which no component and no filter would ever have registered. Where
`self_removed` rows carry `overlap`, the same reduction may also be credited on a server-side filter
list — reported, never netted, because an account credited twice for one removal has a total nobody
can reconcile.

## `toolfilter-off`

When it appears, the dollar figure is **measured, not simulated**: it is the exact set of qualified
declarations the component would have withheld, priced at the tier the requests that carried them
were really billed. It is the only component-level "would have saved" figure in this whole surface
that is not an inference, and it is worth saying so. `toolfilter` is in the `house` and `housellm`
presets and in neither `off` nor `codesmart`.

## Never

- Never recommend removing a Claude Code built-in. They are excluded from every figure here on
  purpose; if one appears in a list you have built, you built it wrong.
- Never present a withheld item as a recommendation with a caveat attached. It is not a
  recommendation yet.
- Never quote `decl_unused_usd` as money the user will get back on a Pro/Max subscription. There the
  saving lands in usage limits and the bill does not change.
