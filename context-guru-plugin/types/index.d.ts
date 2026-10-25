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
}

export type View = {
  url: string
  main: ThreadView
  /** Named sub-agents active in the last 30 minutes, most recent first; cold ones are kept (drawn dim). */
  subs: ThreadView[]
  /** The session's saved $ and what it would have cost: main thread and the rest together (the pane's summary line). */
  saved?: number
  baseline?: number
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
    money?: { main?: { saved_usd?: number; baseline_usd?: number }; others?: { saved_usd?: number; baseline_usd?: number } }
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
    }
  }
}
