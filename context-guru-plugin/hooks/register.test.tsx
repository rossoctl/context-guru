import { expect, mock, test } from 'claude-code/testing'

import { REASON_CODES, usd } from './model'

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
  on('command.register', () => ({ value: { command: 'context-guru' } }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('session.id', () => ({ value: 'sess-1' }))
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

for (const surface of ['terminal', 'desktop'] as const) {
  test(`${surface}: warm band shows the next-message price warm and cold, Ping and the agent chips`, async ($, on) => {
    await boot($, on, true)
    const ui = await $.ui.mount({ ...BAND, surface })
    expect(await ui.find({ type: 'Text', text: /next message/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\$0\.06/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\$0\.78 if cold/ })).toBeDefined()
    expect(await ui.find({ key: 'ping' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /Explore/ })).toBeDefined()
    expect(await ui.find({ type: 'Link' })).toBeUndefined() // the link lives in the pane: the terminal draws its address too
    await ui.unmount()
  })

  test(`${surface}: the band has no clock, no saved money, no tip rotation and no brand text`, async ($, on) => {
    await boot($, on, true)
    const ui = await $.ui.mount({ ...BAND, surface })
    const all = await dump(ui)
    expect(all).not.toMatch(/Context Guru/)
    expect(all).not.toMatch(/█|░/) // no gauge
    expect(all).not.toMatch(/saved/i)
    expect(all).not.toMatch(/\$22|\$18|\$1\.23/) // no session money
    expect(all).not.toMatch(/save more:/)
    expect(all).not.toMatch(/\b4m\b.*next message|cache warm/) // the clock is the status line's
    expect(statusCalls).toBe(0) // no $.ui.status copy of the clock
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

test('the band is never more than three rows, even with agents and a stale tip', async ($, on) => {
  tipStale = ['save-more', 'remove-mcp-and-skills']
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, maxRows: 50 }, surface: 'terminal' })
  expect(((await dump(ui)).match(/\n/g) ?? []).length).toBeLessThanOrEqual(2)
  await ui.unmount()
  tipStale = []
})

test('no weekly tip while every command ran this week', async ($, on) => {
  tipStale = []
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await dump(ui)).not.toMatch(/tip:/)
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

test('/ping answers with text alone and starts no turn', async ($, on) => {
  const calls: string[][] = []
  const started: string[] = []
  on('turn.start', (_: any, e: any) => {
    started.push('turn')
    return e
  })
  on('prompt.submit', (_: any, e: any) => {
    started.push('prompt')
    return e
  })
  await boot($, on, true, calls)
  const r = await $.command.run({ command: 'ping', args: '' })
  expect(r.text).toMatch(/kept warm .* saved \$0\.46 net \(ping cost \$0\.04\)/)
  expect(r.context).toBeUndefined() // nothing is recorded for the model
  expect(calls.some(c => c.includes('ping'))).toBe(true)
  expect(started).toEqual([])
  expect(calls.filter(c => c.includes('ping')).length).toBe(1) // the helper ran once
})

test('a server without the ping route is not blamed on the session', async ($, on) => {
  // The helper reports Go's plain-text 404 as a status with no error field.
  await boot($, on, true, [], { status: 404 })
  const r = await $.command.run({ command: 'ping', args: '' })
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
  test(`/ping says the right thing for ${status} ${error}`, async ($, on) => {
    await boot($, on, true, [], { status, error })
    const r = await $.command.run({ command: 'ping', args: '' })
    expect(r.text).toMatch(want)
  })
}

test('a refused press explains itself with the server\'s ping_reason', async ($, on) => {
  await boot($, on, true, [], { status: 409, error: 'not held' }, { pingable: false, ping_reason: 'too large' })
  const r = await $.command.run({ command: 'ping', args: '' })
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
  ['unsupported', /this route cannot be pinged/],
  ['expired', /already cold/],
  ['some-future-code', /cannot ping this session right now/],
] as const) {
  test(`/ping words ping_reason ${reason}`, async ($, on) => {
    await boot($, on, true, [], { status: 409, error: 'not held' }, { pingable: false, ping_reason: reason })
    const r = await $.command.run({ command: 'ping', args: '' })
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
    expect(rows[0].length).toBeLessThan(80) // row 1 holds no address: it fits one line
    expect(rows[0]).not.toMatch(/localhost|dashboard/)
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

test('band row 1 fits one line at 60 columns for EVERY ping reason, with hide; no entry draws nothing', async ($, on) => {
  const extra: { v: object } = { v: {} }
  await boot($, on, true, [], undefined as any, extra.v)
  const reasons = [...REASON_CODES, 'some-future-code']
  for (const reason of ['', ...reasons]) {
    Object.assign(extra.v, reason === '' ? { pingable: undefined, ping_reason: undefined } : { pingable: false, ping_reason: reason })
    await clock.advance(16_000) // the next poll reads the new document
    const ui = await $.ui.mount({ ...BAND, props: { ...BAND.props, bodyColumns: 60 }, surface: 'terminal' })
    const row = (await dump(ui)).split('\n')[0]
    expect(row.length, `${reason}: ${row}`).toBeLessThanOrEqual(60)
    expect(row, reason).toMatch(/hide$/)
    if (reason === '' ) expect(row).toMatch(/\[ Ping \]/)
    else if (reason === 'no entry' || reason === 'some-future-code') expect(row, reason).not.toMatch(/ping off/)
    else expect(row, reason).toMatch(/ping off: /)
    await ui.unmount()
  }
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
