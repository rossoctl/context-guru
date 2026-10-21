export type ThreadView = {
  id: string
  isWarm: boolean
  /** False only when the server says the proxy holds nothing to ping with (keep-alive off). */
  isPingable: boolean
  /** Coarse words for the time left ("4m"). */
  left: string
  leftMs: number
  warmUsd?: number
  coldUsd?: number
  tokens?: number
}

export type View = {
  url: string
  main: ThreadView
  subs: ThreadView[]
  /** The session's saved $ (main thread and the rest). */
  saved?: number
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
