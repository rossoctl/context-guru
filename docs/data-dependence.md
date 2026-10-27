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
- **2 are special cases.** `skeleton` refuses to run without storage. `extract_llm_sweep` holds only
  metadata (ids, hashes, counts, verdicts), but it keeps copies of the conversation in memory and
  its question to the model needs last turn's text.
- **Context Guru keeps content after the user opts out**, in 7 places. The worst are 2 full
  request bodies per thread, and the last request body for keep-alive.
- **With storage off today, use preset `conservative` and cache strategy `none` or `1-hour-head`.**
  Every other plugin preset pays for savings that last one turn, so it does not work.
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
- **Store**: Context Guru's in-memory key-value store for each conversation. It is never written to
  disk. `store.enabled: false` replaces it with a Store that keeps nothing.
- **Original**: the text of a tool output before a component shortened it. Context Guru keeps it in
  the Store so the model can get it back with the `expand` tool.
- **Frozen decision**: the exact shortened text a component sent. Context Guru keeps it in the Store
  and sends the same text on every later turn.
- **Breaks the cache**: the next turn sends different bytes for a message the provider already
  cached. The user then pays to write the cache again, from that message to the end.
- **No way back**: the model cannot get removed text back through the `expand` tool. The agent
  still has its own copy.
- **Host**: the program that runs Context Guru. That is the proxy binary, or another program
  that uses Context Guru as a library. The host sends requests to the provider and holds
  credentials.
- **Kind** (in the tables): `none` (keeps nothing), `metadata`, or `content`. It is what the
  component keeps at the "Everything" level.
- **Decided by**: the person, and the issue or PR, that decided the current behaviour. If no later
  decision exists, it is the author of the PR that added the component. Ask that person before
  you change the row.

## The three storage levels

| Level | What Context Guru keeps | Exists today? |
|---|---|---|
| **Off** | Nothing in the Store. It keeps only the in-memory trackers that every request needs: how much of the conversation the provider cached, and when. These hold counts and timestamps. | **Partly.** `store.enabled: false` removes the Store. It does **not** remove the in-memory content in [Features outside the pipeline](#features-outside-the-pipeline). |
| **Metadata only** | Hashes, counts, ids, token numbers, timestamps and flags, in the Store and in memory. No content. | **No.** The "Metadata only" columns say what the code would do with a Store that refuses content. [What "metadata only" would need](#what-metadata-only-would-need) lists the changes. |
| **Everything** | Content and metadata. | Yes. This is the default. |

## Why Context Guru stores rewrites at all

The client never sees the rewritten request, so it cannot keep it.

1. The client (for example Claude Code) sends its **whole transcript** on every request.
2. Context Guru rewrites that copy and sends the result to the provider.
3. The provider returns **only the new assistant message**. The API cannot hand an edited history
   back.
4. The client **adds** that message to its own transcript. Its transcript still holds the
   original tool outputs.
5. On the next turn the client sends the originals again. Context Guru must make the same rewrite
   again.

The results:

- **A rewrite that depends only on the message needs no storage.** Context Guru runs it again every
  turn and gets the same bytes. Only CPU time is repeated. These components work this way:
  `format`, `toon`, `searchfold`, `textclean`, `toolfilter`, `cachesplit`, `cmdfilter`, `linecap`,
  `dedup`, `smartcrush` and `extract`.
- **Storage is necessary only when Context Guru cannot rebuild the rewrite from the message:**
  - **Model-written text** (`extract_llm`, `summarize`, `cache_aware_summarizer`, `agentdiet`). A
    model gives different words each time.
  - **Gated rewrites** (`mask`, `failed_run`, `collapse`, `readlifecycle`, `coref`). They act only
    on the part of the conversation that is not cached yet. On the next turn they must know that
    they already acted. A flag is enough.
- **The originals for `expand`** are a copy of what the client already has. Context Guru keeps them
  only because it cannot ask the client for its copy.

**Where the data sits matters.** Context Guru runs inside a host: the proxy binary, or another
program that uses it as a library. With the local plugin install, the Store is in the proxy process
on the user's own machine. That machine already holds the full transcript. Storing content is
mainly a question for the hosted service, where the Store is on our side.

**A possible change, not checked:** a plugin inside the client could rewrite the local transcript
itself. The client would then keep the shortened version, and Context Guru would not need to store
it. Nobody has checked if Claude Code's plugin or hook interface lets a plugin change earlier
messages.

## Will my setup work?

Find your preset and your cache strategy below. Each cell says what happens at that storage level.

- ✅ **Works**: the same as with everything stored.
- ✅ **Works, no `expand`**: the same savings. The model cannot ask for removed text back. The
  agent still has its own copy.
- 🟡 **Works, metadata**: it works, and it keeps **only metadata** to do it (hashes, counts,
  timestamps). It keeps no content.
- ❌ **Does not work**: it does nothing, or it pays for something that lasts only one turn. That
  is a model call, or a cache write when a shortened message comes back in full the next turn.
  The cell says which.

There is no "partly". A setup that pays for a saving that lasts one turn does not work.

"Metadata only" does not exist yet. That column says what would happen if it were added to the
code as it is today.

### Plugin presets

These are the presets in the Claude Code plugin picker.

| Preset | Off | Metadata only | Everything |
|---|---|---|---|
| `off` | ✅ Works | ✅ Works | ✅ Works |
| `conservative` | ✅ Works, no `expand` | ✅ Works, no `expand` | ✅ Works |
| `medium` | ❌ **Does not work.** `extract_llm` pays for a model call on each new large output, and the rewrite lasts one turn. The summary is never made. | ❌ The same as Off | ✅ Works |
| `high` | ❌ **Does not work.** The same as `medium`. | ❌ The same as `medium`. On OpenAI it also pays for a summary every turn and throws it away. | ✅ Works |
| `xhigh` | ❌ **Does not work.** The same as `high`. The sweep also pays full price to ask which outputs to remove, and the removals last one turn. | ❌ The same as Off | ✅ Works |

**With storage off, use `conservative`.** It is the only plugin preset that saves anything.

### Cache strategies

| Strategy | Off | Metadata only | Everything |
|---|---|---|---|
| `none` | ✅ Works | ✅ Works | ✅ Works |
| `5-min-ping` | ❌ **Cannot work without storage.** A ping sends your last request again, so Context Guru must keep it. **Today it runs anyway and keeps it.** That breaks the opt-out (see [What this audit found](#what-this-audit-found)). | ❌ Cannot work, for the same reason | ✅ Works |
| `1-hour-head` | ✅ Works. It sends no request of its own. It changes only a label on the request the client is sending, so the provider keeps that cache for 1 hour. It keeps nothing. | ✅ Works | ✅ Works |

**Why `5-min-ping` needs storage and `1-hour-head` does not:** a ping is a **new** request that
Context Guru sends by itself while the user is idle. To read the same cache, the ping must have the
exact bytes of the last prompt, so Context Guru keeps them. `1-hour-head` only changes the request
that the client sends anyway. The client supplies the bytes, so Context Guru keeps nothing.

**The credential:** Context Guru itself does not keep it. It keeps only a handle, and the host that
runs it (for example the proxy binary) sends the ping through its Dispatcher. The proxy binary's
Dispatcher keeps the credential in memory, masked, until the hold ends.

`medium` makes its summary in the idle time that `5-min-ping` keeps warm. Without `5-min-ping`,
`medium` almost never makes a summary, at any level.

### Other presets

These presets are for the standalone proxy and for benchmarks.

| Preset | Off | Metadata only | Everything |
|---|---|---|---|
| `cache`, `safe` | ✅ Works | ✅ Works | ✅ Works |
| `house`, `coding`, `mcp` | ✅ Works, no `expand` | ✅ Works, no `expand` | ✅ Works |
| `balanced`, `codesafe` | ❌ **Does not work.** `failed_run` (and `collapse` in `codesafe`) shortens a message once, then sends it in full the next turn. The user pays a cache write for a saving of one turn. | ❌ The same as Off | ✅ Works |
| `codesmart`, `aggressive`, `agent`, `general`, `housellm` | ❌ **Does not work.** `extract_llm` (and the sweep in `housellm`) pays for model calls that last one turn. Some rule-based rewrites also last one turn. | ❌ The same as Off | ✅ Works |
| `summarize` | ❌ **Does not work.** It does not start, because it cannot tell how full the window is. | ❌ **Does not work, and costs money.** It pays for a summary every turn and throws it away. | ✅ Works |
| `summarizer1509` | ❌ **Does not work, and costs money.** It starts at a fixed size (20,000 tokens), pays for a summary every turn and throws it away. | ❌ The same as Off | ✅ Works |
| `agentdiet` | ❌ **Does not work.** It pays for a model call every turn, and the rewrite lasts one turn. | ❌ The same as Off | ✅ Works |

## Pipeline components

One row for each registered component. The test `components/all/datadep_test.go` fails when:

- a registered component has no row;
- a row names a component that does not exist;
- a cell is empty, or the reason is shorter than 8 words or is only a pointer into the code.

| Component | Kind | Where | Off | Metadata only | Everything | Reason | Decided by |
|---|---|---|---|---|---|---|---|
| `format` | none | Nothing kept | ✅ Works | ✅ Works | ✅ Works | It removes whitespace from JSON tool output. The result depends only on that message, so there is nothing to remember. | Osher Elhadad, first commit a91d6f4 |
| `toon` | none | Nothing kept | ✅ Works | ✅ Works | ✅ Works | It rewrites a uniform JSON array as a compact table, and checks that the table converts back to the exact original. The result depends only on the message. | Osher Elhadad, release commit 7f0379a |
| `toolschema` | content | Memory: trimmed tool declarations, for the whole process, with no size limit | ✅ Works. **Today the cache of trimmed declarations stays on.** It should be off. | ✅ Works without the cache. Only speed changes. | ✅ Works | The agent sends the same tool list every turn, so it keeps the trimmed list to save work. The trim always gives the same result, so the cache only adds speed. | Osher Elhadad, #84 |
| `searchfold` | none | Nothing kept | ✅ Works | ✅ Works | ✅ Works | It folds repeated file paths in search output into headings. It uses a fold only when the fold converts back to the exact original, so nothing needs saving. | Osher Elhadad, #89 |
| `cacheinject` | metadata | Store: one short hash per message of the last turn | ✅ Works, with less. It puts cache bookmarks only at fixed positions, so an edited earlier message costs the full price. | 🟡 Works, metadata | 🟡 Works, metadata | It puts a cache bookmark just before the first message that changed. To find that message, it remembers a short hash of each message from the last turn, never the text. | Osher Elhadad, first commit a91d6f4; budget in #32 |
| `cachesplit` | none | Nothing kept. The dashboard keeps one hash per thread for savings reports. | ✅ Works | ✅ Works | ✅ Works | It moves the part of the system prompt that changes every session after the cache bookmark. That depends only on the current request. | #32 |
| `textclean` | none | Nothing kept | ✅ Works | ✅ Works | ✅ Works | It removes terminal colour codes and screen-redraw leftovers that were never visible text. The result depends only on the output itself. | Osher Elhadad, #89 |
| `toolfilter` | none | Nothing kept | ✅ Works | ✅ Works | ✅ Works | It removes the tool, MCP-server and skill declarations the operator listed. It decides from that fixed list and the current request. | Osher Elhadad, #84 |
| `mask` | content | Store: the frozen decision and the original | ❌ **Does not work.** Masks a message for one turn, then **breaks the cache**. No way back. | ❌ **Does not work today**: the same as Off. **With a change:** 🟡 it works on metadata, and the cache stays stable. No way back. | ✅ Works, with `expand` | It hides old tool outputs, and must send the same masked text every later turn. It builds that text from the message alone, so a "masked" flag is enough to build it again. | Osher Elhadad, first commit a91d6f4; storage rules in #187 |
| `failed_run` | content | Store: the frozen decision and the original | ❌ **Does not work.** Shortens once, then **breaks the cache**. No way back. | ❌ **Does not work today**: the same as Off. **With a change:** 🟡 it works on metadata, and the cache stays stable. No way back. | ✅ Works, with `expand` | It replaces a failed run that the agent ran again with a fixed sentence. A "replaced" flag is enough to send the same sentence again. | Osher Elhadad, first commit a91d6f4 |
| `collapse` | content | Store: the frozen decision and the original | ❌ **Does not work.** Cuts once, then **breaks the cache**. No way back. | ❌ **Does not work today**: the same as Off. **With a change:** 🟡 it works on metadata if it also keeps the cut size, a number. No way back. | ✅ Works, with `expand` | It is the last-resort limit for very large outputs. It can build the cut again from the output and the cut size, so it needs a flag and one number. | Osher Elhadad, first commit a91d6f4 |
| `readlifecycle` | content | Store: the frozen decision and the original file read | ❌ **Does not work.** Removes an old read once, then **breaks the cache**. No way back. | ❌ **Does not work today**: the same as Off. **With a change:** 🟡 it works on metadata if it also keeps its label ("stale" or "superseded"). No way back. | ✅ Works, with `expand` | It removes file reads that a later edit or read made out of date. The label can change later in the conversation, so it must keep the label it first used. | Osher Elhadad, #89 |
| `skeleton` | content | Store: the frozen decision and the original source | ❌ **Does not work.** It skips, so it saves nothing. | ❌ **Does not work.** It skips. | ✅ Works | It removes function bodies from source code. Without the original, the agent cannot tell a removed body from an empty one, so it refuses to run. | Osher Elhadad, first commit a91d6f4 |
| `cmdfilter` | content | Store: the filtered text (a speed-up) and the original | ✅ Works, no `expand`. Same bytes every turn. | ✅ Works, no `expand` | ✅ Works | It applies a filter for each known command, and the same output always gives the same text. It keeps the original only so the agent can get dropped lines back. | Osher Elhadad, first commit a91d6f4; memo in #188 |
| `linecap` | content | Store: the original | ✅ Works, no `expand`. Same bytes every turn. | ✅ Works, no `expand` | ✅ Works | It shortens very long lines and folds repeated lines, the same way every time. It keeps the original only so the agent can get the cut lines back. | Osher Elhadad, #89 |
| `dedup` | content | Store: the original | ✅ Works, no `expand`. Same bytes every turn. The note still says "expand", with nothing to expand. | ✅ Works, no `expand` | ✅ Works | It replaces a repeated tool output with a pointer to the earlier copy in the same request. It keeps the repeat only so the agent can get it back. | Osher Elhadad, first commit a91d6f4 |
| `smartcrush` | content | Store: the original | ✅ Works, no `expand`. Same bytes every turn. | ✅ Works, no `expand` | ✅ Works | It keeps the first and last items of a long JSON array, and any error items, the same way every time. It keeps the full array only for `expand`. | Osher Elhadad, first commit a91d6f4 |
| `extract` | content | Store: the original | ✅ Works, no `expand`. Same bytes every turn. | ✅ Works, no `expand` | ✅ Works | It removes clear noise, such as repeated lines and progress bars, the same way every time. It keeps the original only for `expand`. | Osher Elhadad, first commit a91d6f4 |
| `coref` | content | Store: the frozen decision, the original, and a count of cache rebuilds it spent (metadata) | ❌ **Does not work.** **No spending limit**, because the count always reads zero. Each cut lasts one turn, then **breaks the cache**. | ❌ **Does not work today, and is harmful.** The limit works, but each cut undoes itself next turn. **Turn it off.** **With a change:** 🟡 it works on metadata. | ✅ Works, with `expand` | It removes tool outputs that nothing later refers to, and so changes the cached history on purpose. It must remember which outputs it cut, and how many cache rebuilds it spent. | David Amid, commit b224704; docs/proposals/coref-compaction.md |
| `extract_llm` | content | Store: the model's rewrite, a copy for other sessions, and the original. Memory: counts per session, and the time of the last stash refusal. | ❌ **Does not work.** Pays one model call per new large output, and uses it for one turn. Then it **breaks the cache**. Without prompt caching it pays again every turn, up to 8 calls. | ❌ **Does not work**: the same as Off, and it approves **more** calls. It remembers an output, and counts on a reuse that cannot happen. **Cannot work.** | ✅ Works: one call, then free reuse | A model rewrites each large tool output, and the words differ each time. To send the same bytes next turn, it must keep the rewrite, which is content. | Osher Elhadad, #51; storage rules in #40 and #188 |
| `extract_llm_sweep` | content | Store: one **held answer** per thread (pinned, `cg:swh:`): for each judged output its tool-use id, a short hash of the message, its token counts, the model's keep-or-drop verdict, the length of the conversation when asked, the model name, the cost, and the wait cause. No text. Memory, not the Store: **two full copies of the conversation per thread** (the request as the pipeline holds it, and as the agent sent it), kept only for a thread the keep-alive can ping, for at most 30 minutes, for at most 256 threads. It writes **no** removals (`cg:res:`) any more. **Since the cold apply (#404 PR C), it also keeps, per applied drop, the original output** (the stash behind the `<<cg:HASH>>` marker, which is content, the same as every other offloader), and **metadata only** besides: a drop record per thread and output (`cg:swd:`: tool-use id, content key, marker id, token count, time, the kind, and **4-byte hashes of up to 48 identifiers** the output introduced, used to see a later reference), a marker-to-drop map (`cg:swx:`), and a process-wide counter ledger for the false-drop rate. No output text is in any record. **With `warm: true` (#491, off by default, in no preset)** it also keeps, **in memory only and never in the Store**: one waiting answer per thread (ids, hashes, counts, verdicts: metadata), for at most 30 minutes, and per thread a few numbers (the warm run length, the previous request's local token size, the request count since the last question, a last-touched time, and a flag with the time a question is pending, 5 minutes at most), for at most 256 threads. The warm question uses the ping registry's existing conversation copy (already counted above) and keeps **no copy of its own**, so it asks only on a thread the keep-alive can ping. An applied warm drop writes the same records and stash as a cold drop. **Since #721** the candidate scan and the cold apply also check whether a `<<cg:HASH>>` in an output has a Store key (presence only, no content read), so an output that merely quotes marker text is still a candidate. | ❌ **Does nothing.** It asks only at a keep-alive ping, and a ping needs the body stash. With no Store, the held answer is not kept. It costs nothing and removes nothing. | ✅ **Works for what it does today**: the held answer is metadata, so it is kept and rechecked. **But** the in-memory conversation copies are content, so this level is not metadata only in full (see the memory column). | ✅ Works: the question at the ping, the held answer, the recheck, shadow mode and the cold apply. The removals need the stash for `expand`. | It asks the agent's model which old outputs are spent, at the keep-alive ping, and keeps the answer until a cold request. The answer is ids, hashes, counts and verdicts, so it is metadata. The question needs the exact text from last turn, and the recheck needs the conversation the answer was given for, so it keeps copies of the conversation in memory. | David Amid, #118; ping-time question and held answer approved 2026-10-25 (#508); cold apply approved 2026-10-25 (#695); warm path approved 2026-10-26 (#491) |
| `summarize` | content | Store: the model-written summary and the replaced turns. Memory: a copy of the turns while the model call runs. | ❌ **Does not work.** **Does not start** by default, because it cannot tell how full the window is. Without a session (`/compact`) it works in one request. No way back. | ❌ **Does not work.** **Pays for a summary every turn above 90% full, and throws it away.** The next turn can wait up to 120 seconds for it. **Cannot work.** | ✅ Works | It replaces old turns with a model-written summary, and the words differ each time. It must keep the summary to send the same bytes next turn. | Osher Elhadad, commit 68e8b51; checkpoint in #430 |
| `cache_aware_summarizer` | content | Store: the summary and the replaced turns. Memory: copies of turns, for summaries during idle time, after a cold turn, and for one request's lifetime while a summary waits for that request's body to be stashed (#722). | ❌ **Does not work.** **Does not start** by default, because it cannot tell how full the window is. | ❌ **Does not work.** It keeps copies of turns in memory, which is content. On Anthropic it does nothing and costs nothing. On OpenAI it **pays and throws the summary away** every turn. **Cannot work.** | ✅ Works | Like `summarize`, it must keep the summary to send the same bytes later. It also keeps the conversation in memory, to summarize while the user is idle. | Itay Nakash, #256 |
| `agentdiet` | content | Store: the frozen decision and the original step | ❌ **Does not work.** **Pays one model call per turn.** The step it shortened last turn comes back in full, which **breaks the cache**. | ❌ **Does not work**: the same as Off. **Cannot work.** | ✅ Works | A model rewrites one older agent step each turn, and the words differ each time. The history stays short only if Context Guru keeps every rewrite. | Itay Nakash, #66 |

### Libraries that are not components

- **`dsl`** (the filter engine) keeps nothing per request. It holds only the rules from your filter
  files. `cmdfilter` and `linecap` use it, and those two keep the originals.

## Features outside the pipeline

| Feature | Kind | Off | Metadata only | Everything | Reason |
|---|---|---|---|---|---|
| `expand` and the originals | content | ❌ Not offered. Removals have no way back. | ❌ Not offered | ✅ Works | When Context Guru replaces a large output with a short marker, it must keep the original. Otherwise it has nothing to give back. This now includes the agent's own summary request (Claude Code auto-compact and `/compact`). Context Guru used to forward that request untouched, so it kept nothing from it. It now shortens the history like any other turn, so the summary can carry a marker, and a marker needs the stored original to be expanded later. A summary request over 32 MiB is still forwarded untouched and keeps nothing. |
| Frozen decisions and saved model results | content | ❌ Gone | ❌ Gone. The rule-based components can use a flag instead. | ✅ Works. Kept for about 2.8 hours. | After Context Guru shortens a cached message, it must send the same text every later turn. Otherwise the user pays to write the cache again. |
| Billed-token and cache-time records | metadata | ❌ Gone. `summarize` cannot tell how full the window is. A warm cache can look cold when one conversation comes under two ids. | 🟡 Works, metadata. This record also makes `summarize` start, and so start to waste calls. | 🟡 Works, metadata | To know how full the window is, Context Guru needs the size the provider billed last turn. It also needs the time the cache was last used. The billed size is kept per session and per thread (one integer each). The per-thread one lets `coref` measure how fast one thread grows. |
| **Keep-alive pings** | content. The host keeps the credential. | ❌ **Should be off. Today it is still on**, and keeps the last body. The proxy binary also keeps the credential. | ❌ Off | ✅ Works | To keep the prompt cache warm, the proxy sends the exact same prompt again with the user's credential. Without the text, it cannot ping. The Ping button (`POST /api/me/cache/ping`) uses the same body and key. With keep-alive off, a request that carries the header `x-context-guru-ping: 1` is still held, for the button only: no ping is sent for it, and the body and key are released 300 seconds after the last request on a 5-minute cache, or 59 minutes after it on a 1-hour cache (a press does not extend it), and one account holds at most 16 such threads. Without the header nothing is held. The install adds the header and tells the user. An older install (from before `--no-ping-header`) gets it at its next session start, with the same notice. Any other install without the header is never changed. It gets a one-time note instead. |
| **Body stash** | content | ❌ **Should be off. Today it is still on**, with 2 full bodies per thread. | ❌ Off. The sweep's question then cannot be asked, and `cache_aware_summarizer` stops. | ✅ Works | To ask the model a cheap question, Context Guru adds the question to the exact text it sent last turn. The provider already cached that text. The sweep keeps **2 more full copies of the conversation per thread**, outside the stash, for a thread the keep-alive can ping (see `extract_llm_sweep` above). |
| Observe-mode store | content | ❌ **Should follow the level. Today it is still a real Store.** | It should follow the level | ✅ Works | Observe mode runs the pipeline later on a copy, to estimate savings. The copy should store only what the real run would store. |
| Dashboard database | content and metadata | ❌ Off | 🟡 Works, metadata: numbers, fixed codes, ids, and tool, MCP-server and skill names. **No** before/after text, summaries, declaration text, error text or rejection text. | ✅ Works. It keeps text only when content capture is on. | The dashboard shows what Context Guru removed and what your setup carries. Every number on it needs only counts and prices. |
| Dashboard memos | content | ❌ Off, with the dashboard | 🟡 Works, metadata: a hash and a token count. That is all they need. | ✅ Works. They keep declaration and system-prompt text. | They remember a tool list so they do not count its tokens again on every request. A hash and a count are enough. |
| Archive | content and metadata | ❌ Off | 🟡 Works, metadata rows only | ✅ Works | It moves old conversations off the local disk. The database stays small, and you can still open old history. |
| Insights | metadata | ❌ Off (it needs the dashboard) | 🟡 Works, metadata | 🟡 Works, metadata | A cost report needs only counts, tool names and prices, not the text. |
| Thread tracker | metadata | 🟡 Works, metadata | 🟡 Works, metadata | 🟡 Works, metadata | It tells the main agent from its subagents. It uses hashes to check if a new request continues an earlier one. |
| Session tracker | metadata | 🟡 Works, metadata | 🟡 Works, metadata | 🟡 Works, metadata | It needs a stable label for each conversation. When the client sends none, it hashes the start of the conversation. |
| Cache tracker | metadata | 🟡 Works, metadata | 🟡 Works, metadata | 🟡 Works, metadata | It must know how much of the conversation the provider already cached, so components never edit that part. |
| Wait-cause and lifecycle signals | metadata | ❌ Off (it needs the dashboard) | 🟡 Works, metadata | 🟡 Works, metadata | To tell why the agent paused, it reads the request once. It keeps only counts and ids. |
| Costly settings check (`/context-guru:save-more settings`, `GET /api/adapt/settings`) | metadata | ❌ Off (the cost needs the dashboard). The plugin still finds the settings and says "cost not measured yet". | 🟡 Works, metadata | 🟡 Works, metadata | It counts and prices dashboard rows by request shape: model name, tool count, `max_tokens` and the stream flag. One check also uses the upstream status (400) of a Haiku row in the same session. It reads no request text and keeps nothing new. The plugin reads only the named keys of the user's own settings files (the betas setting, the recap setting, `ANTHROPIC_SMALL_FAST_MODEL` and `ANTHROPIC_DEFAULT_HAIKU_MODEL`) on the user's machine, and sends none of them to the proxy (#615). |
| Metrics (`/stats`, `/metrics`) | metadata | 🟡 Works, metadata | 🟡 Works, metadata | 🟡 Works, metadata | Totals and rates need only numbers. |
| Accounts (hosted) | account data | ✅ Works | ✅ Works | ✅ Works | This is the account system: who you are, your settings and your consent flag. It holds no transcripts. |

## What "metadata only" would need

1. **A separate "may store content" check.** Every component must ask it before a model call, and
   Context Guru must ask it before it offers `expand`. Today one flag answers two questions: "can I
   keep the original?" and "can I keep the result?". Either answer makes some components pay for
   a model call and then throw the result away.
2. **An "already done" flag per message** for the rule-based components. All of them except
   `skeleton` can build the same text again from the message the client sends again. The flag
   keeps the cache stable. The code already has a path that builds lost text again; the flag
   would start it.
3. **Two small extra values:** the cut size for `collapse`, and the label for `readlifecycle`.
4. **A way for `coref` to send its cuts again.** Until then, turn `coref` off at this level.
5. **A byte bound, and a switch, for the sweep's in-memory conversation copies** (#404 PR C). The removals it used to repeat from a flag are gone: the sweep no longer writes them.
6. **Turn off the model-written components** (`extract_llm`, `summarize`, `cache_aware_summarizer`,
   `agentdiet`). Context Guru cannot build their text again, so they either pay for nothing or break
   the cache.
7. **Turn off** keep-alive, the body stash and `expand`. The dashboard keeps only metadata columns,
   and its memos keep only hashes and counts.

## What this audit found

The audit read the code at commit 2a7a86f. This page records the findings. It does not change any
behaviour. Each finding needs a decision from its owner.

### Content kept after the user opted out

1. **The body stash** keeps 2 full request bodies per thread, on every successful request. No
   switch turns it off. It fills even when no component uses it.
2. **Keep-alive** keeps the last request body, with storage off and with content capture off.
   The proxy binary's Dispatcher also keeps the user's credential, masked, for the same time. The
   plugin turns keep-alive on by default. See #595.
3. **Nothing deletes `declaration_text`**: not retention, and not an account purge. Tool
   declarations and whole system prompts stay after the user turns consent off.
4. **Turning consent off deletes nothing.** Old rows in `request_content`, `extraction_calls` and
   the archive stay until retention or a purge removes them.
5. **The dashboard writes some columns without consent:** tool, MCP-server and skill names,
   `extraction_calls.rejection` (it can hold the provider's error text), and
   `request_components.err` (not scrubbed).
6. **Some in-memory copies ignore both switches:** the dashboard memos, the `toolschema` cache, the
   `cache_aware_summarizer` copies of turns, the `extract_llm_sweep` conversation copies (2 per
   thread, only where the keep-alive can ping), and the observe-mode store.
7. **In hosted mode, `--store` changes only the operator's config.** Each account follows only its
   own `store.enabled`.

### Problems with storage off today

- **7 components do not work.** They pay for a saving that lasts one turn: `mask`, `failed_run`,
  `collapse`, `readlifecycle`, `coref`, `extract_llm` and `agentdiet`.
  - Each one shortens a message once, then sends it in full the next turn. The user pays a cache
    write each time, so storage off can cost more than no compaction.
  - `extract_llm` and `agentdiet` also pay for a model call each time.
  - `coref` also loses its spending limit.
- **The summarizers do not work.**
  - `summarize` and `cache_aware_summarizer` do not start with their default settings. This is
    safe only by accident. They start as soon as Context Guru keeps the billed-token record, as at
    "metadata only".
  - `summarizer1509` starts at a fixed size, pays for a summary every turn and throws it away.
- **`skeleton` refuses to run.** This is the safe behaviour.
- **No way back** for the rest: `cmdfilter`, `linecap`, `dedup`, `smartcrush` and `extract` still
  work, but the model cannot get removed text back through `expand`.
- **Nothing warns at startup** when a component that does not work runs with storage off.

### Code comments that disagreed with the defaults

This PR fixes 3 code comments that said the wrong default:

- The dashboard's `CaptureContent` comment said "on by default". It is off.
- The account `CaptureContent` comment said "Off by default". A new account starts with it **on**.
- The `LifecycleSignals` comment said "Off by default". The flag is on by default. It writes only
  counts.

## Open questions

1. **Does "Off" allow in-memory metadata?** This page says yes. Without the cache tracker,
   components can edit the cached part of the conversation.
2. **Are tool, MCP-server and skill names metadata?** They are ids, but the user chooses them.
3. **Are content hashes metadata?** They are short SHA-256 values with no secret salt. A person who
   can read the Store can check if Context Guru saw a guessed text. A secret salt stops that.
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

## Technical notes: what each switch turns off

| Switch | Default | Turns off | Does not turn off |
|---|---|---|---|
| `store.enabled` (YAML) | on | The Store: originals, frozen decisions, saved model results, billed-token and cache-time records. Context Guru stops offering the `expand` tool. | The observe-mode store, the body stash, keep-alive bodies and credentials, component caches in memory, the dashboard |
| `--store` / `STORE` | follows YAML | Overrides the operator's `store.enabled` | In hosted mode, each account's own `store.enabled` |
| `--dashboard` / `DASHBOARD` | off (the plugin turns it on) | The dashboard database and its in-memory memos. Keep-alive also stops keeping bodies. | The body stash, the Store |
| `--dashboard-content` / `DASHBOARD_CONTENT` | off | New writes of `request_content`, extraction-call text and `declaration_text` | Tool, MCP-server and skill names; `extraction_calls.rejection`; `request_components.err`; rows already written; keep-alive; the body stash; the memos |
| Account consent `capture_content` (hosted) | **on** for a new account | The same as `--dashboard-content`, for one account. Both must be on. | The same as the row above. Turning it off deletes nothing. |
| `--dashboard-retention` and size limits | 7 days, 512 MiB | Old dashboard rows, with their content | `declaration_text`, `tenant_spend`, `archived_sessions` |
| `--archive-remote` | off | Upload of old sessions to remote storage | — |
| `cache.keepalive` (YAML), plugin strategy `none` | off in Go, `5-min-ping` in the plugin | Keep-alive bodies, the held credential, and pings | Manager strategies and per-session overrides can turn it on again |
| `CONTEXT_GURU_KEEPALIVE=off` | unset | All keep-alive, everywhere | — |
| `INJECT_EXPAND` | `auto` | The `expand` tool offer | Storing the originals |
| `CONTEXT_GURU_DUMP`, `CONTEXT_GURU_CAPTURE` | unset | (opt-in) Writes before/after text or whole request bodies to a file, with no redaction. Context Guru refuses them in hosted mode. | — |
| `CG_LOG_LEVEL=debug` | `info` | (opt-in) Logs the first 160 characters of each tool output | — |

The **body stash** keeps the last request body of each thread, for a cheap question to the model
(see [Features outside the pipeline](#features-outside-the-pipeline)). **No switch** turns it off.
