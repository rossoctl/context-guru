// Pure helpers for the Context Guru mod: turn the `cg.py cachestate --json` document into what the
// band draws. Nothing here touches `$`, so the tests can feed it a document and a clock.
import type { Snapshot, ThreadView, View } from '../types'

type Entry = {
  id?: string
  name?: string
  main?: boolean
  end_ts?: number
  last_ts?: number
  ttl_s?: number
  held_until_ms?: number
  keepalive?: boolean
  prefix_tokens?: number
  rewrite_usd?: number
  next_warm_usd?: number
  next_cold_usd?: number
  saved_usd?: number
}

const num = (x: unknown): number | undefined => (typeof x === 'number' && Number.isFinite(x) ? x : undefined)

export const usd = (x: number): string => (x >= 100 ? `$${x.toFixed(0)}` : x >= 1 ? `$${x.toFixed(2)}` : `$${x.toFixed(3)}`)

/** "4m", "45s", "1h05m": coarse on purpose, so the band redraws when the words change, not every poll. */
export function span(ms: number): string {
  const s = Math.max(0, Math.ceil(ms / 1000))
  if (s < 60) return s <= 15 ? '<15s' : s <= 30 ? '<30s' : '<1m'
  const m = Math.ceil(s / 60)
  return m < 60 ? `${m}m` : `${Math.floor(m / 60)}h${String(m % 60).padStart(2, '0')}m`
}

/** A gauge of `frac` (0..1) in `cells` cells: filled blocks, then the rest as light shade. */
export function gauge(frac: number, cells = 8): string {
  const on = Math.max(0, Math.min(cells, Math.round(frac * cells)))
  return '█'.repeat(on) + '░'.repeat(cells - on)
}

/** The three commands that save more, one shown at a time (the band is at most three lines). */
export const TIPS = [
  { cmd: '/context-guru:insights', why: 'see where the money goes' },
  { cmd: '/context-guru:save-more', why: 'find a cheaper configuration' },
  { cmd: '/context-guru:remove-mcp-and-skills', why: 'drop MCP servers and skills you never use' },
] as const

export const tipIndex = (nowLocal: number): number => Math.floor(nowLocal / 60_000) % TIPS.length

function coldAt(e: Entry): number | undefined {
  const held = num(e.held_until_ms)
  if (held && held > 0) return held
  const end = num(e.end_ts) ?? num(e.last_ts)
  const ttl = num(e.ttl_s)
  return end !== undefined && ttl !== undefined ? end + ttl * 1000 : undefined
}

function thread(e: Entry, now: number, fallbackId: string): ThreadView {
  const at = coldAt(e)
  const left = at === undefined ? 0 : at - now
  const ttlMs = (num(e.ttl_s) ?? 300) * 1000
  const cold = num(e.next_cold_usd) ?? num(e.rewrite_usd)
  const end = num(e.end_ts) ?? num(e.last_ts) ?? 0
  const held = num(e.held_until_ms)
  return {
    id: e.id ?? fallbackId,
    name: e.name,
    isWarm: left > 0,
    // Kept warm by pings: the last touch was one, or the keep-alive plan holds it past its own entry.
    isPinged: left > 0 && (e.keepalive === true || (held !== undefined && held > end + ttlMs)),
    left: span(left),
    leftMs: Math.max(0, left),
    frac: Math.max(0, Math.min(1, left / ttlMs)),
    isLastMinute: left > 0 && left <= 60_000,
    warmUsd: num(e.next_warm_usd),
    coldUsd: cold && cold > 0 ? cold : undefined,
    tokens: num(e.prefix_tokens),
    saved: num(e.saved_usd),
  }
}

/** What the band shows at local time `nowLocal`, or null when there is nothing to show (no data, not routed). */
export function view(snap: Snapshot | null, nowLocal: number): View | null {
  const doc = snap?.doc
  if (!snap || !doc || doc.error || !doc.last_ts) return null
  const server = (num(doc.now_ms) ?? snap.at) + (num(doc.age_s) ?? 0) * 1000 + (nowLocal - snap.at)
  const s = doc as Entry & { threads?: Entry[] }
  const list = s.threads ?? []
  const main = list.find(t => t.main) ?? s
  // Only real user sub-agents: `a:` threads, which Claude Code names with its agent-id header. A `p:` thread is
  // a side request (the auto-mode classifier, a title). One without a name is shown as nothing, not as a hex id.
  const subs = list
    .filter(t => !t.main && typeof t.id === 'string' && t.id.startsWith('a:') && typeof t.name === 'string' && t.name !== '')
    .map((t, i) => thread(t, server, `agent${i + 1}`))
    .filter(t => t.isWarm)
  const m = thread({ ...s, ...main, keepalive: s.keepalive }, server, 'main')
  const money = doc.money
  return {
    url: snap.url,
    main: m,
    subs,
    saved: money ? (money.main?.saved_usd ?? 0) + (money.others?.saved_usd ?? 0) : undefined,
    tip: tipIndex(nowLocal),
  }
}

/** The band's identity: the redraw happens only when this string changes. */
export const signature = (v: View | null): string =>
  v === null
    ? ''
    : JSON.stringify([
        v.url,
        v.main.isWarm,
        v.main.isPinged,
        v.main.left,
        v.main.warmUsd,
        v.main.coldUsd,
        v.saved,
        v.tip,
        v.subs.map(t => [t.id, t.name, t.left, t.isPinged]),
      ])

/** Why the server holds nothing to ping with (cachestate `ping_reason`), in words. */
export const reasonWords = (reason: string): string =>
  reason === 'not opted in'
    ? 'turn on Ping: reload Claude Code after updating the plugin'
    : reason === 'too large'
      ? 'context too large to hold for Ping'
      : reason === 'expired'
        ? 'cache already cold'
        : reason === 'over budget'
          ? 'one ping would cost more than the per-ping limit'
          : reason === 'unsupported'
            ? 'this session cannot be pinged (extended thinking)'
            : reason

/** The main thread's `ping_reason` from the last cachestate read, when the server gave one. */
export function pingReason(snap: Snapshot | null): string | undefined {
  const doc = snap?.doc as { ping_reason?: unknown; threads?: { main?: boolean; ping_reason?: unknown }[] } | undefined
  const main = doc?.threads?.find(t => t.main)
  const r = main?.ping_reason ?? doc?.ping_reason
  return typeof r === 'string' && r !== '' ? r : undefined
}

export const pingToast = (
  r: { status?: number; error?: string; saved_usd?: number; ping_usd?: number; retry_after_s?: number },
  reason?: string,
): string => {
  if (r.status === 200) return `kept warm — saved ${usd(r.saved_usd ?? 0)} net (ping cost ${usd(r.ping_usd ?? 0)})`
  if (r.status === 409 && r.error === 'not held' && reason) return `can't ping: ${reasonWords(reason)}`
  if (r.status === 409 && r.error === 'not held') return 'keep-alive is off for this account — turn it on in the dashboard to ping'
  if (r.status === 409 && r.error === 'busy') return 'a request is running; it refreshes the cache itself'
  if (r.status === 409 && (r.error === 'over budget' || r.error === 'too costly')) {
    return 'one ping would cost more than the keep-alive per-ping limit, so nothing was sent'
  }
  if (r.status === 409 && r.error === 'cold') return 'cache already cold — nothing to keep warm'
  if (r.status === 409) return `ping refused: ${r.error ?? 'no detail'}`
  if (r.status === 404 && r.error === 'unknown session') return 'this session is not known to the server yet'
  if (r.status === 404 || r.status === 405) return 'this Context Guru server does not support manual pings yet — update the proxy'
  if (r.status === 429 && r.error === 'no spare capacity') return `no spare capacity — try again in ${r.retry_after_s ?? 5}s`
  if (r.status === 429) return `pinged a moment ago — try again in ${r.retry_after_s ?? 30}s`
  if (r.status === 502) return 'the upstream model refused the ping'
  return `ping failed${r.error ? `: ${r.error}` : ''}`
}
