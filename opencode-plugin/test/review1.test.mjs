import assert from "node:assert/strict"
import test from "node:test"
import { existsSync, mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync, utimesSync } from "node:fs"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { spawn } from "node:child_process"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-review1-test-"))
process.env.CONTEXT_GURU_PORT_START = "9300"

import plugin, { stateDirectory, effectiveSettings, withLock } from "../index.js"
import { stubProxyBinary, deadProxyBinary, pickyProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

const context = (directory, logs = []) => ({ directory, client: { app: { log: async ({ body }) => { logs.push(body.message) } } } })
const anthropic = (baseURL = "https://api.anthropic.com") => ({ anthropic: { options: { baseURL } } })

function run(script, args = []) {
  return new Promise((resolve) => {
    const child = spawn("sh", [script, ...args])
    let out = ""
    child.stdout.on("data", (chunk) => { out += chunk })
    child.stderr.on("data", (chunk) => { out += chunk })
    child.on("close", (status) => resolve({ status, out }))
  })
}

// ---- Round 1, finding 1 (reviewer 1): a picker set must save only the key it changed.
test("a preset set before any install does not save a default strategy over the plugin option", async () => {
  const directory = "/tmp/context-guru-r1-noinstall"
  const state = stateDirectory(directory)
  const options = { preset: "high", cacheStrategy: "none" }
  const hooks = await plugin.server(context(directory), options)
  // No Anthropic or OpenAI provider: nothing is routed, so there is no install.env.
  await hooks.config({ provider: { custom: { options: { baseURL: "https://example.test" } } } })
  assert.ok(!existsSync(join(state, "install.env")))

  const shown = await run(join(state, "context-guru-preset"))
  assert.match(shown.out, /saved=high/, "show must report the plugin option, not the default")

  const saved = await run(join(state, "context-guru-preset"), ["set", "medium"])
  assert.match(saved.out, /result=saved/)
  const settings = readFileSync(join(state, "settings.env"), "utf8")
  assert.match(settings, /preset=medium/)
  assert.ok(!settings.includes("strategy="), `settings.env must not hold a strategy: ${settings}`)
  assert.deepEqual(effectiveSettings(state, options), { preset: "medium", requestedStrategy: "none" })
})

test("a strategy set keeps the saved preset and does not invent one", async () => {
  const directory = "/tmp/context-guru-r1-strategy-only"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const options = { preset: "high" }
    const hooks = await plugin.server(context(directory), options)
    await hooks.config({ provider: anthropic() })
    const saved = await run(join(state, "context-guru-strategy"), ["set", "2-ping"])
    assert.match(saved.out, /result=saved/)
    const settings = readFileSync(join(state, "settings.env"), "utf8")
    assert.match(settings, /strategy=2-ping/)
    assert.ok(!settings.includes("preset="), `settings.env must not hold a preset: ${settings}`)
    assert.deepEqual(effectiveSettings(state, options), { preset: "high", requestedStrategy: "2-ping" })
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

// ---- Round 1, finding 2 (reviewer 2): the lock must outlive the work it protects.
test("a lock that is held and kept alive is not broken, even when it is old", async () => {
  const root = mkdtempSync(join(tmpdir(), "context-guru-lock-"))
  const lock = join(root, ".lock")
  const longer = Date.now() + 3000
  let secondRanWhileFirstHeld = false
  let firstDone = false
  const first = withLock(root, async () => {
    // Pretend the work started long ago: the lock directory looks 2 minutes old. Only the heartbeat,
    // which touches it again, keeps it from looking stale to the second caller.
    const old = new Date(Date.now() - 120000)
    utimesSync(lock, old, old)
    while (Date.now() < longer) await new Promise((resolve) => setTimeout(resolve, 100))
    firstDone = true
  }, { heartbeatMs: 100, staleMs: 1000 })
  await new Promise((resolve) => setTimeout(resolve, 1500))
  const second = withLock(root, async () => { secondRanWhileFirstHeld = !firstDone }, { heartbeatMs: 100, staleMs: 1000 })
  await Promise.all([first, second])
  assert.equal(secondRanWhileFirstHeld, false, "the second caller broke a live lock")
})

test("a lock whose holder is gone is broken after the stale age", async () => {
  const root = mkdtempSync(join(tmpdir(), "context-guru-lock-stale-"))
  mkdirSync(join(root, ".lock"))
  const old = new Date(Date.now() - 120000)
  utimesSync(join(root, ".lock"), old, old)
  let ran = false
  await withLock(root, async () => { ran = true }, { heartbeatMs: 100, staleMs: 1000 })
  assert.equal(ran, true)
})

test("a pinned release that is not published gives an error that says so", async () => {
  const directory = "/tmp/context-guru-r2-unpublished"
  const previousBin = process.env.CONTEXT_GURU_BIN
  const previousPath = process.env.PATH
  const previousReleases = process.env.CONTEXT_GURU_RELEASES_URL
  delete process.env.CONTEXT_GURU_BIN
  process.env.PATH = "/usr/bin:/bin"
  const { createServer } = await import("node:http")
  const server = createServer((request, response) => { response.statusCode = 404; response.end() })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  process.env.CONTEXT_GURU_RELEASES_URL = `http://127.0.0.1:${server.address().port}/releases`
  const logs = []
  try {
    const hooks = await plugin.server(context(directory, logs))
    const config = { provider: anthropic() }
    await hooks.config(config)
    assert.equal(config.provider.anthropic.options.baseURL, "https://api.anthropic.com")
    assert.ok(logs.some((message) => /v0\.7\.2 is not published yet/.test(message)), logs.join("|"))
  } finally {
    server.close()
    process.env.PATH = previousPath
    if (previousBin !== undefined) process.env.CONTEXT_GURU_BIN = previousBin
    if (previousReleases === undefined) delete process.env.CONTEXT_GURU_RELEASES_URL
    else process.env.CONTEXT_GURU_RELEASES_URL = previousReleases
  }
})

test("the download runs outside the lock", async () => {
  const directory = "/tmp/context-guru-r1-download-lock"
  const root = process.env.CONTEXT_GURU_STATE
  const previousBin = process.env.CONTEXT_GURU_BIN
  const previousPath = process.env.PATH
  delete process.env.CONTEXT_GURU_BIN
  process.env.PATH = "/usr/bin:/bin"
  let lockSeen
  const { createServer } = await import("node:http")
  const server = createServer((request, response) => {
    // While the plugin downloads, another instance must be able to take the lock.
    lockSeen = existsSync(join(root, ".lock"))
    response.statusCode = 404
    response.end()
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  const previousReleases = process.env.CONTEXT_GURU_RELEASES_URL
  process.env.CONTEXT_GURU_RELEASES_URL = `http://127.0.0.1:${server.address().port}/releases`
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    assert.equal(lockSeen, false, "the lock was held during the download")
  } finally {
    server.close()
    process.env.PATH = previousPath
    if (previousBin !== undefined) process.env.CONTEXT_GURU_BIN = previousBin
    if (previousReleases === undefined) delete process.env.CONTEXT_GURU_RELEASES_URL
    else process.env.CONTEXT_GURU_RELEASES_URL = previousReleases
  }
})

test("startProxy fails fast when its own child exits, and leaves the base URL alone", async () => {
  const directory = "/tmp/context-guru-r1-child-exits"
  process.env.CONTEXT_GURU_BIN = deadProxyBinary()
  const logs = []
  try {
    const hooks = await plugin.server(context(directory, logs))
    const config = { provider: anthropic() }
    const started = Date.now()
    await hooks.config(config)
    assert.ok(Date.now() - started < 8000, `waited ${Date.now() - started} ms for a child that had exited`)
    assert.equal(config.provider.anthropic.options.baseURL, "https://api.anthropic.com")
    assert.ok(logs.some((message) => /proxy startup failed/.test(message)), logs.join("|"))
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

// ---- Round 1, finding 3 (reviewer 2): fail open when the proxy cannot start.
test("v1: a routed provider keeps its base URL when the proxy cannot start", async () => {
  process.env.CONTEXT_GURU_BIN = deadProxyBinary()
  try {
    const hooks = await plugin.server(context("/tmp/context-guru-r1-v1-failopen"))
    const config = { provider: { ...anthropic(), openai: { options: { baseURL: "https://api.openai.com/v1" } } } }
    await hooks.config(config)
    assert.equal(config.provider.anthropic.options.baseURL, "https://api.anthropic.com")
    assert.equal(config.provider.openai.options.baseURL, "https://api.openai.com/v1")
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("v1: two upstreams for one dialect leave every base URL unchanged", async () => {
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  const logs = []
  try {
    const hooks = await plugin.server(context("/tmp/context-guru-r1-v1-two-upstreams", logs))
    const config = { provider: { anthropic: { options: { baseURL: "https://gw-one.example.test" } }, "my-anthropic": { options: { baseURL: "https://gw-two.example.test" } } } }
    await hooks.config(config)
    assert.equal(config.provider.anthropic.options.baseURL, "https://gw-one.example.test")
    assert.equal(config.provider["my-anthropic"].options.baseURL, "https://gw-two.example.test")
    assert.ok(logs.some((message) => /multiple anthropic upstreams/.test(message)), logs.join("|"))
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

// ---- Non-blocking fixes.
test("a missing proxy.pid does not stop the plugin from starting the proxy it wants", async () => {
  const directory = "/tmp/context-guru-r1-nopid"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const first = await plugin.server(context(directory))
    await first.config({ provider: anthropic() })
    const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    const { rmSync } = await import("node:fs")
    rmSync(join(state, "proxy.pid"))
    // A new choice means the old proxy must be replaced. Its pid file is gone, so it cannot be stopped.
    writeFileSync(join(state, "settings.env"), "preset=high\n")
    const config = { provider: anthropic(`http://127.0.0.1:${before.port}/anthropic/v1`) }
    const second = await plugin.server(context(directory))
    await second.config(config)
    const after = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.notEqual(after.pid, before.pid, "a new proxy must have started")
    assert.equal(config.provider.anthropic.options.baseURL, `http://127.0.0.1:${after.port}/anthropic/v1`)
    assert.match(readFileSync(join(state, "proxy.yaml"), "utf8"), /preset: high/)
    for (const record of [before, after]) {
      try { process.kill(record.pid, "SIGTERM") } catch {}
      await waitUntilUnhealthy(record.port)
    }
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("chat.headers adds no header when the host gives no session id", async () => {
  const directory = "/tmp/context-guru-r1-nosession"
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const output = { headers: {} }
    await hooks["chat.headers"]({ provider: { info: { id: "anthropic" } } }, output)
    assert.deepEqual(output.headers, {})
    const record = JSON.parse(readFileSync(join(stateDirectory(directory), "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("a PATH file that is not executable does not win over an executable one", async () => {
  const bad = mkdtempSync(join(tmpdir(), "context-guru-path-bad-"))
  const good = mkdtempSync(join(tmpdir(), "context-guru-path-good-"))
  writeFileSync(join(bad, "context-guru-proxy"), "#!/bin/sh\nexit 1\n", { mode: 0o644 })
  const stub = stubProxyBinary()
  const { copyFileSync, chmodSync } = await import("node:fs")
  copyFileSync(stub, join(good, "context-guru-proxy"))
  chmodSync(join(good, "context-guru-proxy"), 0o755)
  const { findBinaryForTest } = await import("../index.js")
  const previous = process.env.PATH
  const previousBin = process.env.CONTEXT_GURU_BIN
  delete process.env.CONTEXT_GURU_BIN
  process.env.PATH = `${bad}:${good}`
  try {
    assert.equal(findBinaryForTest("/nonexistent-state")?.path, join(good, "context-guru-proxy"))
  } finally {
    process.env.PATH = previous
    if (previousBin !== undefined) process.env.CONTEXT_GURU_BIN = previousBin
  }
})

test("uninstall --purge removes the saved choices and the proxy scripts", async () => {
  const directory = "/tmp/context-guru-r1-purge"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const hooks = await plugin.server(context(directory), { preset: "high" })
    await hooks.config({ provider: anthropic() })
    await run(join(state, "context-guru-preset"), ["set", "medium"])
    assert.ok(existsSync(join(state, "settings.env")))
    const done = await run(join(state, "context-guru-uninstall"), ["--yes", "--purge"])
    assert.match(done.out, /purged=yes/)
    for (const name of ["settings.env", "options.env", "strategies.tsv", "strategies.aliases", "strategies.default", "install.env", "install.json", "proxy.yaml"]) {
      assert.ok(!existsSync(join(state, name)), `${name} is still there`)
    }
    for (const name of ["context-guru-preset", "context-guru-strategy", "context-guru-update", "context-guru-start", "context-guru-reset"]) {
      assert.ok(!existsSync(join(state, name)), `${name} is still there`)
    }
    // The undo command stays, so /context-guru-install can re-enable routing.
    assert.ok(existsSync(join(state, "context-guru-install")))
    assert.ok(existsSync(join(state, "disabled")))
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("the pid guard also stops a proxy whose binary has another file name", async () => {
  const directory = "/tmp/context-guru-r1-othername"
  const state = stateDirectory(directory)
  const { copyFileSync, chmodSync } = await import("node:fs")
  const dir = mkdtempSync(join(tmpdir(), "context-guru-othername-"))
  const renamed = join(dir, "my-proxy")
  copyFileSync(stubProxyBinary(), renamed)
  chmodSync(renamed, 0o755)
  process.env.CONTEXT_GURU_BIN = renamed
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.equal(record.binary, renamed)
    const reset = await run(join(state, "context-guru-reset"))
    assert.match(reset.out, /stopped/)
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("an install.env version with shell characters cannot run a command", async () => {
  const directory = "/tmp/context-guru-r1-version"
  const state = stateDirectory(directory)
  const marker = join(tmpdir(), `context-guru-injected-${process.pid}`)
  // A managed binary sits in the state directory. install.json from an earlier run names a hostile tag.
  const { copyFileSync, chmodSync } = await import("node:fs")
  mkdirSync(join(state, "bin"), { recursive: true })
  copyFileSync(stubProxyBinary(), join(state, "bin", "context-guru-proxy"))
  chmodSync(join(state, "bin", "context-guru-proxy"), 0o755)
  writeFileSync(join(state, "install.json"), JSON.stringify({ version: `v1;touch ${marker}`, port: 1 }))
  const previousBin = process.env.CONTEXT_GURU_BIN
  const previousPath = process.env.PATH
  delete process.env.CONTEXT_GURU_BIN
  process.env.PATH = `/usr/bin:/bin:${dirname(process.execPath)}`
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const envFile = readFileSync(join(state, "install.env"), "utf8")
    assert.match(envFile, /^version=external$/m, envFile)
    const status = await run(join(state, "context-guru-status"))
    assert.ok(!existsSync(marker), "the version value ran as a command")
    assert.match(status.out, /install=present/)
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    process.env.PATH = previousPath
    if (previousBin !== undefined) process.env.CONTEXT_GURU_BIN = previousBin
  }
})

test("the update script refuses a release tag with shell characters", async () => {
  const directory = "/tmp/context-guru-r1-update-tag"
  const state = stateDirectory(directory)
  const hooks = await plugin.server(context(directory))
  await hooks.config({ provider: { custom: { options: { baseURL: "https://example.test" } } } })
  writeFileSync(join(state, "install.env"), "managed=yes\nversion=v0.7.1\nbinary=/nonexistent\nport=1\n")
  const { createServer } = await import("node:http")
  const server = createServer((request, response) => {
    response.statusCode = 302
    response.setHeader("Location", "/releases/tag/v1;touch-x")
    response.end()
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  try {
    const child = spawn("sh", [join(state, "context-guru-update")], { env: { ...process.env, CONTEXT_GURU_RELEASES_URL: `http://127.0.0.1:${server.address().port}/releases` } })
    let out = ""
    child.stdout.on("data", (chunk) => { out += chunk })
    await new Promise((resolve) => child.on("close", resolve))
    assert.match(out, /reason=odd-tag/)
  } finally {
    server.close()
  }
})

test("uninstall --purge keeps a binary the plugin did not download", async () => {
  const directory = "/tmp/context-guru-r1-purge-external"
  const state = stateDirectory(directory)
  const external = stubProxyBinary()
  process.env.CONTEXT_GURU_BIN = external
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    assert.match(readFileSync(join(state, "managed-binary"), "utf8"), /^no$/m)
    const done = await run(join(state, "context-guru-uninstall"), ["--yes", "--purge"])
    assert.match(done.out, /purged=yes/)
    assert.ok(existsSync(external), "purge deleted a binary the plugin did not install")
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("reset keeps a binary the plugin did not download", async () => {
  const directory = "/tmp/context-guru-r1-reset-external"
  const state = stateDirectory(directory)
  mkdirSync(join(state, "bin"), { recursive: true })
  const external = stubProxyBinary()
  process.env.CONTEXT_GURU_BIN = external
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    writeFileSync(join(state, "bin", "context-guru-proxy"), "keep me")
    await run(join(state, "context-guru-reset"))
    assert.ok(existsSync(join(state, "bin", "context-guru-proxy")), "reset removed a file for an external binary")
    assert.ok(existsSync(external))
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

// ---- Round 2, blocking: a restart must not write the other setting without the check startup does.
const openai = (baseURL = "https://api.openai.com/v1") => ({ openai: { options: { baseURL } } })
const stopAll = async (state) => {
  const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
  let pid = record.pid
  try { pid = Number(readFileSync(join(state, "proxy.pid"), "utf8")) } catch {}
  for (const p of new Set([pid, record.pid])) { try { process.kill(p, "SIGTERM") } catch {} }
  await waitUntilUnhealthy(record.port)
}

test("OpenAI-only: a preset switch with restart keeps the resolved strategy none, not the raw default auto", async () => {
  const directory = "/tmp/context-guru-r2-openai-restart"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = pickyProxyBinary()
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: openai() })
    assert.match(readFileSync(join(state, "proxy.yaml"), "utf8"), /strategy: none/)
    const done = await run(join(state, "context-guru-preset"), ["set", "high", "--restart"])
    assert.match(done.out, /result=restarted/, done.out)
    const yaml = readFileSync(join(state, "proxy.yaml"), "utf8")
    assert.match(yaml, /preset: high/)
    assert.match(yaml, /strategy: none/, `the restart wrote an unfit strategy:\n${yaml}`)
    assert.ok(!yaml.includes("strategy: auto"))
    await stopAll(state)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("a typo in the preset option does not stop a strategy switch with restart", async () => {
  const directory = "/tmp/context-guru-r2-typo-preset"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = pickyProxyBinary()
  const logs = []
  try {
    const hooks = await plugin.server(context(directory, logs), { preset: "hgh" })
    await hooks.config({ provider: anthropic() })
    assert.match(readFileSync(join(state, "proxy.yaml"), "utf8"), /preset: off/)
    const done = await run(join(state, "context-guru-strategy"), ["set", "2-ping", "--restart"])
    assert.match(done.out, /result=restarted/, done.out)
    const yaml = readFileSync(join(state, "proxy.yaml"), "utf8")
    assert.match(yaml, /preset: off/, yaml)
    assert.match(yaml, /strategy: 2-ping/)
    assert.equal((await fetch(`http://127.0.0.1:${JSON.parse(readFileSync(join(state, "install.json"), "utf8")).port}/healthz`)).status, 200)
    await stopAll(state)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("a typo in the strategy option does not stop a preset switch with restart", async () => {
  const directory = "/tmp/context-guru-r2-typo-strategy"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = pickyProxyBinary()
  try {
    const hooks = await plugin.server(context(directory), { cacheStrategy: "aut0" })
    await hooks.config({ provider: anthropic() })
    const done = await run(join(state, "context-guru-preset"), ["set", "high", "--restart"])
    assert.match(done.out, /result=restarted/, done.out)
    const yaml = readFileSync(join(state, "proxy.yaml"), "utf8")
    assert.match(yaml, /preset: high/)
    assert.ok(!yaml.includes("aut0"), yaml)
    await stopAll(state)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("a new proxy that fails to start leaves the old proxy running, with the old file", async () => {
  const directory = "/tmp/context-guru-r2-rollback"
  const state = stateDirectory(directory)
  // The proxy refuses the name 2-ping, as a real proxy would refuse a name it does not know.
  process.env.CONTEXT_GURU_BIN = pickyProxyBinary(undefined, { refuse: ["2-ping"] })
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    const oldYaml = readFileSync(join(state, "proxy.yaml"), "utf8")
    const done = await run(join(state, "context-guru-strategy"), ["set", "2-ping", "--restart"])
    assert.match(done.out, /result=not_applied/, done.out)
    assert.match(done.out, /old_proxy=restored/, done.out)
    assert.equal(readFileSync(join(state, "proxy.yaml"), "utf8"), oldYaml, "the old file was not put back")
    const health = await fetch(`http://127.0.0.1:${before.port}/healthz`)
    assert.equal(health.status, 200, "no proxy is running after a failed switch")
    assert.ok(!existsSync(join(state, "proxy.yaml.old")))
    await stopAll(state)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("a name that is not in the table is refused by the script, and the old proxy is not touched", async () => {
  const directory = "/tmp/context-guru-r2-precheck"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = pickyProxyBinary()
  try {
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    // A name that is not in the table is refused by the script itself.
    const done = await run(join(state, "context-guru-strategy"), ["set", "30-min-ping", "--restart"])
    assert.equal(done.status, 2)
    assert.equal((await fetch(`http://127.0.0.1:${before.port}/healthz`)).status, 200)
    assert.equal(JSON.parse(readFileSync(join(state, "install.json"), "utf8")).pid, before.pid)
    await stopAll(state)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})
