// Pure helpers for the Context Guru mod: turn the `cg.py cachestate --json` document into what the
// band and the pane draw. Nothing here touches `$`, so the tests can feed it a document and a clock.
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
  pingable?: boolean
  ping_reason?: string
}

const num = (x: unknown): number | undefined => (typeof x === 'number' && Number.isFinite(x) ? x : undefined)

/** The one money format (scripts/statusline.py `_money` and scripts/cg.py `usd` follow it, golden-tested on both sides):
 * `$12.34`, two decimals under $1 (`$0.06`), `<$0.01` under one cent, no decimals from $100. */
export const usd = (x: number): string => {
  if (x > 0 && x < 0.01) return '<$0.01'
  const sign = x <= -0.005 ? '-' : ''
  const a = Math.abs(x)
  return `${sign}$${a >= 100 ? a.toFixed(0) : a.toFixed(2)}`
}

/** "4m", "<1m", "1h05m": coarse on purpose, so the band redraws when the words change, not every poll. */
export function span(ms: number): string {
  const s = Math.max(0, Math.ceil(ms / 1000))
  if (s < 60) return '<1m'
  const m = Math.ceil(s / 60)
  return m < 60 ? `${m}m` : `${Math.floor(m / 60)}h${String(m % 60).padStart(2, '0')}m`
}

/** A gauge of `frac` (0..1) in `cells` cells: filled blocks, then the rest as light shade. */
export function gauge(frac: number, cells = 8): string {
  const on = Math.max(0, Math.min(cells, Math.round(frac * cells)))
  return '█'.repeat(on) + '░'.repeat(cells - on)
}

/** The three commands that save more (the pane's SAVE MORE section). */
export const TIPS = [
  { cmd: '/context-guru:insights', why: 'see where the money goes' },
  { cmd: '/context-guru:save-more', why: 'find a cheaper configuration' },
  { cmd: '/context-guru:remove-mcp-and-skills', why: 'drop MCP servers and skills you never use' },
] as const

const TIP_WHY: Record<string, string> = {
  'save-more': 'finds a cheaper config',
  'remove-mcp-and-skills': 'drops MCP servers and skills you never use',
}

/** The weekly tip (`cg.py tip`: the status line's own rule, last-run.json plus first-seen). Never rotates. */
export function weeklyTip(stale: readonly string[] | undefined): string | undefined {
  const first = stale?.find(c => c in TIP_WHY)
  if (!first) return undefined
  const more = stale!.length > 1 ? ` (+${stale!.length - 1} more)` : ''
  return `/context-guru:${first} hasn't run in 7+ days — ${TIP_WHY[first]}${more}`
}

/** Sub-agents stay in the band this long after their last response ended. */
export const AGENT_WINDOW_MS = 30 * 60_000

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
    agoMs: Math.max(0, now - end),
    warmUsd: num(e.next_warm_usd),
    coldUsd: cold && cold > 0 ? cold : undefined,
    tokens: num(e.prefix_tokens),
    saved: num(e.saved_usd),
  }
}

/** What the mod shows at local time `nowLocal`, or null when there is nothing to show (no data, not routed). */
export function view(snap: Snapshot | null, nowLocal: number): View | null {
  const doc = snap?.doc
  if (!snap || !doc || doc.error || !doc.last_ts) return null
  const server = (num(doc.now_ms) ?? snap.at) + (num(doc.age_s) ?? 0) * 1000 + (nowLocal - snap.at)
  const s = doc as Entry & { threads?: Entry[] }
  const list = s.threads ?? []
  const main = list.find(t => t.main) ?? s
  // Only real user sub-agents: `a:` threads, which Claude Code names with its agent-id header. A `p:` thread is
  // a side request (the auto-mode classifier, a title). One without a name is shown as nothing, not as a hex id.
  // A cold agent stays for 30 minutes, drawn dim: its price just went up and the user should see it.
  const subs = list
    .filter(t => !t.main && typeof t.id === 'string' && t.id.startsWith('a:') && typeof t.name === 'string' && t.name !== '')
    .map((t, i) => thread(t, server, `agent${i + 1}`))
    .filter(t => t.agoMs <= AGENT_WINDOW_MS)
    .sort((a, b) => a.agoMs - b.agoMs)
  const m = thread({ ...s, ...main, keepalive: s.keepalive }, server, 'main')
  const money = doc.money
  const pingable = typeof main.pingable === 'boolean' ? main.pingable : typeof s.pingable === 'boolean' ? s.pingable : undefined
  const reason = main.ping_reason ?? s.ping_reason
  const sum = (k: 'saved_usd' | 'baseline_usd'): number | undefined =>
    money ? (money.main?.[k] ?? 0) + (money.others?.[k] ?? 0) : undefined
  return {
    url: snap.url,
    main: m,
    subs,
    saved: sum('saved_usd'),
    baseline: sum('baseline_usd'),
    pingable,
    pingReason: pingable === false && typeof reason === 'string' && reason !== '' ? reason : undefined,
    tip: [...(snap.tip ?? [])],
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
        v.main.tokens,
        v.saved,
        v.baseline,
        v.pingable,
        v.pingReason,
        v.tip,
        v.subs.map(t => [t.id, t.name, t.isWarm, t.left, t.isPinged, t.isLastMinute, t.saved]),
      ])

/** Why the server holds nothing to ping with (cachestate `ping_reason`), in words. */
const REASONS: Record<string, string> = {
  'not opted in': "Ping isn't enabled for this install yet; it turns on in your next session (or run /context-guru:install)",
  'below limits': 'keep-alive is on, but this thread is below its minimum size or limits',
  'over budget': 'one ping would cost more than the per-ping limit',
  'too large': 'context too large to hold for Ping',
  unsupported: "Ping can't hold this session (extended thinking, or a gateway route)",
  'pings used': 'the planned keep-alive pings have run out',
  stopped: 'the proxy stopped keeping this thread warm after a miss or a refused ping',
  expired: 'cache already cold',
  'no entry': 'the server knows nothing about this session yet; send a prompt first',
  unavailable: 'the upstream route or credential is failing right now — try again shortly',
}

/** The same reasons in at most 17 characters, so `ping off: <reason>` (27) and `hide` fit one row at 60 columns.
 * A reason with no entry here (`no entry` is a timing artefact right after a reply, and a code from a newer
 * server is unknown) draws nothing in the band: the user cannot act on it. The pane and the toast keep the full text. */
const SHORT_REASONS: Record<string, string> = {
  'not opted in': 'on next session',
  'below limits': 'below limits',
  'over budget': 'over ping limit',
  'too large': 'context too large',
  unsupported: 'not supported',
  'pings used': 'pings used up',
  stopped: 'stopped, a miss',
  expired: 'cache cold',
  unavailable: 'upstream failing',
}

/** Every code the server can send with a full text: the tests walk it. */
export const REASON_CODES = Object.keys(REASONS)

/** A code from a newer server is never shown raw. */
export const reasonWords = (reason: string): string => REASONS[reason] ?? 'cannot ping this session right now'
export const shortReason = (reason: string | undefined): string | undefined => (reason ? SHORT_REASONS[reason] : undefined)

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

// ---- pane builders: text for the mode sections, from `cg.py panedata` ---------------------------------------------

/** A sparkline of `vals` (the tallest value gets the tallest block; zero is the lowest). */
export function spark(vals: readonly number[]): string {
  const top = Math.max(0, ...vals)
  const blocks = '▁▂▃▄▅▆▇█'
  return vals.map(v => (top > 0 && v > 0 ? blocks[Math.min(7, Math.floor((v / top) * 7.999))] : blocks[0])).join('')
}

/** One heat cell per hour: blank none, `░▒▓█` more misses, `·` an hour not reached yet. */
export function heatCells(hours: readonly number[], nowHour: number): string {
  const top = Math.max(0, ...hours)
  return hours
    .map((n, h) => (h > nowHour ? '·' : n <= 0 || top <= 0 ? ' ' : ' ░▒▓█'[Math.min(4, 1 + Math.floor((n / top) * 3.999))]))
    .join('')
}

/** A stacked bar of `cells` cells: `▒` always loaded, `█` conversation, `░` free. */
export function stack(frac: number, base: number | null | undefined, cells: number): string {
  const used = Math.round(Math.max(0, Math.min(1, frac)) * cells)
  const fixed = base == null ? 0 : Math.min(used, Math.round(Math.max(0, Math.min(1, base)) * cells))
  return '▒'.repeat(fixed) + '█'.repeat(used - fixed) + '░'.repeat(cells - used)
}

/** Seconds as words that cannot be read as a clock: `45s`, `12m`, `1h20m`. */
export function words(sec: number): string {
  const s = Math.max(0, Math.round(sec))
  return s < 60 ? `${s}s` : s < 3600 ? `${Math.floor(s / 60)}m` : `${Math.floor(s / 3600)}h${String(Math.floor((s % 3600) / 60)).padStart(2, '0')}m`
}
