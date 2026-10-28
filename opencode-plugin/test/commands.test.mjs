import assert from "node:assert/strict"
import test from "node:test"
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { createHash } from "node:crypto"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { spawn, spawnSync } from "node:child_process"
import { createServer } from "node:http"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-commands-test-"))

import plugin, { stateDirectory } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

const PLATFORMS = { darwin: "darwin", linux: "linux" }
const ARCHITECTURES = { arm64: "arm64", x64: "amd64" }

function context(directory) {
  return { directory, client: { app: { log: async () => {} } } }
}

// Runs the config hook with only an unsupported provider, so the command
// scripts for `directory` are written without starting a proxy.
async function registerScripts(directory) {
  const hooks = await plugin.server(context(directory))
  const config = { provider: { custom: { options: { baseURL: "https://example.test" } } } }
  await hooks.config(config)
}

// Async on purpose: spawnSync would freeze this process's event loop and the
// in-process stub servers below would never answer the scripts' curl calls.
function run(script, args = [], env = process.env) {
  return new Promise((resolve) => {
    const child = spawn("sh", [script, ...args], { env })
    let stdout = ""
    let stderr = ""
    child.stdout.on("data", (chunk) => { stdout += chunk })
    child.stderr.on("data", (chunk) => { stderr += chunk })
    child.on("close", (status) => resolve({ status, stdout, stderr }))
  })
}

function writeEnv(state, fields) {
  writeFileSync(join(state, "install.env"), `${Object.entries(fields).map(([key, value]) => `${key}=${value}`).join("\n")}\n`)
}

test("status script reports an absent install", async () => {
  const directory = "/tmp/context-guru-project-a"
  await registerScripts(directory)
  const state = stateDirectory(directory)

  const result = await run(join(state, "context-guru-status"))

  assert.equal(result.status, 0)
  assert.match(result.stdout, /install=missing/)
  assert.match(result.stdout, /proxy=down/)
})

test("status script reports a healthy proxy and its savings", async () => {
  const directory = "/tmp/context-guru-project-b"
  await registerScripts(directory)
  const state = stateDirectory(directory)
  const server = createServer((request, response) => {
    if (request.url === "/healthz") response.end("ok")
    else if (request.url === "/stats") response.end(JSON.stringify({ requests: 42, saved_tokens: 1000, wasted_tokens: 3, savings_pct: 12.5 }))
    else { response.statusCode = 404; response.end() }
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  const port = server.address().port
  writeEnv(state, { state, port, binary: "/nonexistent/context-guru-proxy", managed: "yes", version: "v0.0.1-test", preset: "off", strategy: "5-min-ping", dialects: "anthropic" })

  try {
    const result = await run(join(state, "context-guru-status"))

    assert.equal(result.status, 0)
    assert.match(result.stdout, /proxy=up/)
    assert.match(result.stdout, /stats_requests=42/)
    assert.match(result.stdout, /stats_saved_tokens=1000/)
    assert.match(result.stdout, /stats_savings_pct=12\.5/)
    assert.match(result.stdout, /preset=off/)
  } finally {
    server.close()
  }
})

test("install script reports an absent install", async () => {
  const directory = "/tmp/context-guru-project-c"
  await registerScripts(directory)
  const state = stateDirectory(directory)

  const result = await run(join(state, "context-guru-install"))

  assert.equal(result.status, 0)
  assert.match(result.stdout, /result=never-installed/)
})

test("uninstall script plans first, then stops the proxy and disables routing", async () => {
  const directory = "/tmp/context-guru-project-d"
  await registerScripts(directory)
  const state = stateDirectory(directory)
  writeEnv(state, { state, port: 1, binary: "/nonexistent/context-guru-proxy", managed: "no", version: "external" })
  writeFileSync(join(state, "proxy.pid"), "999999999\n")

  const plan = await run(join(state, "context-guru-uninstall"))

  assert.equal(plan.status, 0)
  assert.match(plan.stdout, /result=plan/)
  assert.ok(existsSync(join(state, "proxy.pid")))

  const applied = await run(join(state, "context-guru-uninstall"), ["--yes"])

  assert.equal(applied.status, 0)
  assert.match(applied.stdout, /result=uninstalled/)
  assert.ok(existsSync(join(state, "disabled")))
  assert.ok(!existsSync(join(state, "proxy.pid")))
})

test("disabled marker leaves providers untouched", async () => {
  const directory = "/tmp/context-guru-project-e"
  const state = stateDirectory(directory)
  mkdirSync(state, { recursive: true })
  writeFileSync(join(state, "disabled"), "test\n")
  const hooks = await plugin.server(context(directory))
  const config = { provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } }

  await hooks.config(config)

  assert.equal(config.provider.anthropic.options.baseURL, "https://api.anthropic.com")
  assert.ok(config.command["context-guru-status"])
})

test("update script checks, then downloads and swaps the binary", async () => {
  const directory = "/tmp/context-guru-project-f"
  await registerScripts(directory)
  const state = stateDirectory(directory)

  // A release fixture: a tarball holding a stub context-guru-proxy, plus its checksums line.
  const fixture = mkdtempSync(join(tmpdir(), "context-guru-release-"))
  const stubName = "context-guru-proxy"
  writeFileSync(join(fixture, stubName), "#!/bin/sh\necho stub-v9.9.9\n")
  chmodSync(join(fixture, stubName), 0o755)
  const archive = join(fixture, "archive.tar.gz")
  spawnSync("tar", ["-czf", archive, "-C", fixture, stubName])
  const archiveBytes = readFileSync(archive)
  const checksum = createHash("sha256").update(archiveBytes).digest("hex")
  const platform = PLATFORMS[process.platform]
  const architecture = ARCHITECTURES[process.arch]
  const archiveName = `context-guru_9.9.9_${platform}_${architecture}.tar.gz`

  const server = createServer((request, response) => {
    if (request.url === "/releases/latest") {
      response.statusCode = 302
      response.setHeader("Location", `http://127.0.0.1:${server.address().port}/releases/tag/v9.9.9`)
      response.end()
    } else if (request.url === "/releases/download/v9.9.9/checksums.txt") {
      response.end(`${checksum}  ${archiveName}\n`)
    } else if (request.url === `/releases/download/v9.9.9/${archiveName}`) {
      response.end(archiveBytes)
    } else {
      response.statusCode = 404
      response.end()
    }
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))

  mkdirSync(join(state, "bin"), { recursive: true })
  const installed = join(state, "bin", "context-guru-proxy")
  writeFileSync(installed, "#!/bin/sh\necho stub-v0.0.1\n")
  chmodSync(installed, 0o755)
  writeEnv(state, { state, binary: installed, managed: "yes", version: "v0.0.1", port: 1 })
  const env = { ...process.env, CONTEXT_GURU_RELEASES_URL: `http://127.0.0.1:${server.address().port}/releases` }

  try {
    const check = await run(join(state, "context-guru-update"), [], env)

    assert.equal(check.status, 0)
    assert.match(check.stdout, /installed=v0\.0\.1/)
    assert.match(check.stdout, /latest=v9\.9\.9/)
    assert.match(check.stdout, /result=available/)
    assert.equal(readFileSync(installed, "utf8"), "#!/bin/sh\necho stub-v0.0.1\n")

    const applied = await run(join(state, "context-guru-update"), ["--yes"], env)

    assert.equal(applied.status, 0)
    assert.match(applied.stdout, /result=updated/)
    assert.equal(readFileSync(installed, "utf8"), "#!/bin/sh\necho stub-v9.9.9\n")
    assert.match(readFileSync(join(state, "install.env"), "utf8"), /version=v9\.9\.9/)
  } finally {
    server.close()
  }
})

test("proxy startup records install facts and the start script sees it healthy", async () => {
  const directory = "/tmp/context-guru-project-g"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()

  try {
    const hooks = await plugin.server(context(directory))
    const config = { provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } }

    await hooks.config(config)

    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.ok(record.pid > 0)
    assert.ok(record.args.includes("--listen"))
    assert.equal(record.managed, false)
    assert.equal(record.version, "external")
    assert.equal(config.provider.anthropic.options.baseURL, `http://127.0.0.1:${record.port}/anthropic/v1`)
    assert.match(readFileSync(join(state, "install.env"), "utf8"), /managed=no/)
    assert.match(readFileSync(join(state, "install.env"), "utf8"), /dialects=anthropic/)

    const again = await run(join(state, "context-guru-start"))

    assert.equal(again.status, 0)
    assert.match(again.stdout, /proxy=already-running/)

    const stop = await run(join(state, "context-guru-uninstall"), ["--yes"])

    assert.equal(stop.status, 0)
    assert.match(stop.stdout, /result=uninstalled/)
    assert.ok(!existsSync(join(state, "proxy.pid")))
    await waitUntilUnhealthy(record.port)

    // The start script's launch path must leave a daemon that outlives its shell,
    // and proxy.pid must name that daemon — not the wrapper that was backgrounded.
    const restarted = await run(join(state, "context-guru-start"))

    assert.equal(restarted.status, 0)
    assert.match(restarted.stdout, /proxy=started/)
    const pid = Number(readFileSync(join(state, "proxy.pid"), "utf8"))
    assert.ok(pid > 0)
    process.kill(pid, 0)
    process.kill(pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)

    // Force the setsid branch with a shim so its argument plumbing and the inner
    // sh's pid recording run on every platform, not just where setsid exists.
    const shimDirectory = mkdtempSync(join(tmpdir(), "context-guru-setsid-"))
    writeFileSync(join(shimDirectory, "setsid"), "#!/bin/sh\nexec \"$@\"\n")
    chmodSync(join(shimDirectory, "setsid"), 0o755)
    const withShim = await run(join(state, "context-guru-start"), [], { ...process.env, PATH: `${shimDirectory}:${process.env.PATH}` })

    assert.equal(withShim.status, 0)
    assert.match(withShim.stdout, /proxy=started/)
    const shimPid = Number(readFileSync(join(state, "proxy.pid"), "utf8"))
    assert.ok(shimPid > 0)
    assert.notEqual(shimPid, pid)
    process.kill(shimPid, 0)
    process.kill(shimPid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("first startup downloads the release binary and extracts under the state directory", async () => {
  const directory = "/tmp/context-guru-project-download"
  const state = stateDirectory(directory)

  // A release fixture: the working stub proxy as a tarball, plus its checksums line.
  const stub = stubProxyBinary()
  const fixture = mkdtempSync(join(tmpdir(), "context-guru-release-"))
  const archive = join(fixture, "archive.tar.gz")
  spawnSync("tar", ["-czf", archive, "-C", dirname(stub), "context-guru-proxy"])
  const archiveBytes = readFileSync(archive)
  const checksum = createHash("sha256").update(archiveBytes).digest("hex")
  const archiveName = `context-guru_9.9.9_${PLATFORMS[process.platform]}_${ARCHITECTURES[process.arch]}.tar.gz`

  const server = createServer((request, response) => {
    if (request.url === "/releases/latest") {
      response.statusCode = 302
      response.setHeader("Location", `/releases/tag/v9.9.9`)
      response.end()
    } else if (request.url === "/releases/tag/v9.9.9") {
      response.end("ok")
    } else if (request.url === "/releases/download/v9.9.9/checksums.txt") {
      response.end(`${checksum}  ${archiveName}\n`)
    } else if (request.url === `/releases/download/v9.9.9/${archiveName}`) {
      response.end(archiveBytes)
    } else {
      response.statusCode = 404
      response.end()
    }
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))

  // System dirs plus the running node, so findBinary cannot shortcut the download
  // but tar, sh, and the stub's shebang still resolve.
  const previousBin = process.env.CONTEXT_GURU_BIN
  const previousPath = process.env.PATH
  const previousReleases = process.env.CONTEXT_GURU_RELEASES_URL
  delete process.env.CONTEXT_GURU_BIN
  process.env.CONTEXT_GURU_RELEASES_URL = `http://127.0.0.1:${server.address().port}/releases`
  process.env.PATH = `/usr/bin:/bin:${dirname(process.execPath)}`

  let record
  try {
    const hooks = await plugin.server(context(directory))
    const config = { provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } }

    await hooks.config(config)

    assert.ok(existsSync(join(state, "bin", "context-guru-proxy")))
    record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.equal(record.version, "v9.9.9")
    assert.equal(record.managed, true)
    assert.equal(config.provider.anthropic.options.baseURL, `http://127.0.0.1:${record.port}/anthropic/v1`)
    // The extraction work directory must not linger in the state directory.
    assert.ok(!existsSync(join(state, `download-${process.pid}`)))
  } finally {
    if (record?.pid) {
      try { process.kill(record.pid, "SIGTERM") } catch {}
      await waitUntilUnhealthy(record.port)
    }
    server.close()
    if (previousBin === undefined) delete process.env.CONTEXT_GURU_BIN
    else process.env.CONTEXT_GURU_BIN = previousBin
    process.env.PATH = previousPath
    if (previousReleases === undefined) delete process.env.CONTEXT_GURU_RELEASES_URL
    else process.env.CONTEXT_GURU_RELEASES_URL = previousReleases
  }
})

test("chat.headers restarts a dead proxy on its recorded port", async () => {
  const directory = "/tmp/context-guru-project-h"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()

  try {
    const hooks = await plugin.server(context(directory))
    const config = { provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } }

    await hooks.config(config)

    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.ok(record.pid > 0)

    // Kill the proxy the way --idle-exit or a crash would, then make a request:
    // the chat.headers hook must restart it on the same port before it goes out.
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)

    const output = { headers: {} }
    await hooks["chat.headers"]({ sessionID: "session-recovery" }, output)

    assert.equal(output.headers["x-context-guru-session"], "session-recovery")
    const response = await fetch(`http://127.0.0.1:${record.port}/healthz`)
    assert.equal(response.status, 200)
    const recoveredPid = Number(readFileSync(join(state, "proxy.pid"), "utf8"))
    assert.ok(recoveredPid > 0)
    assert.notEqual(recoveredPid, record.pid)
    process.kill(recoveredPid, "SIGTERM")
    await waitUntilUnhealthy(record.port)

    // With routing disabled, a chat request must not resurrect the proxy.
    writeFileSync(join(state, "disabled"), "test\n")
    const disabledOutput = { headers: {} }
    await hooks["chat.headers"]({ sessionID: "session-disabled" }, disabledOutput)

    assert.equal(disabledOutput.headers["x-context-guru-session"], "session-disabled")
    assert.equal(Number(readFileSync(join(state, "proxy.pid"), "utf8")), recoveredPid)
    await assert.rejects(fetch(`http://127.0.0.1:${record.port}/healthz`))
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})
