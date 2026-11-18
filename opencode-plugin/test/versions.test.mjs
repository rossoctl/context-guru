import assert from "node:assert/strict"
import test from "node:test"
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { createHash } from "node:crypto"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { spawn, spawnSync } from "node:child_process"
import { createServer } from "node:http"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-versions-test-"))
process.env.CONTEXT_GURU_PORT_START = "9800"

import plugin, { stateDirectory } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

const PLATFORMS = { darwin: "darwin", linux: "linux" }
const ARCHITECTURES = { arm64: "arm64", x64: "amd64" }

const context = (directory, logs = []) => ({ directory, client: { app: { log: async ({ body }) => { logs.push(body.message) } } } })
const anthropic = () => ({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })

// Async on purpose: the in-process release server must answer the scripts' curl calls.
function run(script, args = [], env = process.env) {
  return new Promise((resolve) => {
    const child = spawn("sh", [script, ...args], { env })
    let out = ""
    child.stdout.on("data", (chunk) => { out += chunk })
    child.stderr.on("data", (chunk) => { out += chunk })
    child.on("close", (status) => resolve({ status, out }))
  })
}

// A release server whose latest release is v0.7.3, holding the stub proxy that says it is 0.7.3.
async function releaseServer() {
  const stub = stubProxyBinary(undefined, { version: "0.7.3" })
  const fixture = mkdtempSync(join(tmpdir(), "context-guru-release-"))
  const archive = join(fixture, "archive.tar.gz")
  spawnSync("tar", ["-czf", archive, "-C", dirname(stub), "context-guru-proxy"])
  const bytes = readFileSync(archive)
  const checksum = createHash("sha256").update(bytes).digest("hex")
  const name = `context-guru_0.7.3_${PLATFORMS[process.platform]}_${ARCHITECTURES[process.arch]}.tar.gz`
  const server = createServer((request, response) => {
    if (request.url === "/releases/latest") {
      response.statusCode = 302
      response.setHeader("Location", `http://127.0.0.1:${server.address().port}/releases/tag/v0.7.3`)
      response.end()
    } else if (request.url === "/releases/download/v0.7.3/checksums.txt") {
      response.end(`${checksum}  ${name}\n`)
    } else if (request.url === `/releases/download/v0.7.3/${name}`) {
      response.end(bytes)
    } else {
      response.statusCode = 404
      response.end()
    }
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  return { server, url: `http://127.0.0.1:${server.address().port}/releases` }
}

// Puts a managed binary in the state directory, as a download by the plugin does.
function placeManaged(state, source) {
  mkdirSync(join(state, "bin"), { recursive: true })
  const path = join(state, "bin", "context-guru-proxy")
  writeFileSync(path, readFileSync(source), { mode: 0o755 })
  chmodSync(path, 0o755)
  return path
}

// No context-guru-proxy on PATH and no CONTEXT_GURU_BIN, so the plugin uses its managed binary.
// sh, tar, curl and the stub's node still resolve.
function isolatePath() {
  const previous = { bin: process.env.CONTEXT_GURU_BIN, path: process.env.PATH, releases: process.env.CONTEXT_GURU_RELEASES_URL }
  delete process.env.CONTEXT_GURU_BIN
  process.env.PATH = `/usr/bin:/bin:${dirname(process.execPath)}`
  return () => {
    if (previous.bin === undefined) delete process.env.CONTEXT_GURU_BIN
    else process.env.CONTEXT_GURU_BIN = previous.bin
    process.env.PATH = previous.path
    if (previous.releases === undefined) delete process.env.CONTEXT_GURU_RELEASES_URL
    else process.env.CONTEXT_GURU_RELEASES_URL = previous.releases
  }
}

async function stopRecorded(state) {
  const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
  try { process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8").trim()), "SIGTERM") } catch {}
  await waitUntilUnhealthy(record.port)
}

// #1167
test("after an update, the next start records the version the new binary reports", async () => {
  const directory = "/tmp/context-guru-versions-update"
  const state = stateDirectory(directory)
  const restore = isolatePath()
  const { server, url } = await releaseServer()
  process.env.CONTEXT_GURU_RELEASES_URL = url
  try {
    placeManaged(state, stubProxyBinary(undefined, { version: "0.7.1" }))
    writeFileSync(join(state, "install.json"), JSON.stringify({ version: "v0.7.1" }))
    await (await plugin.server(context(directory))).config(anthropic())
    assert.match(readFileSync(join(state, "install.env"), "utf8"), /^version=v0\.7\.1$/m)

    const applied = await run(join(state, "context-guru-update"), ["--yes"])
    assert.equal(applied.status, 0, applied.out)
    assert.match(applied.out, /result=updated/)
    // OpenCode restarts later; by then the proxy is gone.
    await stopRecorded(state)
    await (await plugin.server(context(directory))).config(anthropic())

    assert.equal(JSON.parse(readFileSync(join(state, "install.json"), "utf8")).version, "v0.7.3")
    assert.match(readFileSync(join(state, "install.env"), "utf8"), /^version=v0\.7\.3$/m)
    const status = await run(join(state, "context-guru-status"))
    assert.match(status.out, /^version=v0\.7\.3$/m)
    const check = await run(join(state, "context-guru-update"))
    assert.match(check.out, /^installed=v0\.7\.3$/m)
    assert.match(check.out, /^result=latest$/m)
  } finally {
    if (existsSync(join(state, "install.json"))) await stopRecorded(state)
    server.close()
    restore()
  }
})

// #1168
test("update installs the latest release over a too-old managed binary that has no install record", async () => {
  const directory = "/tmp/context-guru-versions-too-old"
  const state = stateDirectory(directory)
  const restore = isolatePath()
  const { server, url } = await releaseServer()
  process.env.CONTEXT_GURU_RELEASES_URL = url
  // Like v0.7.0: it knows --version, not the strategies command.
  const oldDirectory = mkdtempSync(join(tmpdir(), "context-guru-v070-"))
  const old = join(oldDirectory, "context-guru-proxy")
  writeFileSync(old, "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'context-guru-proxy 0.7.0 (commit old)'; exit 0; fi\nexit 2\n", { mode: 0o755 })
  const logs = []
  try {
    const binary = placeManaged(state, old)
    await (await plugin.server(context(directory, logs))).config(anthropic())
    assert.ok(logs.some((message) => /cannot list cache strategies; update the proxy to v0\.7\.1 or later/.test(message)), logs.join("|"))
    assert.equal(existsSync(join(state, "install.env")), false)

    const check = await run(join(state, "context-guru-update"))
    assert.equal(check.status, 0, check.out)
    assert.match(check.out, /^installed=v0\.7\.0$/m)
    assert.match(check.out, /^latest=v0\.7\.3$/m)
    assert.match(check.out, /^result=available$/m)

    const applied = await run(join(state, "context-guru-update"), ["--yes"])
    assert.equal(applied.status, 0, applied.out)
    assert.match(applied.out, /^result=updated$/m)
    assert.match(applied.out, /^note=restart OpenCode to start the updated proxy$/m)
    assert.match(spawnSync(binary, ["--version"], { encoding: "utf8" }).stdout, /^context-guru-proxy 0\.7\.3 /)

    const config = anthropic()
    await (await plugin.server(context(directory))).config(config)
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.equal(record.version, "v0.7.3")
    assert.equal(config.provider.anthropic.options.baseURL, `http://127.0.0.1:${record.port}/anthropic/v1`)
  } finally {
    if (existsSync(join(state, "install.json"))) await stopRecorded(state)
    server.close()
    restore()
  }
})
