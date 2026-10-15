# Data dependence

This page says, for each component and feature, whether it needs **stored customer data**. It also
says what each one does at three storage levels: off, metadata only, and everything.

## Summary

- **The rule:** a component or feature works from **metadata** only. A feature that needs
  **content** is a rare, deliberate exception. Many users turn storage off, and a feature that
  quietly needs content then works worse, with nothing reported.
- **13 of 24 components need no stored content to do their job:**
  - 6 keep nothing.
  - 1 keeps only hashes.
  - 1 keeps only a speed-up cache.
  - 5 keep only the original. Without it they still work, but the `expand` tool cannot give it back.
- **5 more could work on metadata only**, with a small change: one "already done" flag per message.
- **4 components cannot work without content.** A model writes their text, and the words differ
  each time. These are `extract_llm`, `summarize`, `cache_aware_summarizer` and `agentdiet`.
- **2 are special cases.** `skeleton` refuses to run without storage. `extract_llm_sweep` could
  repeat its removals from a flag, but its cheap question to the model needs last turn's text.
- **The proxy keeps content after the user opts out**, in 7 places. The worst are 2 full request
  bodies per thread, and the user's credential for keep-alive.
- **"Metadata only" does not exist yet.** Today the Store keeps everything or nothing.

## Terms

- **Content**: text from the user or the agent, or text made from it. Examples: transcript text,
  tool output, request bodies, system prompts, tool declarations. A summary, a shortened tool
  output and a code skeleton are content too.
- **Metadata**: values that do not let you read the content back. Examples: hashes, counts,
  sizes, ids, token numbers, timestamps, flags. **A short hash of content is metadata.** A
  shortened copy, a sample, or the first characters of content are content.
- **Kept**: held after the request ends, in memory or on disk. Text held only while one request
  runs is not kept.
- **Store**: the proxy's in-memory key-value store for each conversation. It is never written to
  disk. `store.enabled: false` replaces it with a Store that keeps nothing.
- **Original**: the text of a tool output before a component shortened it. The proxy keeps it in
  the Store so the model can get it back with the `expand` tool.
- **Frozen decision**: the exact shortened text a component sent. The proxy keeps it in the Store
  and sends the same text on every later turn.
- **Breaks the cache**: the next turn sends different bytes for a message the provider already
  cached. The user then pays to write the cache again, from that message to the end.
- **No way back**: the model cannot get removed text back through the `expand` tool. The agent
  still has its own copy.
- **Kind** (in the tables): `none` (keeps nothing), `metadata`, or `content`. It is what the
  component keeps at the "Everything" level.
- **Decided by**: the person, and the issue or PR, that decided the current behaviour. If no later
  decision exists, it is the author of the PR that added the component. Ask that person before
  you change the row.

## The three storage levels

| Level | What the proxy keeps | Exists today? |
|---|---|---|
| **Off** | Nothing in the Store. It keeps only the in-memory trackers that every request needs: how much of the conversation the provider cached, and when. These hold counts and timestamps. | **Partly.** `store.enabled: false` removes the Store. It does **not** remove the in-memory content in [Features outside the pipeline](#features-outside-the-pipeline). |
| **Metadata only** | Hashes, counts, ids, token numbers, timestamps and flags, in the Store and in memory. No content. | **No.** The "Metadata only" columns say what the code would do with a Store that refuses content. [What "metadata only" would need](#what-metadata-only-would-need) lists the changes. |
| **Everything** | Content and metadata. | Yes. This is the default. |

## Why the proxy stores rewrites at all

The client never sees the rewritten request, so it cannot keep it.

1. The client (for example Claude Code) sends its **whole transcript** on every request.
2. The proxy rewrites that copy and sends the result to the provider.
3. The provider returns **only the new assistant message**. The API cannot hand an edited history
   back.
4. The client **adds** that message to its own transcript. Its transcript still holds the
   original tool outputs.
5. On the next turn the client sends the originals again. The proxy must make the same rewrite
   again.

The results:

- **A rewrite that depends only on the message needs no storage.** The proxy runs it again every
  turn and gets the same bytes. Only CPU time is repeated. These components work this way:
  `format`, `toon`, `searchfold`, `textclean`, `toolfilter`, `cachesplit`, `cmdfilter`, `linecap`,
  `dedup`, `smartcrush` and `extract`.
- **Storage is necessary only when the proxy cannot rebuild the rewrite from the message:**
  - **Model-written text** (`extract_llm`, `summarize`, `cache_aware_summarizer`, `agentdiet`). A
    model gives different words each time.
  - **Gated rewrites** (`mask`, `failed_run`, `collapse`, `readlifecycle`, `coref`). They act only
    on the part of the conversation that is not cached yet. On the next turn they must know that
    they already acted. A flag is enough.
- **The originals for `expand`** are a copy of what the client already has. The proxy keeps them
  only because it cannot ask the client for its copy.

**Where the data sits matters.** With the local plugin install, the Store is in the proxy process
on the user's own machine. That machine already holds the full transcript. Storing content is
mainly a question for the hosted service, where the Store is on our side.

**A possible change, not checked:** a plugin inside the client could rewrite the local transcript
itself. The client would then keep the shortened version, and the proxy would not need to store
it. Nobody has checked if Claude Code's plugin or hook interface lets a plugin change earlier
messages.

## What turns off when

| Switch | Default | Turns off | Does not turn off |
|---|---|---|---|
| `store.enabled` (YAML) | on | The Store: originals, frozen decisions, saved model results, billed-token and cache-time records. The proxy stops offering the `expand` tool. | The observe-mode store, the body stash, keep-alive bodies and credentials, component caches in memory, the dashboard |
| `--store` / `STORE` | follows YAML | Overrides the operator's `store.enabled` | In hosted mode, each account's own `store.enabled` |
| `--dashboard` / `DASHBOARD` | off (the plugin turns it on) | The dashboard database and its in-memory memos. Keep-alive also stops keeping bodies. | The body stash, the Store |
| `--dashboard-content` / `DASHBOARD_CONTENT` | off | New writes of `request_content`, extraction-call text and `declaration_text` | Tool, MCP-server and skill names; `extraction_calls.rejection`; `request_components.err`; rows already written; keep-alive; the body stash; the memos |
| Account consent `capture_content` (hosted) | **on** for a new account | The same as `--dashboard-content`, for one account. Both must be on. | The same as the row above. Turning it off deletes nothing. |
| `--dashboard-retention` and size limits | 7 days, 512 MiB | Old dashboard rows, with their content | `declaration_text`, `tenant_spend`, `archived_sessions` |
| `--archive-remote` | off | Upload of old sessions to remote storage | — |
| `cache.keepalive` (YAML), plugin strategy `none` | off in Go, `5-min-ping` in the plugin | Keep-alive bodies, credentials and pings | Manager strategies and per-session overrides can turn it on again |
| `CONTEXT_GURU_KEEPALIVE=off` | unset | All keep-alive, everywhere | — |
| `INJECT_EXPAND` | `auto` | The `expand` tool offer | Storing the originals |
| `CONTEXT_GURU_DUMP`, `CONTEXT_GURU_CAPTURE` | unset | (opt-in) Writes before/after text or whole request bodies to a file, with no redaction. The proxy refuses them in hosted mode. | — |
| `CG_LOG_LEVEL=debug` | `info` | (opt-in) Logs the first 160 characters of each tool output | — |

The **body stash** keeps the last request body of each thread, for a cheap question to the model
(see [Features outside the pipeline](#features-outside-the-pipeline)). **No switch** turns it off.

## Pipeline components

One row for each registered component. The test `components/all/datadep_test.go` fails when:

- a registered component has no row;
- a row names a component that does not exist;
- a cell is empty, or the reason is shorter than 8 words or is only a pointer into the code.

| Component | Kind | Where | Off | Metadata only | Everything | Reason | Decided by |
|---|---|---|---|---|---|---|---|
| `format` | none | Nothing kept | Works fully | Works fully | Works fully | It removes whitespace from JSON tool output. The result depends only on that message, so there is nothing to remember. | Osher Elhadad, first commit a91d6f4 |
| `toon` | none | Nothing kept | Works fully | Works fully | Works fully | It rewrites a uniform JSON array as a compact table, and checks that the table converts back to the exact original. The result depends only on the message. | Osher Elhadad, release commit 7f0379a |
| `toolschema` | content | Memory: trimmed tool declarations, for the whole process, with no size limit | Works fully. **Today the cache stays on.** | Works fully without the cache. Only speed changes. | Works fully | The agent sends the same tool list every turn, so it keeps the trimmed list to save work. The trim always gives the same result, so the cache only adds speed. | Osher Elhadad, #84 |
| `searchfold` | none | Nothing kept | Works fully | Works fully | Works fully | It folds repeated file paths in search output into headings. It uses a fold only when the fold converts back to the exact original, so nothing needs saving. | Osher Elhadad, #89 |
| `cacheinject` | metadata | Store: one short hash per message of the last turn | Works with less. It puts cache bookmarks only at fixed positions, so an edited earlier message costs the full price. | Works fully | Works fully | It puts a cache bookmark just before the first message that changed. To find that message, it remembers a short hash of each message from the last turn, never the text. | Osher Elhadad, first commit a91d6f4; budget in #32 |
| `cachesplit` | none | Nothing kept. The dashboard keeps one hash per thread for savings reports. | Works fully | Works fully | Works fully | It moves the part of the system prompt that changes every session after the cache bookmark. That depends only on the current request. | #32 |
| `textclean` | none | Nothing kept | Works fully | Works fully | Works fully | It removes terminal colour codes and screen-redraw leftovers that were never visible text. The result depends only on the output itself. | Osher Elhadad, #89 |
| `toolfilter` | none | Nothing kept | Works fully | Works fully | Works fully | It removes the tool, MCP-server and skill declarations the operator listed. It decides from that fixed list and the current request. | Osher Elhadad, #84 |
| `mask` | content | Store: the frozen decision and the original | Masks a message for one turn, then **breaks the cache**. No way back. | Today: same as Off. **With a change:** works, and the cache stays stable. No way back. | Works fully, with `expand` | It hides old tool outputs, and must send the same masked text every later turn. It builds that text from the message alone, so a "masked" flag is enough to build it again. | Osher Elhadad, first commit a91d6f4; storage rules in #187 |
| `failed_run` | content | Store: the frozen decision and the original | Shortens once, then **breaks the cache**. No way back. | Today: same as Off. **With a change:** works, and the cache stays stable. No way back. | Works fully, with `expand` | It replaces a failed run that the agent ran again with a fixed sentence. A "replaced" flag is enough to send the same sentence again. | Osher Elhadad, first commit a91d6f4 |
| `collapse` | content | Store: the frozen decision and the original | Cuts once, then **breaks the cache**. No way back. | Today: same as Off. **With a change:** works if it also keeps the cut size, a number. No way back. | Works fully, with `expand` | It is the last-resort limit for very large outputs. It can build the cut again from the output and the cut size, so it needs a flag and one number. | Osher Elhadad, first commit a91d6f4 |
| `readlifecycle` | content | Store: the frozen decision and the original file read | Removes an old read once, then **breaks the cache**. No way back. | Today: same as Off. **With a change:** works if it also keeps its label ("stale" or "superseded"). No way back. | Works fully, with `expand` | It removes file reads that a later edit or read made out of date. The label can change later in the conversation, so it must keep the label it first used. | Osher Elhadad, #89 |
| `skeleton` | content | Store: the frozen decision and the original source | **Skips** | **Skips** | Works fully, with `expand` | It removes function bodies from source code. Without the original, the agent cannot tell a removed body from an empty one, so it refuses to run. | Osher Elhadad, first commit a91d6f4 |
| `cmdfilter` | content | Store: the filtered text (a speed-up) and the original | Works. Same bytes every turn, so the cache stays stable. No way back. | Same as Off | Works fully, with `expand` | It applies a filter for each known command, and the same output always gives the same text. It keeps the original only so the agent can get dropped lines back. | Osher Elhadad, first commit a91d6f4; memo in #188 |
| `linecap` | content | Store: the original | Works. Same bytes every turn. No way back. | Same as Off | Works fully, with `expand` | It shortens very long lines and folds repeated lines, the same way every time. It keeps the original only so the agent can get the cut lines back. | Osher Elhadad, #89 |
| `dedup` | content | Store: the original | Works. Same bytes every turn. The note still says "expand", with nothing to expand. | Same as Off | Works fully, with `expand` | It replaces a repeated tool output with a pointer to the earlier copy in the same request. It keeps the repeat only so the agent can get it back. | Osher Elhadad, first commit a91d6f4 |
| `smartcrush` | content | Store: the original | Works. Same bytes every turn. No way back. | Same as Off | Works fully, with `expand` | It keeps the first and last items of a long JSON array, and any error items, the same way every time. It keeps the full array only for `expand`. | Osher Elhadad, first commit a91d6f4 |
| `extract` | content | Store: the original | Works. Same bytes every turn. No way back. | Same as Off | Works fully, with `expand` | It removes clear noise, such as repeated lines and progress bars, the same way every time. It keeps the original only for `expand`. | Osher Elhadad, first commit a91d6f4 |
| `coref` | content | Store: the frozen decision, the original, and a count of cache rebuilds it spent (metadata) | **No spending limit**, because the count always reads zero. Each cut lasts one turn, then **breaks the cache**. | Today: **harmful.** The limit works, but each cut undoes itself next turn. **Turn it off.** **With a change:** works. | Works fully, with `expand` | It removes tool outputs that nothing later refers to, and so changes the cached history on purpose. It must remember which outputs it cut, and how many cache rebuilds it spent. | David Amid, commit b224704; docs/proposals/coref-compaction.md |
| `extract_llm` | content | Store: the model's rewrite, a copy for other sessions, and the original. Memory: counts per session. | Pays one model call per new large output, and uses it for one turn. Then it **breaks the cache**. Without prompt caching it pays again every turn, up to 8 calls. | Same as Off, and it approves **more** calls. It remembers an output, and counts on a reuse that cannot happen. **Cannot work.** | Works fully: one call, then free reuse | A model rewrites each large tool output, and the words differ each time. To send the same bytes next turn, it must keep the rewrite, which is content. | Osher Elhadad, #51; storage rules in #40 and #188 |
| `extract_llm_sweep` | content | Store: its removals with a short description of each output (no text), the originals, and "still needed" answers | It has no body stash, so it **pays full price** to send parts of up to 12 outputs to the model. Each removal lasts one turn, then **breaks the cache**. | Today: same as Off. **With a change:** it can repeat its removals from a flag. The cheap question still needs the body stash. | Works fully: a cheap question, then free reuse | It asks the agent's model which old outputs are no longer needed. The cheap question needs the exact text from last turn; repeating a removal needs only a flag. | David Amid, #118 |
| `summarize` | content | Store: the model-written summary and the replaced turns. Memory: a copy of the turns while the model call runs. | **Does not start** by default, because it cannot tell how full the window is. Without a session (`/compact`) it works in one request. No way back. | **Pays for a summary every turn above 90% full, and throws it away.** The next turn can wait up to 120 seconds for it. **Cannot work.** | Works fully | It replaces old turns with a model-written summary, and the words differ each time. It must keep the summary to send the same bytes next turn. | Osher Elhadad, commit 68e8b51; checkpoint in #430 |
| `cache_aware_summarizer` | content | Store: the summary and the replaced turns. Memory: copies of turns, for summaries during idle time and after a cold turn. | **Does not start** by default, because it cannot tell how full the window is. | It keeps copies of turns in memory, which is content. On Anthropic it does nothing and costs nothing. On OpenAI it **pays and throws the summary away** every turn. **Cannot work.** | Works fully | Like `summarize`, it must keep the summary to send the same bytes later. It also keeps the conversation in memory, to summarize while the user is idle. | Itay Nakash, #256 |
| `agentdiet` | content | Store: the frozen decision and the original step | **Pays one model call per turn.** The step it shortened last turn comes back in full, which **breaks the cache**. | Same as Off. **Cannot work.** | Works fully | A model rewrites one older agent step each turn, and the words differ each time. The history stays short only if the proxy keeps every rewrite. | Itay Nakash, #66 |

### Libraries that are not components

- **`dsl`** (the filter engine) keeps nothing per request. It holds only the rules from your filter
  files. `cmdfilter` and `linecap` use it, and those two keep the originals.

## Features outside the pipeline

| Feature | Kind | Off | Metadata only | Everything | Reason |
|---|---|---|---|---|---|
| `expand` and the originals | content | Not offered. Removals have no way back. | Not offered | Works | When the proxy replaces a large output with a short marker, it must keep the original. Otherwise it has nothing to give back. |
| Frozen decisions and saved model results | content | Gone | Gone. The rule-based components can use a flag instead. | Kept for about 2.8 hours | After the proxy shortens a cached message, it must send the same text every later turn. Otherwise the user pays to write the cache again. |
| Billed-token and cache-time records | metadata | Gone. `summarize` cannot tell how full the window is. A warm cache can look cold when one conversation comes under two ids. | **Kept.** This record makes `summarize` start, and so start to waste calls. | Kept | To know how full the window is, the proxy needs the size the provider billed last turn. It also needs the time the cache was last used. |
| **Keep-alive pings** | content and a credential | **Today: still on.** It keeps the last body and the credential. It should be off. | Off | Works | To keep the prompt cache warm, the proxy sends the exact same prompt again with the user's credential. Without the text, it cannot ping. |
| **Body stash** | content | **Today: still on**, 2 full bodies per thread. It should be off. | Off. `extract_llm_sweep` then pays full price, and `cache_aware_summarizer` stops. | Works | To ask the model a cheap question, the proxy adds the question to the exact text it sent last turn. The provider already cached that text. |
| Observe-mode store | content | **Today: still a real Store.** It should follow the level. | It should follow the level | Works | Observe mode runs the pipeline later on a copy, to estimate savings. The copy should store only what the real run would store. |
| Dashboard database | content and metadata | Off | Numbers, fixed codes, ids, and tool, MCP-server and skill names. **No** before/after text, summaries, declaration text, error text or rejection text. | Everything, when content capture is on | The dashboard shows what the proxy removed and what your setup carries. Every number on it needs only counts and prices. |
| Dashboard memos | content | Off, with the dashboard | A hash and a token count only. That is all they need. | Declaration and system-prompt text | They remember a tool list so they do not count its tokens again on every request. A hash and a count are enough. |
| Archive | content and metadata | Off | Metadata rows only | Everything | It moves old conversations off the local disk. The database stays small, and you can still open old history. |
| Insights | metadata | Off (it needs the dashboard) | Works | Works | A cost report needs only counts, tool names and prices, not the text. |
| Thread tracker | metadata | Works | Works | Works | It tells the main agent from its subagents. It uses hashes to check if a new request continues an earlier one. |
| Session tracker | metadata | Works | Works | Works | It needs a stable label for each conversation. When the client sends none, it hashes the start of the conversation. |
| Cache tracker | metadata | Works | Works | Works | It must know how much of the conversation the provider already cached, so components never edit that part. |
| Wait-cause and lifecycle signals | metadata | Off (it needs the dashboard) | Works | Works | To tell why the agent paused, it reads the request once. It keeps only counts and ids. |
| Metrics (`/stats`, `/metrics`) | metadata | Works | Works | Works | Totals and rates need only numbers. |
| Accounts (hosted) | account data | Works | Works | Works | This is the account system: who you are, your settings and your consent flag. It holds no transcripts. |

## What "metadata only" would need

1. **A separate "may store content" check.** Every component must ask it before a model call, and
   the proxy must ask it before it offers `expand`. Today one flag answers two questions: "can I
   keep the original?" and "can I keep the result?". Either answer makes some components pay for
   a model call and then throw the result away.
2. **An "already done" flag per message** for the rule-based components. All of them except
   `skeleton` can build the same text again from the message the client sends again. The flag
   keeps the cache stable. The code already has a path that builds lost text again; the flag
   would start it.
3. **Two small extra values:** the cut size for `collapse`, and the label for `readlifecycle`.
4. **A way for `coref` to send its cuts again.** Until then, turn `coref` off at this level.
5. **A way for `extract_llm_sweep` to repeat its removals** from the flag.
6. **Turn off the model-written components** (`extract_llm`, `summarize`, `cache_aware_summarizer`,
   `agentdiet`). The proxy cannot build their text again, so they either pay for nothing or break
   the cache.
7. **Turn off** keep-alive, the body stash and `expand`. The dashboard keeps only metadata columns,
   and its memos keep only hashes and counts.

## What this audit found

The audit read the code at commit 2a7a86f. This page records the findings. It does not change any
behaviour. Each finding needs a decision from its owner.

### Content kept after the user opted out

1. **The body stash** keeps 2 full request bodies per thread, on every successful request. No
   switch turns it off. It fills even when no component uses it.
2. **Keep-alive** keeps the last request body and the user's credential. It does this with storage
   off and with content capture off. The plugin turns keep-alive on by default.
3. **Nothing deletes `declaration_text`**: not retention, and not an account purge. Tool
   declarations and whole system prompts stay after the user turns consent off.
4. **Turning consent off deletes nothing.** Old rows in `request_content`, `extraction_calls` and
   the archive stay until retention or a purge removes them.
5. **The dashboard writes some columns without consent:** tool, MCP-server and skill names,
   `extraction_calls.rejection` (it can hold the provider's error text), and
   `request_components.err` (not scrubbed).
6. **Some in-memory copies ignore both switches:** the dashboard memos, the `toolschema` cache, the
   `cache_aware_summarizer` copies of turns, and the observe-mode store.
7. **In hosted mode, `--store` changes only the operator's config.** Each account follows only its
   own `store.enabled`.

### Problems with storage off today

- **No way back:** `mask`, `failed_run`, `collapse`, `readlifecycle`, `cmdfilter`, `linecap`,
  `dedup`, `smartcrush`, `extract`, `extract_llm`, `extract_llm_sweep`, `agentdiet`, `coref`, and
  `summarize` without a session. Only `skeleton` refuses.
- **Breaks the cache:** `mask`, `failed_run`, `collapse`, `readlifecycle`, `extract_llm`,
  `extract_llm_sweep`, `agentdiet` and `coref`. Each one shortens a message once, then sends it in
  full the next turn. So storage off can cost more than no compaction.
- **Pays for model calls that last one turn:** `extract_llm`, `extract_llm_sweep` (at full price)
  and `agentdiet`.
- **`coref` loses its spending limit.**
- **`summarize` and `cache_aware_summarizer` do not start.** This is safe only by accident. They
  start as soon as the proxy keeps the billed-token record, as at "metadata only".
- **Nothing warns at startup** when these components run with storage off.

### Code comments that disagree with the defaults

- The dashboard's `CaptureContent` comment says "on by default". It is off.
- The account `CaptureContent` comment says "Off by default". A new account starts with it **on**.
- The `LifecycleSignals` comment says "Off by default". The flag is on by default. It writes only
  counts.

## Open questions

1. **Does "Off" allow in-memory metadata?** This page says yes. Without the cache tracker,
   components can edit the cached part of the conversation.
2. **Are tool, MCP-server and skill names metadata?** They are ids, but the user chooses them.
3. **Are content hashes metadata?** They are short SHA-256 values with no secret salt. A person who
   can read the Store can check if the proxy saw a guessed text. A secret salt stops that.
4. **Is a session id from the client metadata?** It can contain a user or account id.

## Keep this page right

[`CLAUDE.md`](https://github.com/rossoctl/context-guru/blob/main/CLAUDE.md) holds the rule, under
"Hard boundaries". The PR template asks for it.

- Every PR that adds or changes a component or feature states its data dependence at each level.
  It also updates this page.
- A PR that changes a row's **Kind** or a level column, in either direction, says so at the top.
  The person in "Decided by" confirms it before merge.
- Write the reason in plain words: **why** the component needs the data. A file name or line
  number is not a reason, and the test rejects it.
