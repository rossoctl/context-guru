// Context Guru mod: a calm band above the prompt and a pane (/context-guru).
//
// One home per fact (the status line owns the money, the cache clock and the context bar):
//   band row 1  next-message price (warm vs cold), Ping or why it is off, hide (the dashboard link is in the pane)
//   band row 2  one chip per named sub-agent cache (default on; `a` or the pane button turns it off)
//   band row 3  the weekly tip, only when a command is stale
//   pane        next message, agents table, saved lately, cache hits, misses by hour, context, save more
// A cold cache is drawn (dim), never removed: the next message pays the rewrite and the user should see it.
//
// The mod never touches the network. Every read and the one write go through scripts/cg.py, which owns
// auth, the routed port and TLS trust:
//   cg.py cachestate --json      reads the status line's on-disk cache (single-flight, backed off)
//   cg.py panedata               the pane's mode sections (spark, strip, heat, fill), from the same caches
//   cg.py tip                    the commands not run for a week (the status line's last-run.json rule)
//   cg.py ping --session ID --json   one explicit POST /api/me/cache/ping, on a button press
import { atom, read, update } from 'claude-code'
import type { EngineInterface as Engine, Register } from 'claude-code'

import type { PaneData, Snapshot, ThreadView, View } from '../types'
import { TIPS, heatCells, pingReason, pingToast, reasonWords, shortReason, signature, spark, stack, usd, view, weeklyTip, words } from './model'

const PANE = 'context-guru'
const POLL_MS = 15_000
const TIP_EVERY_MS = 10 * 60_000
const PANE_EVERY_MS = 60_000
const AGENT_CHIPS = 4

const snapshot = atom({ plugin: 'context-guru', key: 'snapshot' } as const, null)
const isPinging = atom({ plugin: 'context-guru', key: 'isPinging' } as const, false)
const isHidden = atom({ plugin: 'context-guru', key: 'isHidden' } as const, false)
const isAgentsOff = atom({ plugin: 'context-guru', key: 'isAgentsOff' } as const, false)
const paneData = atom({ plugin: 'context-guru', key: 'paneData' } as const, null)
const lastPing = atom({ plugin: 'context-guru', key: 'lastPing' } as const, '')

let seen = ''
let transcript = '' // this session's transcript path, from the classic hooks: it names the sub-agents
let tipAt = -Infinity
let tipStale: string[] | undefined
let paneAt = -Infinity
let paneOpen = false

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
    if (paneOpen) await loadPane($, false)
  } catch {
    // a poll that fails leaves the band as it was
  }
}

/** The status line's refresher writes the first cache-state document a moment after the first reply. Until it
 * exists, ask again every 1.5 s, twelve times (about 15 s), so the band appears at once and not at the next 15 s poll. */
let soon = false // one loop at a time: a second turn while it runs returns at once
async function refreshSoon($: Engine): Promise<void> {
  if (soon) return
  soon = true
  try {
    for (let i = 0; i < 12; i++) {
      await refresh($)
      if ((await read($, snapshot))?.doc?.last_ts) return
      await $.clock.sleep(1500)
    }
  } catch {
    // the next 15 s poll asks again
  } finally {
    soon = false
  }
}

async function load($: Engine): Promise<void> {
  const doc = await helper($, ['cachestate', '--json', '--session', await $.session.id(), ...(transcript ? ['--transcript', transcript] : [])])
  const now = await $.clock.now()
  if (now - tipAt >= TIP_EVERY_MS) {
    tipAt = now
    const t = await helper($, ['tip'])
    tipStale = Array.isArray(t?.stale) ? (t!.stale as string[]) : []
  }
  // `cg.py cachestate --json` prints the raw /api/cachestate entry (+ age_s, stale), or {"error": ...}.
  // The dashboard is the loopback proxy this session is routed through (http://localhost:PORT/dashboard/); anything else shows no link.
  const base = /^(https?:\/\/(?:127\.0\.0\.1|localhost)(?::\d+)?)/.exec((await $.env.get('ANTHROPIC_BASE_URL')) ?? '')
  const snap: Snapshot | null = doc
    ? { at: now, url: base ? `${base[1].replace('127.0.0.1', 'localhost')}/dashboard/` : '', tip: tipStale, doc }
    : null
  // Redraw only when what the band says changes: the words, not the seconds.
  const sig = signature(view(snap, now))
  if (sig === seen) return
  seen = sig
  await update($, snapshot, () => snap)
}

async function loadPane($: Engine, force: boolean): Promise<void> {
  const now = await $.clock.now()
  if (!force && now - paneAt < PANE_EVERY_MS) return
  paneAt = now
  const data = await helper($, ['panedata', '--session', await $.session.id()])
  await update($, paneData, () => (data && !data.error ? (data as PaneData) : null))
}

/** One explicit ping through the helper; resolves the one-line outcome. Never starts a model turn. */
async function pingNow($: Engine): Promise<string> {
  if (await read($, isPinging)) return 'a ping is already in flight'
  await update($, isPinging, () => true)
  let text = ''
  try {
    const r = await helper($, ['ping', '--session', await $.session.id(), '--json'])
    text = pingToast(r ?? { status: 0, error: 'no answer from the helper' }, (r?.ping_reason as string | undefined) || pingReason(await read($, snapshot)))
    return text
  } finally {
    await update($, isPinging, () => false)
    await update($, lastPing, () => text)
    seen = ''
    await refresh($)
  }
}

async function ping($: Engine): Promise<void> {
  $.ui.toast(await pingNow($), { timeoutMs: 6000 })
}

async function setHidden($: Engine, hidden: boolean): Promise<void> {
  await update($, isHidden, () => hidden)
  try {
    await $.store.set(`hidden:${await $.session.id()}`, hidden)
  } catch {
    // remembering is a convenience
  }
}

async function setAgentsOff($: Engine, off: boolean): Promise<void> {
  await update($, isAgentsOff, () => off)
  try {
    await $.store.set('agents', off ? 'off' : 'on')
  } catch {
    // remembering is a convenience
  }
}

/** Glyph and colour of one cache: ● green warm, ◐ amber in the last minute, cyan with ↻ when pinged, ○ dim when cold. */
function state(t: ThreadView): { glyph: string; color?: string; dim: boolean; mark: string } {
  if (!t.isWarm) return { glyph: '○', dim: true, mark: '' }
  if (t.isPinged) return { glyph: '●', color: 'cyan', dim: false, mark: ' ↻' }
  if (t.isLastMinute) return { glyph: '◐', color: 'warning', dim: false, mark: '' }
  return { glyph: '●', color: 'success', dim: false, mark: '' }
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
    await $.command.register({ name: 'context-guru', description: 'Open the Context Guru pane: next message, agents, savings, cache hits, context. Shows the band again if it was hidden' })
    try {
      await update($, isHidden, () => false)
      await update($, isAgentsOff, () => false)
      if ((await $.store.get(`hidden:${await $.session.id()}`)) === true) await update($, isHidden, () => true)
      if ((await $.store.get('agents')) === 'off') await update($, isAgentsOff, () => true)
    } catch {
      // the stored choices are a convenience
    }
    void refreshSoon($)
    $.clock.every(POLL_MS, () => void refresh($))
    return started
  })

  on('turn.complete', async ($, e, next) => {
    void refreshSoon($) // a turn touched the cache: the countdown restarted; the first document may not exist yet
    return next(e)
  })

  on('command.run', { command: 'ping' }, async $ => ({ text: await pingNow($) }))

  on('command.run', { command: 'context-guru' }, async $ => {
    await setHidden($, false) // the pane command also brings a hidden band back
    paneOpen = true
    await $.ui.open({ id: PANE, title: 'Context Guru', focus: true })
    void loadPane($, true)
    return { text: 'Context Guru pane opened.' }
  })

  on('ui.close', async ($, e, next) => {
    if (e.id === PANE) paneOpen = false
    return next(e)
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const snap = await read($, snapshot)
    const now = await $.clock.now()
    const v = view(snap, now)
    if (e.props.hasSurvey || !v || (await read($, isHidden))) return next(e)

    const { Box, Text, Button } = $.ui.resolve(e)
    const busy = await read($, isPinging)
    const agentsOff = await read($, isAgentsOff)
    const m = v.main
    const rows = e.props.maxRows
    const narrow = (e.props.bodyColumns ?? 100) < 80 // row 1 must fit one line with `hide` at 60 columns
    const tip = weeklyTip(v.tip)
    const chips = v.subs.slice(0, AGENT_CHIPS)

    return (
      <Box flexDirection="column" paddingX={1} marginTop={1}>
        <Box flexWrap="wrap">
          <Text bold color="claude">{'◆'} </Text>
          {m.isWarm && (
            <Text>
              <Text dimColor>{narrow ? 'next ' : 'next message '}</Text>
              {m.warmUsd !== undefined && <Text color="success">{usd(m.warmUsd)}</Text>}
              {m.coldUsd !== undefined && <Text dimColor>{m.warmUsd !== undefined ? ' · ' : ''}{usd(m.coldUsd)}{narrow ? ' cold' : ' if cold'}</Text>}
            </Text>
          )}
          {!m.isWarm && (
            <Text>
              <Text dimColor>○ cache cold</Text>
              {m.coldUsd !== undefined && <Text dimColor> · next message </Text>}
              {m.coldUsd !== undefined && <Text color="warning">{usd(m.coldUsd)}</Text>}
            </Text>
          )}
          {m.isWarm && v.pingable !== false && (
            <Box>
              <Text> </Text>
              <Button key="ping" label={busy ? 'Pinging…' : 'Ping'} variant="primary" hotkey="p" onPress={() => (busy ? undefined : ping($))} />
            </Box>
          )}
          {m.isWarm && v.pingable === false && shortReason(v.pingReason) !== undefined && (
            <Text dimColor>{'  '}ping off{narrow ? '' : `: ${shortReason(v.pingReason)}`}</Text>
          )}
          <Text>{'  '}</Text>
          <Button key="hide" label="hide" plain dimColor onPress={() => setHidden($, true)} />
        </Box>
        {chips.length > 0 && rows > 2 && !agentsOff && (
          <Box flexWrap="wrap" columnGap={2}>
            <Text dimColor>{'└'} agents</Text>
            {chips.map(t => {
              const s = state(t)
              return (
                <Text key={t.id} color={s.color} dimColor={s.dim}>
                  {s.glyph} {t.name}
                  {t.isWarm && <Text dimColor> {t.left}{s.mark}</Text>}
                </Text>
              )
            })}
            {v.subs.length > chips.length && <Text dimColor>+{v.subs.length - chips.length}</Text>}
          </Box>
        )}
        {tip && rows > 3 && (
          <Text dimColor>
            tip: <Text color="suggestion">{tip}</Text>
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
    const data: PaneData = (await read($, paneData)) ?? {}
    const busy = await read($, isPinging)
    const hidden = await read($, isHidden)
    const agentsOff = await read($, isAgentsOff)
    const last = await read($, lastPing)
    const cols = e.props.bodyColumns ?? 60
    const bar = Math.max(10, Math.min(40, cols - 26))
    const m = v.main
    const tip = weeklyTip(v.tip)
    const head = (label: string) => <Text dimColor bold>{label}</Text>
    const nameWidth = Math.max(8, Math.min(20, cols - 34))

    return (
      <Box flexDirection="column" paddingX={1}>
        <Box flexDirection="column">
          <Text bold color="claude">{'◆'} Context Guru</Text>
          {v.url !== '' && <Link href={v.url} label="open dashboard ↗" />}
          {v.saved !== undefined && v.baseline !== undefined && v.baseline >= 0.005 && (
            <Text>
              <Text color="success">saved {usd(v.saved)}</Text>
              <Text dimColor> of {usd(v.baseline)} this session</Text>
            </Text>
          )}
        </Box>

        <Box flexDirection="column" paddingTop={1}>
          {head('NEXT MESSAGE')}
          <Text>
            <Text dimColor>if the cache is warm </Text>
            {m.warmUsd !== undefined ? <Text color="success">{usd(m.warmUsd)}</Text> : <Text dimColor>unknown</Text>}
          </Text>
          <Text>
            <Text dimColor>if it is cold      </Text>
            {m.coldUsd !== undefined ? <Text color={m.isWarm ? undefined : 'warning'}>{usd(m.coldUsd)}</Text> : <Text dimColor>unknown</Text>}
            {m.tokens !== undefined && <Text dimColor>  ({Math.round(m.tokens / 1000)}k tokens in the cached prefix)</Text>}
          </Text>
          {m.isWarm ? (
            <Text color={state(m).color}>
              {state(m).glyph} cache warm, about {m.left} left{state(m).mark ? ' · kept warm by pings ↻' : ''}
            </Text>
          ) : (
            <Text dimColor>○ cache cold: the next message writes it again</Text>
          )}
          {m.isWarm && v.pingable !== false && (
            <Box columnGap={2}>
              <Button key="ping" label={busy ? 'Pinging…' : 'Ping now'} variant="primary" hotkey="p" onPress={() => (busy ? undefined : ping($))} />
            </Box>
          )}
          {m.isWarm && v.pingable === false && <Text dimColor>ping off: {v.pingReason ? reasonWords(v.pingReason) : 'not available now'}</Text>}
          {last !== '' && <Text dimColor>last ping: {last}</Text>}
        </Box>

        {v.subs.length > 0 && (
          <Box flexDirection="column" paddingTop={1}>
            {head('AGENTS')}
            {v.subs.map(t => {
              const s = state(t)
              return (
                <Text key={t.id} color={s.color} dimColor={s.dim}>
                  {s.glyph} {(t.name ?? '').padEnd(nameWidth).slice(0, nameWidth)}
                  {'  '}
                  {(t.isWarm ? `${t.left}${s.mark}` : 'cold').padEnd(9)}
                  {t.saved !== undefined && t.saved > 0 ? `saved ${usd(t.saved)}` : ''}
                </Text>
              )
            })}
            <Box>
              <Button key="agents" label={agentsOff ? 'Show agents in the band  a' : 'Hide agents in the band  a'} plain hotkey="a" onPress={() => setAgentsOff($, !agentsOff)} />
            </Box>
          </Box>
        )}

        {data.spark && (
          <Box flexDirection="column" paddingTop={1}>
            {head('SAVED LATELY')}
            <Text>
              <Text color="success">{usd(data.spark.today)}</Text>
              <Text dimColor> today · last 14 days </Text>
              <Text color="success">{spark(data.spark.days)}</Text>
            </Text>
          </Box>
        )}

        {data.strip && (
          <Box flexDirection="column" paddingTop={1}>
            {head('CACHE HITS')}
            <Text>
              {data.strip.kinds.map((k, i) => (
                <Text key={i} color={k === 'hit' ? 'success' : k === 'part' ? 'warning' : 'error'}>{k === 'hit' ? '■' : k === 'part' ? '▨' : '✖'}</Text>
              ))}
            </Text>
            <Text dimColor>last {data.strip.kinds.length} requests{cols >= 56 ? ' · ■ hit ▨ partial ✖ miss' : cols >= 40 ? ' · ■ hit ▨ part ✖ miss' : ''}</Text>
            {data.strip.lost != null && data.strip.lost >= 0.005 && <Text dimColor>{usd(data.strip.lost)} lost today</Text>}
          </Box>
        )}

        {data.heat && (
          <Box flexDirection="column" paddingTop={1}>
            {head('MISSES BY HOUR')}
            <Text>
              <Text dimColor>0h </Text>
              <Text color="error">{heatCells(data.heat.hours, data.heat.now)}</Text>
              <Text dimColor> 24h</Text>
            </Text>
            <Text dimColor>{Math.max(0, ...data.heat.hours) > 0 ? (cols >= 44 ? 'darker means more misses in that hour' : 'darker = more misses') : 'no cache misses today'}</Text>
            {!data.strip && data.heat.lost != null && data.heat.lost >= 0.005 && <Text dimColor>{usd(data.heat.lost)} lost today</Text>}
          </Box>
        )}

        {data.fill && (
          <Box flexDirection="column" paddingTop={1}>
            {head('CONTEXT')}
            <Text>
              <Text color={data.fill.frac >= 0.85 ? 'error' : data.fill.frac >= 0.65 ? 'warning' : undefined}>{stack(data.fill.frac, data.fill.base, bar)}</Text>
              <Text> {Math.round(data.fill.frac * 100)}%</Text>
            </Text>
            {data.fill.base != null && (
              <Text dimColor>
                {cols >= 70
                  ? `▒ always loaded ${Math.round(data.fill.base * 100)}% · █ conversation ${Math.round(Math.max(0, data.fill.frac - data.fill.base) * 100)}% · ░ free`
                  : `▒ fixed ${Math.round(data.fill.base * 100)}% · █ chat ${Math.round(Math.max(0, data.fill.frac - data.fill.base) * 100)}% · ░ free`}
              </Text>
            )}
            {data.fill.eta_s ? <Text dimColor>full in {words(data.fill.eta_s)}</Text> : null}
            {data.fill.miss_usd ? <Text dimColor>a miss now costs {usd(data.fill.miss_usd)}</Text> : null}
          </Box>
        )}

        <Box flexDirection="column" paddingTop={1}>
          {head('SAVE MORE')}
          {tip && (
            <Text>
              <Text color="warning">tip </Text>
              <Text color="suggestion">{tip}</Text>
            </Text>
          )}
          {TIPS.map(t =>
            cols >= 80 ? (
              <Text key={t.cmd}>
                <Text color="suggestion">{t.cmd}</Text> <Text dimColor>{t.why}</Text>
              </Text>
            ) : (
              <Box key={t.cmd} flexDirection="column">
                <Text color="suggestion">{t.cmd}</Text>
                <Text dimColor>  {t.why}</Text>
              </Box>
            ),
          )}
        </Box>

        {hidden && <Box paddingTop={1}><Button key="show" label="Show the band again" plain onPress={() => setHidden($, false)} /></Box>}
      </Box>
    )
  })
}
