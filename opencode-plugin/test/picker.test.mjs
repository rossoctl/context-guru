import assert from "node:assert/strict"
import test from "node:test"
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { spawn } from "node:child_process"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-picker-test-"))
process.env.CONTEXT_GURU_PORT_START = "9700"

import plugin, { stateDirectory, renderProxyConfig, effectiveSettings, loadStrategyTable, offeredStrategies, resolveStrategy } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy, FAKE_STRATEGIES } from "./helpers.mjs"

const context = (directory) => ({ directory, client: { app: { log: async () => {} } } })

function run(script, args = []) {
  return new Promise((resolve) => {
    const child = spawn("sh", [script, ...args])
    let out = ""
    child.stdout.on("data", (chunk) => { out += chunk })
    child.stderr.on("data", (chunk) => { out += chunk })
    child.on("close", (status) => resolve({ status, out }))
  })
}

test("the plugin writes the strategy name and lets the proxy fill the keys", () => {
  const yaml = renderProxyConfig("medium", "auto")
  assert.match(yaml, /preset: medium\ncache:\n  strategy: auto\n/)
  assert.ok(!yaml.includes("keepalive"), "no cache key is written by the plugin")
  assert.match(renderProxyConfig("off", "none"), /strategy: none/)
})

test("the table comes only from the proxy binary, and a binary without it is an error that says to update", () => {
  const table = loadStrategyTable(stubProxyBinary())
  assert.equal(table.default, "auto")
  assert.deepEqual(table.strategies.map((row) => row.name), FAKE_STRATEGIES.strategies.map((row) => row.name))
  // An old binary does not know the word: it exits non-zero, or prints something that is not the table.
  const dir = mkdtempSync(join(tmpdir(), "context-guru-old-"))
  for (const [name, body] of [["fails", "#!/bin/sh\nexit 2\n"], ["junk", "#!/bin/sh\necho serving\n"], ["empty", "#!/bin/sh\necho '{}'\n"]]) {
    const path = join(dir, name)
    writeFileSync(path, body, { mode: 0o755 })
    assert.throws(() => loadStrategyTable(path), /cannot list cache strategies; update the proxy to v0\.7\.1/, name)
  }
})

test("a binary that does not know the word and keeps serving is killed at the limit, not waited for", () => {
  // The v0.7.0 proxy treats "strategies" as an ordinary argument and starts serving. A stand-in does the same.
  const dir = mkdtempSync(join(tmpdir(), "context-guru-serving-"))
  const path = join(dir, "context-guru-proxy")
  writeFileSync(path, "#!/bin/sh\nexec sleep 60\n", { mode: 0o755 })
  const started = Date.now()
  assert.throws(() => loadStrategyTable(path), /cannot list cache strategies; update the proxy/)
  assert.ok(Date.now() - started < 15000, `took ${Date.now() - started} ms`)
})

test("only strategies for a routed provider are offered", () => {
  const names = (dialects) => offeredStrategies(FAKE_STRATEGIES, dialects).map((row) => row.name)
  assert.deepEqual(names(["anthropic"]), ["none", "auto", "1-ping", "2-ping", "1-hour-head"])
  assert.deepEqual(names(["openai"]), ["none", "30-min-ping"])
  assert.deepEqual(names(["anthropic", "openai"]), FAKE_STRATEGIES.strategies.map((row) => row.name))
})

test("the default is the proxy default, the alias is read as its new name, and an unfit name gives none", () => {
  assert.deepEqual(resolveStrategy(FAKE_STRATEGIES, undefined, ["anthropic"]), { name: "auto" })
  assert.deepEqual(resolveStrategy(FAKE_STRATEGIES, "5-min-ping", ["anthropic"]), { name: "auto" })
  // The proxy default is not offered on an OpenAI-only setup: run none, and say why.
  const openai = resolveStrategy(FAKE_STRATEGIES, undefined, ["openai"])
  assert.equal(openai.name, "none")
  assert.match(openai.reason, /not offered for openai/)
  assert.equal(resolveStrategy(FAKE_STRATEGIES, "nonsense", ["anthropic"]).name, "none")
  assert.deepEqual(effectiveSettings("/nonexistent-state", { preset: "ultrahigh", cacheStrategy: "5-min-ping" }), { preset: "ultrahigh", requestedStrategy: "5-min-ping" })
  assert.deepEqual(effectiveSettings("/nonexistent-state", { preset: "bogus" }), { preset: "off", requestedStrategy: undefined })
})

test("the proxy gets cache.strategy at start, and the picker files hold only the offered rows", async () => {
  for (const [strategy, expected] of [[undefined, "auto"], ["none", "none"], ["1-ping", "1-ping"]]) {
    const directory = `/tmp/context-guru-picker-start-${strategy}`
    process.env.CONTEXT_GURU_BIN = stubProxyBinary()
    try {
      const hooks = await plugin.server(context(directory), strategy ? { cacheStrategy: strategy } : {})
      await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
      const state = stateDirectory(directory)
      const yaml = readFileSync(join(state, "proxy.yaml"), "utf8")
      assert.match(yaml, new RegExp(`cache:\\n  strategy: ${expected}\\n`))
      assert.ok(!yaml.includes("keepalive"))
      const rows = readFileSync(join(state, "strategies.tsv"), "utf8").trim().split("\n").map((line) => line.split("\t")[0])
      assert.deepEqual(rows, ["none", "auto", "1-ping", "2-ping", "1-hour-head"])
      const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
      process.kill(record.pid, "SIGTERM")
      await waitUntilUnhealthy(record.port)
    } finally {
      delete process.env.CONTEXT_GURU_BIN
    }
  }
})

test("an OpenAI-only setup offers only the OpenAI and any strategies, and runs none by default", async () => {
  const directory = "/tmp/context-guru-picker-openai"
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: { openai: { options: { baseURL: "https://api.openai.com/v1" } } } })
    const state = stateDirectory(directory)
    assert.match(readFileSync(join(state, "proxy.yaml"), "utf8"), /strategy: none/)
    const shown = await run(join(state, "context-guru-strategy"))
    assert.ok(shown.out.includes("choice 30-min-ping:") && shown.out.includes("choice none:"))
    assert.ok(!shown.out.includes("choice auto:") && !shown.out.includes("choice 1-ping:"))
    const refused = await run(join(state, "context-guru-strategy"), ["set", "2-ping"])
    assert.equal(refused.status, 2)
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("a proxy that cannot list strategies is not started and the error says to update", async () => {
  const directory = "/tmp/context-guru-picker-old"
  const dir = mkdtempSync(join(tmpdir(), "context-guru-old-bin-"))
  const old = join(dir, "context-guru-proxy")
  writeFileSync(old, "#!/bin/sh\nexit 2\n", { mode: 0o755 })
  process.env.CONTEXT_GURU_BIN = old
  const logged = []
  try {
    const hooks = await plugin.server({ directory, client: { app: { log: async ({ body }) => { logged.push(body.message) } } } })
    const config = { provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } }
    await hooks.config(config)
    assert.equal(config.provider.anthropic.options.baseURL, "https://api.anthropic.com")
    assert.ok(logged.some((message) => /cannot list cache strategies; update the proxy/.test(message)), logged.join("|"))
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("picker scripts show choices, reject unknown names, and save without a restart", async () => {
  const directory = "/tmp/context-guru-picker-script"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  const hooks = await plugin.server(context(directory))
  await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })

  const shown = await run(join(state, "context-guru-preset"))
  assert.equal(shown.status, 0)
  for (const name of ["off", "conservative", "medium", "high", "xhigh", "ultrahigh"]) assert.ok(shown.out.includes(`choice ${name}:`), name)
  assert.match(shown.out, /saved=off/)
  const strategies = await run(join(state, "context-guru-strategy"))
  for (const name of ["none", "auto", "1-ping", "2-ping", "1-hour-head"]) assert.ok(strategies.out.includes(`choice ${name}:`), name)
  assert.ok(!strategies.out.includes("choice 30-min-ping:"))
  assert.match(strategies.out, /saved=auto/)
  assert.match(strategies.out, /spends=auto,1-ping,2-ping/)
  assert.ok(!strategies.out.includes("5-min-ping"))

  const bad = await run(join(state, "context-guru-preset"), ["set", "house"])
  assert.equal(bad.status, 2)
  assert.match(bad.out, /reason=unknown_preset/)

  const saved = await run(join(state, "context-guru-preset"), ["set", "medium"])
  assert.equal(saved.status, 0)
  assert.match(saved.out, /result=saved/)
  assert.match(saved.out, /restart_needed=true/)
  assert.match(readFileSync(join(state, "settings.env"), "utf8"), /preset=medium\nstrategy=auto\n/)

  const alias = await run(join(state, "context-guru-strategy"), ["set", "5-min-ping"])
  assert.match(alias.out, /strategy=auto/)
  assert.match(alias.out, /cost=/)
  const none = await run(join(state, "context-guru-strategy"), ["set", "none"])
  assert.ok(!none.out.includes("cost="))
  assert.match(readFileSync(join(state, "settings.env"), "utf8"), /preset=medium\nstrategy=none\n/)
  assert.deepEqual(effectiveSettings(state, { preset: "off", cacheStrategy: "auto" }), { preset: "medium", requestedStrategy: "none" })
  const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
  process.kill(record.pid, "SIGTERM")
  await waitUntilUnhealthy(record.port)
  delete process.env.CONTEXT_GURU_BIN
})

test("set --restart restarts the project proxy with the new config", async () => {
  const directory = "/tmp/context-guru-picker-restart"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.match(readFileSync(join(state, "proxy.yaml"), "utf8"), /strategy: auto/)

    const result = await run(join(state, "context-guru-strategy"), ["set", "none", "--restart"])
    assert.match(result.out, /result=restarted/)
    assert.match(readFileSync(join(state, "proxy.yaml"), "utf8"), /strategy: none/)
    const after = Number(readFileSync(join(state, "proxy.pid"), "utf8"))
    assert.ok(after > 0 && after !== before.pid)
    assert.equal((await fetch(`http://127.0.0.1:${before.port}/healthz`)).status, 200)
    assert.match(readFileSync(join(state, "install.env"), "utf8"), /strategy=none/)

    // The next plugin start finds the saved choice and the running proxy already matching it.
    const again = await plugin.server(context(directory))
    await again.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    assert.equal(Number(readFileSync(join(state, "proxy.pid"), "utf8")), after)
    process.kill(after, "SIGTERM")
    await waitUntilUnhealthy(before.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("a changed saved setting makes the next startup replace the running proxy", async () => {
  const directory = "/tmp/context-guru-picker-replace"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const first = await plugin.server(context(directory))
    await first.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    writeFileSync(join(state, "settings.env"), "preset=high\nstrategy=2-ping\n")

    const second = await plugin.server(context(directory))
    await second.config({ provider: { anthropic: { options: { baseURL: "http://127.0.0.1:" + before.port + "/anthropic/v1" } } } })
    const after = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.notEqual(after.pid, before.pid)
    assert.equal(after.port, before.port)
    const yaml = readFileSync(join(state, "proxy.yaml"), "utf8")
    assert.ok(yaml.includes("preset: high") && yaml.includes("strategy: 2-ping"))
    process.kill(after.pid, "SIGTERM")
    await waitUntilUnhealthy(after.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("two instances starting in one project share one proxy", async () => {
  const directory = "/tmp/context-guru-picker-race"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const config = () => ({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    const [one, two] = await Promise.all([plugin.server(context(directory)), plugin.server(context(directory))])
    const [a, b] = [config(), config()]
    await Promise.all([one.config(a), two.config(b)])
    assert.equal(a.provider.anthropic.options.baseURL, b.provider.anthropic.options.baseURL)
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.equal(Number(readFileSync(join(state, "proxy.pid"), "utf8")), record.pid)
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("two projects started together get different ports", async () => {
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  const dirs = ["/tmp/context-guru-picker-p1", "/tmp/context-guru-picker-p2"]
  try {
    const configs = dirs.map(() => ({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } }))
    const hooks = await Promise.all(dirs.map((directory) => plugin.server(context(directory))))
    await Promise.all(hooks.map((hook, index) => hook.config(configs[index])))
    assert.notEqual(configs[0].provider.anthropic.options.baseURL, configs[1].provider.anthropic.options.baseURL)
    for (const directory of dirs) {
      const record = JSON.parse(readFileSync(join(stateDirectory(directory), "install.json"), "utf8"))
      process.kill(record.pid, "SIGTERM")
      await waitUntilUnhealthy(record.port)
    }
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})
