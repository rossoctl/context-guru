import assert from "node:assert/strict"
import test from "node:test"
import { spawn } from "node:child_process"
import { createServer } from "node:http"
import { chmodSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-failopen-test-"))
process.env.CONTEXT_GURU_PORT_START = "9200"

import plugin, { routeProviders, stateDirectory } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

// Ports only this file uses. 9263 stays closed: nothing listens there.
const UPSTREAM_PORT = 9260
const HANG_PORT = 9261
const ERROR_PORT = 9262
const CLOSED_PORT = 9263
const RESET_PORT = 9264
const BROKEN_PORT = 9265

// A made-up key. It must reach the upstream unchanged and appear nowhere else.
const CANARY = "sk-ant-canary-1159-do-not-log-7f3a9c"

// A server that records every request it gets, then answers with handle(request, response, body).
async function recorder(port, handle = (request, response) => response.end("{\"ok\":true}")) {
  const requests = []
  const server = createServer((request, response) => {
    const chunks = []
    request.on("data", (chunk) => chunks.push(chunk))
    request.on("end", () => {
      const body = Buffer.concat(chunks).toString("utf8")
      requests.push({ method: request.method, url: request.url, headers: request.headers, body })
      handle(request, response, body)
    })
  })
  await new Promise((resolve) => server.listen(port, "127.0.0.1", resolve))
  return {
    requests,
    close: () => new Promise((resolve) => { server.closeAllConnections?.(); server.close(resolve) }),
  }
}

function logger() {
  const lines = []
  return { lines, client: { app: { log: async ({ body }) => { lines.push(body) } } } }
}

function requestInit(extra = {}) {
  return {
    method: "POST",
    headers: { "content-type": "application/json", "x-api-key": CANARY, "anthropic-version": "2023-06-01", "x-context-guru-session": "session-1159" },
    body: JSON.stringify({ model: "claude-test", messages: [{ role: "user", content: "hello" }] }),
    ...extra,
  }
}

// A wrapped fetch for one anthropic provider, routed to proxyPort, with a fresh state directory.
function routed(proxyPort, { upstream = `http://127.0.0.1:${UPSTREAM_PORT}`, inner, client = logger().client } = {}) {
  const state = mkdtempSync(join(tmpdir(), "context-guru-failopen-state-"))
  const config = { provider: { anthropic: { options: { baseURL: upstream, ...(inner ? { fetch: inner } : {}) } } } }
  routeProviders(config, proxyPort, undefined, { state, client })
  return { state, config, fetch: config.provider.anthropic.options.fetch, url: `http://127.0.0.1:${proxyPort}/anthropic/v1/messages` }
}

function filesUnder(root) {
  const out = []
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = join(root, entry.name)
    if (entry.isDirectory()) out.push(...filesUnder(path))
    else if (statSync(path).isFile()) out.push(path)
  }
  return out
}

test("routeProviders wraps fetch and chains a fetch another plugin set before", async () => {
  const calls = []
  const inner = async (input, init) => { calls.push({ input: String(input), init }); return new Response("from-inner", { status: 201 }) }
  const { config, fetch: wrapped, url } = routed(RESET_PORT, { inner })

  assert.equal(typeof wrapped, "function")
  assert.notEqual(wrapped, inner)
  const init = requestInit()
  const response = await wrapped(url, init)

  assert.equal(response.status, 201)
  assert.equal(await response.text(), "from-inner")
  assert.deepEqual(calls.map((call) => call.input), [url])
  assert.equal(calls[0].init, init)
  assert.equal(config.provider.anthropic.options.baseURL, `http://127.0.0.1:${RESET_PORT}/anthropic/v1`)
})

test("a refused connection sends the request once to the upstream, unchanged except the session header", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  const { lines, client } = logger()
  try {
    const { fetch: wrapped } = routed(CLOSED_PORT, { upstream: `http://127.0.0.1:${UPSTREAM_PORT}/gw`, client })
    const init = requestInit()

    const response = await wrapped(`http://127.0.0.1:${CLOSED_PORT}/anthropic/v1/messages?beta=true`, init)

    assert.equal(response.status, 200)
    assert.equal(await response.text(), "{\"ok\":true}")
    assert.equal(upstream.requests.length, 1)
    const [sent] = upstream.requests
    assert.equal(sent.method, "POST")
    assert.equal(sent.url, "/gw/v1/messages?beta=true")
    assert.equal(sent.body, init.body)
    assert.equal(sent.headers["x-api-key"], CANARY)
    assert.equal(sent.headers["anthropic-version"], "2023-06-01")
    assert.equal(sent.headers["content-type"], "application/json")
    assert.equal(sent.headers["x-context-guru-session"], undefined)
    assert.equal(lines.length, 1)
    assert.equal(lines[0].level, "warn")
    assert.match(lines[0].message, /ECONNREFUSED/)
    assert.match(lines[0].message, /127\.0\.0\.1:9260/)
    assert.doesNotMatch(lines[0].message, /canary|\/gw|messages|beta/)
  } finally {
    await upstream.close()
  }
})

test("Bun's ConnectionRefused error also sends the request to the upstream", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  try {
    const inner = async (input, init) => {
      if (String(input).includes(`:${CLOSED_PORT}/`)) throw Object.assign(new Error("Unable to connect. Is the computer able to access the url?"), { code: "ConnectionRefused" })
      return fetch(input, init)
    }
    const { fetch: wrapped, url } = routed(CLOSED_PORT, { inner })

    const response = await wrapped(url, requestInit())

    assert.equal(response.status, 200)
    assert.deepEqual(upstream.requests.map((request) => request.url), ["/v1/messages"])
  } finally {
    await upstream.close()
  }
})

test("a 500 from the proxy reaches the caller and nothing goes to the upstream", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  const proxy = await recorder(ERROR_PORT, (request, response) => { response.statusCode = 500; response.end("proxy error") })
  try {
    const { fetch: wrapped, url } = routed(ERROR_PORT)

    const response = await wrapped(url, requestInit())

    assert.equal(response.status, 500)
    assert.equal(await response.text(), "proxy error")
    assert.equal(proxy.requests.length, 1)
    assert.equal(upstream.requests.length, 0)
  } finally {
    await proxy.close()
    await upstream.close()
  }
})

test("a proxy that accepts and never answers is not bypassed: the abort reaches the caller, no second send", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  const proxy = await recorder(HANG_PORT, () => {})
  try {
    const { fetch: wrapped, url } = routed(HANG_PORT)

    await assert.rejects(wrapped(url, requestInit({ signal: AbortSignal.timeout(300) })), (error) => error.name === "TimeoutError" || error.name === "AbortError")

    assert.equal(proxy.requests.length, 1)
    assert.equal(upstream.requests.length, 0)
  } finally {
    await proxy.close()
    await upstream.close()
  }
})

test("a connection reset after the proxy accepted is not bypassed", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  const proxy = await recorder(RESET_PORT, (request) => request.socket.destroy())
  try {
    const { fetch: wrapped, url } = routed(RESET_PORT)

    // The thrown error must not carry the key either.
    await assert.rejects(wrapped(url, requestInit()), (error) => !`${error?.stack} ${error?.cause?.stack} ${JSON.stringify(error?.cause ?? {})}`.includes(CANARY))

    assert.equal(proxy.requests.length, 1)
    assert.equal(upstream.requests.length, 0)
  } finally {
    await proxy.close()
    await upstream.close()
  }
})

test("a stream that started and then broke gives the error to the caller and nothing goes to the upstream", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  const proxy = await recorder(BROKEN_PORT, (request, response) => {
    response.writeHead(200, { "content-type": "text/event-stream" })
    response.write("event: message_start\n\n", () => setTimeout(() => request.socket.destroy(), 20))
  })
  try {
    const { fetch: wrapped, url } = routed(BROKEN_PORT)

    const response = await wrapped(url, requestInit())
    await assert.rejects(response.text())

    assert.equal(proxy.requests.length, 1)
    assert.equal(upstream.requests.length, 0)
  } finally {
    await proxy.close()
    await upstream.close()
  }
})

test("a body that cannot be sent twice is not sent to the upstream after a refused connection", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  try {
    const { fetch: wrapped, url } = routed(CLOSED_PORT)
    const body = new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode("{}")); controller.close() } })

    await assert.rejects(wrapped(url, requestInit({ body, duplex: "half" })), (error) => error?.cause?.code === "ECONNREFUSED")

    assert.equal(upstream.requests.length, 0)
  } finally {
    await upstream.close()
  }
})

test("the disabled marker sends requests straight to the upstream, at once", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  const proxy = await recorder(ERROR_PORT, (request, response) => { response.statusCode = 500; response.end() })
  try {
    const { state, fetch: wrapped, url } = routed(ERROR_PORT)
    writeFileSync(join(state, "disabled"), "disabled by context-guru-uninstall\n")

    const response = await wrapped(url, requestInit())

    assert.equal(response.status, 200)
    assert.equal(proxy.requests.length, 0)
    assert.deepEqual(upstream.requests.map((request) => [request.url, request.headers["x-api-key"]]), [["/v1/messages", CANARY]])
  } finally {
    await proxy.close()
    await upstream.close()
  }
})

test("fallback: false leaves the provider fetch as it was", async () => {
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  const directory = "/tmp/context-guru-failopen-off"
  let record
  try {
    const hooks = await plugin.server({ directory, client: logger().client }, { fallback: false })
    const inner = async () => new Response("inner")
    const config = { provider: { anthropic: { options: { baseURL: `http://127.0.0.1:${UPSTREAM_PORT}`, fetch: inner } }, openai: { options: { baseURL: `http://127.0.0.1:${UPSTREAM_PORT}/v1` } } } }

    await hooks.config(config)

    record = JSON.parse(readFileSync(join(stateDirectory(directory), "install.json"), "utf8"))
    assert.equal(config.provider.anthropic.options.baseURL, `http://127.0.0.1:${record.port}/anthropic/v1`)
    assert.equal(config.provider.anthropic.options.fetch, inner)
    assert.equal(config.provider.openai.options.fetch, undefined)
  } finally {
    if (record?.pid) {
      try { process.kill(record.pid, "SIGTERM") } catch {}
      await waitUntilUnhealthy(record.port)
    }
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("end to end: proxy up goes to the proxy; proxy dead goes to the upstream, and the key is in no log and no file", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  const directory = "/tmp/context-guru-failopen-e2e"
  const { lines, client } = logger()
  let record
  try {
    const hooks = await plugin.server({ directory, client })
    const config = { provider: { anthropic: { options: { baseURL: `http://127.0.0.1:${UPSTREAM_PORT}` } } } }
    await hooks.config(config)
    record = JSON.parse(readFileSync(join(stateDirectory(directory), "install.json"), "utf8"))
    const { baseURL, fetch: wrapped } = config.provider.anthropic.options
    const output = { headers: {} }
    await hooks["chat.headers"]({ sessionID: "session-e2e", provider: { info: { id: "anthropic" } } }, output)

    // Proxy up: the stub proxy answers 404 for chat paths. That answer reaches the caller.
    const up = await wrapped(`${baseURL}/messages`, requestInit({ headers: { ...requestInit().headers, ...output.headers } }))
    assert.equal(up.status, 404)
    assert.equal(upstream.requests.length, 0)

    // The proxy dies between chat.headers and the send.
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
    const down = await wrapped(`${baseURL}/messages`, requestInit())

    assert.equal(down.status, 200)
    assert.equal(upstream.requests.length, 1)
    assert.equal(upstream.requests[0].headers["x-api-key"], CANARY)
    for (const line of lines) assert.doesNotMatch(JSON.stringify(line), new RegExp(CANARY))
    for (const path of filesUnder(process.env.CONTEXT_GURU_STATE)) assert.ok(!readFileSync(path).includes(CANARY), `${path} holds the key`)
  } finally {
    if (record?.pid) { try { process.kill(record.pid, "SIGTERM") } catch {} }
    delete process.env.CONTEXT_GURU_BIN
    await upstream.close()
  }
})

test("a failed restart waits at most 5 s, then goes direct, and starts no new restart for 30 s", { timeout: 60000 }, async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  const directory = "/tmp/context-guru-failopen-restart"
  const state = stateDirectory(directory)
  const { lines, client } = logger()
  let record
  try {
    const hooks = await plugin.server({ directory, client })
    const config = { provider: { anthropic: { options: { baseURL: `http://127.0.0.1:${UPSTREAM_PORT}` } } } }
    await hooks.config(config)
    record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
    // The next restart runs a binary that records each start and exits, so it never becomes healthy.
    const binDirectory = mkdtempSync(join(tmpdir(), "context-guru-failing-"))
    mkdirSync(binDirectory, { recursive: true })
    const failing = join(binDirectory, "context-guru-proxy")
    const starts = join(binDirectory, "starts.log")
    writeFileSync(failing, `#!/bin/sh\necho start >> ${JSON.stringify(starts)}\nexit 1\n`)
    chmodSync(failing, 0o755)
    writeFileSync(join(state, "install.json"), JSON.stringify({ ...record, binary: failing }))
    const countStarts = () => { try { return readFileSync(starts, "utf8").split("\n").filter(Boolean).length } catch { return 0 } }
    const ask = () => hooks["chat.headers"]({ sessionID: "session-restart", provider: { info: { id: "anthropic" } } }, { headers: {} })
    const send = () => config.provider.anthropic.options.fetch(`${config.provider.anthropic.options.baseURL}/messages`, requestInit())

    const started = Date.now()
    await ask()
    const waited = Date.now() - started

    assert.ok(waited < 6000, `chat.headers waited ${waited} ms`)
    assert.ok(waited >= 4500, `chat.headers returned after ${waited} ms, before the 5 s cap`)
    assert.equal((await send()).status, 200)
    assert.equal(upstream.requests.length, 1)

    // The restart goes on in the background and fails. Then the 30 s window starts.
    const deadline = Date.now() + 30000
    while (!lines.some((line) => /proxy recovery failed/.test(line.message)) && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 100))
    assert.ok(lines.some((line) => /proxy recovery failed/.test(line.message)), "the restart did not fail in time")
    assert.equal(countStarts(), 1)

    const again = Date.now()
    await ask()
    assert.ok(Date.now() - again < 1000, `chat.headers in the down window took ${Date.now() - again} ms`)
    assert.equal(countStarts(), 1)
    assert.equal((await send()).status, 200)
    assert.equal(upstream.requests.length, 2)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
    await upstream.close()
  }
})

// A routed project with the stub proxy running. The proxy answers 404 on chat paths, the upstream 200,
// so the status tells where a send went.
async function routedProject(directory) {
  const { lines, client } = logger()
  const hooks = await plugin.server({ directory, client })
  const config = { provider: { anthropic: { options: { baseURL: `http://127.0.0.1:${UPSTREAM_PORT}` } } } }
  await hooks.config(config)
  const state = stateDirectory(directory)
  const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
  const { baseURL } = config.provider.anthropic.options
  return {
    lines,
    state,
    record,
    ask: () => hooks["chat.headers"]({ sessionID: "session-back", provider: { info: { id: "anthropic" } } }, { headers: {} }),
    send: () => config.provider.anthropic.options.fetch(`${baseURL}/messages`, requestInit()),
  }
}

async function waitFor(condition, what, timeoutMs = 25000) {
  const deadline = Date.now() + timeoutMs
  while (!(await condition())) {
    if (Date.now() > deadline) assert.fail(`timed out waiting for ${what}`)
    await new Promise((resolve) => setTimeout(resolve, 100))
  }
}

const isHealthy = (port) => fetch(`http://127.0.0.1:${port}/healthz`).then((response) => response.ok, () => false)

function killRecorded(state) {
  try { process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8")), "SIGTERM") } catch {}
}

test("after a fallback, routing returns to the proxy once chat.headers sees it healthy again", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  let project
  let restarted
  try {
    project = await routedProject("/tmp/context-guru-failopen-back")
    process.kill(project.record.pid, "SIGTERM")
    await waitUntilUnhealthy(project.record.port)
    assert.equal((await project.send()).status, 200)
    assert.equal(upstream.requests.length, 1)

    // Someone else starts the proxy again on the same port.
    restarted = spawn(process.env.CONTEXT_GURU_BIN, ["--listen", `127.0.0.1:${project.record.port}`], { stdio: "ignore" })
    await waitFor(() => isHealthy(project.record.port), "the proxy to start again")
    await project.ask()

    assert.equal((await project.send()).status, 404)
    assert.equal(upstream.requests.length, 1)
  } finally {
    restarted?.kill("SIGTERM")
    delete process.env.CONTEXT_GURU_BIN
    await upstream.close()
  }
})

test("a restart that is healthy before the 5 s cap sends the next request to the proxy", async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  let project
  try {
    project = await routedProject("/tmp/context-guru-failopen-quick")
    process.kill(project.record.pid, "SIGTERM")
    await waitUntilUnhealthy(project.record.port)
    assert.equal((await project.send()).status, 200)

    const started = Date.now()
    await project.ask()

    assert.ok(Date.now() - started < 5000, `chat.headers waited ${Date.now() - started} ms`)
    assert.ok(project.lines.some((line) => /proxy recovered/.test(line.message)), "the plugin did not restart the proxy")
    assert.equal((await project.send()).status, 404)
    assert.equal(upstream.requests.length, 1)
  } finally {
    if (project) killRecorded(project.state)
    delete process.env.CONTEXT_GURU_BIN
    await upstream.close()
  }
})

test("a restart that succeeds after the capped wait sends the next request to the proxy", { timeout: 60000 }, async () => {
  const upstream = await recorder(UPSTREAM_PORT)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  let project
  try {
    project = await routedProject("/tmp/context-guru-failopen-slow")
    process.kill(project.record.pid, "SIGTERM")
    await waitUntilUnhealthy(project.record.port)
    // The restart runs a proxy that serves only after 7 s, past the 5 s cap.
    const slow = join(mkdtempSync(join(tmpdir(), "context-guru-slow-")), "context-guru-proxy")
    writeFileSync(slow, `#!/usr/bin/env node
const port = Number(process.argv[process.argv.indexOf("--listen") + 1].split(":")[1])
setTimeout(() => require("node:http").createServer((request, response) => {
  if (request.url === "/healthz") response.end("ok")
  else { response.statusCode = 404; response.end() }
}).listen(port, "127.0.0.1"), 7000)
`)
    chmodSync(slow, 0o755)
    writeFileSync(join(project.state, "install.json"), JSON.stringify({ ...project.record, binary: slow }))

    await project.ask()
    assert.equal((await project.send()).status, 200)
    assert.equal(upstream.requests.length, 1)

    // No chat.headers call after this point: only the finished restart may clear the down state.
    await waitFor(() => project.lines.some((line) => /proxy recovered/.test(line.message)), "the background restart")
    assert.equal((await project.send()).status, 404)
    assert.equal(upstream.requests.length, 1)
  } finally {
    if (project) killRecorded(project.state)
    delete process.env.CONTEXT_GURU_BIN
    await upstream.close()
  }
})
