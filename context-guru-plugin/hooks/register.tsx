// Context Guru mod: a band above the prompt (brand, dashboard link, the main cache as a gauge with its time left,
// next-request cost, saved $, a Ping button while the cache is warm, one chip per named sub-agent, one tip) and an
// optional pane (/context-guru:context-guru). A cold cache shows nothing: the user knows.
//
// The mod never touches the network. Every read and the one write go through scripts/cg.py, which owns
// auth, the routed port and TLS trust:
//   cg.py cachestate --json      reads the status line's on-disk cache (single-flight, backed off)
//   cg.py ping --session ID --json   one explicit POST /api/me/cache/ping, on a button press
import { atom, read, update } from 'claude-code'
import type { EngineInterface as Engine, Register } from 'claude-code'

import type { Snapshot, View } from '../types'
import { TIPS, gauge, pingReason, pingToast, signature, usd, view } from './model'

const PANE = 'context-guru'
const POLL_MS = 15_000

const snapshot = atom({ plugin: 'context-guru', key: 'snapshot' } as const, null)
const isPinging = atom({ plugin: 'context-guru', key: 'isPinging' } as const, false)
const isHidden = atom({ plugin: 'context-guru', key: 'isHidden' } as const, false)

let seen = ''
let transcript = '' // this session's transcript path, from the classic hooks: it names the sub-agents

async function helper($: Engine, args: string[]): Promise<Record<string, any> | null> {
  try {
    const out = await $.process.run(['python3', `${$.plugin.root}/scripts/cg.py`, ...args], { timeoutMs: 20_000 })
    const line = out.stdout.trim().split('\n').filter(Boolean).pop() ?? ''
    return JSON.parse(line)
  } catch {
    return null
  }
}

async function refresh($: Engine): Promise<void> {
  try {
    await load($)
  } catch {
    // a poll that fails leaves the band as it was
  }
}

async function load($: Engine): Promise<void> {
  const doc = await helper($, ['cachestate', '--json', '--session', await $.session.id(), ...(transcript ? ['--transcript', transcript] : [])])
  const now = await $.clock.now()
  // `cg.py cachestate --json` prints the raw /api/cachestate entry (+ age_s, stale), or {"error": ...}.
  // The dashboard is the loopback proxy this session is routed through (http://localhost:PORT/dashboard/); anything else shows no link.
  const base = /^(https?:\/\/(?:127\.0\.0\.1|localhost)(?::\d+)?)/.exec((await $.env.get('ANTHROPIC_BASE_URL')) ?? '')
  const snap: Snapshot | null = doc ? { at: now, url: base ? `${base[1].replace('127.0.0.1', 'localhost')}/dashboard/` : '', doc } : null
  // Redraw only when what the band says changes: the words, not the seconds.
  const sig = signature(view(snap, now))
  if (sig === seen) return
  seen = sig
  await update($, snapshot, () => snap)
  $.ui.status(status(view(snap, now)))
}

function status(v: View | null): string | undefined {
  if (!v) return undefined
  return v.main.isWarm ? `cache warm ${v.main.left}` : 'cache cold'
}

/** One explicit ping through the helper; resolves the one-line outcome. Never starts a model turn. */
async function pingNow($: Engine): Promise<string> {
  if (await read($, isPinging)) return 'a ping is already in flight'
  await update($, isPinging, () => true)
  try {
    const r = await helper($, ['ping', '--session', await $.session.id(), '--json'])
    return pingToast(r ?? { status: 0, error: 'no answer from the helper' }, pingReason(await read($, snapshot)))
  } finally {
    await update($, isPinging, () => false)
    seen = ''
    await refresh($)
  }
}

async function ping($: Engine): Promise<void> {
  $.ui.toast(await pingNow($), { timeoutMs: 6000 })
}

export const register: Register = on => {
  // The envelope of the classic hooks carries the transcript path; sub-agent names live beside it.
  on('classic.SessionStart', ($, e, next) => {
    transcript = e.transcript_path
    return next(e)
  })
  on('classic.UserPromptSubmit', ($, e, next) => {
    transcript = e.transcript_path
    return next(e)
  })




  on('session.start', async ($, e, next) => {
    const started = await next(e)
    // /ping is answered by the hook below with `{ text }` alone: the engine prints it as the command's output
    // and sends nothing to the model (no `context`, so no hidden user message either), so no turn starts.
    await $.command.register({ name: 'ping', description: 'Keep this session\'s prompt cache warm with one ping now' })
    await $.command.register({ name: 'context-guru', description: 'Open the Context Guru pane: savings, cache timeline, subagents' })
    void refresh($)
    $.clock.every(POLL_MS, () => void refresh($))
    return started
  })

  on('turn.complete', async ($, e, next) => {
    void refresh($) // a turn touched the cache: the countdown restarted
    return next(e)
  })

  on('command.run', { command: 'ping' }, async $ => ({ text: await pingNow($) }))

  on('command.run', { command: 'context-guru' }, async $ => {
    await $.ui.open({ id: PANE, title: 'Context Guru', focus: true })
    return { text: 'Context Guru pane opened.' }
  })


  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const snap = await read($, snapshot)
    const now = await $.clock.now()
    const v = view(snap, now)
    if (e.props.hasSurvey || !v || (await read($, isHidden))) return next(e)

    const { Box, Text, Button, Link } = $.ui.resolve(e)
    const busy = await read($, isPinging)
    const m = v.main
    const rows = e.props.maxRows
    const colour = m.isPinged ? 'cyan' : m.isLastMinute ? 'warning' : 'success'
    const bar = <Text dimColor>{' │ '}</Text>
    const tip = TIPS[v.tip % TIPS.length]

    return (
      <Box flexDirection="column" paddingX={1} marginTop={1}>
        <Box flexWrap="wrap">
          <Text bold color="claude">{'◆'} Context Guru</Text>
          {m.isWarm && (
            <Text>
              {bar}
              <Text color={colour}>{gauge(m.frac)} {m.left}{m.isPinged ? ' ↻' : ''}</Text>
            </Text>
          )}
          {m.isWarm && m.warmUsd !== undefined && m.coldUsd !== undefined && (
            <Text>
              {bar}
              <Text dimColor>next </Text>
              <Text color="success">{usd(m.warmUsd)}</Text>
              <Text dimColor> vs {usd(m.coldUsd)} if cold</Text>
            </Text>
          )}
          {v.saved !== undefined && v.saved > 0 && (
            <Text>
              {bar}
              <Text dimColor>saved </Text>
              <Text bold color="success">{usd(v.saved)}</Text>
            </Text>
          )}
          {m.isWarm && (
            <Box>
              {bar}
              <Button
                key="ping"
                label={busy ? 'Pinging…' : 'Ping'}
                variant="primary"
                hotkey="p"
                onPress={() => (busy ? undefined : ping($))}
              />
            </Box>
          )}
          {v.url !== '' && (
            <Box>
              {bar}
              <Link href={v.url} label="dashboard ↗" />
            </Box>
          )}
          <Text> </Text>
          <Button key="hide" label="hide" plain dimColor onPress={() => update($, isHidden, () => true)} />
        </Box>
        {v.subs.length > 0 && rows > 2 && (
          <Box flexWrap="wrap" columnGap={2}>
            <Text dimColor>{'└'} agents</Text>
            {v.subs.map(t => (
              <Text key={t.id} color={t.isPinged ? 'cyan' : t.isLastMinute ? 'warning' : 'success'}>
                {t.name} <Text dimColor>{gauge(t.frac, 4)}</Text> {t.left}{t.isPinged ? ' ↻' : ''}
              </Text>
            ))}
          </Box>
        )}
        {rows > 3 && (
          <Text dimColor>
            save more: <Text color="suggestion">{tip.cmd}</Text> {'—'} {tip.why}
          </Text>
        )}
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Link, Button } = $.ui.resolve(e)
    const snap = await read($, snapshot)
    const v = view(snap, await $.clock.now())
    if (!v || !snap) {
      return <Text dimColor>No Context Guru data for this session yet. Send a prompt, or check /context-guru:doctor.</Text>
    }
    const m = v.main
    const colour = (t: { isPinged: boolean; isLastMinute: boolean }) => (t.isPinged ? 'cyan' : t.isLastMinute ? 'warning' : 'success')
    const wide = Math.min(24, Math.max(8, (e.props.bodyColumns ?? 60) - 40))

    return (
      <Box flexDirection="column" gap={1} paddingX={1}>
        <Box columnGap={2}>
          <Text bold color="claude">{'◆'} Context Guru</Text>
          {v.url !== '' && <Link href={v.url} label="open dashboard ↗" />}
        </Box>

        {v.saved !== undefined && (
          <Box flexDirection="column">
            <Text dimColor>SAVED THIS SESSION</Text>
            <Text bold color="success">{usd(v.saved)}</Text>
          </Box>
        )}

        <Box flexDirection="column">
          <Text dimColor>MAIN CACHE</Text>
          {m.isWarm ? (
            <Text>
              <Text color={colour(m)}>{gauge(m.frac, wide)} {m.left} left{m.isPinged ? ' ↻ kept warm by pings' : ''}</Text>
              {m.tokens !== undefined && <Text dimColor>  {Math.round(m.tokens / 1000)}k tokens</Text>}
            </Text>
          ) : (
            <Text dimColor>nothing held; the next request writes it again</Text>
          )}
          {m.warmUsd !== undefined && m.coldUsd !== undefined && (
            <Text dimColor>next request {usd(m.warmUsd)} warm, {usd(m.coldUsd)} cold</Text>
          )}
        </Box>

        {v.subs.length > 0 && (
          <Box flexDirection="column">
            <Text dimColor>AGENTS</Text>
            {v.subs.map(t => (
              <Text key={t.id}>
                {t.name?.padEnd(18).slice(0, 18)} <Text color={colour(t)}>{gauge(t.frac, Math.min(wide, 12))} {t.left}{t.isPinged ? ' ↻' : ''}</Text>
                {t.saved !== undefined && t.saved > 0 && <Text dimColor>  saved {usd(t.saved)}</Text>}
              </Text>
            ))}
          </Box>
        )}

        <Box flexDirection="column">
          <Text dimColor>SAVE MORE</Text>
          {TIPS.map(t => (
            <Text key={t.cmd}>
              <Text color="suggestion">{t.cmd}</Text> <Text dimColor>{t.why}</Text>
            </Text>
          ))}
        </Box>

        {m.isWarm && <Button key="ping" label="Ping now" variant="primary" hotkey="p" onPress={() => ping($)} />}
      </Box>
    )
  })
}
