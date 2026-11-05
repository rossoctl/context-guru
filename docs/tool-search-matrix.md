# Tool search matrix

**Rule: use a native search tool when there is one. Add our own only when there is none.**

This page lists which search tool each client and model uses when `ultrahigh` defers tools (#485,
#898). "Defers" means the tool stays out of the model's context until the model loads it.

| Client and model | Search tool | Who answers | What the proxy defers | On a break or disable | Issue |
|---|---|---|---|---|---|
| Claude Code, `ToolSearch` on, Claude model | `ToolSearch` (the client's) | Claude Code. Its search already finds the proxy's deferred tools (it indexes every tool it has). The proxy's added `tool_reference` is a safety net with nothing to add today: `client_loads` stays 0 (#871, comment 6642059429) | Tools the history rule picks | The proxy disables the `ToolSearch` description hash. It defers nothing on new threads with that hash. The live thread stops deferring (one cache rewrite). It never switches to another search tool | #898, this page |
| Claude Code, search off on purpose (`ENABLE_TOOL_SEARCH=false`) | None | - | Nothing | - | #485 |
| Claude Code, search off, not on purpose, Claude model | Anthropic's server-side search (`tool_search_tool_bm25`) | The provider | Tools the history rule picks | A 400 turns deferral off for the tenant and model | #485 |
| Codex, gpt-5.5 (declares its own `tool_search`, `execution: client`) | Codex's own `tool_search` | Codex | Nothing. The proxy adds no `context_guru_tools`. Codex's loads count as uses in the history | - | #876, #897 |
| Codex, gpt-5.6 and later | Codex's own `tool_search` | Codex | Nothing. Codex sends `additional_tools` and no `tools` | - | #876 |
| Chat Completions client with no search (GLM, Kimi, GPT chat) | Our `context_guru_tools` | The proxy, in band | Tools the history rule picks | A 400 turns deferral off for the tenant and model | #825, #879 |
| Responses client with no `tool_search` of its own (the proxy checks the tools, not the model) | Our `context_guru_tools` | The proxy, in band | Tools the history rule picks | A 400 turns deferral off for the tenant and model | #825, #884, #897 |
| Responses client with no search, gpt-5.6 and later | OpenAI's server-side search | The provider | Not built yet | - | #876 |

## What the proxy defers

**What it is.** Every request carries the full definition of every tool the client offers. Many
of them are never used. To defer a tool is to keep its definition out of the prompt, but not
out of reach: the model sees only the tool's name, and loads the definition through a
search tool when it needs it. Nothing is lost, so a wrong guess costs one search turn, not a
missing tool.

**How it works.**

- **On Claude models,** the tool stays in `tools` with `defer_loading: true`. The model sees its
  name in a short names list at `messages[1]`. That is Claude Code's own deferred-tools block when
  it exists, else a system message the proxy inserts there. A search loads the definition inline,
  so the cached prefix stays the same.
- **On GPT, GLM and Kimi,** which ignore `defer_loading`, the proxy removes the tool from `tools`
  and lists its name. When the model searches for it with `context_guru_tools`, the proxy adds the
  tool back, and keeps it for the rest of the thread.
- **The set is decided once per thread,** at its first request, and never changes inside the
  thread. A tool that moved between loaded and deferred would rewrite the whole cached prefix. The
  set is stored with tool names only, and a restart keeps it.

**The tool history rule.** A tool is deferred only when all of these hold:

1. The thread has not used it so far.
2. The user's last 3 sessions, in any project, declared it and used it in none.
3. This project did not use it in its last 10 sessions (the project veto).
4. It is not on the never-defer list. The list holds Bash, Read, Edit, Write, Agent, Skill,
   ToolSearch, AskUserQuestion, Context Guru's own tools, a tool that `tool_choice` forces, and
   the account's `keep` list.
5. It weighs at least 200 tokens on the original request. These are tokens of the proxy's own
   tokenizer (o200k): about 340 Claude tokens on Sonnet and Opus, about 275 on Haiku.

More details:

- **An MCP server is one unit.** All its tools are deferred together, or none is. A use of any of
  its tools counts as a use of the server, and the 200-token minimum applies to the server's total.
- **A load counts as a use.** A tool the model loaded through search stays loaded in the user's
  next sessions.
- **No history, no deferral.** With fewer than 3 sessions of history for the user, nothing is
  deferred.
- **The `always_defer` list** (#878) skips rules 2 and 3 and the 3-session minimum, for the tools
  or MCP servers on it. Rules 1, 4 and 5 still apply, and `keep` wins when a name is on both lists.
- **Turn it off** with `toolfilter: {defer: off}`, or leave `ultrahigh`. In Claude Code,
  `ENABLE_TOOL_SEARCH=false` also turns it off.

The code is in `tooldefer/tooldefer.go`: the rule in its package comment, the thresholds
(`UserSessions`, `ProjectSessions`, `MinTokens`) and the never-defer list.

## Claude Code with `ToolSearch`: how it works

- **Thread start.** The proxy checks a hash of the `ToolSearch` description. The description
  documents the query syntax, so the hash is the format version. An unknown hash, or a disabled
  one, defers nothing on the thread. A second probe checks that the API accepts the proxy's
  `tool_reference` blocks for the model. The mode is fixed per thread.
- **Search calls.** Every `ToolSearch` call goes to Claude Code. Its own search indexes every tool
  Claude Code has, also the tools the proxy deferred, so it already finds them and the model loads
  them. The proxy's addition is a safety net that has nothing to add today (`client_loads` stays 0;
  #871, comment 6642059429). On the next request, the proxy adds
  a `tool_reference` for each of its own deferred tools that the query names. Claude Code's result
  stays. The cap in `max_results` covers the combined result.
- **Accepted query.** Only `select:Name1,Name2`. The prefix is read in any case, names are split on
  `,` and trimmed. This is what Claude Code does (measured live). Every other query passes through.
- **Replay.** The first list is stored in the thread record by `tool_use` id. Every later request
  reads it from there. The bytes stay the same, also after a restart.

## When the proxy disables the mode

| Reason | Trigger |
|---|---|
| `unknown_description` | The `ToolSearch` description hash is not on the verified list |
| `break` | A call to a deferred tool got an error that says the tool is not loaded or does not exist. A direct call that worked is a use (`direct_calls`), not a break |

- **Who is affected:** the tenant, for that description hash, for 24 hours.
- **A keyword query passes through.** A query that names a deferred tool in a form other than
  `select:` disables nothing. Claude Code's own search finds it (#871).
- **What one break costs:** one cache rewrite on the live thread, because `tools` changes once.
- **How to clear the mark:** restart the proxy, or wait 24 hours.
- **How to verify a new Claude Code version:** run the live probe, then add the new hash to
  `knownDescHashes` in `tooldefer/toolsearch.go`.

## What production monitoring watches

- Metric `cg_tooldefer_client_disabled_total{reason="unknown_description|break"}`
- `/stats` fields `tool_defer.client_search_disabled` (the same two reasons), `client_loads`, `direct_calls`
- Log line `cg.tooldefer_client_search_disabled` with fields `reason`, `desc_hash` (8 characters),
  `session`, `thread`. It holds no query text.
