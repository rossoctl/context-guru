# Already done

Do not report these again, unless a new copy of the same concern appeared.

## 2026-11-01 run

Rows are findings that were reported and got an issue or a PR. A row is not proof that the fix
merged. If a PR in this table closes without a merge, delete its row, so the next run reports that
duplicate again.

| # | Concern | Result |
|---|---|---|
| 1 | Providers that accept cache_control (Mantle was missing) | #809, PR #811 |
| 2 | Wire dialect decided in about 5 places | #817, PR #819 |
| 3 | tool_result text and string-or-blocks content | #802, PR #803 |
| 4 | Model family checks; none knew `fable` | #806, PR #807 |
| 5 | Keep-alive ping time defined twice (56 vs 59 min, 240 vs 280 s) | #808, PR #810; the values wait for a decision in #808 |
| 6 | Cache lifetime (5m / 1h / OpenAI 30m) in about 7 places | #814, PR #815 |
| 7 | Breakpoint cap and count | #812, PR #813 |
| 8 | Skills listing and deferred-tools parsing (and #788) | #821, PR #822 |
| 9 | Provider usage parsing | #816, PR #818 |
| 10 | Cache price multiples in about 8 places | #797, for discussion |
| 11 | The bytes/4 token estimate in 5 copies | #798, for discussion |
| 12 | Per-message digests outside the canonical form | #799, PR #800 (comments only) |
| - | Bifrost session id ignored metadata | #804, PR #805 |

## How to add a run

After each run, add its findings below the table above, with the date of the run. Use the same
columns.
