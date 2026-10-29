import { expect, mock, test } from 'claude-code/testing'

import { REASON_CODES, signature, usd, view } from './model'

const NOW = 1_800_000_000_000
const entry = (end: number, extra = {}) => ({ end_ts: end, ttl_s: 300, prefix_tokens: 90000, rewrite_usd: 0.5, next_warm_usd: 0.06, next_cold_usd: 0.78, ...extra })
const doc = (warm: boolean) => ({
  ...entry(warm ? NOW - 60_000 : NOW - 900_000),
  last_ts: warm ? NOW - 60_000 : NOW - 900_000,
  now_ms: NOW,
  age_s: 0,
  stale: false,
  money: { main: { saved_usd: 18.6, baseline_usd: 61.4 }, others: { saved_usd: 4.2, baseline_usd: 20.5 } },
  threads: [
    { id: 'main', main: true, ...entry(warm ? NOW - 60_000 : NOW - 900_000) },
    { id: 'a:explorer1', name: 'Explore', main: false, saved_usd: 1.2, ...entry(NOW - 30_000), ...agentExtra },
    { id: 'a:reviewer', name: 'code-reviewer', main: false, ...entry(NOW - 250_000) }, // last minute
    { id: 'a:planner', name: 'planner', main: false, ...entry(NOW - 900_000) }, // cold, 15 min ago: shown dim
    { id: 'a:ancient', name: 'Ancient', main: false, ...entry(NOW - 3 * 3_600_000) }, // cold for hours: gone
    { id: 'a:unnamed', main: false, ...entry(NOW - 30_000) }, // no name: shown as nothing, never as a hex id
    { id: 'p:classifier', name: 'Classifier', main: false, ...entry(NOW - 30_000) }, // a side request, not an agent
  ],
})

const BAND = {
  plugin: 'context-guru',
  component: 'AbovePrompt',
  props: { hasSurvey: false, isWorking: false, maxRows: 6, bodyColumns: 100, scroll: { offset: 0, bodyRows: 6 } },
} as const

const PANE = {
  plugin: 'context-guru',
  component: 'Pane',
  requestId: 'context-guru',
  props: { title: 'Context Guru', isFocused: false, bodyColumns: 60, placement: 'dock', scroll: { offset: 0, bodyRows: 40 }, view: {} },
} as const

const PANE_DATA = {
  spark: { today: 4.2, days: [1, 2, 2, 3, 4, 4, 5, 5, 6, 7, 7, 8, 8, 4.2] },
  strip: { kinds: ['hit', 'hit', 'hit', 'hit', 'miss', 'hit', 'hit', 'part', 'part', 'hit', 'hit', 'hit', 'hit', 'hit', 'hit', 'hit', 'hit', 'hit', 'hit', 'hit'], lost: 1.4 },
  heat: { hours: [0, 0, 1, 2, 0, 0, 6, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0], now: 8, lost: 1.4 },
  fill: { used: 412000, window: 1000000, frac: 0.412, base: 0.2, eta_s: 3480, miss_usd: 1.2 },
}

let clock: ReturnType<typeof mock.clock>
let tipStale: string[] = []
let paneDoc: object = PANE_DATA
let agentExtra: object = {}
let noAgents = false
let toasts: string[] = []
let docOverride: ((d: any) => object) | null = null // replaces the cachestate answer for one test
let usage: object | null = null // what $.session.usage() answers, when a test sets it

/** The drawn tree as the lines a person reads: a column Box is one line per child, anything else is one line. */
function textOf(n: any, gap = 0): string {
  if (n == null || typeof n === 'boolean') return ''
  if (typeof n === 'string' || typeof n === 'number') return String(n)
  if (Array.isArray(n)) return n.map(c => textOf(c)).join(' '.repeat(gap))
  const kids = textOf(n.children, n.type === 'Box' ? (n.props?.columnGap ?? 0) : 0)
  if (n.type === 'Button') return n.props?.plain ? (n.props.label ?? kids) : `[ ${n.props?.label ?? kids} ]`
  if (n.type === 'Link') return n.props?.label ?? kids
  return kids
}
function lines(n: any): string[] {
  if (n == null || typeof n !== 'object') return []
  if (n.type === 'Box' && n.props?.flexDirection === 'column') return (n.children ?? []).flatMap(lines)
  const t = textOf(n, n.type === 'Box' ? (n.props?.columnGap ?? 0) : 0)
  return t.trim() === '' ? [] : [t.replace(/\s+$/, '')]
}
const dump = async (ui: any): Promise<string> => lines(await ui.drawn()).join('\n')

async function boot($: any, on: any, warm: boolean, calls: string[][] = [], pingOut: object = { status: 200, sent: true, saved_usd: 0.46, ping_usd: 0.04 }, mainExtra: object = {}) {
  registered = []
  on('command.register', (_: any, e: any) => {
    registered.push(e.name)
    return { value: { command: 'context-guru' } }
  })
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('session.id', () => ({ value: 'sess-1' }))
  if (usage) on('session.usage', () => ({ value: usage }))
  clock = mock.clock(on, { now: NOW })
  on('env.get', () => ({ value: 'http://127.0.0.1:8787/anthropic' }))
  on('session.start', (_: any, e: any) => ({ cwd: e.cwd }))
  on('ui.toast', (_: any, e: any) => {
    toasts.push(e.text)
    return { value: undefined }
  })
  on('ui.status', () => {
    statusCalls++
    return { value: undefined }
  })
  on('ui.render', ($: any, e: any) => {
    const { Text } = $.ui.resolve(e)
    return <Text>engine</Text>
  })
  on('process.run', (_: any, e: any) => {
    calls.push([...e.argv])
    const ret = (o: object) => ({ value: { exitCode: 0, stdout: JSON.stringify(o) + '\n', stderr: '' } })
    if (e.argv.includes('tip')) return ret({ stale: tipStale, days: 7 })
    if (e.argv.includes('panedata')) return ret(paneDoc)
    if (e.argv.includes('ping')) return ret(pingOut ?? { status: 200, sent: true, saved_usd: 0.46, ping_usd: 0.04 })
    if (docOverride) {
      const o: any = docOverride(doc(warm))
      return { value: { exitCode: o.error ? 1 : 0, stdout: JSON.stringify(o) + '\n', stderr: '' } }
    }
    const base = doc(warm)
    if (noAgents) base.threads = base.threads.filter((t: any) => t.main)
    return ret({ ...base, ...mainExtra, threads: base.threads.map((t: any) => (t.main ? { ...t, ...mainExtra } : t)) })
  })
  statusCalls = 0
  toasts = []
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000) // the first poll lands
}
let statusCalls = 0
let registered: string[] = []

/** Presses the band's Ping button and returns the toast it raised. */
async function pressPing($: any): Promise<{ text: string }> {
  toasts = []
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  await ui.press({ key: 'ping' })
  await ui.unmount()
  return { text: toasts.join('\n') }
}

for (const surface of ['terminal', 'desktop'] as const) {
  test(`${surface}: warm band shows the next-message price warm and cold, Ping and the agent chips`, async ($, on) => {
    await boot($, on, true)
    const ui = await $.ui.mount({ ...BAND, surface })
    expect(await ui.find({ type: 'Text', text: /next message/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\$0\.06/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\$0\.78 if cold/ })).toBeDefined()
    expect(await ui.find({ key: 'ping' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /Explore/ })).toBeDefined()
    const link = await ui.find({ type: 'Link' }) // the dashboard link is on row 1, last, and in the pane
    expect((link as any)?.props?.href ?? (link as any)?.href).toBe('http://localhost:8787/dashboard/')
    await ui.unmount()
  })

  test(`${surface}: the band carries the status line's row: saved of session, subagents, the cache clock and ctx`, async ($, on) => {
    usage = { startedAt: NOW, context: { tokens: 90000, window: 1_000_000, percent: 9 }, rateLimits: [] }
    docOverride = d => ({ ...d, money: { ...d.money, others: { ...d.money.others, threads: 3 } } })
    await boot($, on, true)
    usage = docOverride = null
    const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface })
    const row = (await dump(ui)).split('\n')[1]
    expect(row).toBe('saved $18.60 of $61.40 session · saved $4.20 of $20.50 subagents │ cache ● █████░ 3:59 │ ctx █░░░░░░░ 9%')
    expect(await dump(ui)).not.toMatch(/Context Guru/)
    expect(statusCalls).toBe(0) // no $.ui.status copy: the band is the one home
    await ui.unmount()
  })

  test(`${surface}: agent chips are named user agents only, most recent first, cold ones dimmed and never a p: thread or a hex id`, async ($, on) => {
    await boot($, on, true)
    const ui = await $.ui.mount({ ...BAND, surface })
    for (const gone of [/Classifier/, /Ancient/, /unnamed/, /a:/]) {
      expect(await ui.find({ type: 'Text', text: gone })).toBeUndefined()
    }
    const all = await dump(ui)
    expect(all).toMatch(/└ agents ?\s*● Explore 5m\s+◐ code-reviewer <1m\s+○ planner/)
    const cold = await ui.find({ type: 'Text', text: /planner/ })
    expect((cold as any)?.props?.dimColor ?? true).toBe(true)
    await ui.unmount()
  })

  test(`${surface}: a cold main cache is drawn, dim, with the cold price and no Ping`, async ($, on) => {
    await boot($, on, false)
    const ui = await $.ui.mount({ ...BAND, surface })
    const all = await dump(ui)
    expect(all).toMatch(/○ cache cold · next message \$0\.78/)
    expect(await ui.find({ key: 'ping' })).toBeUndefined()
    expect(all).not.toMatch(/\$0\.06/) // the warm price no longer applies
    await ui.unmount()
  })
}

test('the band is one row when the band is short, and row 1 stays', async ($, on) => {
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, maxRows: 2 }, surface: 'terminal' })
  expect(((await dump(ui)).match(/\n/g) ?? []).length).toBe(0) // maxRows 2: row 1 only
  expect(await dump(ui)).toMatch(/next message/)
  await ui.unmount()
})

test('the band is never more than four rows, even with agents and a stale tip', async ($, on) => {
  tipStale = ['save-more', 'remove-mcp-and-skills']
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, maxRows: 50 }, surface: 'terminal' })
  expect(((await dump(ui)).match(/\n/g) ?? []).length).toBeLessThanOrEqual(3)
  await ui.unmount()
  tipStale = []
})

test('no weekly tip while every command ran this week: the last row rotates through the commands instead', async ($, on) => {
  tipStale = []
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  const last = async () => (await dump(ui)).split('\n').pop()
  expect(await dump(ui)).not.toMatch(/tip:/)
  const seen = new Set<string>()
  for (let i = 0; i < 3; i++) {
    seen.add((await last())!)
    await clock.advance(12_000)
  }
  expect([...seen].sort()).toEqual([
    'try /context-guru:insights — see where your money goes',
    'try /context-guru:remove-mcp-and-skills — drop unused MCP and skills',
    'try /context-guru:save-more — find a cheaper configuration',
  ])
  for (const l of seen) expect(l.replace(/^try \S+ — /, '').split(' ').length).toBeLessThanOrEqual(5) // 4-5 words
  await ui.unmount()
})

test('a saving below zero (pings and requests not yet earned back) reads $0.00, never a minus', async ($, on) => {
  docOverride = d => ({ ...d, money: { main: { saved_usd: -0.04, baseline_usd: 0.37 }, others: { saved_usd: -0.01, baseline_usd: 0.1, threads: 1 } } })
  await boot($, on, true)
  docOverride = null
  const band = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface: 'terminal' })
  expect(await dump(band)).toMatch(/saved \$0\.00 of \$0\.37 session/)
  expect(await dump(band)).not.toMatch(/-\$/)
  await band.unmount()
  await $.command.run({ command: 'context-guru', args: '' })
  const pane = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await dump(pane)).not.toMatch(/-\$/)
  await pane.unmount()
})

test('the cache clock counts each second like the status line did, and a cold cache counts up dim', async ($, on) => {
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface: 'terminal' })
  const row = async () => (await dump(ui)).split('\n')[1]
  expect(await row()).toMatch(/cache ● █████░ 3:59/)
  await clock.advance(1000)
  expect(await row()).toMatch(/cache ● █████░ 3:58/)
  await ui.unmount()
})

test('a cold main cache shows `cache ○ cold m:ss`, counting up', async ($, on) => {
  await boot($, on, false)
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface: 'terminal' })
  expect((await dump(ui)).split('\n')[1]).toMatch(/cache ○ cold 10:01/) // ended 15 min ago, TTL 5 min, one second in
  await ui.unmount()
})

test('the manual pings figure follows the money: `↻ $0.27 pings` once there is a ping', async ($, on) => {
  docOverride = d => ({ ...d, money: { ...d.money, others: { ...d.money.others, threads: 3 } }, manual_pings: { count: 2, ping_usd: 0.08, saved_usd: 0.27 } })
  await boot($, on, true)
  docOverride = null
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 160 }, surface: 'terminal' })
  expect((await dump(ui)).split('\n')[1]).toMatch(/subagents ↻ \$0\.27 pings │ cache/)
  await ui.unmount()
})

test('at 60 columns the status row drops the subagents phrase, the label and the bars, never the figures', async ($, on) => {
  usage = { startedAt: NOW, context: { tokens: 90000, window: 1_000_000, percent: 9 }, rateLimits: [] }
  await boot($, on, true)
  usage = null
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 60 }, surface: 'terminal' })
  const row = (await dump(ui)).split('\n')[1]
  expect(row.length).toBeLessThanOrEqual(56)
  expect(row).toMatch(/^saved \$18\.60 of \$61\.40 │ cache ● 3:5\d │ ctx 9%$/)
  await ui.unmount()
})

test('nothing ever read, and the proxy is down: the band says so, like the status line did', async ($, on) => {
  docOverride = () => ({ error: 'no cache state for this session yet', notice: 'cg ✕ token refused · /context-guru:status' })
  await boot($, on, true)
  docOverride = null
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await dump(ui)).toBe('cg ✕ token refused · /context-guru:status')
  await ui.unmount()
})

test('a release the user has not declined shows `▲ update` first on the status row, as on the status line', async ($, on) => {
  docOverride = d => ({ ...d, update: '▲ update v0.6.1' })
  await boot($, on, true)
  docOverride = null
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface: 'terminal' })
  expect((await dump(ui)).split('\n')[1]).toMatch(/^▲ update v0\.6\.1 │ saved \$18\.60 of \$61\.40 session/)
  await ui.unmount()
})

test('on a Claude subscription the money gives way to the 5-hour limit and the tokens saved in it', async ($, on) => {
  usage = { startedAt: NOW, context: { tokens: 90000, window: 1_000_000, percent: 9 }, rateLimits: [{ kind: 'five_hour', percentUsed: 34, resetsAt: new Date(NOW + 3_600_000).toISOString() }] }
  const calls: string[][] = []
  docOverride = d => ({ ...d, window_saved: 'saved 1.2M rd / 85.0k wr' })
  await boot($, on, true, calls)
  usage = docOverride = null
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface: 'terminal' })
  const row = (await dump(ui)).split('\n')[1]
  expect(row).toMatch(/^5h 34% · saved 1\.2M rd \/ 85\.0k wr │ cache ● /)
  expect(row).not.toMatch(/\$/)
  // the window's start (its reset less five hours) reaches the helper, which reads that window's totals
  expect(calls.some(c => c.join(' ').includes(`--window-start-ms ${NOW + 3_600_000 - 5 * 3_600_000}`))).toBe(true)
  await ui.unmount()
})

test('the weekly tip shows when a command is stale, never rotates, and needs room for a third row', async ($, on) => {
  tipStale = ['save-more']
  await boot($, on, true)
  let ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await dump(ui)).toMatch(/tip: \/context-guru:save-more hasn't run in 7\+ days — finds a cheaper config/)
  await clock.advance(5 * 60_000)
  expect(await dump(ui)).toMatch(/tip: \/context-guru:save-more hasn't run in 7\+ days/) // the same words minutes later
  expect(await dump(ui)).not.toMatch(/insights|remove-mcp/)
  await ui.unmount()
  ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, maxRows: 3 }, surface: 'terminal' })
  expect(await dump(ui)).not.toMatch(/tip:/)
  await ui.unmount()
  tipStale = []
})

test('the agents row can be turned off from the pane, and stays off', async ($, on) => {
  await boot($, on, true)
  const pane = await $.ui.mount({ ...PANE, surface: 'terminal' })
  await pane.press({ key: 'agents' })
  await pane.unmount()
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await dump(ui)).not.toMatch(/agents/)
  expect(await dump(ui)).toMatch(/next message/) // row 1 stays
  await ui.unmount()
})

test('the pane has the dashboard link http://localhost:PORT/dashboard/ on its own line under the title', async ($, on) => {
  await boot($, on, true)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const link = await ui.find({ type: 'Link' })
  expect((link as any)?.props?.href ?? (link as any)?.href).toBe('http://localhost:8787/dashboard/')
  expect((await dump(ui)).split('\n').slice(0, 3)).toEqual(['◆ Context Guru', 'open dashboard ↗', 'saved $22.80 of $81.90 this session'])
  await ui.unmount()
})

test('a pinged agent chip carries the cycle arrow', async ($, on) => {
  agentExtra = { keepalive: true }
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await dump(ui)).toMatch(/● Explore 5m ↻/)
  const chip = await ui.find({ type: 'Text', text: /Explore/ })
  expect((chip as any)?.props?.color).toBe('cyan')
  await ui.unmount()
  agentExtra = {}
})

test('no data: the band is left to the engine', async ($, on) => {
  on('ui.render', () => ({ type: 'Text', props: {}, children: ['engine'] }) as any)
  on('process.run', () => ({ value: { exitCode: 1, stdout: 'nope', stderr: '' } }))
  on('session.id', () => ({ value: 'sess-1' }))
  on('command.register', () => ({ value: { command: 'context-guru' } }))
  on('session.start', (_: any, e: any) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeUndefined()
  await ui.unmount()
})

test('hide is reversible: the pane command, or the pane button, brings the band back', async ($, on) => {
  await boot($, on, true)
  let ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  await ui.press({ key: 'hide' })
  expect(await ui.find({ key: 'ping' })).toBeUndefined()
  await ui.unmount()
  await $.command.run({ command: 'context-guru', args: '' })
  ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeDefined()
  await ui.press({ key: 'hide' })
  await ui.unmount()
  const pane = await $.ui.mount({ ...PANE, surface: 'terminal' })
  await pane.press({ key: 'show' })
  await pane.unmount()
  ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeDefined()
  await ui.unmount()
})

test('pressing Ping runs the helper with the session id and toasts the saving', async ($, on) => {
  const calls: string[][] = []
  await boot($, on, true, calls)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  await ui.press({ key: 'ping' })
  const ping = calls.find(c => c.includes('ping'))
  expect(ping).toBeDefined()
  expect(ping).toContain('sess-1')
  expect(toasts.join('\n')).toMatch(/kept warm .* saved \$0\.46 net \(ping cost \$0\.04\)/)
  await ui.unmount()
})

test('the /ping slash command is gone: only /context-guru is registered', async ($, on) => {
  await boot($, on, true)
  expect(registered).toEqual(['context-guru'])
})

test('a server without the ping route is not blamed on the session', async ($, on) => {
  // The helper reports Go's plain-text 404 as a status with no error field.
  await boot($, on, true, [], { status: 404 })
  const r = await pressPing($)
  expect(r.text).toMatch(/does not support manual pings yet/)
  expect(r.text).not.toMatch(/not known/)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  await ui.press({ key: 'ping' })
  expect(toasts.join('\n')).toMatch(/does not support manual pings yet/)
  await ui.unmount()
})

test('warm but not pingable: no Ping button, a dim `ping off: <reason>` in its place', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pingable: false, ping_reason: 'too large' })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeUndefined()
  const all = await dump(ui)
  expect(all).toMatch(/ping off: context too large/)
  for (const line of all.split('\n')) {
    const m = /ping off: (.*?)(?:\s{2,}|$)/.exec(line)
    if (m) expect(m[1].length).toBeLessThanOrEqual(40)
  }
  await ui.unmount()
})

for (const [reason, want] of [
  ['not opted in', /ping off: on next session/],
  ['too large', /ping off: context too large/],
  ['pings used', /ping off: pings used up/],
  ['some-future-code', /^[^p]*$|^(?!.*ping off)/s],
  ['no entry', /^(?!.*ping off)/s],
] as const) {
  test(`ping off, reason ${reason}: short words (17 characters at most) or nothing, never a raw code`, async ($, on) => {
    await boot($, on, true, [], undefined as any, { pingable: false, ping_reason: reason })
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    const all = await dump(ui)
    expect(all).toMatch(want)
    expect(all).not.toMatch(/some-future-code/)
    const shown = /ping off: (.*?)(?:\s{2,}|$)/.exec(all)?.[1] ?? ''
    expect(shown.length).toBeLessThanOrEqual(17)
    await ui.unmount()
  })
}

test('a server that does not say (no pingable field) still shows the Ping button', async ($, on) => {
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeDefined()
  expect(await dump(ui)).not.toMatch(/ping off/)
  await ui.unmount()
})

for (const [status, error, want] of [
  [409, 'not held', /keep-alive is off/],
  [409, 'busy', /request is running/],
  [409, 'too costly', /per-ping limit/],
  [409, 'something new', /ping refused: something new/],
  [409, 'over budget', /per-ping limit, so nothing was sent/],
  [409, 'cold', /already cold/],
  [429, 'no spare capacity', /no spare capacity/],
  [502, 'upstream', /upstream model refused/],
] as const) {
  test(`a press says the right thing for ${status} ${error}`, async ($, on) => {
    await boot($, on, true, [], { status, error })
    const r = await pressPing($)
    expect(r.text).toMatch(want)
  })
}

test('a refused press explains itself with the server\'s ping_reason', async ($, on) => {
  await boot($, on, true, [], { status: 409, error: 'not held', ping_reason: 'too large' })
  const r = await pressPing($)
  expect(r.text).toMatch(/can't ping: context too large to hold for Ping/)
})

for (const [reason, want] of [
  ['not opted in', /Ping isn't enabled for this install yet/],
  ['below limits', /below its minimum/],
  ['pings used', /pings have run out/],
  ['stopped', /stopped keeping this thread warm/],
  ['no entry', /knows nothing about this session/],
  ['unavailable', /route or credential is failing right now . try again shortly/],
  ['over budget', /per-ping limit/],
  ['too large', /too large to hold/],
  ['unsupported', /Ping cannot hold this session \(extended thinking, or a route Ping does not support\)/],
  ['expired', /already cold/],
  ['some-future-code', /cannot ping this session right now/],
] as const) {
  test(`a refused press words ping_reason ${reason}`, async ($, on) => {
    await boot($, on, true, [], { status: 409, error: 'not held', ping_reason: reason })
    const r = await pressPing($)
    expect(r.text).toMatch(want)
    expect(r.text).not.toMatch(/some-future-code/)
    expect(r.text).not.toMatch(/restart Claude Code/)
  })
}

// ---- the pane ---------------------------------------------------------------------------------------------------

test('the pane has every section, in order, when the data is there', async ($, on) => {
  tipStale = ['save-more']
  paneDoc = PANE_DATA
  await boot($, on, true)
  await $.command.run({ command: 'context-guru', args: '' })
  await clock.advance(100)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const all = await dump(ui)
  console.log('--- PANE (warm, 60 cols) ---\n' + all)
  const order = ['◆ Context Guru', 'saved $22.80 of $81.90 this session', 'NEXT MESSAGE', 'AGENTS', 'SAVED LATELY', 'CACHE HITS', 'MISSES BY HOUR', 'CONTEXT', 'SAVE MORE']
  let at = -1
  for (const h of order) {
    const i = all.indexOf(h)
    expect(i, `${h} is missing`).toBeGreaterThan(at)
    at = i
  }
  expect(all).toMatch(/if the cache is warm\s+\$0\.06/)
  expect(all).toMatch(/if it is cold\s+\$0\.78/)
  expect(all).toMatch(/\$4\.20 today · last 14 days/)
  expect(all).toMatch(/\$1\.40 lost today/)
  expect(all).toMatch(/^ ?\$4\.20 today/m)
  expect(all).toMatch(/41%/)
  expect(all).toMatch(/tip .*save-more hasn't run in 7\+ days/)
  expect(all).not.toMatch(/gauge|█{6}.*left/) // no cache clock in the pane either
  await ui.unmount()
  tipStale = []
})

test('the pane omits a section whose data is missing, and shows no empty header', async ($, on) => {
  paneDoc = { spark: PANE_DATA.spark }
  await boot($, on, true)
  await $.command.run({ command: 'context-guru', args: '' })
  await clock.advance(100)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const all = await dump(ui)
  expect(all).toMatch(/SAVED LATELY/)
  for (const gone of ['CACHE HITS', 'MISSES BY HOUR', 'CONTEXT']) expect(all).not.toMatch(new RegExp(gone))
  await ui.unmount()
  paneDoc = PANE_DATA
})

test('the pane omits AGENTS when there are none, and no tip line when no command is stale', async ($, on) => {
  tipStale = []
  noAgents = true
  await boot($, on, true)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const all = await dump(ui)
  expect(all).not.toMatch(/AGENTS/)
  expect(all).not.toMatch(/\btip /)
  expect(all).toMatch(/SAVE MORE/)
  await ui.unmount()
  noAgents = false
})

for (const cols of [140, 80, 60]) {
  test(`render: pane and band at ${cols} columns; no "$x lost today" is split and the band is one row`, async ($, on) => {
    tipStale = ['save-more']
    await boot($, on, true)
    await $.command.run({ command: 'context-guru', args: '' })
    await clock.advance(100)
    const pane = await $.ui.mount({ ...PANE, props: { ...PANE.props, bodyColumns: cols }, surface: 'terminal' })
    const all = await dump(pane)
    console.log(`--- PANE ${cols} columns ---\n` + all)
    if (cols < 70) expect(all).toMatch(/▒ fixed \d+% · █ chat \d+% · ░ free/)
    for (const l of all.split('\n').filter(l => /lost today/.test(l))) expect(l).toMatch(/^\$\d+\.\d\d lost today$/)
    await pane.unmount()
    const band = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: cols }, surface: 'terminal' })
    const rows = (await dump(band)).split('\n')
    console.log(`--- BAND ${cols} columns ---\n` + rows.join('\n'))
    expect(rows[0].length).toBeLessThan(80) // row 1 fits one line, the dashboard link included
    expect(rows[0]).toMatch(cols < 80 ? /hide  dash ↗$/ : /hide  dashboard ↗$/)
    await band.unmount()
    tipStale = []
  })
}

test('the pane at 60 columns fits, and a cold pane shows the cold price and no Ping now', async ($, on) => {
  await boot($, on, false)
  await $.command.run({ command: 'context-guru', args: '' })
  await clock.advance(100)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const all = await dump(ui)
  console.log('--- PANE (cold, 60 cols) ---\n' + all)
  expect(all).toMatch(/○ cache cold: the next message writes it again/)
  expect(all).toMatch(/if it is cold\s+\$0\.78/)
  expect(await ui.find({ key: 'ping' })).toBeUndefined()
  for (const l of all.split('\n')) expect(l.length, l).toBeLessThanOrEqual(110) // lines wrap in the pane; none is absurdly long
  await ui.unmount()
})

test('the pane Ping button works and the last ping result shows', async ($, on) => {
  await boot($, on, true)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeDefined()
  await ui.press({ key: 'ping' })
  expect(toasts.join('\n')).toMatch(/kept warm/)
  expect(await dump(ui)).toMatch(/last ping: kept warm — saved \$0\.46 net/)
  await ui.unmount()
})

test('the pane shows the ping-off reason in full words, and no Ping now', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pingable: false, ping_reason: 'not opted in' })
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeUndefined()
  expect(await dump(ui)).toMatch(/ping off: Ping isn.t enabled for this install yet; it turns on in your next session/)
  await ui.unmount()
})

test('render: band, warm, agents, stale tip', async ($, on) => {
  tipStale = ['save-more']
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  console.log('--- BAND warm, agents, stale tip ---\n' + (await dump(ui)))
  await ui.unmount()
  tipStale = []
})

test('render: band, cold', async ($, on) => {
  await boot($, on, false)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  console.log('--- BAND cold ---\n' + (await dump(ui)))
  await ui.unmount()
})

test('render: band, warm, ping off, short band', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pingable: false, ping_reason: 'not opted in' })
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, maxRows: 2 }, surface: 'terminal' })
  console.log('--- BAND warm, ping off, short band ---\n' + (await dump(ui)))
  await ui.unmount()
})

// The same table as TestMoneyFormatTable in the Go suite, which holds statusline.py and cg.py to it.
test('usd() follows the one money format', async () => {
  const table: [number, string][] = [[0, '$0.00'], [0.004, '<$0.01'], [0.006, '<$0.01'], [0.01, '$0.01'], [0.06, '$0.06'], [0.78, '$0.78'], [1, '$1.00'], [99.99, '$99.99'], [100, '$100'], [123.4, '$123'], [1234.2, '$1234'], [-1.5, '-$1.50'], [-0.004, '$0.00'], [-123.4, '-$123']]
  for (const [x, want] of table) expect(usd(x)).toBe(want)
})

test('the first data shows within seconds when the first document is not there yet', async ($, on) => {
  let ready = false
  on('command.register', () => ({ value: { command: 'context-guru' } }))
  on('session.id', () => ({ value: 'sess-1' }))
  clock = mock.clock(on, { now: NOW })
  on('env.get', () => ({ value: 'http://127.0.0.1:8787/anthropic' }))
  on('session.start', (_: any, e: any) => ({ cwd: e.cwd }))
  on('ui.render', ($: any, e: any) => {
    const { Text } = $.ui.resolve(e)
    return <Text>engine</Text>
  })
  on('process.run', (_: any, e: any) => {
    const ret = (o: object) => ({ value: { exitCode: 0, stdout: JSON.stringify(o) + '\n', stderr: '' } })
    if (e.argv.includes('tip')) return ret({ stale: [], days: 7 })
    return ret(ready ? doc(true) : { error: 'proxy not answering' })
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000)
  let ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /next message/ })).toBeUndefined() // no document yet
  await ui.unmount()
  ready = true // the status line's refresher wrote the document
  await clock.advance(3000) // far less than the 15 s poll
  ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /next message/ })).toBeDefined()
  await ui.unmount()
})

for (const warm of [true, false]) {
  test(`band row 1 fits the width minus 4 at 60 columns for EVERY ping reason (${warm ? 'warm' : 'cold'}); below 80 it shows only "ping off"`, async ($, on) => {
    const extra: { v: object } = { v: {} }
    await boot($, on, warm, [], undefined as any, extra.v)
    for (const reason of ['', ...REASON_CODES, 'some-future-code']) {
      Object.assign(extra.v, reason === '' ? { pingable: undefined, ping_reason: undefined } : { pingable: false, ping_reason: reason })
      await clock.advance(16_000) // the next poll reads the new document
      const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 60 }, surface: 'terminal' })
      const row = (await dump(ui)).split('\n')[0]
      expect(row.length, `${reason}: ${row}`).toBeLessThanOrEqual(60 - 4) // the band has its own padding and the terminal a margin
      expect(row, reason).toMatch(/hide  dash ↗$/)
      if (!warm) expect(row, reason).not.toMatch(/ping off|\[ Ping \]/)
      else if (reason === '') expect(row).toMatch(/\[ Ping \]/)
      else if (reason === 'no entry' || reason === 'some-future-code') expect(row, reason).not.toMatch(/ping off/)
      else {
        expect(row, reason).toMatch(/ping off\s+hide  dash ↗$/) // no reason below 80 columns
        expect(row, reason).not.toMatch(/ping off:/)
      }
      await ui.unmount()
    }
  })
}

test('"$x lost today" shows once in the pane, under CACHE HITS', async ($, on) => {
  await boot($, on, true)
  await $.command.run({ command: 'context-guru', args: '' })
  await clock.advance(100)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const lines = (await dump(ui)).split('\n')
  expect(lines.filter(l => /lost today/.test(l)).length).toBe(1)
  const hits = lines.indexOf('CACHE HITS'), heat = lines.indexOf('MISSES BY HOUR')
  const at = lines.findIndex(l => /lost today/.test(l))
  expect(at > hits && at < heat).toBe(true)
  await ui.unmount()
})

test('a slow first document (about 10 s) still shows the band before the next 15 s poll', async ($, on) => {
  let ready = false
  on('command.register', () => ({ value: { command: 'context-guru' } }))
  on('session.id', () => ({ value: 'sess-1' }))
  clock = mock.clock(on, { now: NOW })
  on('env.get', () => ({ value: 'http://127.0.0.1:8787/anthropic' }))
  on('session.start', (_: any, e: any) => ({ cwd: e.cwd }))
  on('ui.render', ($: any, e: any) => {
    const { Text } = $.ui.resolve(e)
    return <Text>engine</Text>
  })
  on('process.run', (_: any, e: any) => {
    const ret = (o: object) => ({ value: { exitCode: 0, stdout: JSON.stringify(o) + '\n', stderr: '' } })
    if (e.argv.includes('tip')) return ret({ stale: [], days: 7 })
    return ret(ready ? doc(true) : { error: 'proxy not answering' })
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(9_000)
  ready = true // the document arrives about 9 s in
  await clock.advance(3_000) // 12 s: before the poll at 15 s
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /next message/ })).toBeDefined()
  await ui.unmount()
})

test('SAVE MORE at 60 columns puts each description on its own line', async ($, on) => {
  await boot($, on, true)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const lines = (await dump(ui)).split('\n')
  const i = lines.findIndex(l => l.startsWith('/context-guru:remove-mcp-and-skills'))
  expect(lines[i]).toBe('/context-guru:remove-mcp-and-skills')
  expect(lines[i + 1].trim()).toMatch(/^drop MCP servers/)
  await ui.unmount()
})

test('"$x lost today" falls back to MISSES BY HOUR when CACHE HITS has no data, and shows once', async ($, on) => {
  paneDoc = { heat: PANE_DATA.heat, spark: PANE_DATA.spark }
  await boot($, on, true)
  await $.command.run({ command: 'context-guru', args: '' })
  await clock.advance(100)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  const lines = (await dump(ui)).split('\n')
  expect(lines).not.toContain('CACHE HITS')
  expect(lines.filter(l => /lost today/.test(l))).toEqual(['$1.40 lost today'])
  expect(lines.findIndex(l => /lost today/.test(l))).toBeGreaterThan(lines.indexOf('MISSES BY HOUR'))
  await ui.unmount()
  paneDoc = PANE_DATA
})

test('two turns in a row with no document run one retry loop: at most 12 helper reads, not 24', async ($, on) => {
  const calls: string[][] = []
  on('command.register', () => ({ value: { command: 'context-guru' } }))
  on('session.id', () => ({ value: 'sess-1' }))
  clock = mock.clock(on, { now: NOW })
  on('env.get', () => ({ value: 'http://127.0.0.1:8787/anthropic' }))
  on('session.start', (_: any, e: any) => ({ cwd: e.cwd }))
  on('process.run', (_: any, e: any) => {
    calls.push([...e.argv])
    return { value: { exitCode: 0, stdout: JSON.stringify(e.argv.includes('tip') ? { stale: [], days: 7 } : { error: 'proxy not answering' }) + '\n', stderr: '' } }
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await $.turn.complete({} as any).catch(() => undefined)
  await $.turn.complete({} as any).catch(() => undefined)
  await clock.advance(14_000) // under the 15 s poll, long enough for the whole loop
  const n = calls.filter(c => c.includes('cachestate')).length
  expect(n).toBeGreaterThan(1) // the loop ran
  expect(n).toBeLessThanOrEqual(12)
})

test('a pinged sub-agent that is the latest touch lends the main clock neither its ↻ nor its plan nor its prices', async ($, on) => {
  docOverride = d => ({
    ...d,
    last_ts: NOW - 5_000, end_ts: NOW - 5_000, keepalive: true, held_until_ms: NOW + 3_600_000, next_warm_usd: 0.01, next_cold_usd: 0.09,
    threads: d.threads.map((t: any) => (t.main ? { ...t, last_ts: NOW - 60_000, held_until_ms: undefined, next_warm_usd: 0.06, next_cold_usd: 0.78 } : t)),
  })
  await boot($, on, true)
  docOverride = null
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface: 'terminal' })
  const all = await dump(ui)
  expect(all).toMatch(/next message \$0\.06 · \$0\.78 if cold/)
  expect(all.split('\n')[1]).toMatch(/cache ● █████░ 3:59$/) // main's own clock, not the sub-agent's hour-long plan
  expect(all.split('\n')[1]).not.toMatch(/↻/)
  await ui.unmount()
})

test('a newer document with the same words still moves the clock (its anchors are in the redraw check)', async ($, on) => {
  let end = NOW - 60_000
  docOverride = d => ({ ...d, last_ts: end, end_ts: end, threads: d.threads.map((t: any) => (t.main ? { ...t, last_ts: end, end_ts: end } : t)) })
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface: 'terminal' })
  expect((await dump(ui)).split('\n')[1]).toMatch(/cache ● █████░ 3:59/)
  // A later document moves the end by 20 s. The coarse words (`5m` left) and every price, token and money figure stay
  // the same, so only the clock's anchors tell the redraw check that it moved.
  end = NOW - 40_000
  await clock.advance(16_000)
  // The fixture's now_ms stays NOW: the clock is end + TTL - (now_ms + the copy's local age), as statusline.py counts.
  expect((await dump(ui)).split('\n')[1]).toMatch(/cache ● █████░ 4:1\d/) // 20 s more than the old anchor gives (3:5x)
  docOverride = null
  await ui.unmount()
})

test('the redraw check sees a moved clock even when every word and figure is the same', async () => {
  const at = (end: number) => view({ at: NOW, url: '', doc: { ...doc(true), last_ts: end, end_ts: end, threads: [{ id: 'main', main: true, ...entry(end), last_ts: end }] } }, NOW)
  // 59 s and 41 s into a 5-minute TTL: both read `5m` left, at the same prices, tokens and money
  expect(at(NOW - 59_000)!.main.left).toBe(at(NOW - 41_000)!.main.left)
  expect(signature(at(NOW - 59_000))).not.toBe(signature(at(NOW - 41_000)))
})

test('a long agent name (a general-purpose agent named by its description) is cut to 24 characters with …', async ($, on) => {
  docOverride = d => ({ ...d, threads: d.threads.map((t: any) => (t.id === 'a:explorer1' ? { ...t, name: 'Strict main-track reviewer, rewrite round 4' } : t)) })
  await boot($, on, true)
  docOverride = null
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 140 }, surface: 'terminal' })
  const row = (await dump(ui)).split('\n').find(l => l.startsWith('└ agents'))!
  expect(row).toMatch(/● Strict main-track revie… 5m/) // 23 characters and `…`
  expect(row).not.toMatch(/rewrite round 4/)
  await ui.unmount()
})

// #771: the Ping button shows only when no automatic ping fires before the cache expires. The warm main cache here
// ends at NOW - 60 s with a 300 s TTL, so it expires at NOW + 240 s.
const PLAN_CASES: [string, object, boolean][] = [
  ['a ping fires before expiry', { pings_left: 2, next_ping_ms: NOW + 100_000 }, false],
  ['no pings left', { pings_left: 0, next_ping_ms: NOW + 100_000 }, true],
  ['no ping planned (keep-alive off)', {}, true],
  ['the next ping comes after expiry', { pings_left: 2, next_ping_ms: NOW + 300_000 }, true],
  ['next_ping_ms is 0 (no ping planned)', { pings_left: 2, next_ping_ms: 0 }, true],
  ['an old proxy that sends pings_left but no next_ping_ms', { pings_left: 2 }, true],
]
for (const [name, extra, shown] of PLAN_CASES) {
  test(`Ping button, ${name}: band and pane agree (${shown ? 'shown' : 'hidden'})`, async ($, on) => {
    await boot($, on, true, [], undefined as any, extra)
    const band = await $.ui.mount({ ...BAND, surface: 'terminal' })
    const bandText = await dump(band)
    expect(!!(await band.find({ key: 'ping' }))).toBe(shown)
    expect(/↻ next ping \d+:\d\d/.test(bandText)).toBe(!shown)
    await band.unmount()
    const pane = await $.ui.mount({ ...PANE, surface: 'terminal' })
    const paneText = await dump(pane)
    expect(!!(await pane.find({ key: 'ping' }))).toBe(shown)
    expect(/↻ next ping \d+:\d\d/.test(paneText)).toBe(!shown)
    await pane.unmount()
  })
}

test('hidden Ping shows the time of the next ping', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pings_left: 2, next_ping_ms: NOW + 100_000 })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await dump(ui)).toMatch(/↻ next ping 1:[34]\d/)
  await ui.unmount()
})

test('a warm cache with a planned ping and pingable=false keeps the ping-off reason, not the next-ping words', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pings_left: 2, next_ping_ms: NOW + 100_000, pingable: false, ping_reason: 'too large' })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  const all = await dump(ui)
  expect(all).toMatch(/ping off/)
  expect(all).not.toMatch(/next ping/)
  await ui.unmount()
})

test('band row 1 fits 60 columns with the next-ping hint, warm', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pings_left: 2, next_ping_ms: NOW + 100_000 })
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 60 }, surface: 'terminal' })
  const row = (await dump(ui)).split('\n')[0]
  expect(row).toMatch(/↻ ping \d+:\d\d/)
  expect(row.length, row).toBeLessThanOrEqual(60 - 4)
  expect(row).toMatch(/hide  dash ↗$/)
  await ui.unmount()
})

// #771: a thread the keeper stopped (a miss, a refused ping, a route failure) plans no ping. While the proxy still
// holds its copy the server says pingable, and the button shows; once the copy is released it says `stopped`.
test('a stopped thread with a warm cache and a held copy shows the Ping button', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pingable: true })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeDefined()
  await ui.unmount()
})

test('a stopped thread whose copy is released shows no Ping button', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pingable: false, ping_reason: 'stopped' })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeUndefined()
  await ui.unmount()
})
