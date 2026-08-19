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
| **Declared by every session, invoked by none** | the actionable list — one row per candidate, its weight, its evidence, and its opt-out switch |
| **Realized by your removals** | what a removal *actually* avoided on requests that were really sent |
| **Every declaration, by weight** | the full table, sortable on any column |
| **MCP servers** | the per-server rollup, because a server is the unit you add or remove |
| **Skills** | the listing's own weight, how many skills were declared, how many were ever called |
| **What these numbers are computed over** | the denominator: sessions in scope, how many were captured, how many were priced, and at which tier a token became a dollar |

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
