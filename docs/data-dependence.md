# Data dependence

This page says, for every component and every feature, whether it needs **stored customer data**,
and what it does when you turn storage off.

**The rule:** a component or feature works from **metadata** only. Depending on **content** is a
rare, deliberate exception. Many users turn storage off. A feature that quietly needs content stops
working for them, or works worse, and nobody notices. See [Keep this page right](#keep-this-page-right).

## Terms

- **Content**: text that came from the user or the agent, or text made from it. This includes
  transcript text, tool output, request bodies, system prompts, tool declarations, and anything a
  model or a rule wrote from them (a summary, a shortened tool output, a code skeleton).
- **Metadata**: values that do not let you read the content back: hashes, counts, sizes, ids,
  token numbers, timestamps, flags. **A short hash or fingerprint of content is metadata.** A
  shortened copy, a sample, or the first N characters of content is content.
- **Storage off**: `store.enabled: false` in the config, or `--store=false`. The proxy then uses a
  store that keeps nothing (`store.Nop`). This switch does **not** turn off the dashboard, the
  keep-alive, or the in-memory stashes in the table under
  [Features outside the pipeline](#features-outside-the-pipeline).
- **Content capture off**: the dashboard's `--dashboard-content` is off (the default), or, in
  hosted mode, the account's consent flag is off. This switch controls only the dashboard.
- **Kind** in the tables: `none` (keeps nothing between requests), `metadata`, or `content`.
- **Decided by**: the person and the issue or PR where the current behaviour was decided. Where
  no later decision exists, it is the author of the PR that added the component. Ask that person
  before you change the row.

## What turns off when

| Switch | Default | Turns off | Does not turn off |
|---|---|---|---|
| `store.enabled` (YAML) | on | The Store: stashed originals, frozen decisions, cached model results, billed-token and cache-touch records. The `expand` tool is no longer offered. | Observe-mode shadow store, the prefix-ask body stash, keep-alive bodies and credentials, component caches in memory, the dashboard |
| `--store` / `STORE` | follows YAML | Overrides the operator's `store.enabled` | In hosted mode, each account's own `store.enabled` |
| `--dashboard` / `DASHBOARD` | off (the plugin turns it on) | The whole dashboard database and its in-memory memos. Also stops keep-alive from keeping bodies. | The prefix-ask body stash, the Store |
| `--dashboard-content` / `DASHBOARD_CONTENT` | off | New writes of `request_content`, extraction-call text and `declaration_text` | Tool, MCP-server and skill names; `extraction_calls.rejection`; `request_components.err`; rows already written; keep-alive; the body stash; the memos |
| Account consent `capture_content` (hosted) | **on** for a new account | The same as `--dashboard-content`, per account. Both must be on. | The same as the row above. Turning it off deletes nothing. |
| `--dashboard-retention` and size limits | 7 days, 512 MiB | Old dashboard rows, with their content | `declaration_text`, `tenant_spend`, `archived_sessions` |
| `--archive-remote` | off | Upload of old sessions to remote storage | — |
| `cache.keepalive` (YAML), plugin strategy `none` | off in Go, `5-min-ping` in the plugin | Keep-alive bodies, credentials and pings | Manager strategies and per-session overrides can turn it on again |
| `CONTEXT_GURU_KEEPALIVE=off` | unset | All keep-alive, everywhere | — |
| `INJECT_EXPAND` | `auto` | Offering the `expand` tool to the model | Stashing the originals |
| `CONTEXT_GURU_DUMP`, `CONTEXT_GURU_CAPTURE` | unset | (opt-in) Write before/after text or whole request bodies to a file, with no redaction. Refused at startup in hosted mode. | — |
| `CG_LOG_LEVEL=debug` | `info` | (opt-in) Logs the first 160 characters of each tool output | — |

There is **no switch** for the prefix-ask body stash.

## Pipeline components

One row per registered component. A test (`components/all/datadep_test.go`) fails when a component
has no row, when a row names a component that does not exist, or when the reason is missing or is
only a pointer into the code.

"Store" means the in-memory Store. All Store data is in memory, and none of it is written to disk.

| Component | Kind | Where | Required? | With storage off | Reason | Decided by |
|---|---|---|---|---|---|---|
| `format` | none | Nothing kept | Not applicable | No change | It only removes whitespace from JSON tool output, and the result depends only on that message. The same input gives the same bytes every turn, so there is nothing to remember. | Osher Elhadad, first commit a91d6f4 |
| `toon` | none | Nothing kept | Not applicable | No change | It rewrites a uniform JSON array as a compact table, and checks that the table converts back to the exact original. The result depends only on the message, so nothing needs saving. | Osher Elhadad, release commit 7f0379a |
| `toolschema` | content | Memory: a cache of trimmed tool declarations, for the whole process, no size limit | No, it is a speed-up | No change. The cache stays on. | The agent sends the same tool list every turn, so it keeps the trimmed list to avoid doing the same work again. The trimming always gives the same result, so without the cache only speed changes. | Osher Elhadad, #84 |
| `searchfold` | none | Nothing kept | Not applicable | No change | It folds repeated file paths in search output into headings, using only that output. It is used only when the fold converts back to the exact original, so nothing needs saving. | Osher Elhadad, #89 |
| `cacheinject` | metadata | Store: one short hash per message of the last turn | No, it improves one rule | Works with less. It cannot see where this turn first differs from the last one, so it puts cache bookmarks only at its fixed positions. An edited earlier message then costs the full price instead of the cached price. | To put a cache bookmark just before the first message that changed, it must remember a fingerprint of each message from the last turn. It keeps only short hashes, never the text. | Osher Elhadad, first commit a91d6f4; budget in #32 |
| `cachesplit` | none | Nothing kept by the component. The dashboard keeps one hash per thread for savings reports. | Not applicable | No change | It cuts the system prompt so the part that changes every session comes after the cache bookmark. That depends only on the current request, so it needs no memory. | #32 |
| `textclean` | none | Nothing kept | Not applicable | No change | It only removes terminal colour codes and screen-redraw leftovers that were never visible text. The result depends only on the output itself, so nothing needs saving. | Osher Elhadad, #89 |
| `toolfilter` | none | Nothing kept | Not applicable | No change. It is lossy by design, with or without storage: the client keeps its own copy of what it removes. | It removes the tool, MCP-server and skill declarations the operator listed. It decides from that fixed list and the current request, so there is nothing to remember. | Osher Elhadad, #84 |
| `mask` | content | Store: the masked text it sent, and the original output | Yes, for a stable cache and for `expand` | Works with less. The mask cannot be undone. Next turn the message goes back to its full text, which breaks the cached prompt. | It hides old tool outputs, and must remember exactly what it wrote so every later turn sends the same bytes. It keeps the original so the agent can ask for it back. | Osher Elhadad, first commit a91d6f4; storage rules in #187 |
| `failed_run` | content | Store: the shortened text it sent, and the original output | Yes, for a stable cache and for `expand` | Works with less. The cut cannot be undone, and the message goes back to full text next turn, which breaks the cached prompt. | It shortens failed test or build runs that the agent has since run again. It must remember the shortened text so the cached prompt stays the same, and keeps the original for the agent to ask back. | Osher Elhadad, first commit a91d6f4 |
| `collapse` | content | Store: the head-and-tail text it sent, and the original output | Yes, for a stable cache and for `expand` | Works with less. The cut cannot be undone, and the message goes back to full text next turn, which breaks the cached prompt. | It is the last-resort limit for very large outputs. It must remember its cut so the start of the prompt stays the same, and keeps the full output so the agent can ask for it. | Osher Elhadad, first commit a91d6f4 |
| `readlifecycle` | content | Store: the rewrite it sent, and the original file read | Yes, for a stable cache and for `expand` | Works with less. Old file reads are removed with no way back, and they come back in full next turn, which breaks the cached prompt. | It removes file reads that a later edit or read made out of date. It must remember its rewrite so the cached prompt does not change, and keeps the original for the agent to ask back. | Osher Elhadad, #89 |
| `skeleton` | content | Store: the code skeleton it sent, and the original source | **Yes, it will not run without storage** | Skips. It leaves every message as it is. | It removes function bodies from source code. Without the saved original, the agent could not tell a removed body from an empty one, so it refuses to run without storage. | Osher Elhadad, first commit a91d6f4 |
| `cmdfilter` | content | Store: the filtered text (a speed-up), and the original output | No, only for `expand` | Works with less. It filters again every turn and gets the same bytes, so the cache is safe. Dropped lines cannot be recovered. | It applies a filter for each known command to its output. It keeps the original so the agent can get dropped lines back. The saved result is only a speed-up, because the same output always filters to the same text. | Osher Elhadad, first commit a91d6f4; memo in #188 |
| `linecap` | content | Store: the original output | No, only for `expand` | Works with less. Long lines are cut with no way back. | It shortens very long lines and folds repeated lines. It stores the original only so that the agent can ask for the cut lines back. | Osher Elhadad, #89 |
| `dedup` | content | Store: the original output | No, only for `expand` | Works with less. The pointer cannot be followed through `expand`, but the first copy is still in the same request. | It replaces a repeated tool output with a pointer to the earlier copy. It saves the repeat only so that the agent can ask for it back. | Osher Elhadad, first commit a91d6f4 |
| `smartcrush` | content | Store: the original output | No, only for `expand` | Works with less. Long JSON arrays are cut with no way back. | It keeps the first and last items of a long JSON array, plus any error items. It stores the full array only so that the agent can ask for it back. | Osher Elhadad, first commit a91d6f4 |
| `extract` | content | Store: the original output | No, only for `expand` | Works with less. Removed noise cannot be recovered. | It removes clear noise such as repeated lines and progress bars. It stores the original only so that the agent can ask for it back. | Osher Elhadad, first commit a91d6f4 |
| `extract_llm` | content | Store: the model's rewrite and summary, a copy shared across sessions, and the original output. Memory: counts per session. | Yes, to reuse a rewrite on later turns and across sessions | Works with less. New rewrites still call the model and cannot be undone. Next turn they are not reused, so the message goes back to full text and breaks the cached prompt. Reuse across sessions stops. | A model rewrites each large tool output. The result is saved because asking again costs money and can give different words, which would break the prompt cache. The original is saved so the agent can ask for it back. | Osher Elhadad, #51; storage rules in #40 and #188 |
| `extract_llm_sweep` | content | Store: the removal decisions with a short description of each output, the original outputs, and "still needed" answers (metadata) | Yes, to repeat its removals on later turns | Works with less. Removals still happen, with no way back, and come back in full next turn. It asks the model again about outputs it already judged. | It asks the agent's own model which old outputs are no longer needed, and removes them. It must remember those choices so that later turns send the same shortened transcript. | David Amid, #118 |
| `summarize` | content | Store: the model-written summary and the replaced turns. Memory: a copy of the turns while the model call runs. | Yes, for proxy traffic | **With a session (proxy):** it still pays for the model call, then throws the summary away, every time it triggers. Nothing is removed. **Without a session (`/compact`):** it summarizes and removes the turns with no way back. | It replaces old turns with a model-written summary. It must keep that summary so the next turn sends the same bytes and the prompt cache still matches, and keeps the replaced turns so they can be restored. | Osher Elhadad, commit 68e8b51; checkpoint in #430 |
| `cache_aware_summarizer` | content | Store: the summary and the replaced turns. Memory: copies of conversation turns for summaries during idle time and after a cold turn. | Yes, a summary is never used without it | **Pays for model calls and throws the results away.** Nothing is removed. The copies in memory are still kept. | Like `summarize`, it must keep the summary so later turns send the same bytes. It also holds the conversation in memory so it can summarize while the user is idle or after the cache went cold. | Itay Nakash, #256 |
| `agentdiet` | content | Store: the model-shortened step it sent, and the original step | Yes, the method builds on earlier turns | Works with less. Each step still costs a model call and cannot be undone. Next turn it goes back to full text, so one turn of saving costs two cache rebuilds. | A model rewrites one older agent step. The rewrite must be saved and sent again every turn, so the history stays shortened and the cache stays stable. The original is kept so the agent can ask for it back. | Itay Nakash, #66 |
| `coref` | content | Store: the cut text it sent, the original output, and a count of cache rebuilds it spent (metadata) | **Yes, for its spending limit** | Works with less, **without its spending limit.** The rebuild count always reads zero, so it can rebuild the cache on every turn. Cuts cannot be undone and come back in full next turn. | It removes tool outputs that nothing later refers to, which deliberately changes the cached history. It must remember what it cut and how many cache rebuilds it already spent, or it repeats that cost with no limit. | David Amid, commit b224704; docs/proposals/coref-compaction.md |

### Libraries that are not components

- **`dsl`** (the filter engine) keeps nothing per request. It holds only the rules compiled from
  your filter files. `cmdfilter` and `linecap` use it, and they store the originals.

## Features outside the pipeline

| Feature | Kind | Where | Required? | With storage off | With content capture off | Reason |
|---|---|---|---|---|---|---|
| `expand` and the stashed originals | content | Store | No | The `expand` tool is not offered. Removals cannot be undone. | No effect | When the proxy replaces a large output with a short marker, it must keep the original so the model can ask for it back. |
| Frozen decisions and cached model results | content | Store (pinned, about 2.8 hours) | No | All gone. Components work again from the request, or skip. | No effect | Once a message is shortened inside the provider's cached prompt, the proxy must send exactly the same text every later turn, or the user pays to rebuild the cache. It also keeps paid model answers so it does not pay twice. |
| Billed-token and cache-touch records | metadata | Store | No | Not kept. `summarize`'s "how full is the window" trigger cannot fire. | No effect | To judge how full the context window is, the proxy needs the size the provider billed last turn, and the time the cached prompt was last used. |
| **Keep-alive pings** | **content and a credential** | Memory, masked, until the next request or about 14 minutes | No | **Still kept.** Pings still go out. | **Still kept** | To keep the prompt cache warm while the user is away, the proxy must send the exact same prompt again with the user's own credential. |
| **Prefix-ask body stash** | **content** | Memory: 2 full bodies per thread, up to 96 MB | No | **Still kept** | **Still kept** | To ask the agent's model a question cheaply, the proxy adds it to the exact text it sent last turn, which the provider has already cached. That needs the last turn's full text. |
| Observe-mode shadow store | content | Memory | Only in observe mode | **Still a real store**, it ignores the switch | No effect | Observe mode runs the pipeline later on a copy to estimate savings, and that copy needs a store to act like the real one. |
| Dashboard database | content and metadata | SQLite file | No | No effect | Content columns stay empty. Names of tools, MCP servers and skills, error text and rejection text are still written. Rows written before stay. | The dashboard shows what was removed and what your setup carries. Every number on it needs only counts and prices. |
| Dashboard memos | content | Memory: declaration and system-prompt text | No | No effect | **Still kept** while the dashboard is on | It remembers declaration text so it does not count the tokens of the same tool list again on every request. |
| Archive | content and metadata | Remote object storage, plus a local index | No | No effect | Archives only what was already captured | It moves old conversations off the local disk, so the database stays small and old history can still be opened. |
| Insights | metadata | Reads the dashboard, keeps nothing | No | No effect | No effect | A cost report needs only counts, tool names and prices, not the text. |
| Thread tracker | metadata | Memory | Yes | No effect | No effect | It tells the main agent from its subagents by checking, with hashes, whether a new request continues an earlier one. |
| Session tracker | metadata | Nothing kept | Yes | No effect | No effect | It needs a stable label per conversation. When the client sends none, it makes one by hashing how the conversation starts. |
| Mode tracker | metadata | Memory | Yes | No effect | No effect | It must know how much of the conversation the provider already cached, so it never edits that part. |
| Wait-cause and lifecycle signals | metadata | Memory, plus dashboard rows | No | No effect | No effect | To tell why the agent paused (a tool, a person, a subagent), it reads the request once and keeps only counts and ids. |
| Metrics (`/stats`, `/metrics`) | metadata | Memory | Yes | No effect | No effect | Totals and rates need only numbers. |
| Accounts (hosted) | account data | Control database | Hosted only | No effect | No effect | This is the account system: who you are, your settings and your consent flag. It holds no transcripts. |

## What this audit found

The audit read the code at commit 2a7a86f. The items below are what the code does today. Each one
needs a decision from the owner; this page does not change any behaviour.

### Content kept after the user opted out

1. **The prefix-ask body stash** keeps 2 full request bodies per thread on every successful request.
   No switch turns it off, and it fills even when no component uses it.
2. **Keep-alive** keeps the last request body and the user's credential with storage off and with
   content capture off. The plugin turns keep-alive on by default.
3. **`declaration_text` is never deleted**, not by retention and not by an account purge. Tool
   declarations and whole system prompts written while consent was on stay after it is off.
4. **Turning consent off deletes nothing.** Rows already in `request_content`, `extraction_calls`
   and the archive stay until retention or a purge removes them.
5. **Some dashboard columns are written without consent:** tool, MCP-server and skill names,
   `extraction_calls.rejection` (can hold the provider's error text), and `request_components.err`
   (not scrubbed).
6. **In-memory copies ignore both switches:** the dashboard memos, the `toolschema` cache, the
   `cache_aware_summarizer` copies of conversation turns, and the observe-mode shadow store.
7. **In hosted mode `--store` changes only the operator's config.** Each account follows only its
   own `store.enabled`.

### Lossy with no way back when storage is off

- `mask`, `failed_run`, `collapse`, `readlifecycle`, `cmdfilter`, `linecap`, `dedup`, `smartcrush`,
  `extract`, `extract_llm`, `extract_llm_sweep`, `agentdiet`, `coref`, and `summarize` without a
  session.
- The agent still has its own copy of the transcript. What is lost is the model's way to get the
  text back through `expand`.
- `skeleton` is the only component that refuses to run.
- Nothing warns at startup when these run with storage off.

### Other problems with storage off

- **`summarize` (with a session) and `cache_aware_summarizer` pay for model calls and throw the
  results away**, every time they trigger.
- **`coref` loses its spending limit.** Its count of cache rebuilds always reads zero.
- `mask`, `failed_run`, `collapse`, `readlifecycle`, `extract_llm`, `extract_llm_sweep`, `agentdiet`
  and `coref` send a shortened message once, then the full message on the next turn. This breaks
  the cached prompt, so storage off can cost more than no compaction.

### Comments that disagree with the defaults

- The dashboard's `CaptureContent` comment says it is on by default. It is off.
- The account `CaptureContent` comment says "Off by default". A new account starts with it **on**.
- The `LifecycleSignals` comment says "Off by default". The flag defaults to on (it writes only
  counts).

## Keep this page right

The rule is in [`CLAUDE.md`](https://github.com/rossoctl/context-guru/blob/main/CLAUDE.md) under
"Hard boundaries", and the PR template asks for it.

- Every PR that adds or changes a component or feature states its data dependence and what it does
  with storage off, and updates this page.
- If a PR changes a row's **Kind** or its storage-off behaviour, in either direction, it says so at
  the top of the PR. The person in "Decided by" confirms it before merge.
- Write the reason in plain words: **why** the component needs the data. A file name or line number
  is not a reason. The test rejects it.
