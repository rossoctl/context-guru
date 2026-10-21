export type ThreadView = {
  id: string
  isWarm: boolean
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
  saved?: number
  manual?: { count?: number; saved_usd?: number }
}

/** One `cg.py cachestate --json` document and the local time it was read at. */
export type Snapshot = {
  at: number
  doc: {
    ok?: boolean
    dashboard_url?: string
    now_ms?: number
    age_s?: number
    session?: unknown
    stats?: unknown
  }
}

declare module 'claude-code' {
  interface PluginState {
    'context-guru': { snapshot: Snapshot | null; isPinging: boolean; isHidden: boolean }
  }
}
