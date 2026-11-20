import assert from "node:assert/strict"
import test from "node:test"
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { spawn } from "node:child_process"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-1161-test-"))
process.env.CONTEXT_GURU_PORT_START = "9950"

import plugin, { stateDirectory, effectiveSettings } from "../index.js"
import { FAKE_STRATEGIES, stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

const context = (directory, logged = []) => ({ directory, client: { app: { log: async ({ body }) => { logged.push(body.message) } } } })

// Runs the config hook with only an unsupported provider, so the command
// scripts for `directory` are written without starting a proxy.
async function registerScripts(directory) {
  const hooks = await plugin.server(context(directory))
  const config = { provider: { custom: { options: { baseURL: "https://example.test" } } } }
  await hooks.config(config)
  return config
}

function run(script, args = []) {
  return new Promise((resolve) => {
    const child = spawn("sh", [script, ...args])
    let out = ""
    child.stdout.on("data", (chunk) => { out += chunk })
    child.stderr.on("data", (chunk) => { out += chunk })
    child.on("close", (status) => resolve({ status, out }))
  })
}

// A stub proxy that also answers /stats with a preset, like the real one.
function statsProxyBinary(runningPreset) {
  const dir = mkdtempSync(join(tmpdir(), "context-guru-stats-"))
  const path = join(dir, "context-guru-proxy")
  writeFileSync(path, `#!/usr/bin/env node
const { createServer } = require("node:http")
if (process.argv[2] === "strategies") { console.log(${JSON.stringify(JSON.stringify(FAKE_STRATEGIES))}); process.exit(0) }
const listen = process.argv.indexOf("--listen")
const port = Number(process.argv[listen + 1].split(":")[1])
createServer((request, response) => {
  if (request.url === "/healthz") response.end("ok")
  else if (request.url === "/stats") response.end(JSON.stringify({ preset: ${JSON.stringify(runningPreset)}, requests: 1 }))
  else { response.statusCode = 404; response.end() }
}).listen(port, "127.0.0.1")
`, { mode: 0o755 })
  return path
}

test("preset command lists every choice including ultrahigh, and only advises against recommending it", async () => {
  const config = await registerScripts("/tmp/context-guru-1161-preset")
  const template = config.command["context-guru-preset"].template
  assert.match(template, /List every choice from the block, one line each, including ultrahigh/)
  assert.ok(!template.includes("Do not suggest ultrahigh"), "the outro must not tell the agent to hide ultrahigh")
  assert.match(template, /Do not recommend ultrahigh unless the user asks for it/)
})

test("a bad preset logs a warning at startup, like a bad cache strategy", async () => {
  const logged = []
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const directory = "/tmp/context-guru-1161-bogus"
    const hooks = await plugin.server(context(directory, logged), { preset: "bogus" })
    await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    const state = stateDirectory(directory)
    assert.match(readFileSync(join(state, "proxy.yaml"), "utf8"), /preset: off/)
    assert.ok(logged.some((message) => /preset "bogus" is not a known preset; using off/.test(message)), logged.join("|"))
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
  // Control: a valid preset warns about nothing.
  const loggedValid = []
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const directory = "/tmp/context-guru-1161-valid"
    const hooks = await plugin.server(context(directory, loggedValid), { preset: "medium" })
    await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    assert.ok(!loggedValid.some((message) => /preset "/.test(message)), loggedValid.join("|"))
    const state = stateDirectory(directory)
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("a saved bad preset also reports why it falls back", () => {
  const state = stateDirectory("/tmp/context-guru-1161-saved-bogus")
  mkdirSync(state, { recursive: true })
  writeFileSync(join(state, "settings.env"), "preset=bogus\n")
  assert.deepEqual(effectiveSettings(state, {}), { preset: "off", requestedStrategy: undefined, presetReason: 'preset "bogus" is not a known preset; using off' })
})

test("install command says the block is script output, not the user's paste", async () => {
  const config = await registerScripts("/tmp/context-guru-1161-install")
  const template = config.command["context-guru-install"].template
  assert.match(template, /output of the install script, which ran just now/)
  assert.match(template, /not text the user pasted/)
})

test("set --restart skips the restart when the running proxy already uses the saved choice", async () => {
  const directory = "/tmp/context-guru-1161-restart"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))

    // The plugin already applied the saved choice (off) at startup, so this is
    // the second restart the issue reports for one switch with `opencode run`.
    const skipped = await run(join(state, "context-guru-preset"), ["set", "off", "--restart"])
    assert.equal(skipped.status, 0)
    assert.match(skipped.out, /result=already_applied/)
    assert.ok(!skipped.out.includes("result=restarted"))
    assert.equal(Number(readFileSync(join(state, "proxy.pid"), "utf8")), before.pid)
    assert.equal((await fetch(`http://127.0.0.1:${before.port}/healthz`)).status, 200)

    // A real change still restarts, and the next identical --restart then skips.
    const changed = await run(join(state, "context-guru-strategy"), ["set", "none", "--restart"])
    assert.match(changed.out, /result=restarted/)
    const restartedPid = Number(readFileSync(join(state, "proxy.pid"), "utf8"))
    assert.notEqual(restartedPid, before.pid)
    const skippedAgain = await run(join(state, "context-guru-strategy"), ["set", "none", "--restart"])
    assert.match(skippedAgain.out, /result=already_applied/)
    assert.equal(Number(readFileSync(join(state, "proxy.pid"), "utf8")), restartedPid)
    process.kill(restartedPid, "SIGTERM")
    await waitUntilUnhealthy(before.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("set --restart still restarts when the live preset disagrees with the saved choice", async () => {
  const directory = "/tmp/context-guru-1161-restart-guard"
  const state = stateDirectory(directory)
  // The proxy reports it runs "high" while the install record says "off":
  // the saved choice is not what runs, so the restart must happen.
  process.env.CONTEXT_GURU_BIN = statsProxyBinary("high")
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    const result = await run(join(state, "context-guru-preset"), ["set", "off", "--restart"])
    assert.match(result.out, /result=restarted/)
    assert.ok(!result.out.includes("result=already_applied"))
    assert.notEqual(Number(readFileSync(join(state, "proxy.pid"), "utf8")), before.pid)
    const pid = Number(readFileSync(join(state, "proxy.pid"), "utf8"))
    process.kill(pid, "SIGTERM")
    await waitUntilUnhealthy(before.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("install after --purge prints one result, not two that disagree", async () => {
  const directory = "/tmp/context-guru-1161-purge"
  await registerScripts(directory)
  const state = stateDirectory(directory)
  // What uninstall --yes --purge leaves behind: a disabled marker, no install.env.
  writeFileSync(join(state, "disabled"), "disabled by context-guru-uninstall\n")
  assert.ok(!existsSync(join(state, "install.env")))

  const result = await run(join(state, "context-guru-install"))

  assert.equal(result.status, 0)
  const results = result.out.split("\n").filter((line) => line.startsWith("result="))
  assert.deepEqual(results, ["result=re-enabled"])
  assert.match(result.out, /note=the next OpenCode start sets up the proxy again/)
  assert.ok(!existsSync(join(state, "disabled")))
})
