# Tool, MCP and skill inventory

*How much of what you carry do you actually use?*

An agent declares its whole toolbox — every tool, every MCP server, every skill — on
**every** request. Those declarations sit at the front of the prompt, inside the cached
prefix, and get re-read on every turn of the session. A tool the model never calls isn't
paid for once; it's paid for on every request of every session that carried it.

## What it measures

The dashboard scans the request's `tools` array and system-prompt skills listing (never
the conversation itself), records the token weight of each declaration, and tracks which
ones actually got called. Reading it: [`GET /api/tools`](reference/routes.md) for the
rollup, [`GET /api/prompt`](reference/routes.md) for one session's actual prefix text.

**Measured on real Claude Code traffic:** of the tokens a session declares in its
toolbox, **82.7%** are never invoked in that session. 27 declarations were carried by
every one of 51 sampled sessions and called by none of them.

## What to do about it

Name the dead weight in [`toolfilter`](components/toolfilter.md)'s `remove:` list — it
never removes a name you didn't list, and keeps anything still described in your system
prompt's own prose. See [Stop carrying unused tools](how-to/declaration-removal.md).

## Consent & privacy

Declaration **names and token weights** are gated on tenant scoping only — a tool name
is an identifier of your own configuration, not your transcript. The **text** of a
declaration (the schema itself, the skill listing, the system prompt) is transcript-class
data and requires the same operator + tenant consent pair that gates any other captured
content — see [Dashboard: access](dashboard.md#access). Without that consent the
measurement still works; only the text is withheld.

Skill discovery fails safe: a listing the parser can't confidently read is reported as
`unknown`, never as "zero skills declared" — which would license stripping a listing that
is actually still there.
