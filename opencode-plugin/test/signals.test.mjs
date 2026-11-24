import assert from "node:assert/strict"
import test from "node:test"
import { mkdtempSync, readFileSync, existsSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-signals-test-"))
process.env.CONTEXT_GURU_PORT_START = "9700"

import plugin, { createSignaler, SIGNAL_EVENT_TYPES, stateDirectory } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

function recorder() {
  const calls = []
  return { calls, fetcher: async (url, init) => { calls.push({ url, init, body: JSON.parse(init.body) }); return { status: 204 } } }
}

test("signaler maps each OpenCode event to the proxy's event and kind", () => {
  const { calls, fetcher } = recorder()
  const signaler = createSignaler(fetcher)
  signaler.setPort(8787)
  signaler.noteSession("ses_1")
  for (const type of ["session.compacted", "session.compaction.ended", "permission.asked", "permission.v2.asked", "question.asked", "session.deleted"]) signaler.handle(type, "ses_1")
  assert.deepEqual(calls.map((call) => [call.body.event, call.body.kind]), [
    ["SessionStart", "compact"], ["SessionStart", "compact"], ["Notification", "permission_prompt"], ["Notification", "permission_prompt"], ["Notification", "elicitation_dialog"], ["SessionEnd", "other"],
  ])
  for (const call of calls) {
    assert.equal(call.url, "http://127.0.0.1:8787/signal")
    assert.equal(call.init.method, "POST")
    assert.equal(call.init.headers["Content-Type"], "application/json")
    assert.deepEqual(Object.keys(call.body).sort(), ["event", "kind", "session"])
    assert.equal(call.body.session, "ses_1")
    assert.ok(call.init.signal instanceof AbortSignal)
  }
})

test("signaler stays silent for an unrouted session, an unknown event, no port, or the off switch", () => {
  const { calls, fetcher } = recorder()
  const signaler = createSignaler(fetcher)
  signaler.noteSession("ses_1")
  signaler.handle("session.deleted", "ses_1") // no port yet
  signaler.setPort(8787)
  signaler.handle("session.deleted", "ses_other") // never sent a routed request
  signaler.handle("message.updated", "ses_1") // not a lifecycle event
  signaler.handle("session.compacted", undefined)
  assert.equal(calls.length, 0)
  process.env.CONTEXT_GURU_CLIENT_SIGNALS = "off"
  try {
    signaler.handle("session.compacted", "ses_1")
  } finally {
    delete process.env.CONTEXT_GURU_CLIENT_SIGNALS
  }
  assert.equal(calls.length, 0)
})

test("the signaler remembers a bounded number of sessions, oldest out", () => {
  const { calls, fetcher } = recorder()
  const signaler = createSignaler(fetcher)
  signaler.setPort(8787)
  for (let i = 0; i < 300; i++) signaler.noteSession(`ses_${i}`)
  signaler.handle("session.compacted", "ses_0") // dropped: no signal
  signaler.handle("session.compacted", "ses_299")
  assert.deepEqual(calls.map((call) => call.body.session), ["ses_299"])
  signaler.noteSession("ses_44") // oldest kept id is ses_44; seeing it again makes it newest
  for (let i = 300; i < 340; i++) signaler.noteSession(`ses_${i}`)
  signaler.handle("session.compacted", "ses_44") // kept: signals
  signaler.handle("session.compacted", "ses_45") // evicted: no signal
  assert.deepEqual(calls.map((call) => call.body.session), ["ses_299", "ses_44"])
})

test("a session that ended signals once; a lost signal throws nothing", () => {
  const { calls, fetcher } = recorder()
  const signaler = createSignaler(fetcher)
  signaler.setPort(8787)
  signaler.noteSession("ses_1")
  signaler.handle("session.deleted", "ses_1")
  signaler.handle("session.deleted", "ses_1")
  assert.equal(calls.length, 1)
  const failing = createSignaler(async () => { throw new Error("refused") })
  failing.setPort(1)
  failing.noteSession("a")
  failing.handle("session.compacted", "a")
  const throwing = createSignaler(() => { throw new Error("sync") })
  throwing.setPort(1)
  throwing.noteSession("a")
  throwing.handle("session.compacted", "a")
})

async function waitForRecord(path, label, count) {
  const deadline = Date.now() + 5000
  while (Date.now() < deadline) {
    const rows = existsSync(path) ? readFileSync(path, "utf8").trim().split("\n").filter(Boolean).map((line) => JSON.parse(line)).filter((row) => row.label === label) : []
    if (rows.length >= count) return rows
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  assert.fail(`no ${count} "${label}" rows in ${path}`)
}

test("v1: event hook posts compact and end signals to the proxy for a routed session", async () => {
  const record = join(mkdtempSync(join(tmpdir(), "context-guru-signals-rec-")), "rec.jsonl")
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  process.env.CG_TEST_RECORD = record
  const directory = "/tmp/context-guru-signals-v1"
  try {
    const hooks = await plugin.server({ directory, client: { app: { log: async () => {} } } })
    await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    await hooks["chat.headers"]({ sessionID: "ses_routed", provider: { info: { id: "anthropic" } } }, { headers: {} })
    await hooks.event({ event: { type: "session.compacted", properties: { sessionID: "ses_routed" } } })
    await hooks.event({ event: { type: "session.deleted", properties: { sessionID: "ses_unrouted", info: {} } } })
    await hooks.event({ event: { type: "session.deleted", properties: { sessionID: "ses_routed", info: {} } } })
    const rows = await waitForRecord(record, "signal", 2)
    assert.deepEqual(rows.map((row) => row.contentType), ["application/json", "application/json"])
    assert.deepEqual(rows.map((row) => JSON.parse(row.body)), [
      { session: "ses_routed", event: "SessionStart", kind: "compact" },
      { session: "ses_routed", event: "SessionEnd", kind: "other" },
    ])
    const info = JSON.parse(readFileSync(join(stateDirectory(directory), "install.json"), "utf8"))
    process.kill(info.pid, "SIGTERM")
    await waitUntilUnhealthy(info.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
    delete process.env.CG_TEST_RECORD
  }
})

test("v2: ctx.event.subscribe feeds the signals for a routed session", async () => {
  const record = join(mkdtempSync(join(tmpdir(), "context-guru-signals-rec2-")), "rec.jsonl")
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  process.env.CG_TEST_RECORD = record
  const directory = "/tmp/context-guru-signals-v2"
  try {
    let push
    let subscribed
    const queue = []
    let wake
    const stream = { [Symbol.asyncIterator]: async function* () { for (;;) { if (!queue.length) await new Promise((resolve) => { wake = resolve }); yield queue.shift() } } }
    const providers = [{ id: "anthropic", name: "Anthropic", settings: { baseURL: "https://api.anthropic.com" } }]
    const hooks = []
    const ctx = {
      location: { directory },
      options: {},
      provider: { transform: async (callback) => callback({ list: () => providers, get: () => providers[0], update: (id, update) => update(providers[0]) }) },
      command: { transform: async (callback) => callback({ add: () => {} }) },
      session: { hook: async (name, callback, options) => { hooks.push({ name, callback, options }) }, prompt: async () => {} },
      event: { subscribe: (options) => { subscribed = options; return stream } },
    }
    push = (event) => { queue.push(event); wake?.() }
    await plugin.setup(ctx)
    assert.deepEqual(subscribed.types, SIGNAL_EVENT_TYPES)
    for (const type of ["session.compaction.ended", "permission.asked", "session.deleted"]) assert.ok(subscribed.types.includes(type))
    const request = new Request("http://127.0.0.1:1/x", { method: "POST" })
    await hooks[0].callback({ sessionID: "ses_v2", request })
    push({ type: "session.compaction.ended", data: { sessionID: "ses_v2", reason: "auto", text: "" } })
    push({ type: "permission.asked", data: { sessionID: "ses_other" } })
    push({ type: "permission.asked", data: { sessionID: "ses_v2", id: "p1", action: "bash", resources: [] } })
    push({ type: "session.deleted", data: { sessionID: "ses_v2" } })
    const rows = await waitForRecord(record, "signal", 3)
    assert.deepEqual(rows.map((row) => JSON.parse(row.body)), [
      { session: "ses_v2", event: "SessionStart", kind: "compact" },
      { session: "ses_v2", event: "Notification", kind: "permission_prompt" },
      { session: "ses_v2", event: "SessionEnd", kind: "other" },
    ])
    const info = JSON.parse(readFileSync(join(stateDirectory(directory), "install.json"), "utf8"))
    process.kill(info.pid, "SIGTERM")
    await waitUntilUnhealthy(info.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
    delete process.env.CG_TEST_RECORD
  }
})

test("v2 without ctx.event still sets up", async () => {
  const { setup } = plugin
  const ctx = {
    location: { directory: "/tmp/context-guru-signals-noevent" },
    options: {},
    provider: { transform: async () => {} },
    command: { transform: async () => {} },
    session: { hook: async () => {}, prompt: async () => {} },
  }
  await setup(ctx)
})
