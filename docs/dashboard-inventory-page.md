# The Inventory page

*The dashboard's **Inventory** tab: what your agent carries in every prompt, what it
actually invokes, and the control to stop carrying the rest.*

This page documents the **UI**. What is measured, how it is measured and what the numbers
came out as is [Tool, MCP and skill inventory](tool-inventory.md); the API it reads is
`GET /api/tools`, described there.

Files: `dash/ui/tools.js`, `dash/ui/tools.css`. The view mounts itself — its tab, its
`<section>`, its stylesheet and its loader registration are all in `tools.js`, so the
shared page carries exactly one line about it (`<script src="tools.js"></script>` in
`dash/ui/index.html`). No edit to `app.js`, and every helper it draws with — `el()`,
`tile()`, `tileGroup()`, `emptyState()`, `sortRows()`, `rangeLabel()` — is `app.js`'s, so
the page belongs to the same design system rather than shipping a second one.

## What is on the page, top to bottom

| Section | Answers |
|---|---|
| **Headline tiles** | declared tokens per session, how many were invoked, how many never were, and the projected cost of the difference |
| **Gauge** | one bar, two segments, drawn to scale: invoked against never-invoked |
| **Who owns your system prompt** | the part-to-whole bar: every region of the prefix, coloured by whether it is yours to change. Headed *"Who owns what you carry in front of every request"* until a session in scope has recorded a system prompt, because until then the bar is declarations only |
| **Your system prompt, and what shares it** | its size, the whole prefix, and the reveal that shows the actual text region by region |
| **Carried by every request, never once called** | the actionable list — grouped by what ONE action removes, each group with the command that removes it |
| **Realized by your removals** | what a removal *actually* avoided on requests that were really sent |
| **Every declaration, by weight** | the full table, sortable on any column |
| **MCP servers** | the per-server rollup, because a server is the unit you add or remove |
| **Skills** | the listing's own weight, how many skills were declared, how many were ever called |
| **What these numbers are computed over** | the denominator: sessions in scope, how many were captured, how many were priced, and at which tier a token became a dollar |
| **The agent's own tools** | built-ins and provider tools, last, collapsed, behind a danger warning on the closed bar |

Every headline figure on the page carries an **"i"** with its plain-English definition, how
it was derived and its catch, from `TILE_INFO` in `app.js` — the same registry the Overview
tiles read. `tools.js` adds its fourteen entries to that object rather than keeping a second
one, so the whole set can be read side by side and checked for one voice.

## Four things this round changed, and why

**The removal command is visible, per group.** It existed on the API
(`dash/toolremoval.go`) from the start and was rendered in exactly one place: the built-ins
table — i.e. the only group on the page nobody should act on. A reader looking for "how do I
get rid of this" found nothing in the actionable list and a danger warning in the one place
the command appeared.

**`kind` is no longer read as the answer to "is this a built-in".** Claude Code's own tools
and a third-party agent's own tools are the *same* kind (`tool`; the stored taxonomy cannot
tell them apart), and the report answers the question in a separate `builtin` boolean.
`KIND_LABEL` used to map kind `tool` straight to the string "built-in", so every removable
client tool an SDK application had declared rendered a pill reading "built-in" *inside the
list of things the page was recommending be removed*.

**The actionable list is grouped by what one action removes.** Flat and sorted by weight,
eighty rows of `$0.0035` read as eighty separate judgement calls. An MCP server is one
decision, not eight. Grouping happens **only when the server says the mechanism is
group-wide** — a plugin-bundled MCP server has no `claude mcp remove` name and its removal is
a per-tool deny, so those stay ungrouped rather than sit under a heading claiming a one-tool
snippet removes all of them. Groups are ordered by the dollar figure they lead with, and
past the eighth the tail folds with its total on the summary.

**The system prompt is in the composition.** It is normally the largest single region of the
prefix, and leaving it out made a "composition of your prompt" that was a composition of the
tools array — a complete-looking answer to a question it was not answering.

## The chart, and why it is the form it is

Part-to-whole with long-named categories, so: one **horizontal stacked bar**. Not a pie (five
parts, one of them 2%), not a treemap (it would imply a hierarchy the data does not have),
not five tiles (a tile row cannot show that these are shares of one thing).

The colour decision is the one worth defending. The four groups a reader can **act on** take
the four categorical slots (`--s1`..`--s4`); the group they must **not** act on takes the
de-emphasis grey (`--s-mute`). Colour is what the eye reads before any label, so four hues of
equal weight asserted that Claude Code's own equipment was a fifth removable category —
exactly the reading that gets somebody to delete `Read`. `--s-mute` is not a fifth series
step and is never used as one; it is exempt by role from the categorical chroma floor, and it
clears 3:1 against the surface in both themes so the segment is still a visible mark.

Segments carry the mark spec's **2px surface gap**, which is not cosmetic here: without it two
adjacent fills of similar lightness read as one segment, which is precisely the misread a
part-to-whole chart must not permit.

### One arithmetic correction

Each **skill's** token weight is the weight of its own entry *inside* the skills listing — a
sub-slice of the same block. The composition used to add the skill rows to the listing, so the
segments summed to 43,933 while the panel printed 36,950 beside them as "the whole that each
share is a part of", and every legend percentage was a share of the inflated figure. The
denominator is now the server's own `declared_set_tokens` (listing + every tool), which
partitions.

The page is scoped by the ordinary filter bar, so a time range, a `?session=`, or (for a
manager) a `?tenant=` narrows every figure on it.

## The six honesty rules, and where each one lives in the code

These are not editorial preferences. A page reporting that 82.7% of a prompt is dead
weight is a page with a strong incentive to overstate, and each rule below closes one
specific way of doing that.

**1. "Not captured" never renders as "nothing unused."** Every session recorded before
this feature existed has requests but no declaration names. `renderHeadline()` therefore
*suppresses the entire headline band* when `coverage.captured == 0` and shows an explicit
awaiting state instead — because a 0-token, 0% bar over an empty set is visually
identical to perfect utilisation. `renderCoverage()` reports `not_captured` as its own
number, always, including when it is zero.

**2. A projection is never presented as a realized saving.** They are two panels, worded
apart: the tile is labelled *Avoidable — projected* ("if none of it had been carried"),
and *Realized by your removals* is noted *measured on requests actually sent — not a
projection*. `renderRealized()` reads only the filter's own accounting and prints an
explicit "nothing removed yet, so nothing realized" when there is none. It never falls
back to the projection.

**3. An unpriced model shows tokens and no dollar.** Every dollar cell on the page goes
through `money(v, priced)`, which renders an `unpriced` pill when `priced:false` — never
`$0`, which would claim the item was free. The headline tile switches to a token count
with the reason, and the coverage panel names how many sessions were affected.

**4. Every suggestion states its basis.** Each row in the opt-out list carries "Unused
across *N* of your *M* captured sessions — *&lt;window&gt;*". When fewer than five
sessions have been captured the panel says so before the list: this is evidence nothing
has called an item *yet*, not proof nothing will. Absence of evidence is not evidence of
disuse, and the reader must be able to see which they are being shown.

**5. The tier is stated, not chosen.** The same wasted tokens cost roughly ten times as
much at the fresh-input rate as at the cache-read rate. The API prices each re-read at the
tier that request *actually* paid — cache read for a hit, cache creation for the turn that
wrote the prefix, full input for a turn with no cache — and the coverage panel says so
explicitly, including the warning that a figure quoted at one flat tier is a different
number and must not be compared to this one. The page offers no "price it all at the
cheap tier" toggle.

**6. A small number reads small.** The gauge is drawn strictly to scale with no minimum
segment width, so a 4%-unused account and an 83%-unused account cannot look alike. No
layout on the page needs a big number to look right: the tables are ordinary tables, the
tiles hold whatever the figure is, and the zero-unused case gets its own line ("nothing
in this scope was declared and left unused") rather than an empty chart.

## The opt-out control

One checkbox per candidate. Above the list, a folded note states the whole deal:

* it is **reversible** — switching an item back on restores the declaration exactly, and
  nothing about the agent's own configuration is edited;
* the **one-time cost is a cache miss**. Declarations sit at the very front of the cached
  prompt, so changing the set changes the prefix, and the next turn of each live session
  re-writes its whole prompt at the cache-creation rate instead of reading it back. It is
  larger per turn than the saving per turn and it is paid once — so switch off everything
  you mean to switch off in one pass, and expect toggling to pay it each time;
* sessions already running keep what they declared on their first turn.

A **provider-side** tool (`web_search_*`, `code_execution_*`, the `mcp_toolset`
connector) is part of the request the agent builds, not something this proxy declares, so
its row is inert and says why rather than offering a switch that does nothing.

### Degrading without the filter endpoint

The control needs `GET /api/toolfilter` and `POST /api/toolfilter`. When the endpoint is
absent (or answers `enabled:false`), `loadTools()` keeps the whole analysis and renders
the switches **disabled with the reason stated once** — a control that vanishes looks like
a page with no opinion. The shape the page expects:

```json
GET /api/toolfilter
{ "enabled": true,
  "excluded": [{"kind":"tool","name":"Workflow","server":"","since":1755640000000}],
  "realized": {"reads":30084,"usd":0.0271,"priced":true,"requests":6,"since":1755640000000} }

POST /api/toolfilter   {"kind":"tool","name":"Workflow","server":"","action":"exclude"}
→ the same document, updated
```

`realized` must be what the filter **actually avoided** — tokens not re-read on requests
that were really sent — and must carry its own `priced` flag. If it cannot be measured it
must be omitted, not estimated: the page will print "nothing realized" rather than borrow
the projection.

## Verified states

Driven with real headless Claude Code sessions through a local proxy on `:4330` against a
scratch database (3 sessions, 6 requests, 139 tool declarations, 11 skills, one real MCP
server), then rendered with the real functions:

| State | How it was produced | What the page showed |
|---|---|---|
| populated | 3 real sessions; Bash, Write and Read genuinely called | 32,047 declared/session, 30,877 (96.3%) never invoked, `Workflow` 5,014 tok/request at the top |
| `not_captured` | the same database with the inventory rows deleted — production's exact shape | no tiles, no bar, no zero: "3 of 3 sessions have no captured inventory" and the reason |
| unpriced | the live API before the gateway's price list resolved (`priced:false`) | "183,825 tok" with "no dollar: some models here are unpriced", every dollar cell an `unpriced` pill |
| zero-unused | the same database with an invocation inserted for every declaration | 100% invoked, a 0.00%-wide waste segment, "nothing in this scope was declared and left unused" |
| skills `unknown` | `skills.state = "unknown"` | "a skills listing was present … and could not be read", its size stated, explicitly *not* "you have no skills" |

## The prompt-text panel, and the four states it must tell apart

`GET /api/prompt` serves one session's prefix as a list of **regions**: the system prompt,
the skills listing, and every tool schema, each with its measured weight, its share and its
text. Its own route rather than more fields on `/api/tools` — the report is forty numbers and
is fetched on every tab switch, the text is tens of kilobytes and almost nobody opens the page
to read a tool schema. `tools.js` fetches it **once** per report and repaints only the reveals
that are open; a `renderTools()` would rebuild every `<details>` and close the reveal whose
click started the fetch.

One session's, not an aggregate: a prefix averaged over sessions is not a prefix anybody sent,
and the panel names which one and when. Regions are ordered heaviest first with the system
prompt pinned to the front, and the panel says out loud that this is **not** the order the
model reads them in — the array order a client sends its tools in is not stored.

| State | Cause | What the reader is told |
|---|---|---|
| text present | consent on, row written since the column exists | the text, with its weight and share of that session's prefix |
| blocked by **operator** | `--dashboard-content` off service-wide | "not stored on this deployment … there is nothing for you to enable" — never an instruction to change a setting that is not the one shut |
| blocked by **tenant** | the account has not opted in | "not captured — enable transcript storage to see this", and that turning it on does not backfill |
| **not recorded yet** | the row predates the column | from an explicit coverage count (`N of M declarations stored their text`), never a fabricated empty string |

The coverage count is shown **before** the reveal, not inside it: a reader has to know how
much of their history can answer the question before concluding anything from the one session
below. `PromptStat.Rows`/`TextRows` and `PromptView.Rows`/`TextRows` count the **same way** —
one per `(session, kind, name, server)`, matching how `scopedDecls` groups them. They did not
at first: raw table rows came out at 4,192 where the report said 309, and two coverage figures
13x apart on one page with nothing connecting them is how a dashboard loses a reader for good.

## Where the screenshots are

Local only — the repo is public and the captures are of real accounts' inventories. Three
states, light and dark, against a **copy of a copy** of the production snapshot scrubbed to
the two authorised accounts with every session id rewritten to a synthetic one:

- `/tmp/r2/shots/` — full-page before/after pairs, Overview and Inventory
- `/tmp/r2/slices/`, `/tmp/r2/slices2/` — the before and after Inventory pages sliced for
  panel-by-panel reading
- `/tmp/r2/slicestext/` — the prompt-text panel with text captured
- `/tmp/r2/slicesop/` — the same page with the operator's content gate shut

The capture driver asserts that no email, no UUID and no unexpected 16-hex account id appears
in the rendered text of any page it shoots, and fails the run if one does.
