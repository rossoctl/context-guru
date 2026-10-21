// Pure helpers for the Context Guru mod: turn the `cg.py cachestate --json` document into what the
// band draws. Nothing here touches `$`, so the tests can feed it a document and a clock.
import type { Snapshot, ThreadView, View } from '../types'

type Entry = {
  id?: string
  main?: boolean
  end_ts?: number
  last_ts?: number
  ttl_s?: number
  held_until_ms?: number
  prefix_tokens?: number
  rewrite_usd?: number
  next_warm_usd?: number
  next_cold_usd?: number
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
  const cold = num(e.next_cold_usd) ?? num(e.rewrite_usd)
  return {
    id: e.id ?? fallbackId,
    isWarm: left > 0,
    left: span(left),
    leftMs: Math.max(0, left),
    warmUsd: num(e.next_warm_usd),
    coldUsd: cold && cold > 0 ? cold : undefined,
    tokens: num(e.prefix_tokens),
    // #655: absent means an older server, which is treated as pingable
    isPingable: (e as { pingable?: unknown }).pingable !== false,
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
  const subs = list
    .filter(t => !t.main)
    .map((t, i) => thread(t, server, `agent${i + 1}`))
    .filter(t => t.isWarm || t.leftMs === 0)
  const m = thread(main, server, 'main')
  const money = doc.money
  return {
    url: snap.url,
    main: m,
    subs,
    saved: money ? (money.main?.saved_usd ?? 0) + (money.others?.saved_usd ?? 0) : undefined,
  }
}

/** The band's identity: the redraw happens only when this string changes. */
export const signature = (v: View | null): string =>
  v === null
    ? ''
    : JSON.stringify([v.url, v.main.isWarm, v.main.left, v.main.warmUsd, v.main.coldUsd, v.saved, v.subs.map(t => [t.id, t.isWarm, t.left])])

export const pingToast = (r: { status?: number; error?: string; saved_usd?: number; ping_usd?: number; retry_after_s?: number }): string => {
  if (r.status === 200) return `kept warm — saved ${usd(r.saved_usd ?? 0)} − ping ${usd(r.ping_usd ?? 0)}`
  if (r.status === 409 && r.error === 'not held') return 'keep-alive is off for this account \u2014 turn it on in the dashboard to ping'
  if (r.status === 409 && r.error === 'busy') return 'a request is running; it refreshes the cache itself'
  if (r.status === 409) return 'cache already cold \u2014 nothing to keep warm'
  if (r.status === 404 && r.error === 'unknown session') return 'this session is not known to the server yet'
  if (r.status === 404 || r.status === 405) return 'this Context Guru server does not support manual pings yet \u2014 update the proxy'
  if (r.status === 429 && r.error === 'no spare capacity') return `no spare capacity \u2014 try again in ${r.retry_after_s ?? 5}s`
  if (r.status === 429) return `pinged a moment ago \u2014 try again in ${r.retry_after_s ?? 30}s`
  if (r.status === 502) return 'the upstream model refused the ping'
  return `ping failed${r.error ? `: ${r.error}` : ''}`
}
