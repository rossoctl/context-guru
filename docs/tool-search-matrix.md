# Tool search matrix

**Rule: use a native search tool when there is one. Add our own only when there is none.**

This page lists which search tool each client and model uses when `ultrahigh` defers tools (#485,
#898). "Defers" means the tool stays out of the model's context until the model loads it.

| Client and model | Search tool | Who answers | What the proxy defers | On a break or disable | Issue |
|---|---|---|---|---|---|
| Claude Code, `ToolSearch` on, Claude model | `ToolSearch` (the client's) | Claude Code. The proxy adds `tool_reference` blocks for its own tools to the result | Tools the history rule picks | The proxy disables the `ToolSearch` description hash. It defers nothing on new threads with that hash. The live thread stops deferring (one cache rewrite). It never switches to another search tool | #898, this page |
| Claude Code, search off on purpose (`ENABLE_TOOL_SEARCH=false`) | None | - | Nothing | - | #485 |
| Claude Code, search off, not on purpose, Claude model | Anthropic's server-side search (`tool_search_tool_bm25`) | The provider | Tools the history rule picks | A 400 turns deferral off for the tenant and model | #485 |
| Codex, gpt-5.5 | Our `context_guru_tools` | The proxy, in band | Tools the history rule picks | A 400 turns deferral off for the tenant and model | #825 |
| Codex, gpt-5.6 and later | Codex's own `tool_search` | Codex | Nothing. Codex sends `additional_tools` and no `tools` | - | #876 |
| Chat Completions client with no search (GLM, Kimi, GPT chat) | Our `context_guru_tools` | The proxy, in band | Tools the history rule picks | A 400 turns deferral off for the tenant and model | #825, #879 |
| Responses client with no search, gpt-5.6 and later | OpenAI's server-side search | The provider | Not built yet | - | #876 |

## Claude Code with `ToolSearch`: how it works

- **Thread start.** The proxy checks a hash of the `ToolSearch` description. The description
  documents the query syntax, so the hash is the format version. An unknown hash, or a disabled
  one, defers nothing on the thread. A second probe checks that the API accepts the proxy's
  `tool_reference` blocks for the model. The mode is fixed per thread.
- **Search calls.** Every `ToolSearch` call goes to Claude Code. On the next request, the proxy adds
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
| `non_select_query_for_our_tool` | A query names a deferred tool of the proxy in a form other than `select:` |
| `break` | The model called a deferred tool that no search loaded, or got a "not loaded" error |

- **Who is affected:** the tenant, for that description hash, for 24 hours.
- **What one break costs:** one cache rewrite on the live thread, because `tools` changes once.
- **How to clear the mark:** restart the proxy, or wait 24 hours.
- **How to verify a new Claude Code version:** run the live probe, then add the new hash to
  `knownDescHashes` in `tooldefer/toolsearch.go`.

## What production monitoring watches

- Metric `cg_tooldefer_client_disabled_total{reason="unknown_description|non_select_query_for_our_tool|break"}`
- `/stats` field `tool_defer.client_search_disabled` (the same three reasons)
- Log line `cg.tooldefer_client_search_disabled` with fields `reason`, `desc_hash` (8 characters),
  `session`, `thread`. It holds no query text.
