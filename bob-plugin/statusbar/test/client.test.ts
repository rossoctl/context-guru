import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import * as http from 'node:http'
import * as net from 'node:net'
import * as os from 'node:os'
import * as path from 'node:path'
import { getJson, MAX_BODY_BYTES, poll, readPort, stateDir } from '../src/client'
import { render } from '../src/model'

type Reply = { status: number, body: unknown }
type Routes = Record<string, Reply | (() => Reply)>

// A fake CG proxy on an ephemeral 127.0.0.1 port. It returns JSON in the shape of dash/api.go
// and dash/cachestate.go, and records each path it was asked for.
async function fakeProxy(routes: Routes) {
  const seen: string[] = []
  const server = http.createServer((req, res) => {
    seen.push(req.url ?? '')
    const key = Object.keys(routes).find(k => (req.url ?? '').startsWith(k))
    const route = key === undefined ? undefined : routes[key]
    const reply = typeof route === 'function' ? route() : route
    if (!reply) { res.writeHead(404); res.end(); return }
    res.writeHead(reply.status, { 'Content-Type': 'application/json' })
    res.end(typeof reply.body === 'string' ? reply.body : JSON.stringify(reply.body))
  })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const port = (server.address() as net.AddressInfo).port
  return { port, seen, close: () => new Promise<void>(resolve => server.close(() => resolve())) }
}

async function deadPort() {
  const server = net.createServer()
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const port = (server.address() as net.AddressInfo).port
  await new Promise<void>(resolve => server.close(() => resolve()))
  return port
}

function tempState(record?: object) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'cg-statusbar-'))
  const dir = path.join(root, 'context-guru-bob')
  if (record) {
    fs.mkdirSync(dir)
    fs.writeFileSync(path.join(dir, 'install.json'), JSON.stringify(record))
  }
  return { root, dir, cleanup: () => fs.rmSync(root, { recursive: true, force: true }) }
}

const NOW = 1_800_000_000_000
const HEALTHY: Routes = { '/healthz': { status: 200, body: 'ok' } }
const SESSIONS = { sessions: [{ session_id: 'bob-s1', agents: 'bob', saved: 0, tokens_before: 5,
  tokens_after: 5 }], total: 1 }

test('state dir follows XDG_STATE_HOME, else ~/.local/state', () => {
  assert.equal(stateDir({ XDG_STATE_HOME: '/x/state' }, '/home/u'), '/x/state/context-guru-bob')
  assert.equal(stateDir({}, '/home/u'), '/home/u/.local/state/context-guru-bob')
  assert.equal(stateDir({ XDG_STATE_HOME: '' }, '/home/u'), '/home/u/.local/state/context-guru-bob')
})

test('readPort reads the port from install.json, 0 when it is missing', (t) => {
  const present = tempState({ port: 8891, complete: true })
  const absent = tempState()
  t.after(() => { present.cleanup(); absent.cleanup() })
  assert.equal(readPort(present.dir), 8891)
  assert.equal(readPort(absent.dir), 0)
})

test('getJson refuses a host other than 127.0.0.1 and makes no call', async () => {
  await assert.rejects(getJson('localhost', 8891, '/healthz', 500), /loopback/)
  await assert.rejects(getJson('192.168.1.2', 8891, '/healthz', 500), /loopback/)
})

test('getJson times out on a proxy that never answers', async (t) => {
  const server = net.createServer(() => { /* accept and say nothing */ })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => { server.close() })
  const port = (server.address() as net.AddressInfo).port
  const start = Date.now()
  await assert.rejects(getJson('127.0.0.1', port, '/healthz', 200), /timeout/)
  assert.ok(Date.now() - start < 2000)
})

test('routed and up: renders ctx, clock and saved from the fake proxy', async (t) => {
  const proxy = await fakeProxy({ ...HEALTHY,
    '/api/sessions': { status: 200, body: SESSIONS },
    '/api/cachestate': { status: 200, body: { session: 'bob-s1', end_ts: NOW - 350_000, now_ms: NOW,
      ttl_s: 3600, prefix_tokens: 21_000, cache_read: 0, cache_write: 20_315, window: 270_000,
      window_exact: true, priced: false } } })
  const state = tempState({ port: proxy.port })
  t.after(async () => { state.cleanup(); await proxy.close() })
  const snap = await poll(state.dir, () => 0)
  assert.equal(render(snap, 0).text, 'CG ✓ ctx 21k/270k · cache 1h 54:10 · saved 0')
  assert.deepEqual(proxy.seen, ['/healthz', '/api/sessions?agent=bob&limit=1',
    '/api/cachestate?session=bob-s1'])
})

test('proxy down: renders CG ✗', async (t) => {
  const state = tempState({ port: await deadPort() })
  t.after(() => state.cleanup())
  const snap = await poll(state.dir, () => 0)
  assert.equal(snap.state, 'down')
  assert.equal(render(snap, 0).text, 'CG ✗')
})

test('healthz not 200: renders CG ✗', async (t) => {
  const proxy = await fakeProxy({ '/healthz': { status: 503, body: 'no' } })
  const state = tempState({ port: proxy.port })
  t.after(async () => { state.cleanup(); await proxy.close() })
  assert.equal(render(await poll(state.dir, () => 0), 0).text, 'CG ✗')
})

test('not installed: renders CG – and calls nothing', async (t) => {
  const state = tempState()
  t.after(() => state.cleanup())
  const snap = await poll(state.dir, () => 0)
  assert.equal(render(snap, 0).text, 'CG –')
})

test('no Bob session: renders CG ✓ with — and asks for no cache state', async (t) => {
  const proxy = await fakeProxy({ ...HEALTHY,
    '/api/sessions': { status: 200, body: { sessions: [], total: 0 } } })
  const state = tempState({ port: proxy.port })
  t.after(async () => { state.cleanup(); await proxy.close() })
  const snap = await poll(state.dir, () => 0)
  assert.equal(render(snap, 0).text, 'CG ✓ ctx —/270k · cache — · saved —')
  assert.equal(proxy.seen.some(p => p.startsWith('/api/cachestate')), false)
})

test('expired cache: renders cold', async (t) => {
  const proxy = await fakeProxy({ ...HEALTHY,
    '/api/sessions': { status: 200, body: { sessions: [{ session_id: 'bob-s1', saved: 3100 }],
      total: 1 } },
    '/api/cachestate': { status: 200, body: { session: 'bob-s1', end_ts: NOW - 4_000_000,
      now_ms: NOW, ttl_s: 3600, prefix_tokens: 40_000, window: 0, priced: false } } })
  const state = tempState({ port: proxy.port })
  t.after(async () => { state.cleanup(); await proxy.close() })
  assert.equal(render(await poll(state.dir, () => 0), 0).text,
    'CG ✓ ctx 40k/270k · cache cold · saved 3.1k tok')
})

test('a session id is sent URL-encoded', async (t) => {
  const proxy = await fakeProxy({ ...HEALTHY,
    '/api/sessions': { status: 200, body: { sessions: [{ session_id: 'bob:a.b' }], total: 1 } },
    '/api/cachestate': { status: 200, body: { session: 'bob:a.b', none: true, now_ms: NOW } } })
  const state = tempState({ port: proxy.port })
  t.after(async () => { state.cleanup(); await proxy.close() })
  const snap = await poll(state.dir, () => 0)
  assert.equal(render(snap, 0).text, 'CG ✓ ctx —/270k · cache — · saved —')
  assert.ok(proxy.seen.includes('/api/cachestate?session=bob%3Aa.b'), proxy.seen.join(','))
})

test('a broken API reply after a good healthz keeps CG ✓ and shows —', async (t) => {
  const proxy = await fakeProxy({ ...HEALTHY,
    '/api/sessions': { status: 500, body: 'boom' } })
  const state = tempState({ port: proxy.port })
  t.after(async () => { state.cleanup(); await proxy.close() })
  assert.equal(render(await poll(state.dir, () => 0), 0).text, 'CG ✓ ctx —/270k · cache — · saved —')
})

// A raw server for replies the route table cannot make: redirects, drips, huge bodies.
async function rawServer(handler: http.RequestListener) {
  const server = http.createServer(handler)
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const port = (server.address() as net.AddressInfo).port
  return { port, close: () => { server.closeAllConnections(); server.close() } }
}

test('a redirect is not followed', async (t) => {
  let followed = false
  const target = await rawServer((_req, res) => { followed = true; res.end('{}') })
  const proxy = await rawServer((_req, res) => {
    res.writeHead(302, { Location: `http://127.0.0.1:${target.port}/healthz` })
    res.end()
  })
  t.after(() => { proxy.close(); target.close() })
  await assert.rejects(getJson('127.0.0.1', proxy.port, '/healthz', 500), /status 302/)
  assert.equal(followed, false)
})

test('a server that drips bytes fails within the total deadline', async (t) => {
  const proxy = await rawServer((_req, res) => {
    res.writeHead(200)
    const timer = setInterval(() => res.write(' '), 50)
    res.on('close', () => clearInterval(timer))
  })
  t.after(() => proxy.close())
  const start = Date.now()
  await assert.rejects(getJson('127.0.0.1', proxy.port, '/healthz', 300), /timeout/)
  assert.ok(Date.now() - start < 1000, `took ${Date.now() - start} ms`)
})

// A proxy restart can close the socket after the headers and part of the body. Node then emits
// aborted and close on the reply, but no end and no error, so a promise that waits for one of
// those never settles, and the status bar stops polling while it shows CG ✓.
test('a reply cut off mid-body fails within the total deadline', { timeout: 3000 }, async (t) => {
  const proxy = net.createServer(socket => {
    socket.once('data', () => {
      socket.write('HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{"a":')
      setTimeout(() => socket.destroy(), 20)
    })
  })
  await new Promise<void>(resolve => proxy.listen(0, '127.0.0.1', resolve))
  t.after(() => proxy.close())
  const port = (proxy.address() as net.AddressInfo).port
  const start = Date.now()
  await assert.rejects(getJson('127.0.0.1', port, '/healthz', 300))
  assert.ok(Date.now() - start < 1000, `took ${Date.now() - start} ms`)
})

test('a body over 1 MiB fails', async (t) => {
  const proxy = await rawServer((_req, res) => {
    res.writeHead(200)
    res.end(Buffer.alloc(MAX_BODY_BYTES + 1, 0x20))
  })
  t.after(() => proxy.close())
  assert.equal(MAX_BODY_BYTES, 1024 * 1024)
  await assert.rejects(getJson('127.0.0.1', proxy.port, '/healthz', 2000), /too large/)
})

test('a body at the cap is read', async (t) => {
  const proxy = await rawServer((_req, res) => {
    res.writeHead(200)
    res.end('"' + 'x'.repeat(MAX_BODY_BYTES - 2) + '"')
  })
  t.after(() => proxy.close())
  const body = await getJson('127.0.0.1', proxy.port, '/healthz', 2000)
  assert.equal((body as string).length, MAX_BODY_BYTES - 2)
})

test('a non-JSON /api/cachestate keeps CG ✓ and shows —', async (t) => {
  const proxy = await fakeProxy({ ...HEALTHY,
    '/api/sessions': { status: 200, body: SESSIONS },
    '/api/cachestate': { status: 200, body: 'not json at all' } })
  const state = tempState({ port: proxy.port })
  t.after(async () => { state.cleanup(); await proxy.close() })
  assert.equal(render(await poll(state.dir, () => 0), 0).text, 'CG ✓ ctx —/270k · cache — · saved 0')
})
