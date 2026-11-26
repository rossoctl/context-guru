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

## 2026-11-24 host-leak audit (tracking #1309)

Rows are leaks of business logic out of the host packages. All issues are closed, except #1441.

| # | Concern | Result |
|---|---|---|
| 1 | Two proxy and library results that differ (server-held history, replay prefix floor) | #1298, a decision |
| 2 | Tool-deferral flow in the proxy | #1299, PR #1325 |
| 3 | Compaction-point choice in the proxy | #1300, PR #1320 |
| 4 | Keep-alive policy resolution in the proxy | #1301, PRs #1328, #1338 |
| 5 | Campaign, replay and fidelity math in the proxy | #1302, PRs #1332, #1333, #1334 |
| 6 | The rest of the accountant glue in the proxy | #1303, PRs #1337, #1370, #1382 |
| 7 | Config-document editors in proxy and dash | #1304, PR #1336 |
| 8 | The steps around each request (Prepare and Finish) | #1305, PRs #1358, #1359 |
| 9 | In-band tool loop and SSE splicing | #1306, PR #1360 |
| 10 | Request and response fact parsing | #1307, PRs #1352, #1353 |
| 11 | Internal packages that Cortex needs | #1308, PR #1369 |
| 12 | Dashboard savings math in dash | #1440, PR #1462 |
| 13 | Tool-deferral savings figure, not from the accountant | #1441, open |

## How to add a run

After each run, add its findings below the table above, with the date of the run. Use the same
columns.
