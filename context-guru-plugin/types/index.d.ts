export type ThreadView = {
  id: string
  /** The name Claude Code shows for a sub-agent (from `cg.py cachestate --transcript`). */
  name?: string
  isWarm: boolean
  /** Kept warm by pings (the last touch was one, or a keep-alive plan holds it). */
  isPinged: boolean
  /** Coarse words for the time left ("4m", "<1m"). */
  left: string
  leftMs: number
  /** Time left as a share of the cache's TTL, 0..1. */
  frac: number
  isLastMinute: boolean
  /** Milliseconds since this thread's last response ended. */
  agoMs: number
  warmUsd?: number
  coldUsd?: number
  tokens?: number
  saved?: number
  /** Seconds until the cache entry itself expires (negative: that long cold), on the server's clock. */
  remS?: number
  /** Seconds until the cache goes cold: the keep-alive plan's horizon while the entry is warm, else remS. */
  untilColdS?: number
  ttlS: number
  /** Seconds until the next automatic ping, only while it fires before the entry expires. */
  nextPingS?: number
  /** The last touch was a keep-alive ping; a keep-alive plan exists. Together they are the clock's `↻`. */
  isKeepalive: boolean
  isHeld: boolean
  /** The clock's anchors (last_ts, end_ts, ttl_s, held_until_ms), so the redraw check sees a moved clock. */
  anchor: string
}

export type View = {
  url: string
  main: ThreadView
  /** Named sub-agents active in the last 30 minutes, most recent first; cold ones are kept (drawn dim). */
  subs: ThreadView[]
  /** The session's saved $ and what it would have cost: main thread and the rest together (the pane's summary line). */
  saved?: number
  baseline?: number
  /** The status line's `saved $x of $y session` (the main thread) and `· saved $z of $q subagents` (every other thread). */
  mainSaved?: number
  mainBaseline?: number
  othersSaved?: number
  othersBaseline?: number
  /** How many threads other than the main one the money block counts. */
  others: number
  /** What this session's manual pings saved, net of their cost, once there is one (`↻ $0.27 pings`). */
  pingsSaved?: number
  /** How old the cache-state copy is, in seconds, and whether it is old enough for `⏳`. */
  ageS: number
  isStale: boolean
  /** The context window the proxy measured for the main thread's model, when it measured one. */
  window?: number
  /** `▲ update v0.6.1` (or `▲ v0.6.1 auto`): a release the user has not declined, from the update-check record. */
  update?: string
  /** On a Claude subscription: `saved 1.2M rd / 85.0k wr`, the tokens compaction kept out of this 5-hour window. */
  windowSaved?: string
  /** The server's answer for a manual ping: true or false, undefined when it did not say (older server). */
  pingable?: boolean
  pingReason?: string
  /** Commands not run for 7+ days, longest idle first (the weekly tip). */
  tip: string[]
}

/** One `cg.py cachestate --json` document (the raw /api/cachestate entry plus age_s, stale) and when it was read. */
export type Snapshot = {
  at: number
  url: string
  /** From `cg.py tip`: the commands whose last run is over a week ago. */
  tip?: string[]
  doc: {
    error?: string
    last_ts?: number
    now_ms?: number
    age_s?: number
    keepalive?: boolean
    pingable?: boolean
    ping_reason?: string
    threads?: unknown[]
    stale?: boolean
    /** Set when nothing was ever read and the server's answer explains why (token refused, unreachable). */
    notice?: string
    money?: { main?: { saved_usd?: number; baseline_usd?: number }; others?: { saved_usd?: number; baseline_usd?: number; threads?: number } }
    /** From `cg.py cachestate`: the status line's update marker and, on a subscription, this window's saved tokens. */
    update?: string
    window_saved?: string
    [key: string]: unknown
  }
}

/** `cg.py panedata`: the data of the pane's mode sections. A section the helper has no data for is absent. */
export type PaneData = {
  spark?: { today: number; days: number[] }
  strip?: { kinds: ('hit' | 'part' | 'miss')[]; lost?: number | null }
  heat?: { hours: number[]; now: number; lost?: number | null }
  fill?: { used: number; window: number; frac: number; base?: number | null; eta_s?: number | null; miss_usd?: number | null }
}

declare module 'claude-code' {
  interface PluginState {
    'context-guru': {
      snapshot: Snapshot | null
      isPinging: boolean
      isHidden: boolean
      isAgentsOff: boolean
      paneData: PaneData | null
      lastPing: string
      /** The engine's live context window ($.session.usage().context), pushed by session.measure. */
      context: { tokens?: number; window?: number; fiveHour?: number; fiveHourResetMs?: number } | null
      /** Bumped once a second while a clock is shown, so the cache clock counts like the status line's. */
      tick: number
    }
  }
}
