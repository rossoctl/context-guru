import assert from "node:assert/strict"
import test from "node:test"
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-v2-test-"))

import plugin, { stateDirectory } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

// A mock of the OpenCode 2 plugin context, matching the domain shapes the v2 host
// provides: provider.transform editor (list/get/update), command.transform editor
// (add), and session.hook registration with an optional providerID scope.
function mockV2(directory, providers) {
  const registered = { commands: [], hooks: [], prompts: [] }
  const ctx = {
    location: { directory },
    options: {},
    app: { version: "2.test" },
    provider: {
      transform: async (callback) => {
        await callback({
          list: () => providers,
          get: (id) => providers.find((provider) => provider.id === id),
          update: (id, update) => {
            const provider = providers.find((candidate) => candidate.id === id)
            if (!provider) throw new Error(`unknown provider: ${id}`)
            update(provider)
          },
        })
      },
    },
    command: {
      transform: async (callback) => {
        await callback({ add: (definition) => registered.commands.push(definition) })
      },
    },
    session: {
      hook: async (name, callback, options) => { registered.hooks.push({ name, callback, options }) },
      prompt: async (input) => { registered.prompts.push(input) },
    },
    event: { subscribe: async () => {} },
  }
  return { ctx, registered }
}

test("v2 setup routes providers, registers commands, and scopes the session hook", async () => {
  const directory = "/tmp/context-guru-project-v2a"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()

  try {
    const providers = [
      { id: "anthropic", name: "Anthropic", settings: { baseURL: "https://api.anthropic.com" } },
      { id: "openai", name: "OpenAI", settings: { baseURL: "https://api.openai.com/v1" } },
      { id: "custom", name: "Custom", settings: { baseURL: "https://example.test" } },
    ]
    const { ctx, registered } = mockV2(directory, providers)

    await plugin.setup(ctx)

    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.equal(providers[0].settings.baseURL, `http://127.0.0.1:${record.port}/anthropic/v1`)
    assert.equal(providers[1].settings.baseURL, `http://127.0.0.1:${record.port}/openai/v1`)
    assert.equal(providers[2].settings.baseURL, "https://example.test")

    assert.deepEqual(registered.commands.map((command) => command.name).sort(), [
      "context-guru-install",
      "context-guru-status",
      "context-guru-uninstall",
      "context-guru-update",
    ])
    for (const command of registered.commands) assert.equal(typeof command.execute, "function")

    assert.deepEqual(registered.hooks.map((hook) => hook.options.providerID).sort(), ["anthropic", "openai"])

    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("v2 http.request hook sets the session header and recovers a dead proxy", async () => {
  const directory = "/tmp/context-guru-project-v2b"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()

  try {
    const providers = [{ id: "anthropic", name: "Anthropic", settings: { baseURL: "https://api.anthropic.com" } }]
    const { ctx, registered } = mockV2(directory, providers)

    await plugin.setup(ctx)

    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    const hook = registered.hooks.find((candidate) => candidate.options.providerID === "anthropic")

    const event = { sessionID: "session-v2", request: new Request("http://127.0.0.1:1/x", { method: "POST" }) }
    await hook.callback(event)
    assert.equal(event.request.headers.get("x-context-guru-session"), "session-v2")

    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)

    const recovered = { sessionID: "session-v2b", request: new Request("http://127.0.0.1:1/x", { method: "POST" }) }
    await hook.callback(recovered)
    assert.equal(recovered.request.headers.get("x-context-guru-session"), "session-v2b")
    const response = await fetch(`http://127.0.0.1:${record.port}/healthz`)
    assert.equal(response.status, 200)
    const pid = Number(readFileSync(join(state, "proxy.pid"), "utf8"))
    assert.ok(pid > 0)
    assert.notEqual(pid, record.pid)
    process.kill(pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("v2 command execute runs its script and prompts the session", async () => {
  const directory = "/tmp/context-guru-project-v2c"
  const { ctx, registered } = mockV2(directory, [{ id: "custom", name: "Custom" }])

  await plugin.setup(ctx)

  // No Anthropic or OpenAI provider means no routing, but the commands are still
  // registered so /context-guru-install can repair a broken install.
  assert.equal(registered.hooks.length, 0)
  const status = registered.commands.find((command) => command.name === "context-guru-status")
  await status.execute({ sessionID: "session-cmd", prompt: { model: "custom/x" }, delivery: {} })

  const prompted = registered.prompts.at(-1)
  assert.equal(prompted.sessionID, "session-cmd")
  assert.match(prompted.text, /context-guru status/)
  assert.match(prompted.text, /install=missing/)
})

test("v2 setup respects the disabled marker", async () => {
  const directory = "/tmp/context-guru-project-v2d"
  const state = stateDirectory(directory)
  mkdirSync(state, { recursive: true })
  writeFileSync(join(state, "disabled"), "test\n")
  const providers = [{ id: "anthropic", name: "Anthropic", settings: { baseURL: "https://api.anthropic.com" } }]
  const { ctx, registered } = mockV2(directory, providers)

  await plugin.setup(ctx)

  assert.equal(providers[0].settings.baseURL, "https://api.anthropic.com")
  assert.equal(registered.hooks.length, 0)
  assert.equal(registered.commands.length, 4)
  assert.ok(existsSync(join(state, "context-guru-install")))
})

test("v2 setup preserves a local gateway as the upstream", async () => {
  const directory = "/tmp/context-guru-project-v2e"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()

  try {
    const providers = [{ id: "anthropic", name: "Anthropic", settings: { baseURL: "http://localhost:4000/anthropic" } }]
    const { ctx } = mockV2(directory, providers)

    await plugin.setup(ctx)

    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    const index = record.args.indexOf("--anthropic-upstream")
    assert.equal(record.args[index + 1], "http://localhost:4000/anthropic")
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("v2 setup recovers the recorded upstream when a provider points at this plugin's own proxy", async () => {
  const directory = "/tmp/context-guru-project-v2f"
  const state = stateDirectory(directory)
  mkdirSync(state, { recursive: true })
  const recordedPort = 18787
  writeFileSync(join(state, "install.json"), JSON.stringify({
    port: recordedPort,
    fingerprint: JSON.stringify({ preset: "off", cacheStrategy: "5-min-ping", routes: [["anthropic", "https://gw.example.test"]] }),
  }) + "\n")
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()

  try {
    const providers = [{ id: "anthropic", name: "Anthropic", settings: { baseURL: `http://127.0.0.1:${recordedPort}/anthropic/v1` } }]
    const { ctx } = mockV2(directory, providers)

    await plugin.setup(ctx)

    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    const index = record.args.indexOf("--anthropic-upstream")
    assert.equal(record.args[index + 1], "https://gw.example.test")
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})
