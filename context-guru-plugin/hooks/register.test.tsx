import { expect, mock, test } from 'claude-code/testing'

const NOW = 1_800_000_000_000
const entry = (end: number, extra = {}) => ({ end_ts: end, ttl_s: 300, prefix_tokens: 90000, rewrite_usd: 0.5, next_warm_usd: 0.04, next_cold_usd: 0.5, ...extra })
const doc = (warm: boolean) => ({
  ok: true,
  dashboard_url: 'http://127.0.0.1:8787/',
  now_ms: NOW,
  age_s: 0,
  session: {
    ...entry(warm ? NOW - 60_000 : NOW - 900_000),
    threads: [
      { id: 'main', main: true, ...entry(warm ? NOW - 60_000 : NOW - 900_000) },
      { id: 'explorer', main: false, ...entry(NOW - 30_000) },
      { id: 'reviewer', main: false, ...entry(NOW - 900_000) },
    ],
  },
  stats: { saved_usd: 1.234, manual_pings: { count: 2, ping_usd: 0.01, saved_usd: 0.4 } },
})

const BAND = {
  plugin: 'context-guru',
  component: 'AbovePrompt',
  props: { hasSurvey: false, isWorking: false, maxRows: 6, bodyColumns: 100, scroll: { offset: 0, bodyRows: 6 } },
} as const

let clock: ReturnType<typeof mock.clock>

async function boot($: any, on: any, warm: boolean, calls: string[][] = []) {
  on('command.register', () => ({ value: { command: 'context-guru' } }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('session.id', () => ({ value: 'sess-1' }))
  clock = mock.clock(on, { now: NOW })
  on('session.start', (_: any, e: any) => ({ cwd: e.cwd }))
  on('ui.status', () => ({ value: undefined }))
  on('ui.render', ($: any, e: any) => {
    const { Text } = $.ui.resolve(e)
    return <Text>engine</Text>
  })
  on('process.run', (_: any, e: any) => {
    calls.push([...e.argv])
    const ping = e.argv.includes('ping')
    const out = ping ? { status: 200, sent: true, saved_usd: 0.46, ping_usd: 0.04 } : doc(warm)
    return { value: { exitCode: 0, stdout: JSON.stringify(out) + '\n', stderr: '' } }
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000) // the first poll lands
}

for (const surface of ['terminal', 'desktop'] as const) {
  test(`${surface}: warm band shows cache, cost, saved, Ping and subagent chips`, async ($, on) => {
    await boot($, on, true)
    const ui = await $.ui.mount({ ...BAND, surface })
    expect(await ui.find({ type: 'Text', text: /cache warm 4m/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\$0\.040 warm/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /\$1\.23/ })).toBeDefined()
    expect(await ui.find({ key: 'ping' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /explorer 5m/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /reviewer.*cold/ })).toBeDefined()
    await ui.unmount()
  })

  test(`${surface}: cold main thread has no Ping button`, async ($, on) => {
    await boot($, on, false)
    const ui = await $.ui.mount({ ...BAND, surface })
    expect(await ui.find({ type: 'Text', text: /cache cold/ })).toBeDefined()
    expect(await ui.find({ key: 'ping' })).toBeUndefined()
    await ui.unmount()
  })
}

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
  expect(toasts.join('\n')).toMatch(/kept warm .* saved \$0\.460 .* ping \$0\.040/)
  await ui.unmount()
})
