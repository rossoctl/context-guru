# Tool, MCP and skill inventory

*How much of what you carry do you actually use?*

An agent declares its whole toolbox on **every** request. Those declarations sit at the
front of the prompt, inside the cached prefix, and every turn of the session re-reads
them. A tool the model never calls is therefore not paid for once — it is paid for on
every request of every session that carried it.

Until this feature the dashboard could not answer the question at all: `requests.tools`
stores a **count**, never a name. The count finds the heavy users (production has
sessions declaring 106 tools); it cannot say what they are carrying, so it cannot say
what they are carrying for nothing.

## What is measured, on real traffic

Measured by running the shipped capture path over 1,844 raw request bodies recorded from
this proxy (`/tmp/cg-runs/capture-*.jsonl`), tokenized with the repo's own BPE tokenizer
(`internal/tokens`, `o200k_base`) — not a `chars/4` estimate. Reproduce with
`go test ./dash -run TestCorpus -v`.

| | SWE-bench corpus (50 sessions) | Terminal-Bench corpus |
|---|---:|---:|
| declared tokens per session | 21,889 | 21,975 |
| **never used in that session** | **18,107 (82.7%)** | 18,452 (84.0%) |
| skills declared / invoked | 11 / 0 | 12 / 0 |
| skill listing, tokens per request | 1,085 | 1,193 |
| requests per session (the re-read multiplier) | 35.4 | 73.0 |

**27 declarations were carried by all 51 sessions and invoked by none**: `Workflow`
(5,220 tokens on its own), `Agent`, all five `Cron*`/`Task*` families, `EnterWorktree`,
`ExitWorktree`, `NotebookEdit`, `ReportFindings`, `SendMessage`, `WebSearch`, `Skill`
itself, and every one of the 11 declared skills.

### What it costs on the deployed service

The 8,801 `claude-cli` requests with tools in the deployed database (135 sessions, three
days) split by the tier each request was actually billed at: 7,853 read the prefix from
cache, 535 created it, 413 had no cache at all. Per-model rates recovered by least
squares from `requests.cost_usd` itself (the operator's price list is never read here),
which lands at ≈ $0.40–0.48 per MTok of cache read for the opus models carrying this
traffic:

```
avoidable cache-READ tokens   142,194,271  →  $60.53   ($0.448/session)
avoidable across all tiers    159,359,707  →  $148.86  ($1.103/session)
share of those sessions' $2,250.59 spend       2.7% read-tier, 6.6% all-tier
```

The read-tier figure is the one comparable to the spike's $52.20. The all-tier figure is
what `/api/tools` reports, because a token in a created prefix is billed at 1.25x and
pretending otherwise understates the waste.

For scale: measured cachesplit value on the same service was **$0.03 across 1,127
sessions**.

### Where these numbers differ from the spike

| | spike | here | why |
|---|---:|---:|---|
| declared tokens/session | 21,051 | 21,889 | real BPE instead of `(len+3)/4` (the spike flagged ±10%) |
| never-used tokens/session | 15,864 | 18,107 | the spike credited a tool as "used" if **any** session called it; a tool called in 10 of 50 sessions is dead weight in the other 40. This report weights per session |
| unused share | 70.0% / 81.9% | 82.7% / 84.0% | same cause |
| priced total | $52.20 | $60.53 read-tier | per-model rates re-derived from the rows |
| TB sessions | 3 | 1 | session keying: these captures carry no session id, so both spike and test key on the first user message |

Everything the spike *structurally* claimed held up: tools are a flat array of
`{name, description, input_schema}`; the skills listing is prose in a `{"role":"system"}`
message and not in the system prompt; the `Skill` tool's schema has **no enum**; and
there is not a single MCP tool in either corpus (the benchmark agents run without MCP, so
the `mcp__<server>__<tool>` convention is verified against real names from an MCP-loaded
host instead).

## What is captured, and what is not

`proxy/dashcapture.go` → `noteInventory` → `dash.ScanInventory`, off the pristine inbound
body, in the same place the request's metadata is already read.

* **Declarations.** Every element of the top-level `tools` array: its name, its token
  weight (the whole element, BPE-counted), and its class — `tool`, `mcp_tool` (with the
  server half of `mcp__<server>__<tool>` split out so per-server rollups are free),
  `server_tool` (provider-side `web_search_*`, `code_execution_*`, the `mcp_toolset`
  connector). A `defer_loading` tool is recorded with **zero** weight: it is advertised
  but not sent, so charging its schema would double-count tool search.
* **Skills.** Parsed out of the `{"role":"system"}` message's listing, plus one marker
  row carrying the listing's own token cost and the parse state.
* **Usage.** Every `tool_use` block of the last tool-using turn (`tool_calls` in the
  OpenAI dialect), and for the `Skill` tool the `skill` argument — the only place a skill
  invocation is identifiable.
* **Never** a description, a prompt, or a message. Names and token counts only.

### Consent: identifiers, not content

These rows are gated on **tenant scoping**, not on `capture_content`. A tool name, an MCP
server name and a skill name are identifiers of the caller's own *configuration* — the
same sensitivity class as `tool_choice`, which this store already keeps — and they are
not the caller's transcript. Requiring transcript consent would deny the feature to the
accounts that declined it, over data that is not their transcript. Every row carries
`tenant_id`, every query filters on it, and the name charset is checked before it is
stored.

### Skill discovery fails safe

Skills are declared in **prose**, so the parser can be wrong in a way a JSON array cannot.
It is built so that being wrong yields *"unknown"* and never a confident empty inventory
— because "0 skills declared" is exactly what would later authorise stripping a 1,100-token
listing describing capabilities the model still has:

1. The header `The following skills are available for use with the Skill tool:` is
   required. The agent-types listing sitting immediately above it in the same message
   uses the identical `- name: description` shape, so the anchor is load-bearing.
2. Entries are read only between that header and the end of the reminder.
3. A name must be a bare identifier (`a-zA-Z0-9._:-/`, so `plugin:skill` and
   `apps/web:deploy` parse, and a sentence does not).
4. A listing that yields **no** entries stores the marker row alone, with state
   `unknown` and the measured size. `/api/tools` then reports `skills.state = "unknown"`
   with `unknown_sessions`, and a reader must not translate that into "no skills".
5. An agent with no such block at all (Codex, Bob) simply has no skill rows —
   `state = "absent"` — and the tools half is unaffected.

### Cost on the request path

Measured on a real 121 KB body with 24 tools
(`go test ./dash -run XXX -bench ScanInventory`, Xeon Cascadelake):

| | ns/op |
|---|---:|
| **warm** (every request after a session's first) | **180 µs** |
| cold (the first request of a declaration set: full parse + BPE of every declaration) | 965 µs |
| for comparison: the metadata pass the capture point already ran | 86 µs |
| for comparison: one `gjson` parse of the same tools array | 295 µs |

The warm path is one structural scan of `tools`, two byte searches for the listing, one
hash of the set and one map lookup: 0.18 ms against a turn whose upstream leg is hundreds
of milliseconds, ~0.05%. Everything expensive is memoized by a digest of the declaration
set, which is why the 102–106-tool MCP sessions pay it once rather than 65 times.

## Storage

Two additive tables (`dash/schema.go`, `additiveDDL`) — **no `schemaVersion` bump**, which
matters: a bump renames the database aside and discards every `requests` row.

| Table | Holds |
|---|---|
| `tool_declarations` | one row per (session, declaration-set digest, kind, name): the token weight of carrying it, and the MCP server it belongs to |
| `tool_uses` | one row per (session, name, skill): call count and first/last seen |

Keyed by **session and digest, not by request**. The declaration set is constant for a
whole session unless the client changes it (measured: 34 of 135 production sessions ever
do), so per-request rows would be ~45 identical rows × 65 requests per session. A session
that changes its set gains a second digest, which is also how the change becomes visible.
Usage is counted from each request's last tool-using turn and deduped by that turn's
fingerprint, because the agent resends the whole transcript every request.

Lifetime is a trigger (`trg_tool_inventory_gc`): these tables have no `request_id` to hang
a cascade on, and every deletion path in the package — retention, disk-pressure eviction,
cold-storage migration, a manager's tenant purge — deletes from `requests`. When a
session's **last** request row goes, its inventory goes with it; a quota trim that removes
only a live session's oldest rows leaves it alone.

## Reading it: `GET /api/tools`

Tenant-scoped through the same `scope()` helper every other data route uses: an account
sees its own sessions, a manager sees the service and narrows with `?tenant=`. `?session=`
narrows to one session; every other standard filter (`since`, `model`, `agent`, …) applies.

```jsonc
{
  "coverage": {                  // the honesty half — read this first
    "sessions": 135,
    "captured": 12,              // sessions with an inventory
    "not_captured": 123,         // requests predating this capture: NOT "nothing unused"
    "unpriced_sessions": 0,
    "requests": 8801
  },
  "totals": {
    "declared_tokens": 21889,    // per captured session
    "unused_tokens": 18107,
    "unused_pct": 82.7,
    "unused_reads": 159359707,   // tokens actually billed for carrying dead declarations
    "unused_usd": 148.86,
    "priced": true,
    "requests_per_session": 65.2
  },
  "tools":   [ { "kind": "tool", "name": "Workflow", "tokens": 5220,
                 "sessions_declared": 50, "sessions_used": 0, "calls": 0,
                 "unused_reads": 9244620, "unused_usd": 4.11, "priced": true } ],
  "servers": [ { "server": "plugin_context7_context7", "tools": 2, "tools_used": 1, … } ],
  "skills":  { "state": "ok", "declared": 11, "invoked": 0, "listing_tokens": 1085,
               "unused_listing_reads": 1921535, "skills": [ … ] }
}
```

Two rules the payload enforces rather than leaves to the reader:

* **A missing measurement is never a zero.** The 14,410 requests already in the deployed
  database have no names, so their sessions appear in `coverage.not_captured` and
  contribute to *nothing* above — not as fully-used, not as fully-wasted. A UI must render
  those as "not captured".
* **An unpriced number is not free.** A model with no known rates leaves the token counts
  visible, `unused_usd` at 0 and `priced: false`, with the session counted in
  `coverage.unpriced_sessions`.

## Not in this layer

The **filter** — actually removing dead declarations — is a separate change in
`apply.BodyOpts`, and the spike's `requests.filtered_decl_tokens` column belongs with it:
a column no writer fills would be a permanently-zero saving figure, which is the exact
failure mode the coverage rules above exist to prevent. The measurement ships first, on
purpose.
