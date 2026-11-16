// Pure helpers for the Context Guru mod: turn the `cg.py cachestate --json` document into what the
// band and the pane draw. Nothing here touches `$`, so the tests can feed it a document and a clock.
import type { Snapshot, ThreadView, View } from '../types'

type Entry = {
  id?: string
  name?: string
  main?: boolean
  model?: string
  end_ts?: number
  last_ts?: number
  ttl_s?: number
  held_until_ms?: number
  pings_left?: number
  next_ping_ms?: number
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

/** A token count as the status line writes it (statusline.py `_human`): `85.0k`, `1.2M`, `593`. */
export const tokens = (n: number): string => (Math.abs(n) >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : Math.abs(n) >= 1e3 ? `${(n / 1e3).toFixed(1)}k` : `${Math.round(n)}`)

/** The learned 5-hour weights (`cg.py cachestate` `fivehour_weights`, from GET /api/me/fivehour-weights, #1047): the
 * share of the 5-hour limit (0..1) that one token of each tier uses. A tier the fit did not learn carries the API
 * price, scaled to the account's spend (`learned` false). */
export type FiveHourWeights = {
  read: number
  write5m?: number
  write1h?: number
  learned5m: boolean
  learned1h: boolean
  /** How many requests the fit used. */
  requests: number
}

const pos = (x: unknown): number | undefined => (typeof x === 'number' && Number.isFinite(x) && x > 0 ? x : undefined)

/** The weights from a `fivehour_weights` document, or undefined unless the fit passed its gate (`ok`) with a learned
 * cache-read weight and at least one learned cache-write weight, each a positive number. cg.py checks the same: either
 * side failing gives the token figure (fail open). */
export function fiveHourWeights(x: unknown): FiveHourWeights | undefined {
  const f = x as { ok?: unknown; weights?: Record<string, unknown>; learned?: Record<string, unknown>; requests?: unknown } | null | undefined
  if (!f || typeof f !== 'object' || f.ok !== true || !f.weights || typeof f.weights !== 'object' || !f.learned || typeof f.learned !== 'object') return undefined
  const read = pos(f.weights.cache_read)
  const write5m = pos(f.weights.cache_write_5m)
  const write1h = pos(f.weights.cache_write_1h)
  const learned5m = f.learned.cache_write_5m === true && write5m !== undefined
  const learned1h = f.learned.cache_write_1h === true && write1h !== undefined
  if (read === undefined || f.learned.cache_read !== true || (!learned5m && !learned1h)) return undefined
  return { read, write5m, write1h, learned5m, learned1h, requests: num(f.requests) ?? 0 }
}

/** The LEARNED write weight for this entry's tier (ttl_s 3600 is the 1-hour tier, 300 the 5-minute one), or undefined
 * when the fit did not learn that tier. A weight the fit did not learn is the API price scaled to the account's spend,
 * a guess, so the band does not show it as a share (owner's decision, David Amid, #1135, 2026-11-12). With the tier
 * unknown: the tier the fit learned (5-minute when it learned both). */
function writeWeight(w: FiveHourWeights, tier: '5m' | '1h' | undefined): number | undefined {
  if (tier === '1h') return w.learned1h ? w.write1h : undefined
  if (tier === '5m') return w.learned5m ? w.write5m : undefined
  return w.learned5m ? w.write5m : w.write1h
}

/** A share of the 5-hour limit in percent, as the band writes it: `≈0.1%`, `≈1.5%`, `≈12%`, and `<0.1%` for a tiny
 * one. `≈` because the weights are an estimate. */
export function sharePct(pct: number): string {
  if (!(pct >= 0.05)) return '<0.1%'
  return pct < 9.95 ? `≈${pct.toFixed(1)}%` : `≈${Math.round(pct)}%`
}

/** The next message's price in the unit the user pays in.
 *
 * - `usd` (API key): the price if the cache is warm and the price if it is cold.
 * - `share` (Claude subscription, learned 5-hour weights that pass their checks): the share of the 5-hour limit if the
 *   cache is warm (the cached prefix read, times the read weight) and if it is cold (the prefix written, times the
 *   write weight for this entry's tier). The band and the pane draw it as they draw `usd`, with `of 5h` (#1095).
 * - `tokens` (Claude subscription, no weights): ONE figure, for the state the cache is in now. A warm cache reads the
 *   cached prefix (`90.0k rd`, success color). A cold one writes it again (`90.0k wr`, warning color). The two counts
 *   are the same prefix, so a `rd · wr if cold` pair says nothing about the cost (#1095). */
export type NextPrice =
  | { kind: 'usd'; warm?: string; cold?: string }
  | { kind: 'share'; warm?: string; cold?: string; coldIsTokens: boolean }
  | { kind: 'tokens'; now?: string; color: 'success' | 'warning' }

export function nextPrice(m: ThreadView, isSubscription: boolean, weights?: FiveHourWeights): NextPrice {
  if (!isSubscription) return { kind: 'usd', warm: m.warmUsd === undefined ? undefined : usd(m.warmUsd), cold: m.coldUsd === undefined ? undefined : usd(m.coldUsd) }
  const color = m.isWarm ? 'success' : 'warning'
  const n = m.tokens
  if (n === undefined || n < 1) return { kind: 'tokens', color }
  // The proxy priced this state (as before): without its price there is no figure.
  if (weights) {
    // The cold figure stays in tokens (`90.0k wr`, `coldIsTokens`) when the fit did not learn this tier's write weight.
    const ww = writeWeight(weights, m.writeTier)
    const warm = m.warmUsd === undefined ? undefined : sharePct(100 * n * weights.read)
    const cold = m.coldUsd === undefined ? undefined : ww === undefined ? `${tokens(n)} wr` : sharePct(100 * n * ww)
    // With no price for either state there is no share: fall through to the one token figure (or `unknown`).
    if (warm !== undefined || cold !== undefined) return { kind: 'share', warm, cold, coldIsTokens: ww === undefined }
  }
  const priced = m.isWarm ? m.warmUsd !== undefined : m.coldUsd !== undefined
  if (!priced) return { kind: 'tokens', color }
  return { kind: 'tokens', now: `${tokens(n)} ${m.isWarm ? 'rd' : 'wr'}`, color }
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

/** The commands the band's last row rotates through, each with a few words on what it does. */
export const ROTATE = [
  { cmd: '/context-guru:insights', why: 'see where your money goes' },
  { cmd: '/context-guru:save-more', why: 'find a cheaper configuration' },
  { cmd: '/context-guru:remove-mcp-and-skills', why: 'drop unused MCP and skills' },
] as const
/** How long each command stays on the band's last row before the next one. */
export const ROTATE_MS = 12_000

/** The command on the band's last row at `now`: one per ROTATE_MS, in order, the same on every redraw within it. */
export const rotatingTip = (now: number): (typeof ROTATE)[number] => ROTATE[Math.floor(now / ROTATE_MS) % ROTATE.length]

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
    isKeepalive: e.keepalive === true,
    left: span(left),
    leftMs: Math.max(0, left),
    frac: Math.max(0, Math.min(1, left / ttlMs)),
    isLastMinute: left > 0 && left <= 60_000,
    agoMs: Math.max(0, now - end),
    warmUsd: num(e.next_warm_usd),
    coldUsd: cold && cold > 0 ? cold : undefined,
    tokens: num(e.prefix_tokens),
    writeTier: e.ttl_s === 3600 ? '1h' : e.ttl_s === 300 ? '5m' : undefined,
    saved: num(e.saved_usd),
    isHeld: num(e.held_until_ms) !== undefined,
    // What the per-second clock is computed from: a newer document that moves it must replace the stored one.
    anchor: [num(e.last_ts), num(e.end_ts), num(e.ttl_s), num(e.held_until_ms), num(e.pings_left), num(e.next_ping_ms)].join(','),
    ...clockOf(e, now),
    nextPingS: nextPingIn(e, now),
  }
}

/** Seconds until the next automatic ping, only while it fires BEFORE the entry expires (so the cache stays warm
 * without a press); undefined otherwise: no pings left, none planned, the next one too late, or an old proxy that
 * sends no `next_ping_ms` (then the rule is the old one: fail open, the button shows). */
function nextPingIn(e: Entry, now: number): number | undefined {
  const left = num(e.pings_left)
  const at = num(e.next_ping_ms)
  const rem = clockOf(e, now).remS
  if (left === undefined || left <= 0 || at === undefined || at <= 0 || rem === undefined || rem <= 0) return undefined
  return at < now + rem * 1000 ? Math.max(0, (at - now) / 1000) : undefined
}

/** The one rule for the band's Ping button and the pane's "Ping now": show it when the cache is warm, the proxy can
 * ping, and no automatic ping fires before the cache expires. */
export const showPing = (v: View): boolean => v.main.isWarm && v.pingable !== false && v.main.nextPingS === undefined

/** While the button is hidden for a planned ping: `↻ next ping 4:12` (`↻ ping 4:12` on a narrow band), `↻ pinging…` at 0:00. */
export const nextPingWords = (v: View, narrow: boolean): string | undefined => {
  const s = v.main.nextPingS
  if (!v.main.isWarm || v.pingable === false || s === undefined) return undefined
  // The planned time has passed and the next poll has not brought a new plan: the ping is going out now.
  if (s < 1) return '↻ pinging…'
  return `↻ ${narrow ? '' : 'next '}ping ${clock(s)}`
}

/** The status line's cache clock (statusline.py remaining_seconds, held_seconds, _until_cold): seconds until the
 * entry expires from the later of its start and end, negative once cold; the keep-alive plan's horizon while the
 * entry itself is still warm. A plan cannot revive an expired entry. */
function clockOf(e: Entry, now: number): { remS?: number; untilColdS?: number; ttlS: number } {
  const last = num(e.last_ts) ?? num(e.end_ts)
  const ttl = num(e.ttl_s)
  const ttlS = ttl && ttl > 0 ? ttl : 300
  if (last === undefined || !ttl || ttl <= 0) return { ttlS }
  const remS = (Math.max(last, num(e.end_ts) ?? 0) + ttl * 1000 - now) / 1000
  const held = num(e.held_until_ms)
  const heldS = held === undefined ? undefined : (held - now) / 1000
  return { remS, untilColdS: heldS !== undefined && remS > 0 && heldS > remS ? heldS : remS, ttlS }
}

/** `m:ss`, or `h:mm:ss` from one hour (statusline.py `_clock`). */
export function clock(sec: number): string {
  const s = Math.floor(Math.max(0, sec))
  const mm = String(Math.floor((s % 3600) / 60)).padStart(2, '0')
  const ss = String(s % 60).padStart(2, '0')
  return s >= 3600 ? `${Math.floor(s / 3600)}:${mm}:${ss}` : `${Math.floor(s / 60)}:${ss}`
}

export const CACHE_BAR_CELLS = 6
export const CTX_BAR_CELLS = 8

/** The status line's `cache` element, as parts the band colours: `cache ● ████░░ 4:12 ↻`, or dim `cache ○ cold 1:07:04`
 * (the clock then counts up how long it has been cold). Green while warm, amber in the last minute (or the last fifth of
 * the TTL), red in the last 15 s with `!`, cyan while pings hold it. Undefined when no clock is known. */
export function cacheClock(t: ThreadView): { isCold: boolean; glyph: string; bar: string; time: string; color?: string; ping: boolean } | undefined {
  if (t.remS === undefined || t.untilColdS === undefined) return undefined
  if (t.untilColdS <= 0) return { isCold: true, glyph: '○', bar: '', time: clock(-t.remS), ping: false }
  const cells = t.remS > 0 ? Math.max(0, Math.min(CACHE_BAR_CELLS, Math.round((t.remS / t.ttlS) * CACHE_BAR_CELLS))) : 0
  const left = t.untilColdS
  const critical = left <= 15
  const expiring = left <= Math.max(60, t.ttlS / 5)
  // statusline.py: `↻` while the last touch was a ping or a keep-alive plan exists, the entry itself still warm.
  const pinged = t.remS > 0 && (t.isKeepalive || t.isHeld)
  // OSS `_cache_segment`: cyan while pinged, except in the critical last 15 s, which keep red and `!`.
  const color = pinged && !critical ? 'cyan' : critical ? 'error' : expiring ? 'warning' : 'success'
  return {
    isCold: false,
    glyph: expiring ? '◐' : '●',
    bar: '█'.repeat(cells) + '░'.repeat(CACHE_BAR_CELLS - cells),
    time: clock(left) + (critical ? '!' : ''),
    color,
    ping: pinged,
  }
}

/** The status line's `ctx` element: (used, window) tokens, preferring the proxy's figures as statusline.py
 * `_ctx_numbers` does: its token count while its copy is a few seconds old (Claude Code updates its own only after a
 * response it accepts), its window when MEASURED for the model the main thread runs. Undefined when a figure is
 * missing or the context is larger than the window (that window is the wrong one; a clamped 100% would be a guess). */
export function ctxNumbers(v: View, engine: { tokens?: number; window?: number } | undefined): { used: number; window: number } | undefined {
  let used = engine?.tokens
  let window = engine?.window
  if (v.ageS <= CTX_PROXY_FRESH_S && (v.main.tokens ?? 0) > 0) used = v.main.tokens
  if (v.window !== undefined && v.window > 0) window = v.window
  if (used === undefined) used = v.main.tokens
  if (used === undefined || !window || window <= 0 || used > window) return undefined
  return { used, window }
}
const CTX_PROXY_FRESH_S = 11 // statusline.py CTX_PROXY_FRESH_SECONDS: the cachestate TTL plus a second

/** The `ctx` bar: green to half, amber to 70%, red above; `⚠ rewrite near` from 85%. */
export function ctxBar(used: number, window: number): { bar: string; pct: string; color: string; isNear: boolean } {
  const f = Math.max(0, Math.min(1, used / window))
  const on = Math.round(f * CTX_BAR_CELLS)
  return {
    bar: '█'.repeat(on) + '░'.repeat(CTX_BAR_CELLS - on),
    pct: `${Math.round(f * 100)}%`,
    color: f <= 0.5 ? 'success' : f <= 0.7 ? 'warning' : 'error',
    isNear: f >= 0.85,
  }
}

/** `saved $x of $y`, or undefined when a half is missing or nothing is priced yet (statusline.py `_money_pair`).
 * A saving never reads below $0.00: a session that has not yet earned back what it spent shows `saved $0.00`. */
export function moneyPair(x: number | undefined, y: number | undefined): { saved: string; of: string } | undefined {
  if (x === undefined || y === undefined || y < 0.005) return undefined
  return { saved: usd(Math.max(0, x)), of: usd(y) }
}


/** The main thread's entry, as statusline.py `_main_cs` builds it: the session entry describes the latest touch on
 * ANY thread, so its fields give way to the main thread's own. The keep-alive plan and the next-message prices are the
 * main thread's or none, never a sub-agent's; `keepalive` (the last touch was a ping) counts only when that touch was
 * the main thread's. */
function mainEntry(s: Entry, main: Entry): Entry {
  if (main === s) return s
  const out: Entry = { ...s }
  for (const k of ['last_ts', 'end_ts', 'ttl_s', 'prefix_tokens', 'rewrite_usd'] as const) if (main[k] !== undefined) (out as any)[k] = main[k]
  for (const k of ['held_until_ms', 'pings_left', 'next_ping_ms', 'next_warm_usd', 'next_cold_usd'] as const) (out as any)[k] = main[k]
  out.keepalive = s.keepalive === true && s.last_ts === main.last_ts
  return out
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
  const m = thread(mainEntry(s, main), server, 'main')
  const money = doc.money
  const pingable = typeof main.pingable === 'boolean' ? main.pingable : typeof s.pingable === 'boolean' ? s.pingable : undefined
  const reason = main.ping_reason ?? s.ping_reason
  const sum = (k: 'saved_usd' | 'baseline_usd'): number | undefined =>
    money ? (money.main?.[k] ?? 0) + (money.others?.[k] ?? 0) : undefined
  // The window the proxy MEASURED for the model it served, only for the main thread's own model (statusline.py
  // `_main_cs`: a sub-agent on a 200k model must not divide a 1M context).
  const d = doc as { window?: unknown; window_exact?: unknown; model?: unknown }
  const mm = (main as { model?: unknown }).model
  const sameModel = main === s || mm === d.model || main.last_ts === s.last_ts // OSS `_main_cs`: dropped only when the latest touch is another thread on another model
  const window = d.window_exact === true && sameModel ? num(d.window) : undefined
  const mp = (doc as { manual_pings?: { count?: unknown; saved_usd?: unknown } }).manual_pings
  return {
    url: snap.url,
    main: m,
    subs,
    saved: sum('saved_usd'),
    baseline: sum('baseline_usd'),
    mainSaved: num(money?.main?.saved_usd),
    mainBaseline: num(money?.main?.baseline_usd),
    othersSaved: num(money?.others?.saved_usd),
    othersBaseline: num(money?.others?.baseline_usd),
    others: num((money?.others as { threads?: unknown } | undefined)?.threads) ?? 0,
    pingsSaved: (num(mp?.count) ?? 0) > 0 ? num(mp?.saved_usd) : undefined,
    ageS: num(doc.age_s) ?? 0,
    isStale: doc.stale === true,
    window,
    update: typeof doc.update === 'string' && doc.update !== '' ? doc.update : undefined,
    windowSaved: typeof doc.window_saved === 'string' && doc.window_saved !== '' ? doc.window_saved : undefined,
    pingable,
    pingReason: pingable === false && typeof reason === 'string' && reason !== '' ? reason : undefined,
    tip: [...(snap.tip ?? [])],
    weights: fiveHourWeights(doc.fivehour_weights),
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
        v.main.anchor,
        v.main.nextPingS === undefined,
        v.main.left,
        v.main.warmUsd,
        v.main.coldUsd,
        v.main.tokens,
        v.saved,
        v.baseline,
        v.mainSaved,
        v.mainBaseline,
        v.othersSaved,
        v.othersBaseline,
        v.others,
        v.pingsSaved,
        v.isStale,
        v.window,
        v.update,
        v.windowSaved,
        v.pingable,
        v.pingReason,
        v.tip,
        v.weights,
        v.subs.map(t => [t.id, t.name, t.isWarm, t.left, t.isPinged, t.isLastMinute, t.saved]),
      ])

/** Why the server holds nothing to ping with (cachestate `ping_reason`), in words. */
const REASONS: Record<string, string> = {
  'not opted in': "Ping isn't enabled for this install yet; it turns on in your next session (or run /context-guru:install)",
  'below limits': 'keep-alive is on, but this thread is below its minimum size or limits',
  'over budget': 'one ping would cost more than the per-ping limit',
  'too large': 'context too large to hold for Ping',
  unsupported: "Ping cannot hold this session (extended thinking, or a route Ping does not support)",
  'pings used': 'the planned keep-alive pings have run out',
  done: 'this agent has finished, so there is nothing to keep warm',
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
  done: 'finished',
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
