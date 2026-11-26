import assert from "node:assert/strict"
import test from "node:test"
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { spawn, spawnSync } from "node:child_process"
import { createHash } from "node:crypto"
import { createServer } from "node:http"
import { copyFileSync, chmodSync, readdirSync } from "node:fs"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-capture-test-"))
process.env.CONTEXT_GURU_PORT_START = "9400"

import plugin, { stateDirectory, withCaptureOff, captureOffEnv } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

// The owner's setting turns capture ON for every proxy that inherits it. The plugin must still start every
// proxy with capture off. Each test runs with DASHBOARD_CONTENT=1 in the parent environment.
const context = (directory) => ({ directory, client: { app: { log: async () => {} } } })
const anthropic = () => ({ anthropic: { options: { baseURL: "https://api.anthropic.com" } } })

function run(script, args = []) {
  return new Promise((resolve) => {
    const child = spawn("sh", [script, ...args])
    let out = ""
    child.stdout.on("data", (chunk) => { out += chunk })
    child.stderr.on("data", (chunk) => { out += chunk })
    child.on("close", (status) => resolve({ status, out }))
  })
}

function recorded(file) {
  if (!existsSync(file)) return []
  return readFileSync(file, "utf8").split("\n").filter(Boolean).map((line) => JSON.parse(line))
}

// Every recorded start must have the flag set to false, no other value of the flag, and the env at "0".
function assertCaptureOff(starts, label) {
  assert.ok(starts.length > 0, `${label}: the stub recorded no start`)
  for (const start of starts) {
    const flags = start.argv.filter((arg) => arg.startsWith("--dashboard-content"))
    assert.deepEqual(flags, ["--dashboard-content=false"], `${label}: argv ${JSON.stringify(flags)}`)
    assert.equal(start.dashboardContent, "0", `${label}: DASHBOARD_CONTENT was ${start.dashboardContent}`)
  }
}

async function withEnv(fn) {
  const file = join(mkdtempSync(join(tmpdir(), "context-guru-rec-")), "starts.jsonl")
  const saved = { content: process.env.DASHBOARD_CONTENT, record: process.env.CG_TEST_RECORD, bin: process.env.CONTEXT_GURU_BIN }
  process.env.DASHBOARD_CONTENT = "1"
  process.env.CG_TEST_RECORD = file
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    await fn(file)
  } finally {
    for (const [key, name] of [["content", "DASHBOARD_CONTENT"], ["record", "CG_TEST_RECORD"], ["bin", "CONTEXT_GURU_BIN"]]) {
      if (saved[key] === undefined) delete process.env[name]
      else process.env[name] = saved[key]
    }
  }
}

test("helpers: the flag is added once, any other value of it is removed, the env is set to 0", () => {
  assert.deepEqual(withCaptureOff(["--listen", "x"]), ["--listen", "x", "--dashboard-content=false"])
  assert.deepEqual(withCaptureOff(["--dashboard-content=true", "--listen", "x"]), ["--listen", "x", "--dashboard-content=false"])
  assert.deepEqual(withCaptureOff(["--dashboard-content", "--listen", "x"]), ["--listen", "x", "--dashboard-content=false"])
  assert.deepEqual(withCaptureOff(["--dashboard-content=false"]), ["--dashboard-content=false"])
  assert.equal(captureOffEnv({ DASHBOARD_CONTENT: "1", A: "b" }).DASHBOARD_CONTENT, "0")
  assert.equal(captureOffEnv({ A: "b" }).A, "b")
})

test("first start: the proxy starts with capture off although the parent has DASHBOARD_CONTENT=1", async () => {
  await withEnv(async (file) => {
    const directory = "/tmp/context-guru-capture-first"
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    assertCaptureOff(recorded(file), "first start")
    const record = JSON.parse(readFileSync(join(stateDirectory(directory), "install.json"), "utf8"))
    assert.ok(record.args.includes("--dashboard-content=false"), "install.json does not record the flag")
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  })
})

test("recovery after a crash: the restarted proxy has capture off", async () => {
  await withEnv(async (file) => {
    const directory = "/tmp/context-guru-capture-recover"
    const state = stateDirectory(directory)
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
    const output = { headers: {} }
    await hooks["chat.headers"]({ sessionID: "s", provider: { info: { id: "anthropic" } } }, output)
    const starts = recorded(file)
    assert.equal(starts.length, 2, "expected the first start and one recovery")
    assertCaptureOff(starts, "recovery")
    process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8")), "SIGTERM")
    await waitUntilUnhealthy(record.port)
  })
})

test("recovery from an older install.json that has no flag still starts with capture off", async () => {
  await withEnv(async (file) => {
    const directory = "/tmp/context-guru-capture-old-record"
    const state = stateDirectory(directory)
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    // An install.json written before this change: the same args without the flag, and no env.
    const old = { ...record, args: record.args.filter((arg) => !arg.startsWith("--dashboard-content")) }
    assert.ok(!old.args.some((arg) => arg.startsWith("--dashboard-content")))
    writeFileSync(join(state, "install.json"), JSON.stringify(old))
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
    await hooks["chat.headers"]({ sessionID: "s", provider: { info: { id: "anthropic" } } }, { headers: {} })
    assertCaptureOff(recorded(file), "old record")
    process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8")), "SIGTERM")
    await waitUntilUnhealthy(record.port)
  })
})

test("picker --restart and the install script: the proxy they start has capture off", async () => {
  await withEnv(async (file) => {
    const directory = "/tmp/context-guru-capture-scripts"
    const state = stateDirectory(directory)
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const first = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))

    const restarted = await run(join(state, "context-guru-preset"), ["set", "high", "--restart"])
    assert.match(restarted.out, /result=restarted/, restarted.out)

    // The install script starts a dead proxy through context-guru-start.
    process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8")), "SIGTERM")
    await waitUntilUnhealthy(first.port)
    const installed = await run(join(state, "context-guru-install"))
    assert.match(installed.out, /result=restarting|proxy=started/, installed.out)

    const starts = recorded(file)
    assert.equal(starts.length, 3, `expected 3 starts (plugin, picker restart, install), got ${starts.length}`)
    assertCaptureOff(starts, "scripts")
    process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8")), "SIGTERM")
    await waitUntilUnhealthy(first.port)
  })
})

test("the start script names the flag and the env, and the proxy pid is the proxy", async () => {
  await withEnv(async () => {
    const directory = "/tmp/context-guru-capture-script-text"
    const state = stateDirectory(directory)
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    const text = readFileSync(join(state, "context-guru-start"), "utf8")
    assert.match(text, /DASHBOARD_CONTENT=0/)
    assert.match(text, /--dashboard-content=false/)
    assert.ok(!/--dashboard-content=true/.test(text))
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    // The stop guard matches the recorded binary path in the process command line.
    const stopped = await run(join(state, "context-guru-reset"))
    assert.match(stopped.out, /stopped/)
    await waitUntilUnhealthy(record.port)
  })
})

test("the update script restarts the new binary with capture off", async () => {
  await withEnv(async (file) => {
    const directory = "/tmp/context-guru-capture-update"
    const state = stateDirectory(directory)
    // A managed binary: copy the stub into the state directory, so the update script may replace it.
    mkdirSync(join(state, "bin"), { recursive: true })
    const managed = join(state, "bin", "context-guru-proxy")
    copyFileSync(process.env.CONTEXT_GURU_BIN, managed)
    chmodSync(managed, 0o755)
    delete process.env.CONTEXT_GURU_BIN
    const previousPath = process.env.PATH
    process.env.PATH = `/usr/bin:/bin:${join(process.execPath, "..")}`
    const hooks = await plugin.server(context(directory))
    await hooks.config({ provider: anthropic() })
    process.env.PATH = previousPath
    const first = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.equal(first.binary, managed)
    assert.equal(first.managed, true)

    // A release fixture whose binary is a recording stub (the same one), so the restart is recorded.
    const fixture = mkdtempSync(join(tmpdir(), "context-guru-release-"))
    copyFileSync(managed, join(fixture, "context-guru-proxy"))
    chmodSync(join(fixture, "context-guru-proxy"), 0o755)
    const archive = join(fixture, "archive.tar.gz")
    spawnSync("tar", ["-czf", archive, "-C", fixture, "context-guru-proxy"])
    const bytes = readFileSync(archive)
    const sum = createHash("sha256").update(bytes).digest("hex")
    const platform = { darwin: "darwin", linux: "linux" }[process.platform]
    const arch = { arm64: "arm64", x64: "amd64" }[process.arch]
    const name = `context-guru_9.9.9_${platform}_${arch}.tar.gz`
    const server = createServer((request, response) => {
      if (request.url === "/releases/latest") { response.statusCode = 302; response.setHeader("Location", `http://127.0.0.1:${server.address().port}/releases/tag/v9.9.9`); response.end() }
      else if (request.url === "/releases/download/v9.9.9/checksums.txt") response.end(`${sum}  ${name}\n`)
      else if (request.url === `/releases/download/v9.9.9/${name}`) response.end(bytes)
      else { response.statusCode = 404; response.end() }
    })
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
    try {
      const before = recorded(file).length
      const result = await new Promise((resolve) => {
        const child = spawn("sh", [join(state, "context-guru-update"), "--yes"], { env: { ...process.env, CONTEXT_GURU_RELEASES_URL: `http://127.0.0.1:${server.address().port}/releases` } })
        let out = ""
        child.stdout.on("data", (chunk) => { out += chunk })
        child.stderr.on("data", (chunk) => { out += chunk })
        child.on("close", (status) => resolve({ status, out }))
      })
      assert.match(result.out, /result=updated/, result.out)
      assert.match(result.out, /proxy=started/, result.out)
      const after = recorded(file)
      assert.ok(after.length > before, "the update did not start a proxy")
      assertCaptureOff(after.slice(before), "update restart")
    } finally {
      server.close()
      try { process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8")), "SIGTERM") } catch {}
      await waitUntilUnhealthy(first.port)
    }
  })
})
