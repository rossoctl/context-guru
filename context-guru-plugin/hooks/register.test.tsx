import { expect, mock, test } from 'claude-code/testing'

const NOW = 1_800_000_000_000
const entry = (end: number, extra = {}) => ({ end_ts: end, ttl_s: 300, prefix_tokens: 90000, rewrite_usd: 0.5, next_warm_usd: 0.04, next_cold_usd: 0.5, ...extra })
const doc = (warm: boolean) => ({
  ...entry(warm ? NOW - 60_000 : NOW - 900_000),
  last_ts: warm ? NOW - 60_000 : NOW - 900_000,
  now_ms: NOW,
  age_s: 0,
  stale: false,
  money: { main: { saved_usd: 1.0 }, others: { saved_usd: 0.234 } },
  threads: [
    { id: 'main', main: true, ...entry(warm ? NOW - 60_000 : NOW - 900_000) },
    { id: 'a:explorer1', name: 'Explore', main: false, ...entry(NOW - 30_000) },
    { id: 'a:old', name: 'Reviewer', main: false, ...entry(NOW - 900_000) }, // cold: not shown
    { id: 'a:unnamed', main: false, ...entry(NOW - 30_000) }, // no name: shown as nothing, never as a hex id
    { id: 'p:classifier', name: 'Classifier', main: false, ...entry(NOW - 30_000) }, // a side request, not an agent
  ],
})

const BAND = {
  plugin: 'context-guru',
  component: 'AbovePrompt',
  props: { hasSurvey: false, isWorking: false, maxRows: 6, bodyColumns: 100, scroll: { offset: 0, bodyRows: 6 } },
} as const

let clock: ReturnType<typeof mock.clock>

async function boot($: any, on: any, warm: boolean, calls: string[][] = [], pingOut: object = { status: 200, sent: true, saved_usd: 0.46, ping_usd: 0.04 }, mainExtra: object = {}) {
  on('command.register', () => ({ value: { command: 'context-guru' } }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('session.id', () => ({ value: 'sess-1' }))
  clock = mock.clock(on, { now: NOW })
  on('env.get', () => ({ value: 'http://127.0.0.1:8787/anthropic' }))
  on('session.start', (_: any, e: any) => ({ cwd: e.cwd }))
  on('ui.status', () => ({ value: undefined }))
  on('ui.render', ($: any, e: any) => {
    const { Text } = $.ui.resolve(e)
    return <Text>engine</Text>
  })
  on('process.run', (_: any, e: any) => {
    calls.push([...e.argv])
    const ping = e.argv.includes('ping')
    const base = doc(warm)
    const out = ping ? (pingOut ?? { status: 200, sent: true, saved_usd: 0.46, ping_usd: 0.04 }) : { ...base, ...mainExtra, threads: base.threads.map((t: any) => (t.main ? { ...t, ...mainExtra } : t)) }
    return { value: { exitCode: 0, stdout: JSON.stringify(out) + '\n', stderr: '' } }
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000) // the first poll lands
}

for (const surface of ['terminal', 'desktop'] as const) {
  test(`${surface}: warm band shows the gauge, cost, saved, Ping, the dashboard link, named agents and one tip`, async ($, on) => {
    await boot($, on, true)
    const ui = await $.ui.mount({ ...BAND, surface })
    expect(await ui.find({ type: 'Text', text: /Context Guru/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /4m/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\$0\.040/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\$1\.23/ })).toBeDefined()
    expect(await ui.find({ key: 'ping' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /Explore.*5m/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /save more: \/context-guru:/ })).toBeDefined()
    if (surface === 'terminal') expect(await ui.find({ type: 'Link' })).toBeDefined()
    await ui.unmount()
  })

  test(`${surface}: only named user agents; never a p: thread, a cold agent or a hex id`, async ($, on) => {
    await boot($, on, true)
    const ui = await $.ui.mount({ ...BAND, surface })
    for (const gone of [/Classifier/, /Reviewer/, /unnamed/, /a:/]) {
      expect(await ui.find({ type: 'Text', text: gone })).toBeUndefined()
    }
    await ui.unmount()
  })

  test(`${surface}: a cold main cache says nothing about being cold, and has no Ping`, async ($, on) => {
    await boot($, on, false)
    const ui = await $.ui.mount({ ...BAND, surface })
    expect(await ui.find({ type: 'Text', text: /cold/i })).toBeUndefined()
    expect(await ui.find({ key: 'ping' })).toBeUndefined()
    await ui.unmount()
  })
}

test('the dashboard link is http://localhost:PORT/dashboard/', async ($, on) => {
  await boot($, on, true)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  const link = await ui.find({ type: 'Link' })
  expect((link as any)?.props?.href ?? (link as any)?.href).toBe('http://localhost:8787/dashboard/')
  await ui.unmount()
})

test('kept warm by pings: the gauge is marked with the cycle arrow', async ($, on) => {
  await boot($, on, true, [], undefined as any, { keepalive: true })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /\u21bb/ })).toBeDefined()
  await ui.unmount()
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

test('pressing Ping runs the helper with the session id and toasts the saving', async ($, on) => {
  const calls: string[][] = []
  const toasts: string[] = []
  on('ui.toast', (_: any, e: any) => {
    toasts.push(e.text)
    return { value: undefined }
  })
  await boot($, on, true, calls)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  await ui.press({ key: 'ping' })
  const ping = calls.find(c => c.includes('ping'))
  expect(ping).toBeDefined()
  expect(ping).toContain('sess-1')
  expect(toasts.join('\n')).toMatch(/kept warm .* saved \$0\.460 net \(ping cost \$0\.040\)/)
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
  expect(r.text).toMatch(/kept warm .* saved \$0\.460 net \(ping cost \$0\.040\)/)
  expect(r.context).toBeUndefined() // nothing is recorded for the model
  expect(calls.some(c => c.includes('ping'))).toBe(true)
  expect(started).toEqual([])
  expect(calls.filter(c => c.includes('ping')).length).toBe(1) // the helper ran once
})

test('a server without the ping route is not blamed on the session', async ($, on) => {
  const toasts: string[] = []
  on('ui.toast', (_: any, e: any) => {
    toasts.push(e.text)
    return { value: undefined }
  })
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

test('warm but not pingable: the Ping button is still shown, and there is no keep-alive hint', async ($, on) => {
  await boot($, on, true, [], undefined as any, { pingable: false })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /keep-alive off/ })).toBeUndefined()
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
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ key: 'ping' })).toBeDefined() // still visible
  const r = await $.command.run({ command: 'ping', args: '' })
  expect(r.text).toMatch(/can't ping: context too large to hold for Ping/)
  await ui.unmount()
})

for (const [reason, want] of [
  ['not opted in', /restart Claude Code/],
  ['below limits', /below its minimum/],
  ['pings used', /pings have run out/],
  ['stopped', /stopped keeping this thread warm/],
  ['no entry', /knows nothing about this session/],
  ['unavailable', /route or credential is failing right now . try again shortly/],
  ['over budget', /per-ping limit/],
  ['too large', /too large to hold/],
  ['unsupported', /extended thinking/],
  ['expired', /already cold/],
  ['some-future-code', /cannot ping this session right now/],
] as const) {
  test(`/ping words ping_reason ${reason}`, async ($, on) => {
    await boot($, on, true, [], { status: 409, error: 'not held' }, { pingable: false, ping_reason: reason })
    const r = await $.command.run({ command: 'ping', args: '' })
    expect(r.text).toMatch(want)
    expect(r.text).not.toMatch(/some-future-code/)
  })
}
