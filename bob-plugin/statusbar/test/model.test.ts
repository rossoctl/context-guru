import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  BOB_WINDOW, DOWN_POLL_MS, formatClock, formatCtx, formatSaved, formatTokens, loopbackUrl,
  MENU, nextPollMs, parsePort, pickSession, remainingMs, render, renderView, Snapshot,
} from '../src/model'

const NOW = 1_800_000_000_000

function cache(extra: Record<string, unknown> = {}) {
  return { end_ts: NOW - 60_000, now_ms: NOW, ttl_s: 3600, prefix_tokens: 21_000, window: 270_000,
    priced: false, ...extra }
}

test('clock on the 1h tier shows 1h mm:ss', () => {
  assert.equal(formatClock(3_600_000, 3600), '1h 60:00')
  assert.equal(formatClock(3_250_000, 3600), '1h 54:10')
  assert.equal(formatClock(65_000, 3600), '1h 01:05')
})

test('clock on the 5m tier shows 5m m:ss', () => {
  assert.equal(formatClock(300_000, 300), '5m 5:00')
  assert.equal(formatClock(61_000, 300), '5m 1:01')
})

test('clock on a 300 s ttl shows 5m and on an 1800 s ttl shows 30m', () => {
  assert.equal(formatClock(299_000, 300), '5m 4:59')
  assert.equal(formatClock(1_800_000, 1800), '30m 30:00')
  assert.equal(formatClock(905_000, 1800), '30m 15:05')
})

test('clock at zero or below is cold', () => {
  assert.equal(formatClock(0, 3600), 'cold')
  assert.equal(formatClock(-5_000, 300), 'cold')
})

test('clock rounds a part second up, so it shows cold only at zero', () => {
  assert.equal(formatClock(500, 300), '5m 0:01')
})

test('remaining time uses the server clock and the local time since the poll', () => {
  // The cache was touched 60 s ago on the server, 10 s passed locally since the poll.
  assert.equal(remainingMs(cache(), 1000, 11_000), 3_600_000 - 60_000 - 10_000)
})

test('remaining time ignores a skewed local clock', () => {
  const skew = 7 * 3_600_000
  assert.equal(remainingMs(cache(), skew, skew + 10_000), 3_600_000 - 70_000)
})

test('tokens are short counts', () => {
  assert.equal(formatTokens(950), '950')
  assert.equal(formatTokens(3_100), '3.1k')
  assert.equal(formatTokens(21_000), '21k')
  assert.equal(formatTokens(270_000), '270k')
  assert.equal(formatTokens(1_200_000), '1.2M')
})

test('ctx shows the prefix over the window', () => {
  assert.equal(formatCtx(21_000, 270_000), 'ctx 21k/270k')
})

test('ctx falls back to Bob window when window is 0 or missing', () => {
  assert.equal(BOB_WINDOW, 270_000)
  assert.equal(formatCtx(21_000, 0), 'ctx 21k/270k')
  assert.equal(formatCtx(21_000, undefined), 'ctx 21k/270k')
})

test('saved shows tokens, 0 when nothing was saved, and — when there is no data', () => {
  assert.equal(formatSaved(3_100, cache()), 'saved 3.1k tok')
  assert.equal(formatSaved(0, cache()), 'saved 0')
  // No session row: the bar must not say "saved 0", which reads as "nothing saved".
  assert.equal(formatSaved(undefined, undefined), 'saved —')
})

test('saved never shows $ when priced is false, even with money present', () => {
  const money = { main: { saved_usd: 1.5, baseline_usd: 9, threads: 1 },
    others: { saved_usd: 0, baseline_usd: 0, threads: 0 } }
  const text = formatSaved(3_100, cache({ priced: false, money }))
  assert.equal(text, 'saved 3.1k tok')
  assert.doesNotMatch(text, /\$/)
})

test('saved shows money only when priced is true', () => {
  const money = { main: { saved_usd: 1.5, baseline_usd: 9, threads: 1 },
    others: { saved_usd: 0.25, baseline_usd: 1, threads: 1 } }
  assert.equal(formatSaved(3_100, cache({ priced: true, money })), 'saved 3.1k tok · $1.75')
})

test('saved shows $ as today when currency is USD or absent (an old proxy)', () => {
  const money = { main: { saved_usd: 1.5 }, others: { saved_usd: 0.25 } }
  assert.equal(formatSaved(3_100, cache({ priced: true, money, currency: 'USD' })),
    'saved 3.1k tok · $1.75')
  assert.equal(formatSaved(3_100, cache({ priced: true, money })), 'saved 3.1k tok · $1.75')
  // Dollars keep today's 2 decimals, also below 0.01.
  const small = { main: { saved_usd: 0.0042 } }
  assert.equal(formatSaved(3_100, cache({ priced: true, money: small, currency: 'USD' })),
    'saved 3.1k tok · $0.00')
})

test('saved shows a unit name after the number, with 2 decimals', () => {
  const money = { main: { saved_usd: 1.5 }, others: { saved_usd: 0.25 } }
  assert.equal(formatSaved(3_100, cache({ priced: true, money, currency: 'Bobcoins' })),
    'saved 3.1k tok · 1.75 Bobcoins')
})

test('saved shows a unit with 4 decimals below 0.01, and 0 for 0, as the dashboard does', () => {
  const small = { main: { saved_usd: 0.0042 } }
  assert.equal(formatSaved(3_100, cache({ priced: true, money: small, currency: 'Bobcoins' })),
    'saved 3.1k tok · 0.0042 Bobcoins')
  const none = { main: { saved_usd: 0 } }
  assert.equal(formatSaved(3_100, cache({ priced: true, money: none, currency: 'Bobcoins' })),
    'saved 3.1k tok · 0 Bobcoins')
})

test('saved trims the unit name', () => {
  const money = { main: { saved_usd: 2 } }
  assert.equal(formatSaved(3_100, cache({ priced: true, money, currency: '  Bobcoins \n' })),
    'saved 3.1k tok · 2.00 Bobcoins')
})

test('saved shows tokens only when currency is mixed', () => {
  const money = { main: { saved_usd: 1.5 }, others: { saved_usd: 0.25 } }
  assert.equal(formatSaved(3_100, cache({ priced: true, money, currency: 'mixed' })),
    'saved 3.1k tok')
})

test('saved shows tokens only for a unit name it cannot show safely', () => {
  const money = { main: { saved_usd: 1.5 } }
  for (const currency of ['', '   ', 'Bob coins', 'Bob$', 'coins·x', '$(alert)', 'Bobcoins2',
    'A'.repeat(17), 'Bobcöins', 42, null, { name: 'x' }]) {
    assert.equal(formatSaved(3_100, cache({ priced: true, money, currency })), 'saved 3.1k tok',
      `currency ${JSON.stringify(currency)}`)
  }
  assert.equal(formatSaved(3_100, cache({ priced: true, money, currency: 'A'.repeat(16) })),
    `saved 3.1k tok · 1.50 ${'A'.repeat(16)}`)
})

test('saved shows tokens only for currency Bobcoins when priced is not true', () => {
  const money = { main: { saved_usd: 1.5 } }
  assert.equal(formatSaved(3_100, cache({ money, currency: 'Bobcoins' })), 'saved 3.1k tok')
})

test('port comes from install.json, else 0', () => {
  assert.equal(parsePort('{"port": 8891, "complete": true}'), 8891)
  assert.equal(parsePort(undefined), 0)
  assert.equal(parsePort('not json'), 0)
  assert.equal(parsePort('{"port": "8891"}'), 0)
  assert.equal(parsePort('{"port": 70000}'), 0)
})

test('session pick takes the first row with a plain id', () => {
  assert.equal(pickSession({ sessions: [{ session_id: 'bob-1' }, { session_id: 'bob-0' }], total: 2 }),
    'bob-1')
  assert.equal(pickSession({ sessions: [], total: 0 }), undefined)
  assert.equal(pickSession({ sessions: [{ session_id: '' }] }), undefined)
  assert.equal(pickSession({ sessions: [{ session_id: 'a b/../x' }] }), undefined)
  assert.equal(pickSession(null), undefined)
})

test('loopback url refuses any host other than 127.0.0.1', () => {
  assert.equal(loopbackUrl('127.0.0.1', 8891, '/healthz'), 'http://127.0.0.1:8891/healthz')
  for (const host of ['localhost', '0.0.0.0', '10.0.0.1', 'example.com', '127.0.0.2', '::1']) {
    assert.throws(() => loopbackUrl(host, 8891, '/healthz'), /loopback/, host)
  }
  assert.throws(() => loopbackUrl('127.0.0.1', 0, '/healthz'), /port/)
})

test('back-off: 60 s when the proxy is down, the setting otherwise', () => {
  assert.equal(DOWN_POLL_MS, 60_000)
  assert.equal(nextPollMs('down', 15), 15_000 * 4)
  assert.equal(nextPollMs('ok', 15), 15_000)
  assert.equal(nextPollMs('ok', 30), 30_000)
  assert.equal(nextPollMs('not_installed', 15), 60_000)
  assert.equal(nextPollMs('ok', 1), 5_000, 'never faster than 5 s')
  assert.equal(nextPollMs('ok', Number.NaN), 15_000)
})

function snap(extra: Partial<Snapshot>): Snapshot {
  return { state: 'ok', port: 8891, polledAtLocal: 0, ...extra }
}

test('no install.json renders CG –', () => {
  const view = render(snap({ state: 'not_installed', port: 0 }), 0)
  assert.equal(view.text, 'CG –')
  assert.match(view.tooltip, /setup/)
})

test('a failed healthz renders CG ✗ with the fix', () => {
  const view = render(snap({ state: 'down' }), 0)
  assert.equal(view.text, 'CG ✗')
  assert.match(view.tooltip, /context-guru-reset/)
  assert.match(view.tooltip, /context-guru-doctor/)
})

test('no Bob session renders CG ✓ with —', () => {
  assert.equal(render(snap({}), 0).text, 'CG ✓ ctx —/270k · cache — · saved —')
})

test('a session with no cache touch renders cache —', () => {
  const view = render(snap({ session: 's1', saved: 0, cache: { none: true, now_ms: NOW } }), 0)
  assert.equal(view.text, 'CG ✓ ctx —/270k · cache — · saved 0')
})

test('a warm session renders ctx, clock and saved', () => {
  const view = render(snap({ session: 's1', saved: 0, cache: cache({ end_ts: NOW - 350_000 }) }),
    0)
  assert.equal(view.text, 'CG ✓ ctx 21k/270k · cache 1h 54:10 · saved 0')
})

test('the clock counts down locally between polls', () => {
  const view = render(snap({ session: 's1', cache: cache({ end_ts: NOW - 350_000 }) }), 10_000)
  assert.match(view.text, /cache 1h 54:00/)
})

test('an expired cache renders cold', () => {
  const view = render(snap({ session: 's1', saved: 0, cache: cache({ end_ts: NOW - 3_700_000 }) }), 0)
  assert.equal(view.text, 'CG ✓ ctx 21k/270k · cache cold · saved 0')
})

test('render never shows $ when priced is false', () => {
  const money = { main: { saved_usd: 4, baseline_usd: 9, threads: 1 },
    others: { saved_usd: 0, baseline_usd: 0, threads: 0 } }
  const view = render(snap({ session: 's1', saved: 500, cache: cache({ money }) }), 0)
  assert.equal(view.text, 'CG ✓ ctx 21k/270k · cache 1h 59:00 · saved 500 tok')
  assert.doesNotMatch(view.text + view.tooltip, /\$/)
})

test('render shows money with priced true and hides it with priced false', () => {
  const money = { main: { saved_usd: 2, baseline_usd: 9, threads: 1 },
    others: { saved_usd: 0.5, baseline_usd: 1, threads: 1 } }
  const priced = render(snap({ session: 's1', saved: 500, cache: cache({ priced: true, money }) }), 0)
  assert.equal(priced.text, 'CG ✓ ctx 21k/270k · cache 1h 59:00 · saved 500 tok · $2.50')
  const unpriced = render(snap({ session: 's1', saved: 500, cache: cache({ money }) }), 0)
  assert.equal(unpriced.text, 'CG ✓ ctx 21k/270k · cache 1h 59:00 · saved 500 tok')
})

test('render shows Bobcoins with no $ in the text or the tooltip', () => {
  const money = { main: { saved_usd: 2 }, others: { saved_usd: 0.5 } }
  const view = render(snap({ session: 's1', saved: 500,
    cache: cache({ priced: true, money, currency: 'Bobcoins' }) }), 0)
  assert.equal(view.text, 'CG ✓ ctx 21k/270k · cache 1h 59:00 · saved 500 tok · 2.50 Bobcoins')
  assert.doesNotMatch(view.text + view.tooltip, /\$|dollar/i)
})

test('no snapshot (a failed poll) renders CG ✗, not the old text', () => {
  const view = renderView(undefined, 0)
  assert.equal(view.text, 'CG ✗')
  assert.match(view.tooltip, /context-guru-doctor/)
})

test('a snapshot renders the same through renderView', () => {
  assert.deepEqual(renderView(snap({ state: 'not_installed', port: 0 }), 0),
    render(snap({ state: 'not_installed', port: 0 }), 0))
})

test('the menu has Status, Doctor, Open dashboard and Preset, and runs only read-only commands', () => {
  assert.deepEqual(MENU.map(item => item.label), ['Status', 'Doctor', 'Open dashboard', 'Preset'])
  const script = 'python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py"'
  assert.deepEqual(MENU.filter(item => item.command).map(item => item.command), [
    `${script} status`, `${script} doctor`, `${script} configure --show`])
  assert.equal(MENU.find(item => item.label === 'Open dashboard')?.dashboard, true)
})

test('the dashboard url is the loopback proxy', () => {
  assert.equal(loopbackUrl('127.0.0.1', 8891, '/dashboard/'), 'http://127.0.0.1:8891/dashboard/')
})
