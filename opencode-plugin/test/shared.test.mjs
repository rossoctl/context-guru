import assert from "node:assert/strict"
import test from "node:test"
import { chmodSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, writeFileSync } from "node:fs"
import { createHash } from "node:crypto"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { spawn, spawnSync } from "node:child_process"
import { createServer } from "node:http"

// #1160: one shared copy of the proxy per version, not one per project.
const ROOT = mkdtempSync(join(tmpdir(), "context-guru-shared-test-"))
process.env.CONTEXT_GURU_STATE = ROOT
process.env.CONTEXT_GURU_PORT_START = "9600"

import plugin, { stateDirectory } from "../index.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

const PLATFORMS = { darwin: "darwin", linux: "linux" }
const ARCHITECTURES = { arm64: "arm64", x64: "amd64" }
const SHARED_BIN = join(ROOT, "bin")

const context = (directory) => ({ directory, client: { app: { log: async () => {} } } })
const anthropic = () => ({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })

// Async on purpose: the in-process release server must answer the scripts' curl calls.
function run(script, args = []) {
  return new Promise((resolve) => {
    const child = spawn("sh", [script, ...args], { env: process.env })
    let out = ""
    child.stdout.on("data", (chunk) => { out += chunk })
    child.stderr.on("data", (chunk) => { out += chunk })
    child.on("close", (status) => resolve({ status, out }))
  })
}

// A release server that holds a stub proxy for each version and counts the archive downloads.
async function releaseServer(versions, latest) {
  const releases = {}
  for (const version of versions) {
    const stub = stubProxyBinary(undefined, { version })
    const archive = join(mkdtempSync(join(tmpdir(), "context-guru-release-")), "archive.tar.gz")
    spawnSync("tar", ["-czf", archive, "-C", dirname(stub), "context-guru-proxy"])
    const bytes = readFileSync(archive)
    const name = `context-guru_${version}_${PLATFORMS[process.platform]}_${ARCHITECTURES[process.arch]}.tar.gz`
    releases[`/releases/download/v${version}/${name}`] = bytes
    releases[`/releases/download/v${version}/checksums.txt`] = `${createHash("sha256").update(bytes).digest("hex")}  ${name}\n`
  }
  const downloads = []
  const server = createServer((request, response) => {
    if (request.url === "/releases/latest") {
      response.statusCode = 302
      response.setHeader("Location", `http://127.0.0.1:${server.address().port}/releases/tag/v${latest}`)
      response.end()
    } else if (releases[request.url] !== undefined) {
      if (request.url.endsWith(".tar.gz")) downloads.push(request.url)
      response.end(releases[request.url])
    } else {
      response.statusCode = 404
      response.end()
    }
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  return { server, downloads, url: `http://127.0.0.1:${server.address().port}/releases` }
}

// No context-guru-proxy on PATH and no CONTEXT_GURU_BIN, so the plugin uses its managed binary.
async function withReleases(versions, latest, fn) {
  const previous = { bin: process.env.CONTEXT_GURU_BIN, path: process.env.PATH, releases: process.env.CONTEXT_GURU_RELEASES_URL }
  delete process.env.CONTEXT_GURU_BIN
  process.env.PATH = `/usr/bin:/bin:${dirname(process.execPath)}`
  const release = await releaseServer(versions, latest)
  process.env.CONTEXT_GURU_RELEASES_URL = release.url
  const started = []
  try {
    await fn({ ...release, start: async (directory) => {
      const state = stateDirectory(directory)
      started.push(state)
      await (await plugin.server(context(directory))).config(anthropic())
      assert.ok(existsSync(join(state, "install.json")), `the proxy for ${directory} did not start`)
      return state
    } })
  } finally {
    for (const state of started) await stopRecorded(state)
    release.server.close()
    if (previous.bin !== undefined) process.env.CONTEXT_GURU_BIN = previous.bin
    process.env.PATH = previous.path
    if (previous.releases === undefined) delete process.env.CONTEXT_GURU_RELEASES_URL
    else process.env.CONTEXT_GURU_RELEASES_URL = previous.releases
  }
}

async function stopRecorded(state) {
  let record
  try { record = JSON.parse(readFileSync(join(state, "install.json"), "utf8")) } catch { return }
  try { process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8").trim()), "SIGTERM") } catch {}
  await waitUntilUnhealthy(record.port)
}

const projectBinary = (state) => join(state, "bin", "context-guru-proxy")
const shared = (version) => join(SHARED_BIN, version, "context-guru-proxy")
const versionsOnDisk = () => readdirSync(SHARED_BIN).filter((name) => name.startsWith("v")).sort()

test("two projects on the same version share one download and one file", async () => {
  await withReleases(["0.7.3"], "0.7.3", async ({ downloads, start }) => {
    const a = await start("/tmp/context-guru-shared-a")
    const b = await start("/tmp/context-guru-shared-b")

    assert.equal(downloads.length, 1, downloads.join(" "))
    assert.deepEqual(versionsOnDisk(), ["v0.7.3"])
    for (const state of [a, b]) {
      assert.ok(lstatSync(projectBinary(state)).isSymbolicLink(), `${state} has its own copy`)
      assert.equal(realpathSync(projectBinary(state)), realpathSync(shared("v0.7.3")))
    }
  })
})

// Puts a copy in the project's own bin/, as an older plugin downloaded it.
function placeOwnCopy(state, version) {
  mkdirSync(join(state, "bin"), { recursive: true })
  const own = readFileSync(stubProxyBinary(undefined, { version }), "utf8") + "// the old copy\n"
  writeFileSync(projectBinary(state), own)
  chmodSync(projectBinary(state), 0o755)
  return own
}

test("a project with its own old copy of the pinned version moves it to the shared place and downloads nothing", async () => {
  process.env.CONTEXT_GURU_VERSION = "v0.7.2"
  try {
    await withReleases(["0.7.2"], "0.7.2", async ({ downloads, start }) => {
      const directory = "/tmp/context-guru-shared-legacy-move"
      const state = stateDirectory(directory)
      const own = placeOwnCopy(state, "0.7.2")
      assert.equal(existsSync(shared("v0.7.2")), false)

      await start(directory)

      assert.equal(downloads.length, 0, downloads.join(" "))
      assert.ok(lstatSync(projectBinary(state)).isSymbolicLink(), "the old copy stayed in the project")
      assert.equal(realpathSync(projectBinary(state)), realpathSync(shared("v0.7.2")))
      assert.equal(readFileSync(shared("v0.7.2"), "utf8"), own)
    })
  } finally {
    delete process.env.CONTEXT_GURU_VERSION
  }
})

test("a project with its own old copy of a version that is shared already uses the shared copy and downloads nothing", async () => {
  await withReleases(["0.7.3"], "0.7.3", async ({ downloads, start }) => {
    await start("/tmp/context-guru-shared-legacy-first")
    const before = downloads.length
    const directory = "/tmp/context-guru-shared-legacy-drop"
    const state = stateDirectory(directory)
    const sharedBytes = readFileSync(shared("v0.7.3"), "utf8")
    placeOwnCopy(state, "0.7.3")

    await start(directory)

    assert.equal(downloads.length, before, downloads.join(" "))
    assert.ok(lstatSync(projectBinary(state)).isSymbolicLink(), "the old copy stayed in the project")
    assert.equal(realpathSync(projectBinary(state)), realpathSync(shared("v0.7.3")))
    assert.equal(readFileSync(shared("v0.7.3"), "utf8"), sharedBytes)
  })
})

test("purge keeps a shared copy that another project records, and the last purge removes it", async () => {
  await withReleases(["0.7.3"], "0.7.3", async ({ start }) => {
    const a = await start("/tmp/context-guru-shared-purge-a")
    const b = await start("/tmp/context-guru-shared-purge-b")

    const first = await run(join(a, "context-guru-uninstall"), ["--yes", "--purge"])
    assert.equal(first.status, 0, first.out)
    assert.equal(existsSync(projectBinary(a)), false)
    assert.ok(existsSync(shared("v0.7.3")), "purge in A removed the copy that B uses")
    assert.match(spawnSync(projectBinary(b), ["--version"], { encoding: "utf8" }).stdout, /^context-guru-proxy 0\.7\.3 /)

    // The other test projects in this file also link to v0.7.3. Purge them, then B is the last user.
    for (const name of readdirSync(ROOT)) {
      const other = join(ROOT, name)
      if (other === b || name === "bin" || !existsSync(join(other, "context-guru-uninstall"))) continue
      await run(join(other, "context-guru-uninstall"), ["--yes", "--purge"])
    }
    assert.ok(existsSync(shared("v0.7.3")), "B still uses the copy")
    const last = await run(join(b, "context-guru-uninstall"), ["--yes", "--purge"])
    assert.equal(last.status, 0, last.out)
    assert.equal(existsSync(shared("v0.7.3")), false, "the last purge kept the shared copy")
    assert.equal(existsSync(join(SHARED_BIN, "v0.7.3")), false)
  })
})

test("update in one project installs the new version beside the old one that another project uses", async () => {
  await withReleases(["0.7.3", "0.7.4"], "0.7.4", async ({ start }) => {
    const a = await start("/tmp/context-guru-shared-update-a")
    const b = await start("/tmp/context-guru-shared-update-b")

    const applied = await run(join(a, "context-guru-update"), ["--yes"])
    assert.equal(applied.status, 0, applied.out)
    assert.match(applied.out, /^result=updated$/m)

    assert.equal(realpathSync(projectBinary(a)), realpathSync(shared("v0.7.4")))
    assert.equal(realpathSync(projectBinary(b)), realpathSync(shared("v0.7.3")))
    assert.match(spawnSync(projectBinary(b), ["--version"], { encoding: "utf8" }).stdout, /^context-guru-proxy 0\.7\.3 /)
    assert.match(readFileSync(join(a, "install.env"), "utf8"), /^version=v0\.7\.4$/m)

    // B updates too. Now no project uses v0.7.3, so the update removes it.
    for (const name of readdirSync(ROOT)) {
      const other = join(ROOT, name)
      if (other === a || other === b || name === "bin" || !existsSync(join(other, "context-guru-uninstall"))) continue
      await run(join(other, "context-guru-uninstall"), ["--yes", "--purge"])
    }
    const second = await run(join(b, "context-guru-update"), ["--yes"])
    assert.equal(second.status, 0, second.out)
    assert.equal(realpathSync(projectBinary(b)), realpathSync(shared("v0.7.4")))
    assert.deepEqual(versionsOnDisk(), ["v0.7.4"])
  })
})

test("reset in one project keeps the shared copy that another project uses", async () => {
  await withReleases(["0.7.3"], "0.7.3", async ({ start }) => {
    const a = await start("/tmp/context-guru-shared-reset-a")
    const b = await start("/tmp/context-guru-shared-reset-b")

    const reset = await run(join(a, "context-guru-reset"))
    assert.equal(reset.status, 0, reset.out)
    assert.equal(existsSync(projectBinary(a)), false)
    assert.equal(realpathSync(projectBinary(b)), realpathSync(shared("v0.7.3")))
  })
})
