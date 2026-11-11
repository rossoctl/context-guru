// The pure logic of the status bar: formatting, state, session pick and back-off.
// No `vscode` import here, so `node --test` can run it. It uses only counts and times.

// Bob's context window, from the Bob book. Used when the proxy does not know the window.
export const BOB_WINDOW = 270_000
export const DOWN_POLL_MS = 60_000
export const DEFAULT_POLL_S = 15
const MIN_POLL_S = 5
export const LOOPBACK = '127.0.0.1'
export const DEFAULT_RESET = '~/.local/state/context-guru-bob/context-guru-reset'
// The same session id shape the plugin's signal hook accepts.
const SESSION_ID = /^[A-Za-z0-9._:-]{1,128}$/

export type State = 'ok' | 'down' | 'not_installed'

type Money = { saved_usd?: number }

// The fields of GET /api/cachestate (dash/cachestate.go) that the status bar reads.
export type CacheState = {
  none?: boolean
  end_ts?: number
  now_ms?: number
  ttl_s?: number
  prefix_tokens?: number
  window?: number
  priced?: boolean
  money?: { main?: Money, others?: Money }
}

export type Snapshot = {
  state: State
  port: number
  // The local clock when the replies came. The countdown runs from here.
  polledAtLocal: number
  session?: string
  // Tokens saved in the session (`saved` of the /api/sessions row).
  saved?: number
  cache?: CacheState
  resetPath?: string
}

export type View = { text: string, tooltip: string }

export type MenuItem = { label: string, detail: string, command?: string, dashboard?: boolean }

const SCRIPT = 'python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py"'

// Every command here only reads. The menu never runs a command that changes state.
export const MENU: MenuItem[] = [
  { label: 'Status', detail: 'Is Bob routed and is the proxy up?', command: `${SCRIPT} status` },
  { label: 'Doctor', detail: 'Read-only checks, each with a fix.', command: `${SCRIPT} doctor` },
  { label: 'Open dashboard', detail: 'The local context-guru dashboard.', dashboard: true },
  { label: 'Preset', detail: 'Show the preset in use.', command: `${SCRIPT} configure --show` },
]

function num(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined
}

export function formatTokens(n: number): string {
  if (n < 1000) return String(Math.round(n))
  if (n < 1_000_000) return `${trim(n / 1000)}k`
  return `${trim(n / 1_000_000)}M`
}

// One decimal under 10, none above: 3.1k, 21k, 270k.
function trim(value: number): string {
  return value < 10 ? String(Math.round(value * 10) / 10) : String(Math.round(value))
}

export function formatCtx(prefixTokens: number | undefined, window: number | undefined): string {
  const size = window && window > 0 ? window : BOB_WINDOW
  const used = prefixTokens === undefined ? '—' : formatTokens(prefixTokens)
  return `ctx ${used}/${formatTokens(size)}`
}

// The time left on the cache. The server's clock sets the start; the local clock only counts
// the time since the poll, so a skewed laptop clock does not matter.
export function remainingMs(cache: CacheState, polledAtLocal: number, nowLocal: number): number {
  const end = num(cache.end_ts) ?? 0
  const ttl = num(cache.ttl_s) ?? 0
  const serverNow = num(cache.now_ms) ?? end
  return end + ttl * 1000 - serverNow - (nowLocal - polledAtLocal)
}

export function formatClock(remaining: number, ttlS: number): string {
  if (remaining <= 0) return 'cold'
  const seconds = Math.ceil(remaining / 1000)
  const minutes = Math.floor(seconds / 60)
  const rest = String(seconds % 60).padStart(2, '0')
  if (ttlS >= 3600) return `1h ${String(minutes).padStart(2, '0')}:${rest}`
  return `${Math.round(ttlS / 60)}m ${minutes}:${rest}`
}

export function formatSaved(saved: number | undefined, cache: CacheState | undefined): string {
  // undefined is "no data" (no session row, or a failed read), not zero.
  if (saved === undefined) return 'saved —'
  const tokens = saved > 0 ? `saved ${formatTokens(saved)} tok` : 'saved 0'
  // Money only when the proxy has a price. Bob has none, so it never shows `$` today.
  if (cache?.priced !== true || !cache.money) return tokens
  const usd = (num(cache.money.main?.saved_usd) ?? 0) + (num(cache.money.others?.saved_usd) ?? 0)
  return `${tokens} · $${usd.toFixed(2)}`
}

export function parsePort(text: string | undefined): number {
  if (text === undefined) return 0
  try {
    const port = JSON.parse(text)?.port
    return Number.isInteger(port) && port > 0 && port < 65536 ? port : 0
  } catch {
    return 0
  }
}

// The newest Bob session: /api/sessions lists the newest first.
export function pickSession(reply: unknown): string | undefined {
  const rows = (reply as { sessions?: unknown })?.sessions
  if (!Array.isArray(rows) || rows.length === 0) return undefined
  const id = (rows[0] as { session_id?: unknown })?.session_id
  return typeof id === 'string' && SESSION_ID.test(id) ? id : undefined
}

export function loopbackUrl(host: string, port: number, path: string): string {
  if (host !== LOOPBACK) throw new Error(`refused: ${host} is not the loopback address`)
  if (!Number.isInteger(port) || port <= 0 || port >= 65536) throw new Error('refused: bad port')
  if (!path.startsWith('/')) throw new Error('refused: bad path')
  return `http://${LOOPBACK}:${port}${path}`
}

export function nextPollMs(state: State, pollSeconds: number): number {
  if (state !== 'ok') return DOWN_POLL_MS
  const seconds = Number.isFinite(pollSeconds) && pollSeconds > 0 ? pollSeconds : DEFAULT_POLL_S
  return Math.max(MIN_POLL_S, seconds) * 1000
}

// A failed poll gives no snapshot. Show `CG ✗`, not the old text with a clock still ticking.
export function renderView(snap: Snapshot | undefined, nowLocal: number): View {
  if (snap) return render(snap, nowLocal)
  return { text: 'CG ✗', tooltip: 'context-guru could not read its state. Run /context-guru-doctor.' }
}

export function render(snap: Snapshot, nowLocal: number): View {
  if (snap.state === 'not_installed') {
    return { text: 'CG –', tooltip: 'context-guru is not set up for Bob. Run /context-guru-setup.' }
  }
  if (snap.state === 'down') {
    const reset = snap.resetPath ?? DEFAULT_RESET
    return { text: 'CG ✗', tooltip: `The context-guru proxy at ${LOOPBACK}:${snap.port} does not ` +
      `answer. Fix: run ${reset} --yes in a terminal, or run /context-guru-doctor.` }
  }
  const cache = snap.cache
  const touched = cache !== undefined && cache.none !== true
  const ttl = num(cache?.ttl_s) ?? 0
  const clock = touched ? formatClock(remainingMs(cache, snap.polledAtLocal, nowLocal), ttl) : '—'
  const ctx = formatCtx(touched ? num(cache.prefix_tokens) : undefined, num(cache?.window))
  const text = `CG ✓ ${ctx} · cache ${clock} · ${formatSaved(snap.saved, cache)}`
  const lines = [`context-guru proxy: ${LOOPBACK}:${snap.port}`,
    snap.session ? 'Latest Bob session.' : 'No Bob session yet.',
    'Click for Status, Doctor, Open dashboard, Preset.']
  return { text, tooltip: lines.join('\n') }
}
