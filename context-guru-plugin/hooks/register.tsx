// Context Guru mod: a band above the prompt (dashboard link, cache warm/cold, next-request cost, saved $,
// a Ping button while the cache is warm, one chip per subagent) and an optional pane (/context-guru:pane).
//
// The mod never touches the network. Every read and the one write go through scripts/cg.py, which owns
// auth, the routed port and TLS trust:
//   cg.py cachestate --json      reads the status line's on-disk cache (single-flight, backed off)
//   cg.py ping --session ID --json   one explicit POST /api/me/cache/ping, on a button press
import { atom, read, update } from 'claude-code'
import type { EngineInterface as Engine, Register } from 'claude-code'

import type { Snapshot, View } from '../types'
import { pingToast, signature, span, usd, view } from './model'

const PANE = 'context-guru'
const POLL_MS = 15_000

const snapshot = atom({ plugin: 'context-guru', key: 'snapshot' } as const, null)
const isPinging = atom({ plugin: 'context-guru', key: 'isPinging' } as const, false)
const isHidden = atom({ plugin: 'context-guru', key: 'isHidden' } as const, false)

let seen = ''

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
  const doc = await helper($, ['cachestate', '--json', '--session', await $.session.id()])
  const now = await $.clock.now()
  const snap: Snapshot | null = doc ? { at: now, doc } : null
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
    return pingToast(r ?? { status: 0, error: 'no answer from the helper' })
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
    const v = view(snap, await $.clock.now())
    if (e.props.hasSurvey || !v || (await read($, isHidden))) return next(e)

    const { Box, Text, Button, Link } = $.ui.resolve(e)
    const busy = await read($, isPinging)
    const m = v.main
    const rows = e.props.maxRows

    const cost = (
      <Text dimColor>
        {m.warmUsd !== undefined && m.coldUsd !== undefined ? (
          <Text>
            next request <Text color="success">{usd(m.warmUsd)} warm</Text> vs <Text color="warning">{usd(m.coldUsd)} cold</Text>
          </Text>
        ) : m.coldUsd !== undefined ? (
          <Text>a cold restart would cost {usd(m.coldUsd)}</Text>
        ) : null}
      </Text>
    )

    return (
      <Box flexDirection="column" paddingX={1}>
        <Box flexWrap="wrap" columnGap={2}>
          <Text bold color="claude">{'◆'} Context Guru</Text>
          {v.url !== '' && <Link href={v.url.replace('127.0.0.1', 'localhost')} label="dashboard ↗" />}
          {m.isWarm ? (
            <Text color="success">{'●'} cache warm {m.left}</Text>
          ) : (
            <Text color="warning">{'○'} cache cold</Text>
          )}
          {cost}
          {v.saved !== undefined && <Text>saved <Text bold color="success">{usd(v.saved)}</Text></Text>}
          {m.isWarm && (
            <Button
              key="ping"
              label={busy ? 'Pinging…' : 'Ping'}
              variant="primary"
              hotkey="p"
              onPress={() => (busy ? undefined : ping($))}
            />
          )}
          <Button key="hide" label="Hide" plain dimColor onPress={() => update($, isHidden, () => true)} />
        </Box>
        {v.subs.length > 0 && rows > 1 && (
          <Box flexWrap="wrap" columnGap={2}>
            <Text dimColor>subagents</Text>
            {v.subs.map(t => (
              <Text key={t.id} color={t.isWarm ? 'success' : 'warning'}>
                {t.isWarm ? '●' : '○'} {t.id.slice(0, 12)} {t.isWarm ? t.left : 'cold'}
              </Text>
            ))}
          </Box>
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
    const stats = (snap.doc.stats ?? {}) as { waterfall?: { key: string; label?: string; delta_usd: number }[]; manual_pings?: { count?: number; ping_usd?: number; saved_usd?: number } }
    const cats = [
      ...(stats.waterfall ?? []).map(w => ({ name: w.label ?? w.key, usd: -w.delta_usd })),
      ...(stats.manual_pings ? [{ name: 'Manual pings', usd: stats.manual_pings.saved_usd ?? 0 }] : []),
    ].filter(c => Math.abs(c.usd) >= 0.005)
    const top = Math.max(0.01, ...cats.map(c => Math.abs(c.usd)))
    const width = Math.max(8, Math.min(24, (e.props.bodyColumns ?? 60) - 36))

    return (
      <Box flexDirection="column" gap={1}>
        <Box columnGap={2}>
          <Text bold color="claude">Context Guru</Text>
          {v.url !== '' && <Link href={v.url.replace('127.0.0.1', 'localhost')} label="open dashboard" />}
          {v.saved !== undefined && <Text>saved <Text bold color="success">{usd(v.saved)}</Text></Text>}
        </Box>

        <Box flexDirection="column">
          <Text bold>Savings by category</Text>
          {cats.length === 0 && <Text dimColor>Nothing saved yet.</Text>}
          {cats.map(c => (
            <Text key={c.name}>
              {c.name.padEnd(26).slice(0, 26)} {usd(c.usd).padStart(8)} <Text color={c.usd >= 0 ? 'success' : 'error'}>{'█'.repeat(Math.max(0, Math.round((Math.abs(c.usd) / top) * width)))}</Text>
            </Text>
          ))}
          {stats.manual_pings && (stats.manual_pings.count ?? 0) > 0 && (
            <Text dimColor>
              {stats.manual_pings.count} manual ping{stats.manual_pings.count === 1 ? '' : 's'}, cost {usd(stats.manual_pings.ping_usd ?? 0)}
            </Text>
          )}
        </Box>

        <Box flexDirection="column">
          <Text bold>Cache</Text>
          {[{ ...v.main, id: 'main' }, ...v.subs].map(t => (
            <Text key={t.id}>
              <Text color={t.isWarm ? 'success' : 'warning'}>{t.isWarm ? '● warm' : '○ cold'}</Text> {t.id.slice(0, 14).padEnd(14)} {t.isWarm ? `${t.left} left` : 'rewrite on next send'}
              {t.tokens !== undefined && <Text dimColor>  {Math.round(t.tokens / 1000)}k tokens</Text>}
              {t.warmUsd !== undefined && t.coldUsd !== undefined && <Text dimColor>  {usd(t.warmUsd)} warm / {usd(t.coldUsd)} cold</Text>}
            </Text>
          ))}
        </Box>

        {v.main.isWarm && <Button key="ping" label="Ping now" variant="primary" hotkey="p" onPress={() => ping($)} />}
      </Box>
    )
  })
}
