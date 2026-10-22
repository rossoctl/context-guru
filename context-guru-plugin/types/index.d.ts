export type ThreadView = {
  id: string
  /** The name Claude Code shows for a sub-agent (from `cg.py cachestate --transcript`). */
  name?: string
  isWarm: boolean
  /** Kept warm by pings (the last touch was one, or a keep-alive plan holds it). */
  isPinged: boolean
  /** Coarse words for the time left ("4m"). */
  left: string
  leftMs: number
  /** Time left as a share of the cache's TTL, 0..1: the gauge. */
  frac: number
  isLastMinute: boolean
  warmUsd?: number
  coldUsd?: number
  tokens?: number
  saved?: number
}

export type View = {
  url: string
  main: ThreadView
  subs: ThreadView[]
  /** The session's saved $ (main thread and the rest). */
  saved?: number
  /** Which of the recommended commands the band shows now. */
  tip: number
}

/** One `cg.py cachestate --json` document (the raw /api/cachestate entry plus age_s, stale) and when it was read. */
export type Snapshot = {
  at: number
  url: string
  doc: {
    error?: string
    last_ts?: number
    now_ms?: number
    age_s?: number
    keepalive?: boolean
    threads?: unknown[]
    money?: { main?: { saved_usd?: number }; others?: { saved_usd?: number } }
    [key: string]: unknown
  }
}

declare module 'claude-code' {
  interface PluginState {
    'context-guru': { snapshot: Snapshot | null; isPinging: boolean; isHidden: boolean }
  }
}
