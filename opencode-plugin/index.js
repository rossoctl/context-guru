import { createHash } from "node:crypto"
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, readdirSync, renameSync, writeFileSync } from "node:fs"
import { homedir, tmpdir } from "node:os"
import { join, dirname } from "node:path"
import { spawn, spawnSync } from "node:child_process"
import { createServer } from "node:net"

const REPOSITORY = "rossoctl/context-guru"
const DEFAULT_PRESET = "off"
const DEFAULT_CACHE_STRATEGY = "5-min-ping"
const PRESETS = new Set(["off", "conservative", "medium", "high", "xhigh"])
const CACHE_STRATEGIES = new Set(["none", "5-min-ping"])

function stateDirectory(directory) {
  const root = process.env.CONTEXT_GURU_STATE || join(process.env.XDG_STATE_HOME || join(homedir(), ".local", "state"), "context-guru-opencode")
  return join(root, createHash("sha256").update(directory).digest("hex").slice(0, 16))
}

function readJSON(path) {
  try {
    return JSON.parse(readFileSync(path, "utf8"))
  } catch {
    return undefined
  }
}

function writeAtomic(path, contents, mode = 0o600) {
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 })
  const temporary = `${path}.tmp-${process.pid}`
  writeFileSync(temporary, contents, { mode })
  renameSync(temporary, path)
}

function executableOnPath(name) {
  if (name.includes("/")) return existsSync(name) ? name : undefined
  for (const directory of (process.env.PATH || "").split(":").filter(Boolean)) {
    const candidate = join(directory, name)
    if (existsSync(candidate)) return candidate
  }
  return undefined
}

function releasePlatform() {
  const platform = { darwin: "darwin", linux: "linux" }[process.platform]
  const architecture = { arm64: "arm64", x64: "amd64" }[process.arch]
  if (!platform || !architecture) throw new Error(`unsupported platform: ${process.platform}/${process.arch}`)
  return { platform, architecture }
}

async function downloadBinary(state) {
  const requested = process.env.CONTEXT_GURU_VERSION
  let version = requested
  if (!version) {
    const response = await fetch(`https://github.com/${REPOSITORY}/releases/latest`)
    if (!response.ok) throw new Error(`release lookup failed: HTTP ${response.status}`)
    version = new URL(response.url).pathname.split("/").filter(Boolean).pop()
  }
  if (!version || version === "latest") throw new Error("could not resolve the latest context-guru release")
  if (!version.startsWith("v")) version = `v${version}`

  const { platform, architecture } = releasePlatform()
  const number = version.slice(1)
  const archiveName = `context-guru_${number}_${platform}_${architecture}.tar.gz`
  const base = `https://github.com/${REPOSITORY}/releases/download/${version}`
  const [archiveResponse, checksumResponse] = await Promise.all([
    fetch(`${base}/${archiveName}`),
    fetch(`${base}/checksums.txt`),
  ])
  if (!archiveResponse.ok) throw new Error(`binary download failed: HTTP ${archiveResponse.status}`)
  if (!checksumResponse.ok) throw new Error(`checksum download failed: HTTP ${checksumResponse.status}`)
  const archive = Buffer.from(await archiveResponse.arrayBuffer())
  const checksums = await checksumResponse.text()
  const expected = checksums.split(/\r?\n/).map((line) => line.trim().split(/\s+/)).find((fields) => fields.at(-1)?.replace(/^\*/, "") === archiveName)?.[0]
  if (!expected) throw new Error(`release checksum does not list ${archiveName}`)
  const actual = createHash("sha256").update(archive).digest("hex")
  if (actual !== expected.toLowerCase()) throw new Error(`release checksum mismatch for ${archiveName}`)

  const archivePath = join(tmpdir(), `context-guru-${process.pid}-${Date.now()}.tar.gz`)
  const extractDirectory = join(tmpdir(), `context-guru-${process.pid}-${Date.now()}`)
  const destination = join(state, "bin", "context-guru-proxy")
  mkdirSync(extractDirectory, { recursive: true, mode: 0o700 })
  try {
    writeFileSync(archivePath, archive, { mode: 0o600 })
    const extracted = spawnSync("tar", ["-xzf", archivePath, "-C", extractDirectory], { encoding: "utf8" })
    if (extracted.status !== 0) throw new Error(`could not extract release: ${extracted.stderr || "tar failed"}`)
    const stack = [extractDirectory]
    let source
    while (stack.length && !source) {
      const current = stack.pop()
      for (const entry of readdirSync(current, { withFileTypes: true })) {
        const path = join(current, entry.name)
        if (entry.isDirectory()) stack.push(path)
        else if (entry.name === "context-guru-proxy") source = path
      }
    }
    if (!source) throw new Error("release archive does not contain context-guru-proxy")
    mkdirSync(dirname(destination), { recursive: true, mode: 0o700 })
    renameSync(source, destination)
    spawnSync("chmod", ["755", destination])
  } finally {
    spawnSync("rm", ["-rf", extractDirectory])
    spawnSync("rm", ["-f", archivePath])
  }
  return { path: destination, version, managed: true }
}

function findBinary(state) {
  const configured = process.env.CONTEXT_GURU_BIN
  if (configured && existsSync(configured)) return { path: configured, managed: false }
  const path = executableOnPath("context-guru-proxy")
  if (path) return { path, managed: false }
  const managed = join(state, "bin", "context-guru-proxy")
  if (existsSync(managed)) return { path: managed, managed: true }
  return undefined
}

function freePort(start = 8787) {
  return new Promise((resolve, reject) => {
    const server = createServer()
    server.once("error", () => {
      server.close()
      freePort(start + 1).then(resolve, reject)
    })
    server.listen(start, "127.0.0.1", () => {
      const port = server.address().port
      server.close(() => resolve(port))
    })
  })
}

async function healthy(port) {
  try {
    const response = await fetch(`http://127.0.0.1:${port}/healthz`, { signal: AbortSignal.timeout(1500) })
    return response.ok
  } catch {
    return false
  }
}

async function waitForHealthy(port) {
  for (let attempt = 0; attempt < 60; attempt += 1) {
    if (await healthy(port)) return true
    await new Promise((resolve) => setTimeout(resolve, 250))
  }
  return false
}

function resetScript(state) {
  return `#!/bin/sh
set -u
PID_FILE=${JSON.stringify(join(state, "proxy.pid"))}
BIN_FILE=${JSON.stringify(join(state, "bin", "context-guru-proxy"))}
if [ -f "$PID_FILE" ]; then
  pid=$(tr -cd '0-9' < "$PID_FILE")
  case "$pid" in
    '' ) ;;
    * )
      command=$(ps -p "$pid" -o command= 2>/dev/null || true)
      case "$command" in *context-guru-proxy*) kill "$pid" 2>/dev/null || true ;; esac
      ;;
  esac
fi
rm -f "$PID_FILE"
if [ -f ${JSON.stringify(join(state, "managed-binary"))} ]; then rm -f "$BIN_FILE"; fi
printf '%s\\n' 'context-guru: proxy stopped; remove the plugin entry and restart OpenCode to unroute.'
`
}

function writeRecoveryFiles(state, pid, managed) {
  writeAtomic(join(state, "proxy.pid"), `${pid}\n`, 0o600)
  writeAtomic(join(state, "managed-binary"), managed ? "yes\n" : "no\n", 0o600)
  writeAtomic(join(state, "context-guru-reset"), resetScript(state), 0o700)
}

function modelProviderIds(config) {
  const ids = new Set()
  const add = (model) => {
    if (typeof model === "string" && model.includes("/")) ids.add(model.split("/", 1)[0])
  }
  add(config.model)
  add(config.small_model)
  for (const agent of Object.values(config.agent || {})) add(agent?.model)
  return ids
}

function proxyUpstreamURL(original) {
  try {
    const url = new URL(original)
    url.pathname = url.pathname.replace(/\/v1\/?$/, "") || "/"
    return url.href.replace(/\/$/, "")
  } catch {
    return original.replace(/\/+$/, "")
  }
}

function routeProviders(config, port) {
  config.provider ||= {}
  const configuredIds = Object.keys(config.provider)
  const modelIds = modelProviderIds(config)
  const ids = new Set([...configuredIds, ...modelIds])
  if (!ids.size) {
    ids.add("anthropic")
    ids.add("openai")
  }
  const routes = []
  for (const id of ids) {
    const lower = id.toLowerCase()
    const dialect = lower === "anthropic" || lower.includes("anthropic") ? "anthropic" : lower === "openai" || lower.includes("openai") ? "openai" : undefined
    if (!dialect) continue
    const original = config.provider[id]?.options?.baseURL
    const defaultUpstream = dialect === "anthropic" ? "https://api.anthropic.com" : "https://api.openai.com"
    const upstream = typeof original === "string" && original && !original.includes("127.0.0.1:") && !original.includes("localhost:") ? proxyUpstreamURL(original) : defaultUpstream
    routes.push({ dialect, upstream })
  }
  for (const dialect of ["anthropic", "openai"]) {
    const upstreams = new Set(routes.filter((route) => route.dialect === dialect).map((route) => route.upstream))
    if (upstreams.size > 1) throw new Error(`multiple ${dialect} upstreams are configured; route them through one gateway first`)
  }
  for (const id of ids) {
    const lower = id.toLowerCase()
    const dialect = lower === "anthropic" || lower.includes("anthropic") ? "anthropic" : lower === "openai" || lower.includes("openai") ? "openai" : undefined
    if (!dialect) continue
    const provider = config.provider[id] ||= {}
    provider.options ||= {}
    provider.options.baseURL = dialect === "anthropic" ? `http://127.0.0.1:${port}/anthropic` : `http://127.0.0.1:${port}/openai/v1`
  }
  return routes
}

function upstreamFor(routes, dialect) {
  return routes.find((route) => route.dialect === dialect)?.upstream || (dialect === "anthropic" ? "https://api.anthropic.com" : "https://api.openai.com")
}

async function startProxy({ directory, preset, cacheStrategy, routes, client }) {
  const state = stateDirectory(directory)
  mkdirSync(state, { recursive: true, mode: 0o700 })
  const fingerprint = JSON.stringify({ preset, cacheStrategy, routes: routes.map((route) => [route.dialect, route.upstream]) })
  const recordPath = join(state, "install.json")
  const existing = readJSON(recordPath)
  if (existing?.port && existing.fingerprint === fingerprint && await healthy(existing.port)) return { port: existing.port, state }

  let binary = findBinary(state)
  if (!binary) binary = await downloadBinary(state)
  const port = await freePort(existing?.port || 8787)
  const configPath = join(state, "proxy.yaml")
  const keepalive = cacheStrategy !== "none"
  writeAtomic(configPath, `# context-guru: managed by opencode-context-guru\npreset: ${preset}\ncache:\n  keepalive: ${keepalive}\n  keepalive_idle_seconds: 280\n  keepalive_openai_idle_seconds: 1680\n  keepalive_max_pings: 2\n  keepalive_min_prefix_tokens: 20000\n  keepalive_max_usd_per_ping: 0.25\n`, 0o600)
  const logPath = join(state, "proxy.log")
  const log = openSync(logPath, "a", 0o600)
  const args = ["--listen", `127.0.0.1:${port}`, "--config", configPath, "--dashboard", "--dashboard-db", join(state, "dashboard.db"), "--idle-exit", "24h", "--anthropic-upstream", upstreamFor(routes, "anthropic"), "--openai-upstream", upstreamFor(routes, "openai")]
  const child = spawn(binary.path, args, { detached: true, stdio: ["ignore", log, log] })
  child.unref()
  closeSync(log)
  if (!(await waitForHealthy(port))) {
    try { process.kill(child.pid, "SIGTERM") } catch {}
    throw new Error(`proxy did not become healthy; see ${logPath}`)
  }
  writeRecoveryFiles(state, child.pid, binary.managed)
  writeAtomic(recordPath, JSON.stringify({ binary: binary.path, managed: binary.managed, directory, fingerprint, pid: child.pid, port }, null, 2) + "\n")
  await safeLog(client, "info", `proxy running on 127.0.0.1:${port}; recovery: ${join(state, "context-guru-reset")}`)
  return { port, state }
}

async function safeLog(client, level, message) {
  try {
    await client?.app?.log?.({ body: { service: "opencode-context-guru", level, message } })
  } catch {
    // A logging failure must never prevent model traffic from being configured.
  }
}

export default async function ContextGuruPlugin({ directory, client }, options = {}) {
  const preset = PRESETS.has(options.preset) ? options.preset : DEFAULT_PRESET
  const configuredStrategy = options.cacheStrategy ?? options.cache_strategy
  const cacheStrategy = CACHE_STRATEGIES.has(configuredStrategy) ? configuredStrategy : DEFAULT_CACHE_STRATEGY
  return {
    config: async (config) => {
      try {
        const preview = { provider: config.provider ? structuredClone(config.provider) : {}, model: config.model, small_model: config.small_model, agent: config.agent }
        const routes = routeProviders(preview, 0)
        if (!routes.length) {
          await safeLog(client, "debug", "no Anthropic or OpenAI provider is configured; leaving OpenCode unchanged")
          return
        }
        const proxy = await startProxy({ directory, preset, cacheStrategy, routes, client })
        routeProviders(config, proxy.port)
      } catch (error) {
        await safeLog(client, "error", `proxy startup failed: ${error instanceof Error ? error.message : String(error)}`)
      }
    },
    "chat.headers": async (input, output) => {
      output.headers["x-context-guru-session"] = input.sessionID
    },
  }
}
