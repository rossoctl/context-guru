# Data dependence

This page says, for every component and every feature, whether it needs **stored customer data**,
and what it does at each of three storage levels.

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
- **Kept**: held after the request ends, in memory or on disk. Text held only while one request
  runs is not kept.
- **Kind** in the tables: `none` (keeps nothing), `metadata`, or `content`. It is what the
  component keeps at the "Everything" level.
- **Decided by**: the person and the issue or PR where the current behaviour was decided. Where
  no later decision exists, it is the author of the PR that added the component. Ask that person
  before you change the row.

## The three storage levels

| Level | What is kept | Exists today? |
|---|---|---|
| **Off** | Nothing in the Store. Only the in-memory trackers that every request needs (how much of the conversation the provider cached, and when it was last used) are kept. These hold counts and timestamps only. | **Partly.** `store.enabled: false` removes the Store. It does **not** remove the in-memory content listed under [Features outside the pipeline](#features-outside-the-pipeline). |
| **Metadata only** | Hashes, counts, ids, token numbers, timestamps and flags, in the Store and in memory. No content. | **No.** The Store keeps everything or nothing. The "Metadata only" columns say what the code would do with a Store that refuses content, and what it would need to change. See [What "metadata only" would need](#what-metadata-only-would-need). |
| **Everything** | Content and metadata. | Yes, the default. |

At every level the agent still has its own copy of the transcript and sends it again each turn.
"No way back" means the model cannot get removed text back through the `expand` tool.

## Why the proxy stores rewrites at all

The client never sees the rewritten request, so it cannot keep it.

1. The client (for example Claude Code) sends its **whole transcript** on every request, as it
   has it locally.
2. The proxy rewrites that copy and sends the rewritten copy to the provider.
3. The provider returns **only the new assistant message**. The API has no way to hand an edited
   history back.
4. The client **adds** that message to its own transcript, which still holds the original tool
   outputs.
5. On the next turn the client sends the originals again, and the proxy must make the same rewrite
   again.

So:

- **A rewrite that depends only on the message needs no storage.** The proxy runs it again every
  turn and gets the same bytes. `format`, `toon`, `searchfold`, `textclean`, `toolfilter`,
  `cachesplit`, `cmdfilter`, `linecap`, `dedup`, `smartcrush` and `extract` work this way. Only
  CPU time is repeated.
- **Storage is needed only when the proxy cannot rebuild the rewrite from the message:**
  - **Model-written text** (`extract_llm`, `summarize`, `cache_aware_summarizer`, `agentdiet`).
    A model gives different words each time it is asked.
  - **Gated rewrites** (`mask`, `failed_run`, `collapse`, `readlifecycle`, `coref`). They act only
    on the part of the conversation that is not cached yet, so next turn they must know that they
    already acted. A flag is enough; see [What "metadata only" would need](#what-metadata-only-would-need).
- **The originals stored for `expand`** are a copy of what the client already has. They exist
  only because the proxy cannot ask the client for its copy.

**Where the data sits matters.** With the local plugin install, the Store is in the proxy process
on the user's own machine, next to a transcript that already holds everything. Storing content is
mainly a question for the hosted service, where the Store is on our side.

**A possible change, not checked:** a plugin that runs inside the client could rewrite the local
transcript itself. The client would then keep the shortened version, and the proxy would not need
to store it. Nobody has checked whether Claude Code's plugin or hook interface allows a plugin to
change earlier messages.

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
"Breaks the cache" means the next turn sends different bytes for a message the provider already
cached, so the user pays to write the cache again from that message on.

| Component | Kind | Where | Off | Metadata only | Everything | Reason | Decided by |
|---|---|---|---|---|---|---|---|
| `format` | none | Nothing kept | Works fully | Works fully | Works fully | It only removes whitespace from JSON tool output, and the result depends only on that message. The same input gives the same bytes every turn, so there is nothing to remember. | Osher Elhadad, first commit a91d6f4 |
| `toon` | none | Nothing kept | Works fully | Works fully | Works fully | It rewrites a uniform JSON array as a compact table, and checks that the table converts back to the exact original. The result depends only on the message, so nothing needs saving. | Osher Elhadad, release commit 7f0379a |
| `toolschema` | content | Memory: a cache of trimmed tool declarations, for the whole process, no size limit | Works fully. **Today the cache stays on.** | Works fully once the cache is removed. Only speed is lost. | Works fully | The agent sends the same tool list every turn, so it keeps the trimmed list to avoid doing the same work again. The trimming always gives the same result, so without the cache only speed changes. | Osher Elhadad, #84 |
| `searchfold` | none | Nothing kept | Works fully | Works fully | Works fully | It folds repeated file paths in search output into headings, using only that output. It is used only when the fold converts back to the exact original, so nothing needs saving. | Osher Elhadad, #89 |
| `cacheinject` | metadata | Store: one short hash per message of the last turn | Works with less: cache bookmarks only at fixed positions. An edited earlier message then costs the full price. | Works fully | Works fully | To put a cache bookmark just before the first message that changed, it must remember a fingerprint of each message from the last turn. It keeps only short hashes, never the text. | Osher Elhadad, first commit a91d6f4; budget in #32 |
| `cachesplit` | none | Nothing kept by the component. The dashboard keeps one hash per thread for savings reports. | Works fully | Works fully | Works fully | It cuts the system prompt so the part that changes every session comes after the cache bookmark. That depends only on the current request, so it needs no memory. | #32 |
| `textclean` | none | Nothing kept | Works fully | Works fully | Works fully | It only removes terminal colour codes and screen-redraw leftovers that were never visible text. The result depends only on the output itself, so nothing needs saving. | Osher Elhadad, #89 |
| `toolfilter` | none | Nothing kept | Works fully | Works fully | Works fully | It removes the tool, MCP-server and skill declarations the operator listed. It decides from that fixed list and the current request, so there is nothing to remember. | Osher Elhadad, #84 |
| `mask` | content | Store: the masked text it sent, and the original output | Shortens a message for one turn, then **breaks the cache** when the full text comes back. No way back. | Today: same as Off. **With a change:** works and keeps the cache stable, because the masked text is rebuilt from the message alone. No way back. | Works fully, with `expand` | It hides old tool outputs, and must send the same masked text every later turn. That text is built only from the message, so a flag that says "masked" is enough to rebuild it; the stored original is only for `expand`. | Osher Elhadad, first commit a91d6f4; storage rules in #187 |
| `failed_run` | content | Store: the shortened text it sent, and the original output | Shortens once, then **breaks the cache**. No way back. | Today: same as Off. **With a change:** works and keeps the cache stable, because the replacement is a fixed sentence. No way back. | Works fully, with `expand` | It replaces failed test or build runs that the agent has since run again with a fixed sentence. A flag that says "replaced" is enough to send the same sentence again; the original is stored only for `expand`. | Osher Elhadad, first commit a91d6f4 |
| `collapse` | content | Store: the head-and-tail text it sent, and the original output | Cuts once, then **breaks the cache**. No way back. | Today: same as Off. **With a change:** works and keeps the cache stable if it also remembers the cut size (a number), which can depend on the model's window size. No way back. | Works fully, with `expand` | It is the last-resort limit for very large outputs. The cut is rebuilt from the output and the cut size, so it needs to remember only a flag and one number; the full output is stored only for `expand`. | Osher Elhadad, first commit a91d6f4 |
| `readlifecycle` | content | Store: the rewrite it sent, and the original file read | Removes an old read once, then **breaks the cache**. No way back. | Today: same as Off. **With a change:** works and keeps the cache stable if it also remembers which label it used ("stale" or "superseded"). No way back. | Works fully, with `expand` | It removes file reads that a later edit or read made out of date. The label can change as the conversation goes on, so it must remember the label it first used, not the text; the original is stored only for `expand`. | Osher Elhadad, #89 |
| `skeleton` | content | Store: the code skeleton it sent, and the original source | **Skips.** | **Skips.** | Works fully, with `expand` | It removes function bodies from source code. Without the saved original, the agent could not tell a removed body from an empty one, so it refuses to run unless it can store the original. | Osher Elhadad, first commit a91d6f4 |
| `cmdfilter` | content | Store: the filtered text (a speed-up), and the original output | Works. Same bytes every turn, so the cache is safe. No way back. | Works, as Off | Works fully, with `expand` | It applies a filter for each known command to its output, and the same output always filters to the same text. It stores the original only so the agent can get dropped lines back. | Osher Elhadad, first commit a91d6f4; memo in #188 |
| `linecap` | content | Store: the original output | Works. Same bytes every turn. No way back. | Works, as Off | Works fully, with `expand` | It shortens very long lines and folds repeated lines, the same way every time. It stores the original only so that the agent can ask for the cut lines back. | Osher Elhadad, #89 |
| `dedup` | content | Store: the original output | Works. Same bytes every turn. The note still says "expand" with nothing to expand. | Works, as Off | Works fully, with `expand` | It replaces a repeated tool output with a pointer to the earlier copy, which is still in the same request. It stores the repeat only so that the agent can ask for it back. | Osher Elhadad, first commit a91d6f4 |
| `smartcrush` | content | Store: the original output | Works. Same bytes every turn. No way back. | Works, as Off | Works fully, with `expand` | It keeps the first and last items of a long JSON array, plus any error items, the same way every time. It stores the full array only so that the agent can ask for it back. | Osher Elhadad, first commit a91d6f4 |
| `extract` | content | Store: the original output | Works. Same bytes every turn. No way back. | Works, as Off | Works fully, with `expand` | It removes clear noise such as repeated lines and progress bars, the same way every time. It stores the original only so that the agent can ask for it back. | Osher Elhadad, first commit a91d6f4 |
| `coref` | content | Store: the cut text it sent, the original output, and a count of cache rebuilds it spent (metadata) | **No spending limit:** the count always reads zero. Each cut lasts one turn, then **breaks the cache**. | Today: **worse than useless.** The limit works, but it spends it on cuts that undo themselves next turn. **Turn it off.** **With a change:** works, because the stub it leaves is built from the cut text alone. | Works fully, with `expand` | It removes tool outputs that nothing later refers to, which deliberately changes the cached history. It must remember which outputs it cut and how many cache rebuilds it spent; the stub itself can be rebuilt from the output. | David Amid, commit b224704; docs/proposals/coref-compaction.md |
| `extract_llm` | content | Store: the model's rewrite and summary, a copy shared across sessions, and the original output. Memory: counts per session. | Pays one model call per new large output, applies it for one turn, then **breaks the cache**. On a provider without prompt caching it pays again every turn, up to 8 calls per turn. | Same as Off, and its cost check approves **more** calls: it remembers it saw an output, and counts on reuse that cannot happen. **Cannot work:** model text cannot be rebuilt. | Works fully: one call, then free reuse | A model rewrites each large tool output. The words differ each time it asks, so the only way to send the same bytes next turn is to keep the rewrite. That is content. | Osher Elhadad, #51; storage rules in #40 and #188 |
| `extract_llm_sweep` | content | Store: the removal decisions with a short description of each output, the original outputs, and "still needed" answers (metadata) | Has no copy of last turn's text, so it **pays full price** to send excerpts of up to 12 outputs to the model. Removals last one turn, then **break the cache**. | Today: same as Off. **With a change:** it can repeat its removals from a flag, because the description it leaves has no text. The cheap question to the model still needs last turn's text. | Works fully: a cheap question over the cached text, then free reuse | It asks the agent's model which old outputs are no longer needed. The cheap way to ask needs the exact text sent last turn; repeating the removal needs only a flag per output. | David Amid, #118 |
| `summarize` | content | Store: the model-written summary and the replaced turns. Memory: a copy of the turns while the model call runs. | **Does not start** by default: it cannot tell how full the window is. Without a session (`/compact`) it works in one request, with no way back. | **Pays for a summary every turn above 90% full and throws it away.** The next turn can wait up to 120 seconds for it. **Cannot work:** model text cannot be rebuilt. | Works fully | It replaces old turns with a model-written summary. The words differ each time, so it must keep the summary to send the same bytes next turn. At "metadata only" it can see the window is full but cannot keep the result. | Osher Elhadad, commit 68e8b51; checkpoint in #430 |
| `cache_aware_summarizer` | content | Store: the summary and the replaced turns. Memory: copies of conversation turns for summaries during idle time and after a cold turn. | **Does not start** by default: it cannot tell how full the window is. | It keeps copies of turns in memory, which is content. On Anthropic it cannot ask, so it costs nothing and does nothing. On OpenAI it **pays and throws the summary away** every turn. **Cannot work.** | Works fully | Like `summarize`, it must keep the summary to send the same bytes later. It also holds the conversation in memory so it can summarize while the user is idle. | Itay Nakash, #256 |
| `agentdiet` | content | Store: the model-shortened step it sent, and the original step | **Pays one model call per turn**, and the step it shortened last turn comes back in full, which **breaks the cache**. | Same as Off. **Cannot work:** model text cannot be rebuilt. | Works fully | A model rewrites one older agent step each turn. The words differ each time, so the history stays shortened only if every rewrite is kept and sent again. | Itay Nakash, #66 |

### Libraries that are not components

- **`dsl`** (the filter engine) keeps nothing per request. It holds only the rules compiled from
  your filter files. `cmdfilter` and `linecap` use it, and they store the originals.

## Features outside the pipeline

| Feature | Kind | Off | Metadata only | Everything | Reason |
|---|---|---|---|---|---|
| `expand` and the stashed originals | content | Not offered. Removals cannot be undone. | Not offered. | Works | When the proxy replaces a large output with a short marker, it must keep the original so the model can ask for it back. You cannot give back text you did not keep. |
| Frozen decisions and cached model results | content | All gone | Gone. Deterministic components could use a flag instead (see above). | Kept for about 2.8 hours | Once a message is shortened inside the cached prompt, the proxy must send exactly the same text every later turn, or the user pays to rebuild the cache. |
| Billed-token and cache-touch records | metadata | Gone. `summarize` cannot tell how full the window is. A warm cache can look cold when one conversation arrives under two ids. | **Kept.** This is the record that makes `summarize` start, and so start wasting calls. | Kept | To judge how full the context window is, the proxy needs the size the provider billed last turn, and the time the cached prompt was last used. |
| **Keep-alive pings** | content and a credential | **Today: still on**, holding the last body and the credential. Should be off. | Off | Works | To keep the prompt cache warm, the proxy must send the exact same prompt again with the user's own credential. Without the text there can be no ping. |
| **Prefix-ask body stash** | content | **Today: still on**, 2 full bodies per thread. Should be off. | Off. `extract_llm_sweep` then pays full price; `cache_aware_summarizer` gives up. | Works | To ask the agent's model a question cheaply, the proxy adds it to the exact text it sent last turn, which the provider already cached. |
| Observe-mode shadow store | content | **Today: still a real store.** Should follow the level. | Should follow the level | Works | Observe mode runs the pipeline later on a copy to estimate savings, and the copy should store what the real run would store. |
| Dashboard database | content and metadata | Off | Numbers, enums, ids, and tool, MCP-server and skill names. **No** before/after text, summaries, declaration text, error text or rejection text. | Everything, when content capture is on | The dashboard shows what was removed and what your setup carries. Every number on it needs only counts and prices. |
| Dashboard memos | content | Off with the dashboard | Hash and token count only. That is all they need. | Declaration and system-prompt text | They remember a tool list so they do not count its tokens again on every request. A hash and a count are enough. |
| Archive | content and metadata | Off | Metadata rows only | Everything | It moves old conversations off the local disk, so the database stays small and old history can still be opened. |
| Insights | metadata | Off (needs the dashboard) | Works | Works | A cost report needs only counts, tool names and prices, not the text. |
| Thread tracker | metadata | Works | Works | Works | It tells the main agent from its subagents by checking, with hashes, whether a new request continues an earlier one. |
| Session tracker | metadata | Works | Works | Works | It needs a stable label per conversation. When the client sends none, it makes one by hashing how the conversation starts. |
| Mode tracker | metadata | Works | Works | Works | It must know how much of the conversation the provider already cached, so it never edits that part. |
| Wait-cause and lifecycle signals | metadata | Off (needs the dashboard) | Works | Works | To tell why the agent paused, it reads the request once and keeps only counts and ids. |
| Metrics (`/stats`, `/metrics`) | metadata | Works | Works | Works | Totals and rates need only numbers. |
| Accounts (hosted) | account data | Works | Works | Works | This is the account system: who you are, your settings and your consent flag. It holds no transcripts. |

## What "metadata only" would need

This level does not exist yet. To add it:

1. **A separate "may store content" check**, asked before every model call and before offering
   `expand`. Today one flag answers both "can I keep the original?" and "can I keep the result?".
   Either answer makes some components pay for a model call and then throw the result away.
2. **An "acted on this" flag per message** for the rule-based components. 9 of the 11 can rebuild
   the exact same text next turn from the message the agent sends again, so the flag is enough to
   keep the cache stable. The code already has a path that rebuilds lost text; the flag would feed it.
3. **Two small extra values:** the cut size for `collapse`, and the label for `readlifecycle`.
4. **A replay path for `coref`.** Until it exists, turn `coref` off at this level.
5. **A replay path for `extract_llm_sweep` removals** from the flag.
6. **Turn off the model-written components** (`extract_llm`, `summarize`, `cache_aware_summarizer`,
   `agentdiet`) at this level. Their text cannot be rebuilt, so they either pay without result or
   break the cache.
7. **Turn off** keep-alive, the prefix-ask stash and `expand`. Keep only metadata columns in the
   dashboard, and only hashes and counts in its memos.

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

### Problems with storage off today

- **Lossy with no way back:** `mask`, `failed_run`, `collapse`, `readlifecycle`, `cmdfilter`,
  `linecap`, `dedup`, `smartcrush`, `extract`, `extract_llm`, `extract_llm_sweep`, `agentdiet`,
  `coref`, and `summarize` without a session. `skeleton` is the only one that refuses.
- **Breaks the cache:** `mask`, `failed_run`, `collapse`, `readlifecycle`, `extract_llm`,
  `extract_llm_sweep`, `agentdiet` and `coref` shorten a message once, then send it in full the
  next turn. Storage off can therefore cost more than no compaction.
- **Pays for model calls that last one turn:** `extract_llm`, `extract_llm_sweep` (at full price)
  and `agentdiet`.
- **`coref` loses its spending limit.**
- `summarize` and `cache_aware_summarizer` do not start. This is safe, but only by accident: they
  start as soon as the billed-token record is kept, as at "metadata only".
- Nothing warns at startup when these components run with storage off.

## Open questions

1. **Does "Off" allow in-memory metadata?** This page assumes yes. Without the tracker of how much
   the provider cached, components can edit the cached part of the conversation.
2. **Are tool, MCP-server and skill names metadata?** They are ids, but a user chooses them.
3. **Are content hashes metadata?** They are short, unsalted SHA-256 values. Anyone who can read
   the store can check whether a guessed exact text was seen. A secret salt would stop that.
4. **Is a client-sent session id metadata?** It can hold a user or account id.

## Comments that disagree with the defaults

- The dashboard's `CaptureContent` comment says it is on by default. It is off.
- The account `CaptureContent` comment says "Off by default". A new account starts with it **on**.
- The `LifecycleSignals` comment says "Off by default". The flag defaults to on (it writes only
  counts).

## Keep this page right

The rule is in [`CLAUDE.md`](https://github.com/rossoctl/context-guru/blob/main/CLAUDE.md) under
"Hard boundaries", and the PR template asks for it.

- Every PR that adds or changes a component or feature states its data dependence and what it does
  with storage off, and updates this page.
- If a PR changes a row's **Kind** or any of its three level columns, in either direction, it says so at
  the top of the PR. The person in "Decided by" confirms it before merge.
- Write the reason in plain words: **why** the component needs the data. A file name or line number
  is not a reason. The test rejects it.
