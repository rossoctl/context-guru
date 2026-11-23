import assert from "node:assert/strict"
import test from "node:test"
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { spawn } from "node:child_process"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-disabled-test-"))
process.env.CONTEXT_GURU_PORT_START = "10100"

import plugin, { stateDirectory } from "../index.js"
import tuiModule from "../tui.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

// Issue #1290, owner decision: after an uninstall and a restart, context-guru is gone from OpenCode.

const COMMANDS = ["context-guru-install", "context-guru-preset", "context-guru-status", "context-guru-strategy", "context-guru-uninstall", "context-guru-update"]
const WAY_BACK = /^context-guru is uninstalled for this project; to turn it on again, run: sh .+\/context-guru-install, then restart OpenCode$/

function context(directory, logs = []) {
  return { directory, client: { app: { log: async ({ body }) => { logs.push(body) } } } }
}

function disable(directory) {
  const state = stateDirectory(directory)
  mkdirSync(state, { recursive: true })
  writeFileSync(join(state, "disabled"), "disabled by context-guru-uninstall\n")
  return state
}

// A plain shell with no OpenCode around it: only PATH and HOME.
function sh(script, args = []) {
  return new Promise((resolve) => {
    const child = spawn("sh", [script, ...args], { env: { PATH: process.env.PATH, HOME: process.env.HOME } })
    let stdout = ""
    child.stdout.on("data", (chunk) => { stdout += chunk })
    child.stderr.on("data", (chunk) => { stdout += chunk })
    child.on("close", (status) => resolve({ status, stdout }))
  })
}

async function startV1(directory, logs) {
  const hooks = await plugin.server(context(directory, logs))
  const config = { provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } }
  await hooks.config(config)
  return config
}

function guruCommands(config) {
  return Object.keys(config.command ?? {}).filter((name) => name.startsWith("context-guru-")).sort()
}

function fakeTui(directory) {
  const layers = []
  const api = { state: { path: { directory } }, keymap: { registerLayer: (layer) => { layers.push(layer); return () => {} } } }
  return { api, layers }
}

function mockV2(directory) {
  const commands = []
  const providers = [{ id: "anthropic", name: "Anthropic", settings: { baseURL: "https://api.anthropic.com" } }]
  const ctx = {
    location: { directory },
    options: {},
    provider: { transform: async (callback) => { await callback({ list: () => providers, get: () => providers[0], update: (id, update) => update(providers[0]) }) } },
    command: { transform: async (callback) => { await callback({ add: (definition) => commands.push(definition) }) } },
    session: { hook: async () => {}, prompt: async () => {} },
  }
  return { ctx, commands }
}

test("a start with the disabled marker registers no command and logs the way back", async () => {
  const directory = "/tmp/context-guru-disabled-a"
  const state = disable(directory)
  const logs = []

  const config = await startV1(directory, logs)

  assert.deepEqual(guruCommands(config), [])
  const messages = logs.map((entry) => entry.message)
  assert.equal(messages.filter((message) => WAY_BACK.test(message)).length, 1, messages.join("\n"))
  assert.ok(messages.includes(`context-guru is uninstalled for this project; to turn it on again, run: sh ${join(state, "context-guru-install")}, then restart OpenCode`))
})

test("a start without the disabled marker registers all six commands", async () => {
  const directory = "/tmp/context-guru-disabled-b"
  const hooks = await plugin.server(context(directory))
  const config = { provider: { custom: { options: { baseURL: "https://example.test" } } } }

  await hooks.config(config)

  assert.deepEqual(guruCommands(config), COMMANDS)
})

test("the TUI plugin registers no command for a disabled project", async () => {
  const directory = "/tmp/context-guru-disabled-c"
  disable(directory)
  const { api, layers } = fakeTui(directory)

  await tuiModule.tui(api)

  assert.deepEqual(layers, [])
})

test("the TUI plugin registers both commands for an enabled project", async () => {
  const { api, layers } = fakeTui("/tmp/context-guru-disabled-d")

  await tuiModule.tui(api)

  assert.deepEqual(layers.flatMap((layer) => layer.commands.map((command) => command.title)).sort(), ["Context Guru: cache strategy", "Context Guru: preset"])
})

test("v2 setup with the disabled marker registers no command and logs the way back", async () => {
  const directory = "/tmp/context-guru-disabled-e"
  const state = disable(directory)
  const { ctx, commands } = mockV2(directory)
  const lines = []
  const original = console.log
  console.log = (line) => lines.push(String(line))
  try {
    await plugin.setup(ctx)
  } finally {
    console.log = original
  }

  assert.deepEqual(commands, [])
  assert.deepEqual(lines, [`opencode-context-guru: context-guru is uninstalled for this project; to turn it on again, run: sh ${join(state, "context-guru-install")}, then restart OpenCode`])
})

test("v2 setup without the disabled marker registers all six commands", async () => {
  const { ctx, commands } = mockV2("/tmp/context-guru-disabled-f")
  ctx.provider.transform = async () => {}

  await plugin.setup(ctx)

  assert.deepEqual(commands.map((command) => command.name).sort(), COMMANDS)
})

test("the status script says enabled=no and how to turn it on again when the marker exists", async () => {
  const directory = "/tmp/context-guru-disabled-g"
  const hooks = await plugin.server(context(directory))
  await hooks.config({ provider: { custom: { options: { baseURL: "https://example.test" } } } })
  const state = stateDirectory(directory)

  const enabled = await sh(join(state, "context-guru-status"))
  writeFileSync(join(state, "disabled"), "disabled by context-guru-uninstall\n")
  const disabled = await sh(join(state, "context-guru-status"))

  assert.match(enabled.stdout, /^enabled=yes$/m)
  assert.doesNotMatch(enabled.stdout, /uninstalled/)
  assert.match(disabled.stdout, /^enabled=no$/m)
  assert.match(disabled.stdout, new RegExp(`^note=context-guru is uninstalled for this project; to turn it on again, run: sh ${join(state, "context-guru-install")}, then restart OpenCode$`, "m"))
})

test("uninstall, then a restart has no commands; the install script brings all six back", async () => {
  const directory = "/tmp/context-guru-disabled-h"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  let port
  try {
    await startV1(directory)
    port = Number((await sh(join(state, "context-guru-status"))).stdout.match(/^port=(\d+)$/m)[1])
    const uninstalled = await sh(join(state, "context-guru-uninstall"), ["--yes"])
    assert.match(uninstalled.stdout, /result=uninstalled/)
    await waitUntilUnhealthy(port)

    const afterUninstall = await startV1(directory)
    assert.deepEqual(guruCommands(afterUninstall), [])

    const installed = await sh(join(state, "context-guru-install"))
    assert.match(installed.stdout, /result=re-enabled/)

    const afterInstall = await startV1(directory)
    assert.deepEqual(guruCommands(afterInstall), COMMANDS)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
    await sh(join(state, "context-guru-reset"))
    if (port) await waitUntilUnhealthy(port)
  }
})
