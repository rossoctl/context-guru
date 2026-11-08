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
| Codex, gpt-5.6 and later | None (Codex sends an empty `tools` array and puts its tools in `additional_tools`) | - | Nothing. There is nothing to defer | - | #876 |
| Chat Completions client with no search (GLM, Kimi, GPT chat) | Our `context_guru_tools` | The proxy, in band | Tools the history rule picks | A 400 turns deferral off for the tenant and model | #825, #879 |
| Responses client with no `tool_search` of its own, any non-Claude model (the proxy checks the model name, then the tools) | Our `context_guru_tools` | The proxy, in band | Tools the history rule picks | A 400 turns deferral off for the tenant and model | #825, #884, #897 |

**Planned, not built (low priority):** OpenAI's server-side search for a Responses client with no
search of its own, on gpt-5.6 and later (#876). Today such a client uses `context_guru_tools` (the
last row). Only one kind of client would gain from it:

| Client | Search tool today | Does #876 help? |
|---|---|---|
| Codex on gpt-5.5 | Codex's own `tool_search`, run by Codex | No. The proxy keeps Codex's search and defers nothing (#897). |
| Codex on gpt-5.6 and later | None: Codex sends an empty `tools` array, and its tools go in `additional_tools` | No. There is nothing to defer. |
| **An app on the OpenAI Responses API directly** (the OpenAI SDK, an agent framework) **with no search tool of its own**, gpt-5.6 and later | Our `context_guru_tools` | **Yes.** It is the only case. OpenAI's search would replace ours. |
| The same app, but it declares its own `tool_search` | The app's own search | No. Native search first. |
| A Chat Completions client (GLM, Kimi, GPT chat) | Our `context_guru_tools` | No. #876 is for the Responses API only. |

Even there the gain is small:

- **Gain:** the tool stays in `tools` with `defer_loading`, so it is not removed and added back.
- **Cost:** each server-side search is a paid tool call on the OpenAI side.

So #876 waits for a real client that needs it.

## Configuration by client

All five setups use the `ultrahigh` preset. It is `xhigh` plus `toolfilter` with `defer: auto`
(#485). No other preset defers a tool.

**Turn it on.** Use one of these:

- **The plugin preset picker:** `/context-guru:preset-picker`, then `ultrahigh`. It runs
  `settings.py preset set --name ultrahigh`. The first session after the change defers nothing,
  because the provider check runs first (Claude models only).
- **The proxy flag:** `context-guru-proxy --preset ultrahigh`, or `PRESET=ultrahigh`.
- **The config file:** `preset: ultrahigh` in the YAML given to `--config`.

**The `toolfilter` keys.**

| Key | Values | Meaning |
|---|---|---|
| `defer` | `auto` or `off` | `auto` defers tools by the history rule. `off` (the default) defers nothing. |
| `keep` | list of names | Never defer these. It wins over `always_defer`. |
| `always_defer` | list of names | Defer these in every new thread, without the history rule (#878). The default is empty. |

A name is a tool name, or `mcp__<server>` for a whole server.

```yaml
preset: ultrahigh
components:
  toolfilter:
    defer: auto
    keep: [mcp__github]
    always_defer: [mcp__jira, NotebookEdit]
```

The same keys apply to every client below. Only the client-side settings differ.

### Claude Code, `ToolSearch` on

- **Turn on:** pick `ultrahigh` in the plugin preset picker. This is the normal setup.
- **Client setting:** none needed. Claude Code sends `ToolSearch`, and the model loads deferred
  tools through it.
- **Turn off:** `toolfilter: {defer: off}`, or use a lower preset.
- **Watch** `/stats` `tool_defer`:
  - `client_search_disabled` (`unknown_description`, `break`): above 0 means the mode is off for a
    `ToolSearch` description hash.
  - `client_loads`: stays 0 today. See the table above.
  - `direct_calls`: the model called a deferred tool without a search. This is a use, not a break.
  - `requests` and `retries_400`: deferred requests, and 400s and 422s that the proxy retried with the original.

### Claude Code, `ToolSearch` off

Two cases. The result differs.

| Setting | Result |
|---|---|
| `ENABLE_TOOL_SEARCH=false` (also `0`, `no`, `off`) | A deliberate opt-out. **Nothing is deferred.** The plugin's session-start hook sends the value to the proxy. |
| `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` | Claude Code sends no `ToolSearch`, so the thread uses the server-side search (`tool_search_tool_bm25`). |

- **Turn deferral off:** set `ENABLE_TOOL_SEARCH=false` for one session, or `toolfilter: {defer: off}`
  for the account.
- **Watch:** `requests`, `retries_400` and `turned_off`. A 400 or 422 on a deferred request turns deferral
  off for the tenant and model.

### Codex

- **Turn on:** pick `ultrahigh` in the Codex plugin preset picker, or use `--preset` or the config file.
- **Codex on gpt-5.5 and later keeps its own `tool_search`.** The proxy adds no `context_guru_tools`
  and **defers nothing** (#876). `ultrahigh` still runs its other components.
- **Codex uses HTTPS.** It tries a WebSocket first. The proxy answers the upgrade with 426, so Codex
  falls back to HTTPS at once.
- **`ENABLE_TOOL_SEARCH` does not apply.** The Codex plugin has no hook that sends it.
- **Turn deferral off:** `toolfilter: {defer: off}`.

### Chat Completions client (GLM, Kimi, GPT chat)

- **Turn on:** `--preset ultrahigh` or the config file. No provider check runs, so the first request
  of a thread can already defer.
- **How it works:** the proxy removes the tool from `tools`. The model loads it through
  `context_guru_tools`, and the proxy adds it back for the rest of the thread.
- **Client setting:** none.
- **Turn off:** `toolfilter: {defer: off}`.
- **Watch:**
  - `add_backs`: tools the search added back. Each one costs one cache miss on `tools`.
  - `retries_400` and `turned_off`: a 400 or 422 turns deferral off for the tenant and model.
  - `wrong_guesses`: a deferred tool that a search then loaded.

### Responses client with no search of its own

- **Turn on:** `--preset ultrahigh` or the config file. The proxy checks the model name, then the tools in the request.
- **Claude models:** a Claude model on this wire defers nothing today. The `defer_loading` path does not run on Responses.
- **Request shape:** the request `input` must be an array. A string `input` defers nothing
  (`input_not_array`). A request with `previous_response_id` makes no new decision.
- **Turn off:** `toolfilter: {defer: off}`.
- **Watch:** `add_backs`, `retries_400` and `turned_off`, as for Chat Completions.
- **Planned:** OpenAI's server-side search (#876).

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
- **Turn it off** with `toolfilter: {defer: off}`, or use a lower preset. In Claude Code,
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
