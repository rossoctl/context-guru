# User tasks and requirements

These tasks come from a question-and-answer session with the product owner. The
user is a developer who uses Claude Code and pays per token. They look at the band
between turns and rarely read it. The band has to answer their questions **at a glance**.

## 1. User tasks

| # | Task (what the user wants to know or do) | Priority |
|---|---|---|
| T1 | Know the cost of the next message: cheap (warm cache) or expensive (cold cache). | Glance |
| T2 | Know how long they can pause before the cache goes cold. | Glance |
| T3 | Keep the cache warm now with one click, before they leave or think. | Glance |
| T4 | Trust the automatic keep-alive: see that extensions are planned and that they fire. | Glance |
| T5 | See what ContextGuru saved. | Glance |
| T6 | Open the dashboard for the full detail. | Glance |
| T7 | See how full the context window is against its maximum. | Glance |
| T8 | Decide whether to compact now. | Verdict, reason on hover |
| T9 | Decide whether to use a sub-agent. | Verdict, reason on hover |
| T10 | Decide whether to start a new session. | Verdict, reason on hover |
| T11 | Tell a **planned** extension (potential, not paid) from an **activated** one (a ping that was sent and paid). | Glance |
| T12 | Understand that ContextGuru offers X planned 5-minute extensions. X can be configured. | Glance |

The product owner also wrote "etc." for the context decisions (T8 to T10). More
decisions may exist and are not listed.

## 2. Decisions about how to show tasks

These were agreed during the session.

| Decision | Detail |
|---|---|
| D1 | The cache state is shown by the bar itself. The words `warm` and `held` are removed. |
| D2 | There is no prose on the CACHE row. It has the bar, one time number and one link. |
| D3 | A planned extension is a **dashed** bar. An activated one is a **solid** bar. |
| D4 | X is the number of dashed bars. No extra text is needed. |
| D5 | A click on the link adds one more **planned** (dashed) bar on the right. |
| D6 | Bars are drawn to scale in time. Elapsed time stays visible as a dark part left of a `now` tick. |
| D7 | The `Ping` button becomes the link `more 5 mins ($0.01)`. |
| D8 | `hide` moves to the top-right corner. `dash ↗` moves to the SESSION row. |
| D9 | The rotating tip row is removed. |
| D10 | Details (times per bar, `cold at`) appear in a hover card on the bar. |
| D11 | Context decisions (T8 to T10) show a short verdict, with the reason in the hover card. |

## 3. Requirements

### Functional

| # | Requirement | Task |
|---|---|---|
| R1 | The band shows the warm and the cold price of the next message. | T1 |
| R2 | The band shows the time until the cache goes cold as one number, at the end of the bar. | T2 |
| R3 | The bar shows the built-in TTL, then each extension as a block, to scale. | T2, T4, T12 |
| R4 | A planned extension is dashed. An activated extension is solid. | T11 |
| R5 | When the built-in TTL is about to expire, the next extension turns solid and is outlined. | T4, T11 |
| R6 | One click on the link adds one planned (dashed) extension on the right. Whether the click also sends a ping at once is open (Q8). | T3, D5 |
| R7 | The link shows the cost of one extension, in the unit the user pays in: dollars with an API key, tokens on a subscription. | T3 |
| R8 | The session saving and the cost without ContextGuru are shown with a percentage. On a subscription the band keeps its 5-hour window figures. | T5 |
| R9 | A link opens the dashboard. | T6 |
| R10 | Context size relative to the window maximum is shown. | T7 |
| R11 | A one-word verdict for compact, sub-agent and new session is shown near `ctx`. | T8–T10 |
| R12 | A hover card on the bar lists each block: kind, time to start, time added, and `cold at`. | T2, T4, T12 |
| R13 | A hover card on the verdict gives the reason and the numbers behind it. | T8–T10 |
| R14 | Sub-agent caches stay visible when a named sub-agent exists. | T9 |
| R15 | In the cold state the link is removed, and the elapsed time since going cold is shown. | T1, T2 |

### Non-functional

| # | Requirement |
|---|---|
| N1 | The band must stay readable in about 60 columns, as the current band does for row 1. |
| N2 | The band must be useful as text in a terminal: block characters must carry the bar. |
| N3 | Meaning must not rest on color alone. Dashed versus solid carries planned versus activated. |
| N4 | The band must not promise more than the data supports. A forecast must look like a forecast. |
| N5 | The band must have no more rows than today (3, or 4 with a sub-agent). |
| N6 | The hover card must not move other rows. |
| N7 | Text that can be replaced by a shape is replaced. |

## 4. Open questions

| # | Question | Why it matters |
|---|---|---|
| Q1 | What does a manual click add to the plan, and does it count against X? | Decides the length of the added bar and the total (the mockup shows `≈15:10`). |
| Q2 | Where is X configured, and is the plan's total in the cache document? | Needed to draw all dashed bars from real data. |
| Q3 | What rules produce the context verdict (compact, sub-agent, new session)? | The band cannot show a verdict without a rule. The data may not have the inputs. |
| Q4 | What does one ping cost? | The link shows `$0.01`, which is the warm price, not a measured ping cost. |
| Q5 | Does the VS Code surface support hover on the band? | The hover card in the mockup depends on it. |
| Q6 | Should the hover card show the cost of the whole plan if every extension is activated? | A user may want the worst case. |
| Q7 | Should the names in the hover card and the legend ("extension") change to "more time", to match the link? | Wording consistency. |
| Q8 | Does a click send a ping at once (a paid, solid bar), or only plan one (a dashed bar)? | D5 says planned. The link shows a price, which suggests a payment. |
| Q9 | What does the link show on a subscription, where a ping costs limit and not dollars? | `nextPrice()` in 0.7.2 shows token counts (`rd`, `wr`). Anthropic does not publish how the 5-hour limit weights them, so a dollar price would be wrong there. |
| Q10 | Does the proposal replace the `↻ next ping` text of 0.7.2, or keep it beside the bars? | The bars show the same fact. Keeping both repeats it. |
