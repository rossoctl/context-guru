# Design decisions and alternatives

This records how the proposal reached its current shape, and what was tried and
rejected. Each step lists the diagram that shows it.

## 1. The six states in the proposal

See [the proposal diagram](assets/04-band-proposed.svg).

| State | What it shows | Tasks |
|---|---|---|
| A. Now | Built-in TTL (green), two planned extensions (dashed), total `10:10`, 0:25 elapsed | T1, T2, T4, T12 |
| B. You click the link | A third dashed bar is added on the right; the total reads `≈15:10` | T3, T11 |
| C. At 4:30 the next block is activated | The built-in block has about 30 s left; the next block is solid and outlined; total `10:30` | T4, T11 |
| D. After the first extension was sent | The built-in block is dark (spent); the sent extension burns; one planned extension remains | T4, T11 |
| E. Hover on a bar | A card lists each block with start time and time added | T2, T4, T12 |
| F. Cold | A dark bar, `7:06 ⏳`, no link, one price (`$0.29 next`) | T1, T2 |

Panels B and C use the product owner's description of how clicks and activation
behave. They were not checked against the plugin source. In C the three blocks are
drawn as equal 5-minute thirds.

## 2. How the shape of the visualization was chosen

| Step | Idea | Outcome | Diagram |
|---|---|---|---|
| 1 | Describe the current band | The clock shows a forecast (analysis, section 5) | [as-is](assets/01-band-as-is.svg) |
| 2 | Text layout with an `AUTO` row and a manual button | Replaced; it added a row and used words | not kept |
| 3 | TTL as **rings**: the built-in TTL first, one ring per ping, a click adds a ring | **Rejected.** Rings are not drawn to scale and do not fit a text band | [rings](assets/03-ttl-as-rings-rejected.svg) |
| 4 | TTL as **bars to scale**: time burns left to right, a click adds a bar on the right | Kept as the base idea. A bar fits text (`█▒▓`) | [bars](assets/02-ttl-as-bars-first.svg) |
| 5 | Planned and sent bars drawn the same | Reversed in step 6 | not kept |
| 6 | Planned = dashed, sent = solid, elapsed time stays as a dark part | Kept. It answers T11 without words | [proposal](assets/04-band-proposed.svg) |
| 7 | Remove state words, the first-line text and the tip row | Kept. The band keeps 3 rows (4 with a sub-agent) | [proposal](assets/04-band-proposed.svg) |
| 8 | The button becomes a link with the price; `hide` to the corner; `dash` to SESSION | Kept | [proposal](assets/04-band-proposed.svg) |
| 9 | A hover card on the bar | Kept for detail. Needs surface support (Q5) | [proposal](assets/04-band-proposed.svg), panel E |

## 3. Why bars instead of words or rings

1. **Time is to scale.** Bars have a length. The sum of the blocks equals the number at
   the end.
2. **One shape carries state.** Dark, green, dashed, solid and outlined replace the
   words `warm`, `held`, `expiring`, `cold`.
3. **It works as text.** A band cell can hold `█` and `▒`. A ring cannot.
4. **It makes the forecast visible.** A dashed bar shows plainly that time is promised
   but not paid (problem P1).

## 4. Known weaknesses of the proposal

| # | Weakness | Possible fix |
|---|---|---|
| W1 | A very short bar (a few seconds left) is almost invisible; in C the outline hides the green block. | Set a minimum bar width in the plugin. |
| W2 | Several planned blocks in a row merge into one dashed line. | Add a short gap between blocks. |
| W3 | Long chains (many clicks) make the bar too wide for narrow mode. | Cap the chain and show `+N more`, or change the scale. |
| W4 | The mockup uses 5 px lines. A terminal band can use only block characters, and the thinnest is `▁` or `▔`. | Test a text-only version. |
| W5 | The hover card covers the NEXT and SESSION rows while it is open. | It is an overlay that does not move rows; accepted. |
| W6 | A cold band shows only an elapsed bar and a number. | Accepted; the bar and the ⏳ carry the state. |
| W7 | Panel titles, the hover card and the legend say "extension"; the link says "more 5 mins". | Q7 in the task document. |

## 5. Implementation notes

- The band is drawn by
  [`context-guru-plugin/hooks/register.tsx`](../../../context-guru-plugin/hooks/register.tsx),
  with helpers in [`model.ts`](../../../context-guru-plugin/hooks/model.ts) and tests in
  `register.test.tsx`. The plugin version is in `context-guru-plugin/.claude-plugin/plugin.json`.
- The bar, the link, the hover card and the layout need changes in both files. The
  context verdict needs new rules and possibly new data from `cg.py`.
- `model.ts` already has `nextPingIn()`, `showPing()` and `nextPrice()`. The bars can
  build on them.
- A hover card uses the mod API's hover-reveal on a keyed box with an absolutely
  positioned child. The API says such a child is clipped by the band's region, so a
  card that opens upward may be cut off.
- Rules of this repo that a change would meet:
  - It changes what users see. `CLAUDE.md` asks for a human developer's permission and a
    note at the top of the PR for a change in behavior. Say who it affects and how to turn
    it off.
  - The band reads metadata only (counts, times, prices). A change must keep it so, and
    state its data dependence in `docs/data-dependence.md` if it adds a source.
  - The internal build (`/cg:*`) has its own copy of the band. A change here needs a port.

## 6. Next steps

1. Answer the open questions in the task document (Q1 to Q10).
2. Define the context verdict rules and check that the data supports them.
3. Test the hover card on the VS Code surface with a small mod.
4. Test a text-only band with block characters (W4).
5. Write a spec, then an implementation plan, before any code.
