// Reads the local CG proxy. Loopback only, short timeouts, no `vscode` import.
// It reads counts and times only. It never logs a reply.

import * as fs from 'node:fs'
import * as http from 'node:http'
import * as path from 'node:path'
import { CacheState, LOOPBACK, loopbackUrl, parsePort, pickSession, Snapshot } from './model'

export const TIMEOUT_MS = 2000
export const MAX_BODY_BYTES = 1024 * 1024

export function stateDir(env: NodeJS.ProcessEnv, home: string): string {
  const base = env.XDG_STATE_HOME || path.join(home, '.local/state')
  return path.join(base, 'context-guru-bob')
}

export function readPort(dir: string): number {
  try {
    return parsePort(fs.readFileSync(path.join(dir, 'install.json'), 'utf8'))
  } catch {
    return 0
  }
}

// GET a loopback URL. Resolves the parsed JSON (or the raw text when it is not JSON).
// Rejects on a non-200 status (redirects are not followed), a body over MAX_BODY_BYTES,
// a call longer than timeoutMs in total, or a host that is not 127.0.0.1.
export function getJson(host: string, port: number, urlPath: string, timeoutMs = TIMEOUT_MS):
  Promise<unknown> {
  return new Promise((resolve, reject) => {
    let url: string
    try {
      url = loopbackUrl(host, port, urlPath)
    } catch (error) {
      reject(error)
      return
    }
    // Settle once. The deadline rejects by itself: destroy() on a socket that is already closed
    // emits nothing, and the socket timeout never fires on a server that drips bytes.
    let settled = false
    const fail = (error: Error) => {
      if (settled) return
      settled = true
      clearTimeout(deadline)
      request.destroy()
      reject(error)
    }
    const deadline = setTimeout(() => fail(new Error('timeout')), timeoutMs)
    const request = http.get(url, response => {
      // A reply cut off mid-body emits aborted and close, but no end and no error.
      response.on('aborted', () => fail(new Error('reply cut off')))
      response.on('error', fail)
      response.on('close', () => { if (!response.complete) fail(new Error('reply cut off')) })
      if (response.statusCode !== 200) {
        fail(new Error(`status ${response.statusCode}`))
        return
      }
      const chunks: Buffer[] = []
      let size = 0
      response.on('data', (chunk: Buffer) => {
        size += chunk.length
        if (size > MAX_BODY_BYTES) {
          fail(new Error('reply too large'))
          return
        }
        chunks.push(chunk)
      })
      response.on('end', () => {
        if (settled) return
        settled = true
        clearTimeout(deadline)
        const text = Buffer.concat(chunks).toString('utf8')
        try {
          resolve(JSON.parse(text))
        } catch {
          resolve(text)
        }
      })
    })
    request.on('error', fail)
  })
}

// One poll: install.json, then /healthz, the newest Bob session and its cache state.
// `clock` is the local clock; it is read when the replies came, for the countdown.
export async function poll(dir: string, clock: () => number = Date.now): Promise<Snapshot> {
  const port = readPort(dir)
  const resetPath = path.join(dir, 'context-guru-reset')
  if (!port) return { state: 'not_installed', port: 0, polledAtLocal: clock() }
  try {
    await getJson(LOOPBACK, port, '/healthz')
  } catch {
    return { state: 'down', port, polledAtLocal: clock(), resetPath }
  }
  const snap: Snapshot = { state: 'ok', port, polledAtLocal: clock(), resetPath }
  try {
    const sessions = await getJson(LOOPBACK, port, '/api/sessions?agent=bob&limit=1')
    const session = pickSession(sessions)
    if (!session) return snap
    snap.session = session
    const saved = (sessions as { sessions: { saved?: unknown }[] }).sessions[0].saved
    if (typeof saved === 'number' && Number.isFinite(saved)) snap.saved = saved
    const cache = await getJson(LOOPBACK, port,
      `/api/cachestate?session=${encodeURIComponent(session)}`)
    if (cache && typeof cache === 'object') snap.cache = cache as CacheState
    snap.polledAtLocal = clock()
  } catch {
    // The proxy is up, but a read failed. Show what we have, with `—` for the rest.
  }
  return snap
}
