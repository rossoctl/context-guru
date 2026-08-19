# Stop carrying tools you never use

Your agent re-sends its whole tool catalogue on every turn. Measured with this repo's own
tokenizer over 1,844 real request bodies: **21,889 declared tokens per session, of which
18,107 (82.7%) belong to declarations that no session ever invoked.** 27 items were declared
by every session and called by none.

That is the largest single lever measured in this project — one to two orders of magnitude
above anything in [the 2026-08 results](../results/measured-2026-08.md) — and it is also the
one that most easily turns into a correctness bug, so it is **opt-in, per account, per name,
and never inferred**.

## What it does

Configure the `toolfilter` component with the declarations you want to stop sending:

```yaml
pipeline: [format, dedup, extract, cachesplit, toolfilter]
components:
  toolfilter:
    remove: [CronCreate, CronDelete, CronList, Workflow, mcp__playwright]
```

Names are exactly as the inventory reports them. `mcp__<server>` (no tool half) removes a
whole MCP server, which is the unit you actually added and the unit you will remove.

On the hosted service you do not hand-write that: the dashboard's **Inventory** page has a
switch per declaration, which posts one change to `POST /api/toolfilter`
(`{kind, name, server, action:"exclude"|"include"}`) and repaints from the document the server
answers with. That route is part of the **control plane**, not the dashboard, on purpose — the
removal list is your compaction configuration, so it goes through the same account-update path
as `PUT /api/me` and inherits its validation, its audit entry and its manager gate. A junk name
or a wildcard is a 400; changing what we send is attributable; and setting it is a manager's
action, like every other decision about what runs on the traffic.

`GET /api/toolfilter` is the read half, served by the dashboard:

```json
{
  "enabled": true,
  "excluded": [{"kind": "tool", "name": "Workflow", "since": 1755000000000}],
  "realized": {"reads": 446390, "usd": 0.1186, "priced": true, "requests": 35, "since": 1755000000000},
  "suggestions": [{"name": "CronCreate", "tokens": 812, "sessions": 6, "days": 10,
                   "projected_usd": 0.004, "basis": "declared but never invoked across 6 of your sessions since 2026-06-01 (10 days)"}],
  "min_sessions": 5, "min_days": 7, "withheld": 4, "coverage": {"sessions": 21, "captured": 20, "not_captured": 1}
}
```

`enabled:false` is a working state, not an error: `reason` says why the control is
unavailable (a local proxy has no per-account configuration; a stored document that does not
load must be repaired on the account page first) and the analysis is served regardless.

## What it will refuse to do

**It never removes a name you did not list.** There is no `auto` mode and no threshold. The
inventory can only show that something was not used in the sessions it captured, and an
unused tool is not the same thing as an unwanted one.

**It keeps any declaration whose name still appears in the prose.** This is the hazard that
matters, and it is not rejection — it is silence. Claude Code's system prompt keeps
*describing* its tools in English:

> Prefer dedicated tools over Bash when one fits (Read, Edit, Write) — reserve Bash for
> shell-only operations.

Strip `Read`'s declaration while that sentence survives and the model may write the call into
prose instead of emitting a `tool_use`. Nothing errors; you just get a worse agent.

Our proxy can *see* that prose but cannot safely *edit* it — it is hand-written English whose
sentences name several tools at once, so there is no span whose removal is known to leave a
coherent instruction. So the rule is the conservative half of "strip a declaration and its
prose together, or neither": **a declaration mentioned in the prose is kept, whatever the
configuration says.** On the real Claude Code catalogue that gate keeps 9 of 24 tools and
clears the other 15 — including every never-used item that carries real weight.

Also kept, always: the tool `tool_choice` forces (removing it would turn a forced call into a
400), provider-side tools declared by `type` rather than by a schema, and the last remaining
tool (a body that declares a catalogue and one that declares none are different shapes).

**Skills are reported but not removable.** A skill is declared as prose inside a transcript
message rather than as an element of `tools`, so removing one means editing that listing; and
the `Skill` tool's schema carries no enum, so the model can still name a skill that is no
longer listed. Different mechanism, different failure mode — see *Not done* below.

## Suggestions, and what "enough evidence" means

`GET /api/toolfilter` returns candidates with their weight, their projected value, and the
evidence behind the offer. A candidate is offered only when it

1. was declared in at least **5 distinct sessions whose inventory was captured**,
2. whose first and last are at least **7 days** apart, and
3. was invoked in **none** of them.

Both thresholds target a specific way this can be wrong. The session count is about
*opportunity*: a measured session with tools makes ~65 requests, so five sessions is a few
hundred turns in which the model chose something else every time. The span is about *variety*,
which a count alone cannot buy: five sessions in one afternoon are usually five sessions on
one task, and the tool you did not need this afternoon is exactly the tool the next task type
needs.

Sessions with **no captured inventory are not evidence**. They are not counted toward (1) and
not counted as unused either. Every request row that predates this capture looks like that,
and letting "we have no rows" stand in for "it went unused" is how absence of evidence
becomes authorisation to remove something. The report's `coverage` block says how many
sessions in scope could answer the question at all, and `withheld` says how many never-used
items did not reach the bar — because a short suggestion list has two very different causes
and silence cannot tell them apart.

## Realized vs projected — which number is which

These are separate fields because confusing them is how this feature's headline becomes a
lie.

| | Field | What it is |
|---|---|---|
| **Realized** | `realized.reads`, `realized.usd` | Tokens the filter really did not send, priced at the tier each of those requests really was billed at. Sourced from `requests.filtered_decl_tokens`, written by the filter and by nothing else. |
| **Projected** | `suggestions[].projected_usd` | What removing an item *would* save, extrapolated from what carrying it has cost. A forecast. |

**`realized` is omitted, never zeroed, when nothing has been measured.** An absent field reads
as "nothing realized"; a zero-valued object would render as a measured $0.00, and a page that
then borrowed the projection to fill the gap would be presenting a forecast as a fact. There is
no path that puts an estimate in this field. `priced:false` means at least one contributing
model has no known rates, in which case the token count is real and the dollar is not — render
the tokens with an "unpriced" mark rather than a $0.00.

The realized figure is reported split by tier (`cache_read_tokens`, `cache_write_tokens`,
`fresh_tokens`) because one blended number cannot be checked. The tiers matter more than
anything else in the arithmetic: a cached prefix re-reads at **0.1x**, the turn that *creates*
the prefix writes at **1.25x**, and an uncached request pays **1.0x**. Pricing every avoided
token at the fresh-input rate inflates the whole figure roughly tenfold, which is why this
project's number is smaller than the headroom-style figures it is sometimes compared to (see
[measurement conventions](../results/measured-2026-08.md)).

Before any account opts in, `realized` is **zero**, and that zero is the truth. A
permanently-zero saving column is the failure mode the coverage rules above exist to prevent,
so the column exists only because there is a filter populating it.

## Measured

Replayed through `apply` over real captured Claude Code traffic
(`apply/toolfilter_capture_test.go`, so the numbers are reproducible), with the removal list
an account would actually arrive at from its own inventory — the never-invoked, prose-free
declarations:

| capture | requests | sessions | removed per request | tokens avoided | avoided |
|---|---|---|---|---|---|
| `long.jsonl` (one real session) | 35 | 1 | 12,754 | 446,390 | **$0.1186** |
| `capture-tb.jsonl` | 73 | 3 | 12,854 | 938,342 | **$0.2764** |

Tier assignment: a session's first request writes the prefix (1.25x) and every later one
reads it (0.1x) — 1,105 of 1,127 real session starts were cold, so crediting the first
request at the read rate would understate it. Input rate $2.00/MTok, the same default `ab.sh`
uses.

Against the shipped default the filter is a no-op and costs 2.4 ms across 35 requests
(0.07 ms/request), with message-token savings and the cache-safety ceiling byte-for-byte
unchanged:

```
arm            before      after   removed       %    net@write       ms  ms/req
main          1614822    1614822         0    0.00      +0.0000      844    24.1
tf            1614822    1614822         0    0.00      +0.0000      505    14.4
```

## Cache cost, and putting a tool back

`tools` is hashed at position 0, ahead of `system` and `messages`, and no breakpoint sits on
it — so **any** edit here re-anchors the entire cached prefix. Two consequences:

- **From a session's first request, the filter is free.** That request is a cold start
  anyway. Every later request of the session is then strictly cheaper.
- **Turning it on mid-session costs one re-anchor**: measured $0.70 for a 147,099-token
  prefix at the 1.25x creation tier. Payback is under two sessions.

Recovery is the same switch, flipped back (or the same edit in reverse: delete the name from
`remove` and save). Emptying the list takes the component out of the pipeline entirely rather
than leaving a pass over every request that removes nothing. Nothing else
has to be undone — the filter has no state, the declarations were never modified, only
omitted, and the inventory kept recording the full set the client declared, so your history
and your suggestions are intact. **The revert also re-anchors once, at the same ~$0.70**, so
prefer restoring a tool between sessions rather than mid-task, exactly as when adding it.

A transcript that already *called* a tool you have since removed keeps working: verified on
production first — a window of 66 mid-session tool-set shrinks returned 64x HTTP 200 and 0x
400, and 20 of the captured bodies carried a `tool_use` whose name was absent from their own
`tools[]` and completed normally. `TestFilterKeepsHistoricalToolUseForwardable` pins it.

## Determinism, and why there is no "smart" version

A prefix transform that fires on some turns of a session and not others re-anchors on **every
one of them**. A sibling proved that the expensive way. The only safe shapes are ALWAYS and
NEVER for a given session, so every input to this decision is session-invariant:

- the removal list is configuration;
- the prose region is the `system` blocks minus the environment snapshot `cachesplit` already
  isolates as volatile, plus the first message — which is exactly the text
  `schema.SessionHead` feeds to `session.Scoped`, so if it changed we would be looking at a
  different session by this codebase's own definition. (A commit subject in the git snapshot
  naming a tool must not veto its removal; `TestFilterIgnoresTheVolatileSnapshot` pins that.)
- the output preserves input order and re-uses each kept element's raw bytes, so removing
  nothing is byte-identical to the input. `TestFilterDeclarationsByteStable` re-runs itself in
  child processes, because map iteration and the maphash seed are re-randomized per process
  and same-process repetition cannot prove this alone.

There is one deliberate narrowing of a stated safety rule. "Skip any request whose last turn
is a pending tool call" would, applied broadly, *violate* the rule above — a session whose
turns end in an unanswered `tool_use` would alternate filtered and unfiltered and re-anchor
every turn. So the skip is scoped to a pending call **for a name in the removal list**, which
is unreachable in the steady state rather than merely rare: with the filter on from turn 1 the
model never saw the declaration, so it cannot have emitted the call. The only way to reach it
is the turn an account switches the filter on mid-session, which re-anchors once by
definition.

## Not done, and why

- **Skill removal.** Biggest remaining piece of the measured waste (all 11 skills in the
  corpus went uninvoked). It needs a different mechanism — editing prose inside a transcript
  message — and it cannot be made as safe: the `Skill` tool's schema has no enum, so removal
  turns "unused" into "errors when called" rather than "unavailable". Worth doing on its own
  evidence, not as a footnote to this.
- **Truncating or dropping tool *descriptions*.** 110 KB of the 131 KB of `tools` on real
  traffic, and the one lever whose failure mode is a wrong tool call. See
  `components/reformat/toolschema.go`, which declines it for the same reason.
- **Per-name attribution of the realized saving.** The column is one total per request, so
  `realized` is an account-level figure and `excluded[].since` is when the filter first acted
  for the account, not a per-item date — that history is in the audit log, where the change was
  recorded. A per-name breakdown means a second table; nothing yet needs it.
- **Letting a non-manager set their own list.** It is the compaction configuration, and the
  hosted service already makes that a manager's field. Loosening it here would be loosening it
  everywhere, quietly.
