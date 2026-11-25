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

// The agent follows the uninstall command text. It must say what the script says (#1291 review, round 2).
test("the uninstall command text names the install script and says fallback: false only matters if you set it", async () => {
  const directory = "/tmp/context-guru-disabled-c"
  const hooks = await plugin.server(context(directory))
  const config = { provider: { custom: { options: { baseURL: "https://example.test" } } } }

  await hooks.config(config)
  const text = config.command["context-guru-uninstall"].template

  assert.ok(text.includes(`sh "${join(stateDirectory(directory), "context-guru-install")}" and restart OpenCode`), text)
  assert.match(text, /only if they set the plugin option fallback: false themselves/i)
  assert.doesNotMatch(text, /with the plugin option fallback: false, they must/)
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
    // The way back must still work after a restart, when /context-guru-install is gone: name the script.
    assert.ok(uninstalled.stdout.includes(`undo=sh ${join(state, "context-guru-install")}, then restart OpenCode`), uninstalled.stdout)
    // The fallback note must not read as if fallback: false were set (a real agent told the user it was).
    assert.match(uninstalled.stdout, /only if you set the plugin option fallback: false yourself/)
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

// In the session of an uninstall, before a restart, OpenCode keeps the commands it loaded. Each one must then
// say that context-guru is uninstalled and change nothing, as the Pi package does (#1290).
const GUARDED = ["context-guru-update", "context-guru-preset", "context-guru-strategy"]

test("in the session of an uninstall, update, preset and strategy say uninstalled and change nothing", async () => {
  const directory = "/tmp/context-guru-disabled-session"
  const config = await startV1(directory, [])
  assert.equal(guruCommands(config).length, 6)
  const state = disable(directory)

  for (const name of GUARDED) {
    for (const args of [[], ["set", "none"], ["--yes"]]) {
      const { status, stdout } = await sh(join(state, name), args)
      assert.equal(status, 0, `${name} ${args.join(" ")}`)
      const lines = stdout.trim().split("\n")
      assert.deepEqual(lines.slice(0, 1), ["enabled=no"], `${name} ${args.join(" ")}: ${stdout}`)
      assert.match(lines[1].replace(/^note=/, ""), WAY_BACK)
      assert.equal(lines.length, 2, `${name} ${args.join(" ")} printed more: ${stdout}`)
    }
  }
})

test("a command whose script is gone says uninstalled instead of a shell error", async () => {
  const directory = "/tmp/context-guru-disabled-gone"
  const config = await startV1(directory, [])
  const state = stateDirectory(directory)
  const block = config.command["context-guru-preset"].template.match(/!`([^`]+)`/)[1]
  const { rmSync } = await import("node:fs")
  rmSync(join(state, "context-guru-preset"))

  const out = await new Promise((resolve) => {
    const child = spawn("sh", ["-c", block])
    let text = ""
    child.stdout.on("data", (chunk) => { text += chunk })
    child.stderr.on("data", (chunk) => { text += chunk })
    child.on("close", (status) => resolve({ status, text }))
  })

  assert.equal(out.status, 0)
  assert.equal(out.text, "enabled=no\nnote=context-guru is uninstalled for this project; restart OpenCode to remove these commands\n")
})

test("runCommandScript for a script that is gone says uninstalled (OpenCode 2 and the TUI)", async () => {
  const { runCommandScript } = await import("../index.js")
  const state = mkdtempSync(join(tmpdir(), "context-guru-gone-"))

  const out = await runCommandScript(state, "context-guru-strategy", ["set", "none"])

  assert.deepEqual(out, { status: 0, output: "enabled=no\nnote=context-guru is uninstalled for this project; restart OpenCode to remove these commands\n" })
})

test("every command text but status tells the agent to stop at enabled=no", async () => {
  const config = await startV1("/tmp/context-guru-disabled-text", [])

  for (const name of COMMANDS.filter((command) => command !== "context-guru-status")) {
    assert.match(config.command[name].template, /If the block printed enabled=no, say only that context-guru is uninstalled for this project, give the way back from its note, and do nothing else\./, name)
  }
})
