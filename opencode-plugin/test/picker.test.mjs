import assert from "node:assert/strict"
import test from "node:test"
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { spawn } from "node:child_process"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-picker-test-"))
process.env.CONTEXT_GURU_PORT_START = "9700"

import plugin, { stateDirectory, renderProxyConfig, canonicalStrategy, effectiveSettings } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

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

// The expected text is copied from context-guru-plugin/scripts/settings.py (STRATEGIES).
test("strategy config matches the Claude Code plugin for every name", () => {
  assert.equal(renderProxyConfig("off", "none"), "")
  const auto = renderProxyConfig("off", "auto")
  assert.match(auto, /^# context-guru: strategy=auto written by /)
  assert.match(auto, /preset: off\ncache:\n/)
  for (const line of ["keepalive: true", "keepalive_idle_seconds: 240", "keepalive_max_pings: 2", "keepalive_max_usd_per_ping: 0.5", "keepalive_min_prefix_tokens: 80000"]) assert.ok(auto.includes(`  ${line}\n`), line)
  // The proxy default for a 1-hour cache is left out, so an older proxy still loads the file.
  assert.ok(!auto.includes("_1h"))
  assert.ok(renderProxyConfig("off", "1-ping").includes("keepalive_max_pings: 1\n"))
  const two = renderProxyConfig("off", "2-ping")
  assert.ok(two.includes("keepalive_max_pings: 2\n") && two.includes("keepalive_max_pings_1h: 2\n"))
  const head = renderProxyConfig("medium", "1-hour-head")
  assert.ok(head.includes("preset: medium\n") && head.includes("head_ttl_1h: true\n") && head.includes("head_ttl_min_tokens: 50000\n") && !head.includes("keepalive"))
})

test("default strategy is auto and the old name 5-min-ping is an alias for it", () => {
  assert.equal(canonicalStrategy("5-min-ping"), "auto")
  assert.equal(canonicalStrategy("nonsense"), undefined)
  assert.deepEqual(effectiveSettings("/nonexistent-state", {}), { preset: "off", cacheStrategy: "auto" })
  assert.deepEqual(effectiveSettings("/nonexistent-state", { preset: "ultrahigh", cacheStrategy: "5-min-ping" }), { preset: "ultrahigh", cacheStrategy: "auto" })
  assert.deepEqual(effectiveSettings("/nonexistent-state", { preset: "bogus", cache_strategy: "bogus" }), { preset: "off", cacheStrategy: "auto" })
})

test("the proxy gets the strategy config at start, and none writes no keep-alive", async () => {
  for (const [strategy, expectKeepalive] of [[undefined, true], ["none", false]]) {
    const directory = `/tmp/context-guru-picker-start-${strategy}`
    process.env.CONTEXT_GURU_BIN = stubProxyBinary()
    try {
      const hooks = await plugin.server({ ...context(directory), }, strategy ? { cacheStrategy: strategy } : {})
      await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
      const state = stateDirectory(directory)
      const yaml = readFileSync(join(state, "proxy.yaml"), "utf8")
      assert.equal(yaml.includes("keepalive: true"), expectKeepalive)
      assert.match(yaml, /preset: off/)
      const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
      process.kill(record.pid, "SIGTERM")
      await waitUntilUnhealthy(record.port)
    } finally {
      delete process.env.CONTEXT_GURU_BIN
    }
  }
})

test("picker scripts show choices, reject unknown names, and save without a restart", async () => {
  const directory = "/tmp/context-guru-picker-script"
  const state = stateDirectory(directory)
  const hooks = await plugin.server(context(directory))
  await hooks.config({ provider: { custom: { options: { baseURL: "https://example.test" } } } })

  const shown = await run(join(state, "context-guru-preset"))
  assert.equal(shown.status, 0)
  for (const name of ["off", "conservative", "medium", "high", "xhigh", "ultrahigh"]) assert.ok(shown.out.includes(`choice ${name}:`), name)
  assert.match(shown.out, /saved=off/)
  const strategies = await run(join(state, "context-guru-strategy"))
  for (const name of ["none", "auto", "1-ping", "2-ping", "1-hour-head"]) assert.ok(strategies.out.includes(`choice ${name}:`), name)
  assert.match(strategies.out, /saved=auto/)
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
  assert.deepEqual(effectiveSettings(state, { preset: "off", cacheStrategy: "auto" }), { preset: "medium", cacheStrategy: "none" })
})

test("set --restart restarts the project proxy with the new config", async () => {
  const directory = "/tmp/context-guru-picker-restart"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
    const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.ok(readFileSync(join(state, "proxy.yaml"), "utf8").includes("keepalive: true"))

    const result = await run(join(state, "context-guru-strategy"), ["set", "none", "--restart"])
    assert.match(result.out, /result=restarted/)
    assert.ok(!readFileSync(join(state, "proxy.yaml"), "utf8").includes("keepalive"))
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
    assert.ok(yaml.includes("preset: high") && yaml.includes("keepalive_max_pings_1h: 2"))
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
