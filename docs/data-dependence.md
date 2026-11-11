# Data dependence

This page says, for each component and feature, whether it needs **stored customer data**. It also
says what each one does at three storage levels: off, metadata only, and everything.

## Summary

- **The rule:** a component or feature works from **metadata** only. A feature that needs
  **content** is a rare, deliberate exception. Many users turn storage off, and a feature that
  quietly needs content then works worse, with nothing reported.
- **13 of 24 components need no stored content to do their job:**
  - 5 keep nothing.
  - 2 keep only hashes or names: `cacheinject` keeps hashes, and `toolfilter` keeps tool names when
    tool deferral is on (the `ultrahigh` preset).
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

| Strategy | Off | Metadata only | Everything | Decided by |
|---|---|---|---|---|
| `none` | ✅ Works | ✅ Works | ✅ Works | David Amid, #569 |
| `auto` (was `5-min-ping`; `5-min-ping` is now an alias) | ❌ **Cannot work without storage.** A ping sends your last request again, so Context Guru must keep it. **Today it runs anyway and keeps it.** That breaks the opt-out (see [What this audit found](#what-this-audit-found)). | ❌ Cannot work, for the same reason | ✅ Works | David Amid, #569 |
| `1-ping`, `2-ping`, `30-min-ping` (the same ping mechanism as `auto`; they differ only in how many pings and when) | ❌ The same as `auto` | ❌ The same as `auto` | ✅ Works | David Amid, #569 |
| `1-hour-head` | ✅ Works. It sends no request of its own. It changes only a label on the request the client is sending, so the provider keeps that cache for 1 hour. It keeps nothing. | ✅ Works | ✅ Works | David Amid, #569 |

The named strategies are one table in the proxy (`config/strategy.go`, #980). That change only gives
the existing strategies one home and a `cache.strategy` key. It does not change what any strategy
keeps.

**Why `auto` (and the other ping strategies) needs storage and `1-hour-head` does not:** a ping is a **new** request that
Context Guru sends by itself while the user is idle. To read the same cache, the ping must have the
exact bytes of the last prompt, so Context Guru keeps them. `1-hour-head` only changes the request
that the client sends anyway. The client supplies the bytes, so Context Guru keeps nothing.

**The credential:** Context Guru itself does not keep it. It keeps only a handle, and the host that
runs it (for example the proxy binary) sends the ping through its Dispatcher. The proxy binary's
Dispatcher keeps the credential in memory, masked, until the hold ends.

`medium` makes its summary in the idle time that `auto` keeps warm. Without a ping strategy,
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
| `toolfilter` | metadata | Nothing kept for `remove`. With `defer: auto` (only the `ultrahigh` preset): one record per thread with the names of the deferred tools (`cg:tdf:`; on GPT, GLM and Kimi, where our own search tool runs, a second record under the same prefix also lists the tools a search added back and, for each search the client received, its call id and the tool names it asked for, #825; on a Claude Code thread with its own `ToolSearch`, the thread record also keeps each search call id and the names the proxy added to its result, #898), and a session's tool search opt-out flag (`cg:tso:`), both pinned. They live in the session-state table when it is on (`--dashboard`), else in tool deferral's own file, `context-guru-tooldefer.db`, next to the dashboard database path. Plus the tool history (see *Tool deferral history* below). No tool text kept. In Claude Code mode (#898) the feature also reads the text of a `ToolSearch` result, in two places: the "No matching deferred tools found" text, and the error text "not loaded" or "does not exist". It reads them in the request and keeps nothing of them. | ✅ Works for `remove`. `defer: auto` defers nothing when there is no durable place for the record: no session-state table and the file cannot be opened (a read-only folder, `--dashboard-db=:memory:`). A record that dies with the process would move tools inside live cached prefixes at the next start. With the Store off nothing is deferred. | 🟡 Works, metadata. The record, the flag and the history are tool names, ids, flags and counts. | 🟡 Works, metadata. The same as metadata only: deferral keeps no content. | It removes the declarations the operator listed, from that fixed list and the current request. Deferral must send the same set on every turn of a thread, also across a restart, so it keeps the tool names it deferred on disk, and it decides from which tools the user declared and used before. | Osher Elhadad, #84; deferral: David Amid, #485, 2026-11-02 |
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
| `extract_llm_sweep` | content | Store: one **held answer** per thread (pinned, `cg:swh:`): for each judged output its tool-use id, a short hash of the message, its token counts, the model's keep-or-drop verdict, the length of the conversation when asked, the model name, the cost, and the wait cause. No text. Memory, not the Store: **two full copies of the conversation per thread** (the request as the pipeline holds it, and as the agent sent it), kept only for a thread the keep-alive can ping, for at most 30 minutes, for at most 256 threads. It writes **no** removals (`cg:res:`) any more. **Since the cold apply (#404 PR C), it also keeps, per applied drop, the original output** (the stash behind the `<<cg:HASH>>` marker, which is content, the same as every other offloader), and **metadata only** besides: a drop record per thread and output (`cg:swd:`: tool-use id, content key, marker id, token count, time, the kind, and **4-byte hashes of up to 48 identifiers** the output introduced, used to see a later reference), a marker-to-drop map (`cg:swx:`), and a process-wide counter ledger for the false-drop rate. No output text is in any record. **With `warm: true` (#491, off by default, in no preset)** it also keeps, **in memory only and never in the Store**: one waiting answer per thread (ids, hashes, counts, verdicts: metadata), for at most 30 minutes, **which also holds the drop verdicts a refused answer left behind (the same fields, at most 256 per thread, none older than 30 minutes since the question that judged them)**, and per thread a few numbers (the warm run length, the previous request's local token size, the request count since the last question, a last-touched time, and a flag with the time a question is pending, 5 minutes at most), for at most 256 threads. The warm question uses the ping registry's existing conversation copy (already counted above) and keeps **no copy of its own**, so it asks only on a thread the keep-alive can ping. An applied warm drop writes the same records and stash as a cold drop. **With sweep v3 (`selector: rule` or `hybrid`, or any v3 setting; off by default, needs `warm: true`)** the rules read tool names, tool input paths and commands from the request in flight, to see a file read that was read or written again, and a build, test or lint run that ran again. They write nothing new: no path or command is stored. Per thread they keep, in memory only, two numbers and two flags (the billed size after the last accepted cut, and whether a cut or a question is waiting). The counters hold counts and ratios only. **Rule (c), `rule_unreferenced` (off by default)** reads the text of later messages in the request in flight, to see whether any of them reuses an identifier the output introduced. This is the same reading the recheck already does for `referenced_later`, and it also stores nothing. The drop record gains the **name of the rule** (or `judge`) that chose the drop, a label with no content, so an expand can be counted per rule. **With sweep v3 on, the drop record also gains the tool name and a hash of the file path or command of the call** that produced the output (an 8-byte hash, with a prefix that keeps a path and a command apart). **The path or command itself is not stored.** On each later request the sweep hashes the path or command of every later tool call and compares it with those hashes, to count a re-read of what a drop removed. The calls it hashes are read in flight, the same request text the rules already read. **Since #721** the candidate scan and the cold apply also check whether a `<<cg:HASH>>` in an output has a Store key (presence only, no content read), so an output that merely quotes marker text is still a candidate. | ❌ **Does nothing.** It asks only at a keep-alive ping, and a ping needs the body stash. With no Store, the held answer is not kept. It costs nothing and removes nothing. | ✅ **Works for what it does today**: the held answer is metadata, so it is kept and rechecked. **But** the in-memory conversation copies are content, so this level is not metadata only in full (see the memory column). | ✅ Works: the question at the ping, the held answer, the recheck, shadow mode and the cold apply. The removals need the stash for `expand`. | It asks the agent's model which old outputs are spent, at the keep-alive ping, and keeps the answer until a cold request. The answer is ids, hashes, counts and verdicts, so it is metadata. The question needs the exact text from last turn, and the recheck needs the conversation the answer was given for, so it keeps copies of the conversation in memory. | David Amid, #118; ping-time question and held answer approved 2026-10-25 (#508); cold apply approved 2026-10-25 (#695); warm path approved 2026-10-26 (#491); sweep v3 rules reading tool names, paths and commands in flight approved 2026-10-28 (#491); the stored path hash (a hash of the file path or command in the drop record, never the path) approved 2026-11-01 (#491) |
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
| Frozen decisions and saved model results | content | ❌ Gone | ❌ Gone. The rule-based components can use a flag instead. | ✅ Works. Kept for about 2.8 hours in memory. **With the dashboard on (`--session-state`, on by default): kept 24 hours in memory and also written to the dashboard database, then loaded back when the session returns.** | After Context Guru shortens a cached message, it must send the same text every later turn. Otherwise the user pays to write the cache again. |
| Billed-token and cache-time records | metadata | ❌ Gone. `summarize` cannot tell how full the window is. A warm cache can look cold when one conversation comes under two ids. | 🟡 Works, metadata. This record also makes `summarize` start, and so start to waste calls. | 🟡 Works, metadata | To know how full the window is, Context Guru needs the size the provider billed last turn. It also needs the time the cache was last used. The billed size is kept per session and per thread (one integer each). The per-thread one lets `coref` measure how fast one thread grows. Bob's model route also reports its cache writes, and the 1-hour part of them, in its OpenAI-style usage block. Only that route reads these counts. They are metadata, so this works the same at every storage level. |
| **Session state table** (`--session-state`, #769; on with `--dashboard`) | content and metadata | ❌ Off. Dashboard off, `--session-state=false`, or store off: nothing is written. | 🟡 Works, metadata: only the keys of counts, hashes, ids and verdicts are written (`cg:bin:`, `cg:bint:`, `cg:len:`, `cg:ttl:`, `cg:seen:`, `cg:swd:`, `cg:swk:`, `cg:swx:`, `cg:rst:`, `cg:use:`, `cg:tdf:`). Set `--session-state-metadata-only`. The table is in the dashboard database. | ✅ Works. **Also writes the content keys to the dashboard database**: the summary text (`cg:sum:`), `extract_llm` results (`cg:res:`, `cg:xres:`), freeze decisions (`cg:frz:`) and the sweep's held answer (`cg:swh:`). The rewind payloads (the stash) are never written. | A restart, or a pause of more than 2.8 hours, deletes the summary checkpoint and the last billed input from memory. The next turn then sends the full transcript, which can be over the window, and the summary cannot start. A table lets the proxy give a returning session the same state it had. The table is **not encrypted**. It has its own retention, not the dashboard's. A row lives 30 days after its last write. An eviction or an archive can end it earlier, when no other session uses its key. Decided by: David Amid, 2026-10-29 (the dashboard database, on with the dashboard). David Amid confirmed the data-dependence change for `extract_llm_sweep`. Rows to confirm: `cache_aware_summarizer`, `summarize` (Itay Nakash). |
| **Tool deferral history** (`ultrahigh`, #485) | metadata | ❌ Off with no durable place: no dashboard database and tool deferral's own file cannot be opened. Then it lives in memory only, a restart starts it from zero, and nothing is deferred until 3 new sessions. With the Store off nothing is deferred at all. | 🟡 Works, metadata | 🟡 Works, metadata | To defer only the tools a user does not use, it keeps, per user, session and tool: whether the session declared the tool, whether it used or loaded it, the project folder and the time. The rule groups MCP tools by server (`mcp__<server>__*`) when it reads this; the table still holds tool names only. It keeps the newest 64 sessions per user, plus the newest 10 sessions of each project, up to 256 sessions per user in all. The extra sessions are there because the rule checks whether this project used a tool in its last 10 sessions; without them, a user in many projects would push a project's older sessions out, and the check would not see them. It deletes sessions idle longer than the session-state retention (30 days by default). The table is `tool_defer_history`, in the dashboard database when there is one, else in tool deferral's own file. A user's first deferral request copies that user's own sessions, and no other user's, from the dashboard's inventory tables. It also keeps the provider check result per endpoint, model and route (`tool_defer_probe`); that row describes an endpoint, not a user. An account purge deletes the user's history rows, thread records and opt-out flags. Tool names, ids, paths, flags and times only. Nothing is shared between users: the history, the records and the opt-outs are keyed by user, and the pending opt-outs are capped per user. The check spends the user's own credential, under $0.01 per check, once a day per model (a failed check is retried after 15 minutes). Decided by: David Amid, #485, 2026-11-02. |
| **Client compaction setting** (Claude Code #948; Bob and Codex #963) | metadata | ✅ Works. Nothing is kept: the proxy holds the setting in memory. | 🟡 Works, metadata | 🟡 Works, metadata | The user can move the point where Claude Code compacts its own transcript. The savings figures and the summarize trigger must use that point, or they are wrong for that user. The local proxy reads a few short setting values once, at start-up, from its env and from `settings.json` and `.claude.json` (Claude Code), `~/.bob/settings.json` (Bob) and `config.toml` (Codex), and holds them in memory. For Bob it also uses the client's own default threshold. For Bob, the local proxy also reads the model list that passes through it (`/inference/v1/model/info`). It keeps one window number for each model id, in memory only, at most 512 entries, replaced on each fetch, and never written anywhere. It writes the resulting C (a number) and its source (`configured` or `client_default`) on each request row (`compaction_point`, `compaction_point_source`). No transcript text, no paths, no other settings. A hosted proxy reads and captures nothing. Decided by: David Amid, #948, #963. |
| **Keep-alive pings** | content. The host keeps the credential. | ❌ **Should be off. Today it is still on**, and keeps the last body. The proxy binary also keeps the credential. | ❌ Off | ✅ Works | To keep the prompt cache warm, the proxy sends the exact same prompt again with the user's credential. Without the text, it cannot ping. The Ping button (`POST /api/me/cache/ping`) uses the same body and key. With keep-alive off, a request that carries the header `x-context-guru-ping: 1` is still held, for the button only: no ping is sent for it, and the body and key are released 300 seconds after the last request on a 5-minute cache, or 59 minutes after it on a 1-hour cache (a press does not extend it), and one account holds at most 16 such threads. Without the header nothing is held. The install adds the header and tells the user. An older install (from before `--no-ping-header`) gets it at its next session start, with the same notice. Any other install without the header is never changed. It gets a one-time note instead. The cache-state answer also carries the time of the next planned ping (a number, metadata), so the mod can hide the button while a ping fires before the cache expires. |
| **Body stash** | content | ❌ **Should be off. Today it is still on**, with 2 full bodies per thread. | ❌ Off. The sweep's question then cannot be asked, and `cache_aware_summarizer` stops. | ✅ Works | To ask the model a cheap question, Context Guru adds the question to the exact text it sent last turn. The provider already cached that text. The sweep keeps **2 more full copies of the conversation per thread**, outside the stash, for a thread the keep-alive can ping (see `extract_llm_sweep` above). |
| Observe-mode store | content | ❌ **Should follow the level. Today it is still a real Store.** | It should follow the level | ✅ Works | Observe mode runs the pipeline later on a copy, to estimate savings. The copy should store only what the real run would store. |
| Dashboard database | content and metadata | ❌ Off | 🟡 Works, metadata: numbers, fixed codes, ids, and tool, MCP-server and skill names. **No** before/after text, summaries, declaration text, error text or rejection text. | ✅ Works. It keeps text only when content capture is on. | The dashboard shows what Context Guru removed and what your setup carries. Every number on it needs only counts and prices. Every request row comes from traffic the proxy saw itself. On a local install the dashboard also reads two things from your disk, and stores neither: the working folder recorded in Claude Code's session files, to group folders into projects for the project picker, and each installed skill's description from its `SKILL.md`. The keep-alive calculator and recommendation (#868) replay the per-wait ping plan from columns the proxy already writes for each request: stop reason, wait cause, thread id, cache tier and provider. They read no text. Tool deferral's net-saving panel (#869) adds nine count and price columns per request: tokens deferred, the names list's tokens, searches, dropped searches, tools loaded, wrong guesses, and the gross, net and cost dollars. The gross and the cost are the proxy's own ledger: the definitions kept out of the prompt, and the names list and searches that cost. On a request the accountant library priced, the net is the library's figure for the deferral, which it prices from the bodies, less the cost of the searches and the definitions they loaded, which no request body shows. On any other request the net is the ledger's own net. The tool inventory also records the built-in tools Claude Code defers (by name, with 0 tokens) and counts a tool loaded through search as a use. `save-more tools` reads the same inventory, the tool deferral history and the account's keep and always-defer lists. All of it is tool names, counts and prices, and works at the metadata level. Each request row also has one flag, `acc_priced` (context-guru#912), set when the accountant library priced the request. The keep-alive views read it to trust the library's stored rescue figure. It is a yes or no, with no text. Two more flags and two numbers sit beside it: `estimate_held` (a yes or no: the request's estimated figure was kept out of the totals), `estimated_saved_usd` (that figure, in dollars, the most it could be) and `estimated_saved_low_usd` (the least it could be). They hold no text. A request booked at $0 because the price list had not loaded is priced later from its own token counts and the model name, with no text (#920). An earlier history import copied whole request rows from those session files; it is removed, and its rows are deleted when the database opens. |
| Dashboard memos | content | ❌ Off, with the dashboard | 🟡 Works, metadata: a hash and a token count. That is all they need. | ✅ Works. They keep declaration and system-prompt text. | They remember a tool list so they do not count its tokens again on every request. A hash and a count are enough. |
| Archive | content and metadata | ❌ Off | 🟡 Works, metadata rows only | ✅ Works | It moves old conversations off the local disk. The database stays small, and you can still open old history. |
| Insights | metadata | ❌ Off (it needs the dashboard) | 🟡 Works, metadata | 🟡 Works, metadata | A cost report needs only counts, tool names and prices, not the text. |
| **Subscription headline** (dashboard Overview, #985) | metadata | ❌ Off (it needs the dashboard) | 🟡 Works, metadata | 🟡 Works, metadata | On a Claude subscription the Overview shows every saving in tokens, not dollars, and an estimate of the 5-hour limit share. It reads the auth kind and the latest rate-limit numbers the proxy kept for the account (#984), and the token counts by tier and the cost totals the dashboard already has (#998). Since #1023 the Sessions tab shows the same token counts per session, summed from the same stored numbers. It keeps nothing new. Decided by: David Amid, #985 (2026-11-09) and #998. |
| Thread tracker | metadata | 🟡 Works, metadata | 🟡 Works, metadata | 🟡 Works, metadata | It tells the main agent from its subagents. It uses hashes to check if a new request continues an earlier one. |
| Session tracker | metadata | 🟡 Works, metadata | 🟡 Works, metadata | 🟡 Works, metadata | It needs a stable label for each conversation. When the client sends none, it hashes the start of the conversation. The Bob IDE sends its task id in a request header (`X-Task-Id`), and only the Bob model route reads it. That is metadata only, and it works at every storage level. |
| Cache tracker | metadata | 🟡 Works, metadata | 🟡 Works, metadata | 🟡 Works, metadata | It must know how much of the conversation the provider already cached, so components never edit that part. After a restart it asks the dashboard (when on) for the time each thread's cached prefix was last refreshed — by its own last request or a keep-alive ping that read the cache, under either session id the conversation uses (the row stores the content-derived alias id, a hash like the session id itself); timestamps and ids, metadata only — and treats a thread refreshed inside the request's cache lifetime as cached, not as new, until that thread's first request. Without the dashboard — and for a tenant running observe mode, whose rows carry no session id — it uses the proxy's start time instead. |
| Accountant savings (per-component figures from the accountant library) | metadata | ❌ Off. It needs the dashboard, because the figure is attached to a dashboard event. | 🟡 Works, metadata | 🟡 Works, metadata | To price a saving it must know which part of the conversation a rewrite changed and whether the provider had that part cached. It reads each request once and keeps only a hash and a size for each part, plus the cache breakpoints. The text is dropped as soon as it is hashed, and the hashes are held in memory, one set per conversation thread, plus each thread's last set for the sibling check (SharedWarm). So a restart does not lose them, it also saves them to one file beside the dashboard database (`<dashboard-db>.accountant-state.json`, readable by its owner only), every minute and when it stops, and loads them again when it starts. The file holds hashes, sizes, positions, and the ids and names the state needs (session and thread ids, tool and component names, model names, times). It holds no message text. It also holds one proxy version number, and it is ignored when that number differs or when it is more than one hour old. It is not written when there is no dashboard. It reads the tools and system sections the same way, as names, hashes and sizes, to report the envelope changes. The result it writes to the dashboard is dollar amounts for each component, with their split by tier. Since #998 it also writes the same figures as token counts by tier (fresh input, cache read, 5-minute and 1-hour cache write), saved and caused: per component row, and per request for the keep-alive rescue, the cache split, the 1-hour head and the ended-change break. They are numbers only. `/api/stats` sums them in its `tokens` object. No level changes what it does. Decided by: David Amid, #998. |
| Wait-cause and lifecycle signals | metadata | ❌ Off (it needs the dashboard) | 🟡 Works, metadata | 🟡 Works, metadata | To tell why the agent paused, it reads the request once. It keeps only counts and ids. Since #857 it also reads the **tool name** of the last tool call, to tell that an agent has finished (`done`). The keep-alive then sends no pings for that thread. A tool name is metadata, and the rule works at every storage level. Since #858 the keep-alive also uses the **wait cause** (from the stop reason, the tool names and the open-task counts), whether the thread id starts with `a:`, the token count of the prefix and the time of day, to pick the ping count, to ignore a strategy's time windows for a wait that is not for a person, and to skip the sweep question during a tool wait. It also keeps one flag per thread: "has a tool call been seen". All of it is metadata, and it works at every storage level. |
| Costly settings check (`/context-guru:save-more settings`, `GET /api/adapt/settings`) | metadata | ❌ Off (the cost needs the dashboard). The plugin still finds the settings and says "cost not measured yet". | 🟡 Works, metadata | 🟡 Works, metadata | It counts and prices dashboard rows by request shape: model name, tool count, `max_tokens` and the stream flag. One check also uses the upstream status (400) of a Haiku row in the same session. It reads no request text and keeps nothing new. The plugin reads only the named keys of the user's own settings files (the betas setting, the recap setting, `ANTHROPIC_SMALL_FAST_MODEL` and `ANTHROPIC_DEFAULT_HAIKU_MODEL`) on the user's machine, and sends none of them to the proxy (#615). |
| Subscription view of `/context-guru:status` and `/context-guru:insights savings` (#986) | metadata | ❌ Off (the saved tokens need the dashboard). The screen says there is no data yet, as today. | 🟡 Works, metadata | 🟡 Works, metadata | On a Claude subscription the screens show everything in tokens, not dollars. They read the token counts by tier the dashboard already keeps (`tokens` in `/api/stats`, #998), the dollar totals for the 5-hour estimate, and the 5-hour usage share from the limit headers of the last response (`GET /api/me/limits`, #984). These are numbers only. They read no request text and keep nothing new. |
| Beep (`beep.sh`, the `beep` plugin option) | none | 🟡 Works, no data | 🟡 Works, no data | 🟡 Works, no data | It plays a sound when Claude finishes or waits for you. It reads no stored data and never reads the hook payload. It sends nothing to the proxy or anywhere else. The level changes nothing. |
| Bob plugin (`bob-plugin/context-guru`: routing block, hooks, `status`, `doctor`, status bar) | metadata | 🟡 Works, but `status` and `doctor` cannot see Bob traffic, and the status bar shows `—` | 🟡 Works, metadata | 🟡 Works, metadata | It routes Bob with one block in the shell startup file and adds two hooks to Bob's settings. The hooks read only the session id, the event name and the `SessionStart` source. They never read the prompt, and they send nothing for events that carry model text. `status` and `doctor` read only the time of the newest Bob request from the dashboard. The status bar reads the newest Bob session id and its saved-token count, and that session's cache times, prefix size and context window. All of these are counts, times and ids. With the dashboard off, those reads find nothing: `status` and `doctor` cannot see Bob traffic, and the status bar shows `saved —` and `cache —`, never `saved 0`. Everything else still works. The level changes nothing in the plugin. |
| Pi proxy check at session start (`pi-package`, `pi_plugin.py restart`) | none | 🟡 Works, no data | 🟡 Works, no data | 🟡 Works, no data | When a Pi session starts, the Pi extension sends one bare `GET /healthz` to the local proxy, with no body. If the proxy does not answer, it restarts the proxy's user service once and checks again. The extension reads the port, the proxy URLs, the cache strategy and the chained upstreams from the install record, and the model URLs Pi knows. `restart` reads the port, the preset, the cache strategy, the chained upstreams and the binary path from the record. Before the restart it runs the binary's `--version`, and it writes its own `proxy.yaml` again from the record (only a file with its marker; #1044). It reads no stored data and sends no request content. |
| **Subscription limit headers** (#984, #995, #1016, `GET /api/me/limits`) | metadata | ❌ Off (it needs the dashboard). The proxy reads the headers and keeps nothing. | 🟡 Works, metadata | 🟡 Works, metadata | To show how much of the 5-hour limit a session used, and later to learn how the limit counts each token kind, the proxy stores the used share, the reset time and the status word from the provider's response headers on each request row. Keep-alive pings are real requests too, so the proxy stores the same values on each ping row (#995). A summary or sweep call that takes the place of a ping counts as that ping (#1016). Only an answer sent with the user's own credential counts. Then the share stays current while the user is idle. It also stores one yes or no: did the client send a subscription (OAuth) token. It tests only the token's fixed prefix and never stores the token. These are numbers, short status words and a flag, not content. |
| Metrics (`/stats`, `/metrics`) | metadata | 🟡 Works, metadata | 🟡 Works, metadata | 🟡 Works, metadata | Totals and rates need only numbers. |
| Accounts (hosted) | account data | ✅ Works | ✅ Works | ✅ Works | This is the account system: who you are, your settings and your consent flag. It holds no transcripts. |
| **Trace recording** (`--record`, `--record-otlp`) | content | ❌ Off | ❌ **Cannot work.** A trace without bodies cannot be replayed. | ✅ Works, only for accounts that opted in. Off for every account until its owner turns it on. | It records the full request and response of each call, redacted, so a replay tool can send the real prompts again and reproduce real prefix-cache behaviour. It is all content, so it is a deliberate exception to the metadata-only rule. Decided by David Amid, #649 (2026-11-01). |

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
