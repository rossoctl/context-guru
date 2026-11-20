import { createHash, randomBytes } from "node:crypto"
import { accessSync, closeSync, constants, existsSync, mkdirSync, openSync, readFileSync, readdirSync, renameSync, rmSync, statSync, utimesSync, writeFileSync } from "node:fs"
import { homedir } from "node:os"
import { join, dirname } from "node:path"
import { spawn, spawnSync } from "node:child_process"
import { createServer } from "node:net"

const REPOSITORY = "rossoctl/context-guru"
const DEFAULT_PRESET = "off"
// The picker ladder is a plugin choice. The proxy lists every preset it registers, not this ladder.
const PRESETS = ["off", "conservative", "medium", "high", "xhigh", "ultrahigh"]
// The plugin downloads this release unless CONTEXT_GURU_VERSION says otherwise. `strategies` exists
// from v0.7.1. An older binary cannot give the plugin its strategy table, so there is no fallback.
const PINNED_VERSION = "v0.7.4"

// The strategy table has one owner: the proxy. `context-guru-proxy strategies` prints it as JSON.
// The plugin keeps no copy. It runs the command with a short time limit. A binary that does not know
// the word would start serving, so the command gets a loopback port 0 and is killed at the limit.
export function loadStrategyTable(binaryPath) {
  const update = `update the proxy to ${PINNED_VERSION} or later (/context-guru-update, or remove the binary so the plugin downloads ${PINNED_VERSION})`
  const run = spawnSync(binaryPath, ["strategies"], {
    encoding: "utf8",
    timeout: 5000,
    killSignal: "SIGKILL",
    env: { ...process.env, LISTEN_ADDR: "127.0.0.1:0", CONFIG: "" },
  })
  let table
  try { table = JSON.parse(run.stdout) } catch {}
  const valid = table && typeof table.default === "string" && Array.isArray(table.strategies) && table.strategies.length &&
    table.strategies.every((row) => row && typeof row.name === "string" && Array.isArray(row.providers) && typeof row.spends === "boolean" && typeof row.desc === "string")
  if (run.status !== 0 || !valid) throw new Error(`${binaryPath} cannot list cache strategies; ${update}`)
  return { default: table.default, aliases: table.aliases ?? {}, strategies: table.strategies, presets: table.presets ?? [] }
}

// Strategies that fit a provider this plugin routes. "any" fits all.
export function offeredStrategies(table, dialects) {
  return table.strategies.filter((row) => row.providers.includes("any") || row.providers.some((provider) => dialects.includes(provider)))
}

// The strategy to run: the requested name (alias read as its new name), else the proxy default.
// A name that is unknown, or not offered for the routed providers, gives "none" and a reason.
export function resolveStrategy(table, requested, dialects) {
  const offered = new Set(offeredStrategies(table, dialects).map((row) => row.name))
  const wanted = requested ? (table.aliases[requested] ?? requested) : table.default
  if (offered.has(wanted)) return { name: wanted }
  return { name: "none", reason: `cache strategy "${wanted}" is not offered for ${dialects.join(" + ") || "these providers"}; using none` }
}

export function renderProxyConfig(preset, strategy) {
  return `# context-guru: written by opencode-context-guru; the proxy fills the cache keys of the strategy\npreset: ${preset}\ncache:\n  strategy: ${strategy}\n`
}

// Files the picker script reads. Tab-separated, so no JSON parser is needed in sh.
function strategyFiles(table, dialects) {
  const clean = (text) => String(text).replace(/[\t\r\n]+/g, " ")
  return {
    list: table.strategies.length ? offeredStrategies(table, dialects).map((row) => [row.name, row.spends ? "yes" : "no", clean(row.desc)].join("\t")).join("\n") + "\n" : "",
    aliases: Object.entries(table.aliases).map(([from, to]) => `${from}\t${to}`).join("\n") + "\n",
    default: `${table.default}\n`,
  }
}

// Content capture is always off for a proxy this plugin starts. The owner decided this on 2026-11-10.
// The flag wins over the environment in the proxy, and the environment is set too, so no start path can
// miss it. Every start path goes through these two helpers: the first start, the recovery spawn
// (which also fixes an older install.json that was written without the flag), and the start script.
const CAPTURE_FLAG = "--dashboard-content=false"
export function withCaptureOff(args) {
  return [...args.filter((arg) => !/^--dashboard-content(=|$)/.test(arg)), CAPTURE_FLAG]
}
export function captureOffEnv(env = process.env) {
  return { ...env, DASHBOARD_CONTENT: "0" }
}

export function stateDirectory(directory) {
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
  const temporary = `${path}.tmp-${process.pid}-${randomBytes(4).toString("hex")}`
  writeFileSync(temporary, contents, { mode })
  renameSync(temporary, path)
}

function shQuote(value) {
  return `'${String(value).replace(/'/g, "'\\''")}'`
}

function isExecutable(path) {
  try {
    accessSync(path, constants.X_OK)
    return statSync(path).isFile()
  } catch {
    return false
  }
}

function executableOnPath(name) {
  if (name.includes("/")) return isExecutable(name) ? name : undefined
  for (const directory of (process.env.PATH || "").split(":").filter(Boolean)) {
    const candidate = join(directory, name)
    if (isExecutable(candidate)) return candidate
  }
  return undefined
}

function releasePlatform() {
  const platform = { darwin: "darwin", linux: "linux" }[process.platform]
  const architecture = { arm64: "arm64", x64: "amd64" }[process.arch]
  if (!platform || !architecture) throw new Error(`unsupported platform: ${process.platform}/${process.arch}`)
  return { platform, architecture }
}

function releasesBase() {
  return process.env.CONTEXT_GURU_RELEASES_URL || `https://github.com/${REPOSITORY}/releases`
}

async function downloadBinary(state) {
  let version = process.env.CONTEXT_GURU_VERSION || PINNED_VERSION
  if (!version.startsWith("v")) version = `v${version}`

  const { platform, architecture } = releasePlatform()
  const number = version.slice(1)
  const archiveName = `context-guru_${number}_${platform}_${architecture}.tar.gz`
  const base = `${releasesBase()}/download/${version}`
  const [archiveResponse, checksumResponse] = await Promise.all([
    fetch(`${base}/${archiveName}`),
    fetch(`${base}/checksums.txt`),
  ])
  if (archiveResponse.status === 404) throw new Error(`release ${version} is not published yet (HTTP 404); put a ${PINNED_VERSION} or later context-guru-proxy on PATH, or set CONTEXT_GURU_BIN`)
  if (!archiveResponse.ok) throw new Error(`binary download failed: HTTP ${archiveResponse.status}`)
  if (!checksumResponse.ok) throw new Error(`checksum download failed: HTTP ${checksumResponse.status}`)
  const archive = Buffer.from(await archiveResponse.arrayBuffer())
  const checksums = await checksumResponse.text()
  const expected = checksums.split(/\r?\n/).map((line) => line.trim().split(/\s+/)).find((fields) => fields.at(-1)?.replace(/^\*/, "") === archiveName)?.[0]
  if (!expected) throw new Error(`release checksum does not list ${archiveName}`)
  const actual = createHash("sha256").update(archive).digest("hex")
  if (actual !== expected.toLowerCase()) throw new Error(`release checksum mismatch for ${archiveName}`)

  // Extract under the state directory, not os.tmpdir(): the renameSync onto the
  // destination must stay on one filesystem, or it fails with EXDEV wherever /tmp
  // is tmpfs and the state directory is on disk (common on Linux).
  const workDirectory = join(state, `download-${process.pid}-${randomBytes(3).toString("hex")}`)
  const archivePath = join(workDirectory, "archive.tar.gz")
  const extractDirectory = join(workDirectory, "extract")
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
    spawnSync("rm", ["-rf", workDirectory])
  }
  return { path: destination, version, managed: true }
}

export function findBinaryForTest(state) { return findBinary(state) }

function findBinary(state) {
  const configured = process.env.CONTEXT_GURU_BIN
  if (configured && existsSync(configured)) return { path: configured, managed: false }
  const path = executableOnPath("context-guru-proxy")
  if (path) return { path, managed: false }
  const managed = join(state, "bin", "context-guru-proxy")
  if (existsSync(managed)) return { path: managed, managed: true, version: readJSON(join(state, "install.json"))?.version }
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
if [ -f ${JSON.stringify(join(state, "install.env"))} ]; then . ${JSON.stringify(join(state, "install.env"))}; fi
if [ -f "$PID_FILE" ]; then
  pid=$(tr -cd '0-9' < "$PID_FILE")
  case "$pid" in
    '' ) ;;
    * )
      command=$(ps -p "$pid" -o command= 2>/dev/null || true)
      case "$command" in *"\${binary:-context-guru-proxy}"*) kill "$pid" 2>/dev/null || true ;; esac
      ;;
  esac
fi
rm -f "$PID_FILE"
if [ "$(head -1 ${JSON.stringify(join(state, "managed-binary"))} 2>/dev/null)" = yes ]; then rm -f "$BIN_FILE"; fi
printf '%s\\n' 'context-guru: proxy stopped; remove the plugin entry and restart OpenCode to unroute.'
`
}

function writeRecoveryFiles(state, pid, managed) {
  writeAtomic(join(state, "proxy.pid"), `${pid}\n`, 0o600)
  writeAtomic(join(state, "managed-binary"), managed ? "yes\n" : "no\n", 0o600)
  writeAtomic(join(state, "context-guru-reset"), resetScript(state), 0o700)
}

function statusScript(state) {
  return `#!/bin/sh
# context-guru: read-only status report. Every fact is a key=value line; the command layer presents it.
set -u
STATE=${shQuote(state)}
if [ -f "$STATE/install.env" ]; then . "$STATE/install.env"; fi
if [ -n "\${port:-}" ]; then
  printf '%s\\n' "install=present" "port=$port" "dashboard=http://127.0.0.1:$port/dashboard/"
else
  printf '%s\\n' "install=missing"
fi
if [ -n "\${binary:-}" ] && [ -f "$binary" ]; then
  printf '%s\\n' "binary=$binary" "managed=\${managed:-unknown}" "version=\${version:-unknown}"
  reported=$("$binary" --version 2>/dev/null | head -1 || true)
  [ -n "$reported" ] && printf '%s\\n' "binary_reports=$reported"
fi
[ -n "\${preset:-}" ] && printf '%s\\n' "preset=$preset" "strategy=\${strategy:-}" "dialects=\${dialects:-}"
if [ -f "$STATE/proxy.pid" ]; then
  pid=$(tr -cd '0-9' < "$STATE/proxy.pid")
  [ -n "$pid" ] && printf '%s\\n' "pid=$pid"
fi
if ! command -v curl >/dev/null 2>&1; then
  printf '%s\\n' "proxy=unknown" "curl=missing"
elif [ -z "\${port:-}" ]; then
  printf '%s\\n' "proxy=down"
elif curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
  printf '%s\\n' "proxy=up"
  stats=$(curl -fsS --max-time 3 "http://127.0.0.1:$port/stats" 2>/dev/null || true)
  if [ -n "$stats" ]; then
    for field in requests saved_tokens wasted_tokens savings_pct; do
      value=$(printf '%s' "$stats" | tr ',' '\\n' | grep -F "\\"$field\\":" | head -1 | tr -cd '0-9.')
      [ -n "$value" ] && printf '%s\\n' "stats_$field=$value"
    done
    if [ "\${1:-}" = "--stats" ]; then printf '%s\\n' "$stats"; fi
  fi
else
  printf '%s\\n' "proxy=down"
fi
printf '%s\\n' "reset=$STATE/context-guru-reset"
`
}

function installScript(state) {
  return `#!/bin/sh
# context-guru: idempotent repair — clear a disable marker and ensure the proxy is up.
set -u
STATE=${shQuote(state)}
if [ -f "$STATE/disabled" ]; then
  rm -f "$STATE/disabled"
  printf '%s\\n' "result=re-enabled"
  printf '%s\\n' "note=restart OpenCode so providers route through the proxy again"
fi
if [ ! -f "$STATE/install.env" ]; then
  printf '%s\\n' "result=never-installed"
  printf '%s\\n' "note=restart OpenCode; the plugin downloads the binary and starts the proxy at startup"
  exit 0
fi
. "$STATE/install.env"
if [ ! -f "\${binary:-}" ]; then
  printf '%s\\n' "result=binary-missing" "binary=\${binary:-}"
  printf '%s\\n' "note=restart OpenCode to re-download the release binary"
  exit 0
fi
if command -v curl >/dev/null 2>&1 && curl -fsS --max-time 2 "http://127.0.0.1:\${port:-1}/healthz" >/dev/null 2>&1; then
  printf '%s\\n' "result=healthy" "port=$port" "version=\${version:-unknown}"
  exit 0
fi
if [ -f "$STATE/context-guru-start" ]; then
  printf '%s\\n' "result=restarting"
  sh "$STATE/context-guru-start"
  exit $?
fi
printf '%s\\n' "result=down"
printf '%s\\n' "note=restart OpenCode to start the proxy"
`
}

function updateScript(state) {
  return `#!/bin/sh
# context-guru: check for a newer proxy binary. --yes downloads, checksum-verifies, swaps, restarts.
set -u
REPO="\${CONTEXT_GURU_REPO:-rossoctl/context-guru}"
RELEASES="\${CONTEXT_GURU_RELEASES_URL:-https://github.com/\$REPO/releases}"
STATE=${shQuote(state)}
if [ ! -f "$STATE/install.env" ]; then
  printf '%s\\n' "result=never-installed"
  exit 0
fi
. "$STATE/install.env"
if [ "\${managed:-no}" != "yes" ]; then
  printf '%s\\n' "result=external" "binary=\${binary:-}"
  printf '%s\\n' "note=this binary was not downloaded by the plugin; update it where it came from"
  exit 0
fi
latest=$(curl -fsSI --max-time 10 "$RELEASES/latest" 2>/dev/null | tr -d '\\r' | sed -n 's/^[Ll]ocation:.*\\/tag\\///p' | head -1)
case "$latest" in
  ''|*[!A-Za-z0-9._-]*) printf '%s\\n' "result=lookup-failed" "reason=odd-tag"; exit 1 ;;
esac
printf '%s\\n' "installed=\${version:-unknown}" "latest=$latest"
if [ "$latest" = "\${version:-}" ]; then
  printf '%s\\n' "result=latest"
  exit 0
fi
printf '%s\\n' "result=available"
if [ "\${1:-}" != "--yes" ]; then
  printf '%s\\n' "apply=sh $STATE/context-guru-update --yes"
  exit 0
fi
case "$(uname -s):$(uname -m)" in
  Darwin:arm64) platform=darwin; arch=arm64 ;;
  Darwin:x86_64) platform=darwin; arch=amd64 ;;
  Linux:arm64) platform=linux; arch=arm64 ;;
  Linux:x86_64) platform=linux; arch=amd64 ;;
  *) printf '%s\\n' "result=error" "reason=unsupported-platform"; exit 1 ;;
esac
archive="context-guru_\${latest#v}_\${platform}_\${arch}.tar.gz"
base="$RELEASES/download/$latest"
tmp=$(mktemp -d "\${TMPDIR:-/tmp}/context-guru-update.XXXXXX") || { printf '%s\\n' "result=error" "reason=mktemp"; exit 1; }
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$archive" "$base/$archive" || { printf '%s\\n' "result=error" "reason=download"; exit 1; }
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || { printf '%s\\n' "result=error" "reason=checksum-unavailable"; exit 1; }
want=$(awk -v f="$archive" '$2 == f || $2 == "*"f {print $1}' "$tmp/checksums.txt" | head -1)
if [ -z "$want" ]; then printf '%s\\n' "result=error" "reason=checksum-absent"; exit 1; fi
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$archive" | awk '{print $1}')
else got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}'); fi
if [ "$got" != "$want" ]; then printf '%s\\n' "result=error" "reason=checksum-mismatch"; exit 1; fi
tar -xzf "$tmp/$archive" -C "$tmp" || { printf '%s\\n' "result=error" "reason=extract"; exit 1; }
replacement=$(find "$tmp" -type f -name context-guru-proxy | head -1)
if [ -z "$replacement" ]; then printf '%s\\n' "result=error" "reason=binary-absent"; exit 1; fi
if [ -f "$STATE/proxy.pid" ]; then
  pid=$(tr -cd '0-9' < "$STATE/proxy.pid")
  case "$pid" in
    '' ) ;;
    * ) command=$(ps -p "$pid" -o command= 2>/dev/null || true)
      case "$command" in *"\${binary:-context-guru-proxy}"*) kill "$pid" 2>/dev/null || true ;; esac ;;
  esac
  rm -f "$STATE/proxy.pid"
fi
mv "$replacement" "$binary" || { printf '%s\\n' "result=error" "reason=install"; exit 1; }
chmod 755 "$binary"
if [ "$(uname -s)" = "Darwin" ]; then xattr -d com.apple.quarantine "$binary" 2>/dev/null || true; fi
grep -v '^version=' "$STATE/install.env" > "$STATE/install.env.new" || true
printf '%s\\n' "version=$latest" >> "$STATE/install.env.new"
mv "$STATE/install.env.new" "$STATE/install.env"
printf '%s\\n' "result=updated" "version=$latest"
if [ -f "$STATE/context-guru-start" ]; then
  sh "$STATE/context-guru-start"
else
  printf '%s\\n' "restart=skipped" "note=restart OpenCode to start the updated proxy"
fi
`
}

function uninstallScript(state) {
  return `#!/bin/sh
# context-guru: stop the proxy and disable routing for this project. --yes applies; --purge also removes the binary and state files.
set -u
STATE=${shQuote(state)}
yes=0
purge=0
for arg in "$@"; do
  case "$arg" in
    --yes) yes=1 ;;
    --purge) purge=1 ;;
    *) printf '%s\\n' "result=error" "reason=unknown-arg:$arg"; exit 1 ;;
  esac
done
if [ -f "$STATE/install.env" ]; then . "$STATE/install.env"; fi
if [ "$yes" -ne 1 ]; then
  printf '%s\\n' "result=plan"
  [ -n "\${port:-}" ] && printf '%s\\n' "port=$port" "binary=\${binary:-}" "managed=\${managed:-no}"
  printf '%s\\n' "action=stop-proxy" "action=disable-routing"
  [ "$purge" -eq 1 ] && printf '%s\\n' "action=purge-binary-and-state"
  printf '%s\\n' "apply=sh $STATE/context-guru-uninstall --yes"
  printf '%s\\n' "note=providers unroute when OpenCode next restarts; /context-guru-install undoes this"
  exit 0
fi
if [ -f "$STATE/proxy.pid" ]; then
  pid=$(tr -cd '0-9' < "$STATE/proxy.pid")
  case "$pid" in
    '' ) ;;
    * ) command=$(ps -p "$pid" -o command= 2>/dev/null || true)
      case "$command" in *"\${binary:-context-guru-proxy}"*) kill "$pid" 2>/dev/null || true ;; esac ;;
  esac
  rm -f "$STATE/proxy.pid"
fi
printf 'disabled by context-guru-uninstall\\n' > "$STATE/disabled"
if [ "$purge" -eq 1 ]; then
  # Remove a binary only when the plugin downloaded it: the marker file says "yes" AND the binary
  # lives in this project's state directory. A binary from PATH or CONTEXT_GURU_BIN is never removed.
  if [ "$(head -1 "$STATE/managed-binary" 2>/dev/null)" = yes ]; then
    case "\${binary:-}" in "$STATE"/bin/*) rm -f "$binary" 2>/dev/null ;; esac
  fi
  rm -rf "$STATE/bin"
  rm -f "$STATE/install.json" "$STATE/install.env" "$STATE/proxy.yaml" "$STATE/proxy.log" "$STATE/dashboard.db" "$STATE/managed-binary" "$STATE/context-guru-start"
  rm -f "$STATE/settings.env" "$STATE/options.env" "$STATE/strategies.tsv" "$STATE/strategies.aliases" "$STATE/strategies.default"
  rm -f "$STATE/context-guru-reset" "$STATE/context-guru-status" "$STATE/context-guru-update" "$STATE/context-guru-uninstall" "$STATE/context-guru-preset" "$STATE/context-guru-strategy"
  printf '%s\\n' "result=uninstalled" "purged=yes"
else
  printf '%s\\n' "result=uninstalled" "purged=no" "binary_kept=\${binary:-}"
fi
printf '%s\\n' "note=restart OpenCode so providers stop routing through the proxy"
printf '%s\\n' "undo=/context-guru-install"
`
}

function startScript(state, binary, args, port) {
  // "env DASHBOARD_CONTENT=0" sets the variable for the proxy only. The flag is in args already, and
  // withCaptureOff keeps it there even if a caller passes an args list without it.
  const command = ["env", "DASHBOARD_CONTENT=0", binary, ...withCaptureOff(args)].map(shQuote).join(" ")
  return `#!/bin/sh
# context-guru: start this project's proxy with the arguments the plugin recorded.
set -u
STATE=${shQuote(state)}
PORT=${port}
LOG=${shQuote(join(state, "proxy.log"))}
if command -v curl >/dev/null 2>&1 && curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
  printf '%s\\n' "proxy=already-running port=$PORT"
  exit 0
fi
# The proxy must outlive this script and the shell that ran it: sandboxed command
# runners reap whole process groups, so put the daemon in its own session where
# setsid exists and fall back to nohup where it does not. The inner sh records its
# own pid — the process setsid actually runs, not the wrapper this shell
# backgrounded — and execs the proxy in place, so proxy.pid names the daemon
# whichever branch ran.
if command -v setsid >/dev/null 2>&1; then
  setsid sh -c 'printf "%s\\n" "$$" > "$1"; shift; exec "$@"' start ${shQuote(join(state, "proxy.pid"))} ${command} </dev/null >>"$LOG" 2>&1 &
else
  nohup ${command} </dev/null >>"$LOG" 2>&1 &
  printf '%s\\n' "$!" > "$STATE/proxy.pid"
fi
tries=0
while [ "$tries" -lt 15 ]; do
  if command -v curl >/dev/null 2>&1 && curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    pid=$(tr -cd '0-9' 2>/dev/null < "$STATE/proxy.pid" || true)
    printf '%s\\n' "proxy=started port=$PORT pid=\${pid:-unknown}"
    exit 0
  fi
  sleep 1
  tries=$((tries + 1))
done
printf '%s\\n' "proxy=failed" "see=$LOG"
exit 1
`
}

// ---- Preset and cache strategy pickers --------------------------------------------------
// Same names and one-line wording as the Claude Code skills preset-picker and cache-strategy-picker.
const PRESET_HELP = {
  off: "nothing runs, requests go through untouched",
  conservative: "trims obvious waste, the safe first step into carrying less",
  medium: "conservative plus a cheap model that keeps only what looks relevant in recent tool output, and a summary prepared in the background that is used only if you come back after the cache expired",
  high: "medium, but the summary of older turns is used as soon as the context window is nearly full",
  xhigh: "high plus a deep-adjudication sweep over turns whose cache has already gone cold, the deepest cut",
  ultrahigh: "EXPERIMENTAL. xhigh plus tool deferral: tools your last sessions never used are sent as names only, and the model loads one through search when it needs it. Opt-in",
}
function pickerScript(state, kind) {
  const isPreset = kind === "preset"
  return `#!/bin/sh
# context-guru: show and switch the ${isPreset ? "preset" : "cache strategy"}.
#   (no args)          show what runs now and the choices
#   set NAME           save NAME (takes effect at the next proxy start)
#   set NAME --restart save NAME and restart this project's proxy now
set -u
STATE=${shQuote(state)}
KIND=${kind}
if [ "$KIND" = preset ]; then
  VALID=${shQuote(PRESETS.join(" "))}
  DEFAULT=${shQuote(DEFAULT_PRESET)}
else
  # The proxy owns the table. The plugin wrote the offered rows when it started the proxy.
  [ -f "$STATE/strategies.tsv" ] || { printf '%s\\n' "result=error" "reason=no-strategy-table" "note=restart OpenCode so the plugin reads the table from the proxy"; exit 1; }
  VALID=$(cut -f1 "$STATE/strategies.tsv" | tr '\\n' ' ')
  DEFAULT=$(head -1 "$STATE/strategies.default" 2>/dev/null)
fi

[ -f "$STATE/install.env" ] && . "$STATE/install.env"
saved_value() { sed -n "s/^$1=//p" "$STATE/settings.env" 2>/dev/null | head -1; }
# Effective value = what the user saved with a picker, else what the plugin options say, else the default.
# options.env holds the plugin options; the plugin writes it at every start.
opt_value() { sed -n "s/^$1=//p" "$STATE/options.env" 2>/dev/null | head -1; }
# A value counts only if the plugin would accept it at startup. The preset must be in the preset list.
# The strategy must be in strategies.tsv: the proxy table, filtered to the routed providers, after the alias map.
valid_in() { for v in $2; do [ "$v" = "$1" ] && return 0; done; return 1; }
PRESET_LIST=${shQuote(PRESETS.join(" "))}
if [ -f "$STATE/strategies.tsv" ]; then STRATEGY_LIST=$(cut -f1 "$STATE/strategies.tsv" | tr '\n' ' '); else STRATEGY_LIST=; fi
alias_of() { [ -f "$STATE/strategies.aliases" ] && awk -F '\t' -v n="$1" '$1 == n {print $2; exit}' "$STATE/strategies.aliases"; }
# First candidate that checks out wins: saved choice, plugin option, what the running proxy was started with.
pick_preset() {
  for c in "$(saved_value preset)" "$(opt_value preset)" "\${preset:-}"; do
    [ -n "$c" ] && valid_in "$c" "$PRESET_LIST" && { printf '%s' "$c"; return; }
  done
  printf '%s' ${shQuote(DEFAULT_PRESET)}
}
pick_strategy() {
  for c in "$(saved_value strategy)" "$(opt_value strategy)" "\${strategy:-}"; do
    [ -n "$c" ] || continue
    m=$(alias_of "$c"); [ -n "$m" ] && c=$m
    valid_in "$c" "$STRATEGY_LIST" && { printf '%s' "$c"; return; }
  done
  # The proxy default, if it fits the routed providers. Else none, which fits all.
  d=$(head -1 "$STATE/strategies.default" 2>/dev/null)
  if [ -n "$d" ] && valid_in "$d" "$STRATEGY_LIST"; then printf '%s' "$d"; else printf '%s' none; fi
}
cur_preset=$(pick_preset)
if [ "$KIND" = strategy ]; then cur_strategy=$(pick_strategy); else cur_strategy=$(pick_strategy 2>/dev/null || printf none); fi
running_preset=
if command -v curl >/dev/null 2>&1 && [ -n "\${port:-}" ]; then
  running_preset=$(curl -fsS --max-time 2 "http://127.0.0.1:$port/stats" 2>/dev/null | tr ',' '\\n' | sed -n 's/.*"preset": *"\\([A-Za-z0-9_-]*\\)".*/\\1/p' | head -1)
fi
show() {
  if [ "$KIND" = preset ]; then saved=$cur_preset; else saved=$cur_strategy; fi
  printf '%s\\n' "kind=$KIND" "saved=$saved"
  if [ "$KIND" = preset ]; then
    if [ -n "$running_preset" ]; then printf '%s\\n' "running=$running_preset"; else printf '%s\\n' "running=no_proxy"; fi
    if [ -n "$running_preset" ] && [ "$running_preset" != "$saved" ]; then printf '%s\\n' "gap=saved $saved, running $running_preset (a restart applies it)"; fi
  else
    printf '%s\\n' "running_note=the proxy reads the strategy at start; the saved one runs after the last restart"
  fi
  printf '%s\\n' "default=$DEFAULT"
  if [ "$KIND" = preset ]; then
${PRESETS.map((name) => `    printf '%s\\n' ${shQuote(`choice ${name}: ${PRESET_HELP[name]}`)}`).join("\n")}
  else
    while IFS="$(printf '\\t')" read -r n spends desc; do printf '%s\\n' "choice $n: $desc"; done < "$STATE/strategies.tsv"
    spending=$(awk -F '\\t' '$2 == "yes" {printf "%s%s", sep, $1; sep=","}' "$STATE/strategies.tsv")
    printf '%s\\n' "spends=$spending"
  fi
  printf '%s\\n' "set=sh $STATE/context-guru-${kind} set NAME"
}
valid() { for v in $VALID; do [ "$v" = "$1" ] && return 0; done; return 1; }
render() {
  printf '%s\\n' "# context-guru: written by opencode-context-guru; the proxy fills the cache keys of the strategy" "preset: $2" "cache:" "  strategy: $1"
}
case "\${1:-}" in
  "") show ;;
  set)
    name=\${2:-}
    if [ "$KIND" = strategy ] && [ -f "$STATE/strategies.aliases" ]; then
      mapped=$(awk -F '\\t' -v n="$name" '$1 == n {print $2; exit}' "$STATE/strategies.aliases")
      [ -n "$mapped" ] && name=$mapped
    fi
    if ! valid "$name"; then printf '%s\\n' "result=error" "reason=unknown_${kind}" "known=$VALID"; exit 2; fi
    restart=0; [ "\${3:-}" = "--restart" ] && restart=1
    mkdir -p "$STATE"
    # Save ONLY the key that changed. The other key stays as it was, so a plugin option keeps working.
    if [ "$KIND" = preset ]; then new_preset=$name; new_strategy=$cur_strategy; else new_preset=$cur_preset; new_strategy=$name; fi
    umask 077
    { grep -v "^$KIND=" "$STATE/settings.env" 2>/dev/null || true; printf '%s\\n' "$KIND=$name"; } > "$STATE/settings.env.new" && mv "$STATE/settings.env.new" "$STATE/settings.env"
    printf '%s\\n' "result=saved" "$KIND=$name"
    if [ "$KIND" = strategy ] && [ "$(awk -F '\\t' -v n="$name" '$1 == n {print $2; exit}' "$STATE/strategies.tsv")" = yes ]; then printf '%s\\n' "cost=this spends a little of your own credential on pings while nobody is at the keyboard, on every session routed through this proxy"; fi
    if [ "$restart" -ne 1 ]; then
      printf '%s\\n' "restart_needed=true" "note=the running proxy keeps its old setting until it restarts; a restart costs one cache rewrite in every open session of this project; it also happens at the next OpenCode start"
      exit 0
    fi
    if [ ! -f "$STATE/context-guru-start" ] || [ ! -f "\${binary:-}" ]; then printf '%s\\n' "result=not_applied" "reason=no-install" "note=restart OpenCode to start the proxy with the saved setting"; exit 1; fi
    # Both keys come from pick_preset / pick_strategy, which return only names the plugin accepts at
    # startup. The new file is rendered BEFORE the old proxy is touched.
    render "$new_strategy" "$new_preset" > "$STATE/proxy.yaml.new" || { printf '%s\\n' "result=not_applied" "reason=render"; exit 1; }
    # Keep the old file. If the new proxy does not start, it goes back and the old proxy starts again.
    cp "$STATE/proxy.yaml" "$STATE/proxy.yaml.old" 2>/dev/null || rm -f "$STATE/proxy.yaml.old"
    if [ -f "$STATE/proxy.pid" ]; then
      pid=$(tr -cd '0-9' < "$STATE/proxy.pid")
      case "$pid" in '') ;; *) command=$(ps -p "$pid" -o command= 2>/dev/null || true)
        case "$command" in *"\${binary:-context-guru-proxy}"*) kill "$pid" 2>/dev/null || true ;; esac ;; esac
      rm -f "$STATE/proxy.pid"
    fi
    n=0; while [ "$n" -lt 30 ] && curl -fsS --max-time 1 "http://127.0.0.1:\${port:-1}/healthz" >/dev/null 2>&1; do sleep 0.2; n=$((n + 1)); done
    mv "$STATE/proxy.yaml.new" "$STATE/proxy.yaml"
    if sh "$STATE/context-guru-start"; then
      rm -f "$STATE/proxy.yaml.old"
      grep -v '^preset=\\|^strategy=' "$STATE/install.env" > "$STATE/install.env.new" || true
      printf '%s\\n' "preset=$new_preset" "strategy=$new_strategy" >> "$STATE/install.env.new" && mv "$STATE/install.env.new" "$STATE/install.env"
      printf '%s\\n' "result=restarted"
    else
      # The new proxy did not start. Put the old file back and start the old proxy again.
      if [ -f "$STATE/proxy.yaml.old" ]; then
        mv "$STATE/proxy.yaml.old" "$STATE/proxy.yaml"
        if sh "$STATE/context-guru-start" >/dev/null 2>&1; then printf '%s\\n' "old_proxy=restored"; else printf '%s\\n' "old_proxy=not_restored"; fi
      fi
      printf '%s\\n' "result=not_applied" "note=see $STATE/proxy.log"
      exit 1
    fi
    ;;
  *) printf '%s\\n' "result=error" "reason=unknown-arg"; exit 2 ;;
esac
`
}

// A release tag becomes a value in a file the scripts source. Only plain tag characters are kept.
function safeVersion(version) {
  return typeof version === "string" && /^v?[A-Za-z0-9._-]+$/.test(version) ? version : "external"
}

function writeInstallFacts(state, { binary, port, preset, cacheStrategy, routes }) {
  writeAtomic(join(state, "install.env"), [
    `state=${shQuote(state)}`,
    `port=${port}`,
    `binary=${shQuote(binary.path)}`,
    `managed=${binary.managed ? "yes" : "no"}`,
    `version=${safeVersion(binary.version)}`,
    `preset=${preset}`,
    `strategy=${cacheStrategy}`,
    `dialects=${routes.map((route) => route.dialect).join(",") || "none"}`,
    `dashboard=http://127.0.0.1:${port}/dashboard/`,
  ].join("\n") + "\n", 0o600)
}

// The raw plugin options, for the picker scripts. The pickers show "saved, else option, else default",
// so they need the options even when nothing is routed and no proxy has ever started.
function writeOptionsFile(state, options) {
  const optionStrategy = options?.cacheStrategy ?? options?.cache_strategy
  const safe = (value) => (typeof value === "string" && /^[A-Za-z0-9_-]+$/.test(value) ? value : "")
  writeAtomic(join(state, "options.env"), `preset=${safe(options?.preset)}\nstrategy=${safe(optionStrategy)}\n`)
}

function writeCommandScripts(state, options) {
  writeOptionsFile(state, options)
  writeAtomic(join(state, "context-guru-status"), statusScript(state), 0o700)
  writeAtomic(join(state, "context-guru-install"), installScript(state), 0o700)
  writeAtomic(join(state, "context-guru-update"), updateScript(state), 0o700)
  writeAtomic(join(state, "context-guru-uninstall"), uninstallScript(state), 0o700)
  writeAtomic(join(state, "context-guru-preset"), pickerScript(state, "preset"), 0o700)
  writeAtomic(join(state, "context-guru-strategy"), pickerScript(state, "strategy"), 0o700)
}

// The four commands, shared by both plugin generations: v1 renders intro + a !`sh …` embed
// + outro as a command template whose block executes at render time; v2 runs the script in
// execute and prompts the session with the same text around the captured output.
const COMMAND_SPECS = [
  {
    name: "context-guru-status",
    description: "Show the context-guru proxy status, routing, and savings",
    script: "context-guru-status",
    intro: "Report this project's context-guru status.",
    outro: (path) => "That block ran just now, before you were asked anything — read it rather than re-deriving it. Present its findings plainly: proxy up or down, port, version, and the savings numbers. If the proxy is down, suggest /context-guru-install. If the user asked for the raw stats JSON, run sh " + path + " --stats with your shell tool and print its output verbatim instead of interpreting it.",
  },
  {
    name: "context-guru-install",
    description: "Repair the context-guru proxy and routing for this project",
    script: "context-guru-install",
    intro: "Repair this project's context-guru install.",
    outro: () => "That block ran just now and already did what it could; it is idempotent, so re-running it is safe. Report its result plainly. If it printed result=re-enabled or a note about restarting OpenCode, tell the user to restart OpenCode so providers route through the proxy again.",
  },
  {
    name: "context-guru-update",
    description: "Check for a newer context-guru-proxy binary",
    script: "context-guru-update",
    intro: "Check for a newer context-guru-proxy binary.",
    outro: (path) => "That block ran just now and only CHECKED — nothing was downloaded, stopped, or restarted. Report the installed and latest versions. If it printed result=available, ask the user whether to install the new version; only after they confirm, run sh " + path + " --yes with your shell tool and report its output verbatim. Never run it with --yes without asking first. If it printed result=latest or result=external, just say so.",
  },
  {
    name: "context-guru-uninstall",
    description: "Stop the context-guru proxy and disable routing",
    script: "context-guru-uninstall",
    intro: "Uninstall context-guru from this project.",
    outro: (path) => "That block ran just now and only PLANNED — nothing was stopped yet. Present the plan, then ask the user to confirm. Only after they confirm, run sh " + path + " --yes with your shell tool (append --purge to also delete the binary and state files) and report its output verbatim. Tell them to restart OpenCode so providers stop routing through the proxy, and that /context-guru-install undoes this. Never run it with --yes without asking first.",
  },
  {
    name: "context-guru-preset",
    description: "Show the running context-guru preset and switch it",
    script: "context-guru-preset",
    intro: "Show and switch this project's context-guru preset.",
    outro: (path) => "That block ran just now and only SHOWED the preset. Tell the user which preset is saved and which one runs now (a gap means a restart is pending). List the choices, one line each, from the block. Recommend conservative to someone who wants less context but has not asked for a model pass; medium, high, xhigh and ultrahigh are an escalating ladder. Do not suggest ultrahigh unless the user asks for it. To switch, ask which name they want. Run sh " + path + " set NAME with your shell tool and report it. Then say plainly: the new preset is saved; a restart applies it now; a restart costs one cache rewrite in every open session of this project; if they say no, the old preset runs until the next OpenCode start. Ask for a yes. Only after the yes, run sh " + path + " set NAME --restart and report result=restarted or result=not_applied. Never restart without the yes. Never invent a preset name.",
  },
  {
    name: "context-guru-strategy",
    description: "Show the context-guru cache strategy and switch it",
    script: "context-guru-strategy",
    intro: "Show and switch this project's context-guru cache strategy.",
    outro: (path) => "That block ran just now and only SHOWED the cache strategy. Tell the user which strategy is saved. List the choices, one line each, from the block. Before switching TO a strategy whose line says it spends the caller's credential (the spends= line names them), say once in one line that it spends the user's own credential while nobody is at the keyboard, on every session routed through this proxy. Be honest about 1-hour-head: on Sonnet and Opus the 1-hour tier is usually silently downgraded, so expect no benefit, and it only acts on prefixes of 50k tokens or more. To switch, ask which name they want. Run sh " + path + " set NAME with your shell tool and report it. Then say plainly: the strategy is saved; a restart applies it now; a restart costs one cache rewrite in every open session of this project; if they say no, it applies at the next OpenCode start. Ask for a yes. Only after the yes, run sh " + path + " set NAME --restart and report result=restarted or result=not_applied. Never restart without the yes. Never invent a strategy name.",
  },
]

function commandText(spec, block, path) {
  return spec.intro + "\n\n" + block + "\n\n" + spec.outro(path)
}

function registerCommands(config, state, options) {
  mkdirSync(state, { recursive: true, mode: 0o700 })
  writeCommandScripts(state, options)
  const embed = (name) => "!`sh " + JSON.stringify(join(state, name)) + "`"
  const path = (name) => JSON.stringify(join(state, name))
  config.command ||= {}
  for (const spec of COMMAND_SPECS) {
    config.command[spec.name] = {
      description: spec.description,
      template: commandText(spec, embed(spec.script), path(spec.script)),
    }
  }
}

// The one way any host runs a command script: the v2 chat path and the native TUI dialogs both
// call this, so a switch cannot differ between them.
export function runCommandScript(state, name, args = []) {
  return new Promise((resolve) => {
    const child = spawn("sh", [join(state, name), ...args])
    let output = ""
    child.stdout.on("data", (chunk) => { output += chunk })
    child.stderr.on("data", (chunk) => { output += chunk })
    child.on("error", (error) => resolve({ status: 127, output: String(error.message) }))
    child.on("close", (status) => resolve({ status, output }))
  })
}

// Text form used by the v2 chat path.
async function runCommandScriptText(state, name) {
  const { status, output } = await runCommandScript(state, name)
  return output + (status === 0 ? "" : `\nexit=${status}\n`)
}

// Parses the key=value and "choice NAME: text" lines a picker script prints.
export function parsePickerOutput(text) {
  const facts = {}
  const choices = []
  for (const line of String(text).split("\n")) {
    const choice = /^choice ([A-Za-z0-9_-]+): (.*)$/.exec(line)
    if (choice) choices.push({ name: choice[1], help: choice[2] })
    else {
      const fact = /^([a-z_]+)=(.*)$/.exec(line)
      if (fact) facts[fact[1]] = fact[2]
    }
  }
  return { facts, choices }
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

function providerDialect(id) {
  const lower = String(id).toLowerCase()
  return lower === "anthropic" || lower.includes("anthropic") ? "anthropic" : lower === "openai" || lower.includes("openai") ? "openai" : undefined
}

function defaultUpstream(dialect) {
  return dialect === "anthropic" ? "https://api.anthropic.com" : "https://api.openai.com"
}

// What a previous session recorded: this project's proxy port and the upstreams it
// was started with. A provider baseURL aimed at that port is this plugin's own proxy
// (a re-run config hook or a persisted overlay), not a user gateway — recover the
// recorded upstream instead of chaining the proxy to itself or dropping the gateway.
function recordedRoutes(state) {
  const record = readJSON(join(state, "install.json"))
  if (!record?.port || typeof record.fingerprint !== "string") return undefined
  let routes
  try {
    routes = JSON.parse(record.fingerprint)?.routes
  } catch {
    routes = undefined
  }
  const upstreams = new Map()
  for (const [dialect, upstream] of Array.isArray(routes) ? routes : []) upstreams.set(dialect, upstream)
  return { port: record.port, upstreams }
}

function isOwnProxyURL(original, ownPort) {
  if (!ownPort || typeof original !== "string" || !original) return false
  try {
    const url = new URL(original)
    return (url.hostname === "127.0.0.1" || url.hostname === "localhost" || url.hostname === "[::1]") && Number(url.port) === ownPort
  } catch {
    return false
  }
}

function providerUpstream(original, dialect, own) {
  if (typeof original !== "string" || !original) return defaultUpstream(dialect)
  if (isOwnProxyURL(original, own?.port)) return own?.upstreams.get(dialect) || defaultUpstream(dialect)
  return proxyUpstreamURL(original)
}

export function routeProviders(config, port, own) {
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
    const dialect = providerDialect(id)
    if (!dialect) continue
    routes.push({ id, dialect, upstream: providerUpstream(config.provider[id]?.options?.baseURL, dialect, own) })
  }
  for (const dialect of ["anthropic", "openai"]) {
    const upstreams = new Set(routes.filter((route) => route.dialect === dialect).map((route) => route.upstream))
    if (upstreams.size > 1) throw new Error(`multiple ${dialect} upstreams are configured; route them through one gateway first`)
  }
  for (const id of ids) {
    const dialect = providerDialect(id)
    if (!dialect) continue
    const provider = config.provider[id] ||= {}
    provider.options ||= {}
    provider.options.baseURL = `http://127.0.0.1:${port}/${dialect}/v1`
  }
  return routes
}

function upstreamFor(routes, dialect) {
  return routes.find((route) => route.dialect === dialect)?.upstream || defaultUpstream(dialect)
}

// One lock for every project on this machine. It covers picking a free port, starting the proxy
// and writing the records, so two OpenCode instances cannot pick one port or write one pid file at
// the same time. The holder touches the lock every few seconds. A lock nobody has touched for
// staleMs belongs to a holder that is gone, and the next caller removes it. The download is not
// done under this lock (it can take minutes), so a live holder never looks stale.
export async function withLock(root, fn, { heartbeatMs = 5000, staleMs = 30000, waitMs = 45000 } = {}) {
  mkdirSync(root, { recursive: true, mode: 0o700 })
  const lock = join(root, ".lock")
  const deadline = Date.now() + waitMs
  for (;;) {
    try {
      mkdirSync(lock, { mode: 0o700 })
      break
    } catch (error) {
      if (error?.code !== "EEXIST") throw error
      if (Date.now() > deadline) throw new Error(`could not take ${lock}`)
      try {
        if (Date.now() - statSync(lock).mtimeMs > staleMs) { rmSync(lock, { recursive: true, force: true }); continue }
      } catch {
        // The lock vanished between mkdir and stat: try again, the deadline above still bounds the loop.
        continue
      }
      await new Promise((resolve) => setTimeout(resolve, 100))
    }
  }
  const beat = setInterval(() => {
    const now = new Date()
    try { utimesSync(lock, now, now) } catch {}
  }, heartbeatMs)
  beat.unref?.()
  try {
    return await fn()
  } finally {
    clearInterval(beat)
    rmSync(lock, { recursive: true, force: true })
  }
}

// Stops the proxy this plugin recorded. The pid file may be missing, and a pid may have been reused,
// so the process must be running the binary this plugin recorded before it gets a signal.
async function stopRecordedProxy(state, port) {
  let pid = 0
  try { pid = Number(String(readFileSync(join(state, "proxy.pid"), "utf8")).replace(/\D/g, "")) } catch {}
  if (!pid) return false
  const binary = readJSON(join(state, "install.json"))?.binary
  const command = spawnSync("ps", ["-p", String(pid), "-o", "command="], { encoding: "utf8" }).stdout || ""
  if (!binary || !command.includes(binary)) return false
  try { process.kill(pid, "SIGTERM") } catch { return false }
  for (let attempt = 0; attempt < 40 && await healthy(port); attempt += 1) await new Promise((resolve) => setTimeout(resolve, 100))
  return true
}

export function settingsFile(state) {
  return join(state, "settings.env")
}

// The picker commands save the user's choice here. It wins over the plugin options.
function savedSettings(state) {
  const saved = {}
  try {
    for (const line of readFileSync(settingsFile(state), "utf8").split("\n")) {
      const match = /^(preset|strategy)=([A-Za-z0-9_-]+)$/.exec(line)
      if (match) saved[match[1]] = match[2]
    }
  } catch {}
  return saved
}

// The names the user asked for. The proxy table decides later whether a strategy name is valid.
export function effectiveSettings(state, options = {}) {
  const saved = savedSettings(state)
  const preset = [saved.preset, options.preset].find((name) => PRESETS.includes(name)) ?? DEFAULT_PRESET
  const requestedStrategy = saved.strategy ?? options.cacheStrategy ?? options.cache_strategy
  return { preset, requestedStrategy }
}

async function startProxy({ directory, preset, requestedStrategy, options, routes, client }) {
  const state = stateDirectory(directory)
  mkdirSync(state, { recursive: true, mode: 0o700 })
  // The download can take minutes. It runs before the lock: it writes a temp name and renames it, so
  // two instances that both download only replace the file with the same bytes.
  let binary = findBinary(state)
  if (!binary) binary = await downloadBinary(state)
  return withLock(dirname(state), async () => {
    // Another instance may have put a binary in place while this one waited. Keep the version we
    // know: the file on disk does not carry it until install.json is written.
    const found = findBinary(state)
    if (found) binary = { ...found, version: found.version || binary?.version }
    const dialects = [...new Set(routes.map((route) => route.dialect))]
    const table = loadStrategyTable(binary.path)
    const resolved = resolveStrategy(table, requestedStrategy, dialects)
    if (resolved.reason) await safeLog(client, "warn", resolved.reason)
    const cacheStrategy = resolved.name
    const files = strategyFiles(table, dialects)
    writeAtomic(join(state, "strategies.tsv"), files.list)
    writeAtomic(join(state, "strategies.aliases"), files.aliases)
    writeAtomic(join(state, "strategies.default"), files.default)
    const configText = renderProxyConfig(preset, cacheStrategy)
    const fingerprint = JSON.stringify({ routes: routes.map((route) => [route.dialect, route.upstream]) })
    const recordPath = join(state, "install.json")
    const existing = readJSON(recordPath)
    const running = existing?.port ? await healthy(existing.port) : false
    let onDisk
    try { onDisk = readFileSync(join(state, "proxy.yaml"), "utf8") } catch {}
    if (running && existing.fingerprint === fingerprint && onDisk === configText) return { port: existing.port, state }
    // Another setting is wanted: stop this project's old proxy and reuse its port.
    if (running) await stopRecordedProxy(state, existing.port)

    const port = await freePort(existing?.port || Number(process.env.CONTEXT_GURU_PORT_START) || 8787)
    const configPath = join(state, "proxy.yaml")
    writeAtomic(configPath, configText, 0o600)
    const logPath = join(state, "proxy.log")
    const log = openSync(logPath, "a", 0o600)
    const args = ["--listen", `127.0.0.1:${port}`, "--config", configPath, "--dashboard", "--dashboard-db", join(state, "dashboard.db"), "--idle-exit", "24h", "--anthropic-upstream", upstreamFor(routes, "anthropic"), "--openai-upstream", upstreamFor(routes, "openai")].concat(CAPTURE_FLAG)
    const child = spawn(binary.path, args, { detached: true, stdio: ["ignore", log, log], env: captureOffEnv() })
    const exited = new Promise((resolve) => { child.once("exit", (code, signal) => resolve({ code, signal })); child.once("error", (error) => resolve({ error })) })
    child.unref()
    closeSync(log)
    // Healthy counts only while OUR child is alive. A stranger that holds the port also answers
    // /healthz, so a child that exited means the answer is not ours.
    const outcome = await Promise.race([waitForHealthy(port).then((ok) => ({ ok })), exited.then((result) => ({ exited: result }))])
    if (!outcome.ok || (await Promise.race([exited, new Promise((resolve) => setTimeout(() => resolve(undefined), 50))]))) {
      try { process.kill(child.pid, "SIGTERM") } catch {}
      throw new Error(outcome.exited ? `proxy exited at start (${outcome.exited.error?.message ?? `code ${outcome.exited.code}`}); see ${logPath}` : `proxy did not become healthy; see ${logPath}`)
    }
    writeRecoveryFiles(state, child.pid, binary.managed)
    writeInstallFacts(state, { binary, port, preset, cacheStrategy, routes })
    writeAtomic(join(state, "context-guru-start"), startScript(state, binary.path, args, port), 0o700)
    writeAtomic(recordPath, JSON.stringify({ binary: binary.path, managed: binary.managed, version: binary.version || "external", directory, fingerprint, pid: child.pid, port, args }, null, 2) + "\n")
    await safeLog(client, "info", `proxy running on 127.0.0.1:${port}; recovery: ${join(state, "context-guru-reset")}`)
    return { port, state }
  })
}

async function safeLog(client, level, message) {
  try {
    await client?.app?.log?.({ body: { service: "opencode-context-guru", level, message } })
  } catch {
    // A logging failure must never prevent model traffic from being configured.
  }
}

const proxyRecoveries = new Map()

// The config hook runs once, at OpenCode startup, but the proxy can exit later
// (--idle-exit, a crash). Before a request goes out, restart a dead proxy on its
// recorded port so routed providers keep working without an OpenCode restart.
// Single-flight: concurrent requests share one recovery; a failed recovery is
// retried by the next request, never in a loop.
async function ensureProxyRunning(directory, client) {
  const state = stateDirectory(directory)
  const record = readJSON(join(state, "install.json"))
  if (!record?.port || !record.binary || !Array.isArray(record.args) || !record.args.length) return
  if (await healthy(record.port)) return
  if (existsSync(join(state, "disabled"))) return
  const inFlight = proxyRecoveries.get(state)
  if (inFlight) return inFlight
  const recovery = withLock(dirname(state), async () => {
    // Another instance may have restarted it while this one waited for the lock.
    if (await healthy(record.port)) return
    await recoverProxy(state, readJSON(join(state, "install.json")) ?? record, client)
  }).finally(() => proxyRecoveries.delete(state))
  proxyRecoveries.set(state, recovery)
  return recovery
}

async function recoverProxy(state, record, client) {
  const logPath = join(state, "proxy.log")
  const log = openSync(logPath, "a", 0o600)
  let child
  try {
    child = spawn(record.binary, withCaptureOff(record.args ?? []), { detached: true, stdio: ["ignore", log, log], env: captureOffEnv() })
    child.once("error", () => {})
  } catch {
    return
  } finally {
    closeSync(log)
  }
  child.unref()
  if (typeof child.pid !== "number") return
  if (!(await waitForHealthy(record.port))) {
    try { process.kill(child.pid, "SIGTERM") } catch {}
    await safeLog(client, "error", `proxy recovery failed on 127.0.0.1:${record.port}; see ${logPath}`)
    return
  }
  writeRecoveryFiles(state, child.pid, record.managed === true)
  await safeLog(client, "info", `proxy recovered on 127.0.0.1:${record.port}`)
}

async function ContextGuruPlugin({ directory, client }, options = {}) {
  const { preset, requestedStrategy } = effectiveSettings(stateDirectory(directory), options)
  let routedIds = new Set()
  return {
    config: async (config) => {
      const state = stateDirectory(directory)
      try {
        registerCommands(config, state, options)
      } catch (error) {
        await safeLog(client, "error", `command registration failed: ${error instanceof Error ? error.message : String(error)}`)
      }
      try {
        if (existsSync(join(state, "disabled"))) {
          await safeLog(client, "info", "context-guru is disabled for this project; /context-guru-install re-enables it")
          return
        }
        const own = recordedRoutes(state)
        const preview = { provider: config.provider ? structuredClone(config.provider) : {}, model: config.model, small_model: config.small_model, agent: config.agent }
        const routes = routeProviders(preview, 0, own)
        if (!routes.length) {
          await safeLog(client, "debug", "no Anthropic or OpenAI provider is configured; leaving OpenCode unchanged")
          return
        }
        const proxy = await startProxy({ directory, preset, requestedStrategy, options, routes, client })
        routeProviders(config, proxy.port, own)
        routedIds = new Set(routes.map((route) => route.id))
      } catch (error) {
        await safeLog(client, "error", `proxy startup failed: ${error instanceof Error ? error.message : String(error)}`)
      }
    },
    "chat.headers": async (input, output) => {
      // Only a provider this plugin routed sends traffic to the proxy. Do not tag any other.
      const id = input.provider?.info?.id ?? input.model?.providerID
      if (!id || !routedIds.has(id)) return
      try {
        await ensureProxyRunning(directory, client)
      } catch {
        // Recovery must never break the request it is trying to save.
      }
      if (input.sessionID) output.headers["x-context-guru-session"] = input.sessionID
    },
  }
}

// OpenCode 2: setup(ctx) replaces the v1 config and chat.headers hooks with domain
// transforms — provider.transform for routing, command.transform for commands, and a
// per-routed-provider "http.request" session hook for the session header and dead-proxy
// recovery. Registering that hook once per routed provider also scopes the header to
// providers whose traffic actually flows through the proxy.
async function setupV2(ctx) {
  const directory = ctx.location?.directory
  if (!directory) throw new Error("no project directory in the plugin context")
  const options = ctx.options ?? {}
  const state = stateDirectory(directory)
  const { preset, requestedStrategy } = effectiveSettings(state, options)
  // v2 has no client.app.log; the documented channel is the console.
  const log = { app: { log: async ({ body }) => { (body.level === "error" ? console.error : console.log)(body.message) } } }
  let routedIds = []
  // Hooks are registered from inside the provider callback, as soon as the routed ids are known, and
  // each hook checks the routed set at request time. So the order of "callback runs" and
  // "transform() resolves" does not matter: a callback that runs late still gets its hooks, and
  // setup waits for them.
  const hooked = new Set()
  const pendingHooks = []
  const hookProvider = (id) => {
    if (hooked.has(id)) return
    hooked.add(id)
    pendingHooks.push(ctx.session.hook("http.request", async (event) => {
      if (!routedIds.includes(id)) return
      try {
        await ensureProxyRunning(directory, log)
      } catch {
        // Recovery must never break the request it is trying to save.
      }
      if (event.sessionID) event.request.headers.set("x-context-guru-session", event.sessionID)
    }, { providerID: id }))
  }

  try {
    mkdirSync(state, { recursive: true, mode: 0o700 })
    writeCommandScripts(state, options)
    await ctx.command.transform((editor) => {
      for (const spec of COMMAND_SPECS) {
        editor.add({
          name: spec.name,
          description: spec.description,
          execute: async ({ sessionID, prompt, delivery }) => {
            const output = await runCommandScriptText(state, spec.script)
            await ctx.session.prompt({ ...prompt, sessionID, text: commandText(spec, output, JSON.stringify(join(state, spec.script))), delivery })
          },
        })
      }
    })
  } catch (error) {
    console.error(`opencode-context-guru: command registration failed: ${error instanceof Error ? error.message : String(error)}`)
  }

  try {
    if (existsSync(join(state, "disabled"))) {
      console.log("opencode-context-guru: disabled for this project; /context-guru-install re-enables it")
      return
    }
    const own = recordedRoutes(state)
    await ctx.provider.transform(async (editor) => {
      const routes = []
      const targets = []
      for (const provider of editor.list() ?? []) {
        const dialect = providerDialect(provider.id)
        if (!dialect) continue
        routes.push({ dialect, upstream: providerUpstream(provider.settings?.baseURL, dialect, own) })
        targets.push({ id: provider.id, dialect })
      }
      for (const dialect of ["anthropic", "openai"]) {
        const upstreams = new Set(routes.filter((route) => route.dialect === dialect).map((route) => route.upstream))
        if (upstreams.size > 1) throw new Error(`multiple ${dialect} upstreams are configured; route them through one gateway first`)
      }
      if (!routes.length) return
      const proxy = await startProxy({ directory, preset, requestedStrategy, options, routes, client: log })
      for (const target of targets) {
        editor.update(target.id, (provider) => {
          provider.settings = { ...provider.settings, baseURL: `http://127.0.0.1:${proxy.port}/${target.dialect}/v1` }
        })
      }
      routedIds = targets.map((target) => target.id)
      for (const id of routedIds) hookProvider(id)
    })
    // The transform callback may run after transform() resolves, so do not read routedIds here.
    // Register one hook per provider id that could be routed, and decide inside the hook, when a
    // request comes, whether this provider was routed.
    await Promise.all(pendingHooks)
    // If the host runs the provider callback later, nothing is hooked yet and this line is not a verdict:
    // the callback registers its own hooks when it runs. So the wording says "so far".
    if (!hooked.size) console.log("opencode-context-guru: no Anthropic or OpenAI provider routed so far; hooks are added if the host lists one later")
  } catch (error) {
    console.error(`opencode-context-guru: proxy startup failed: ${error instanceof Error ? error.message : String(error)}`)
  }
}

// One entrypoint for both plugin generations. v1 (OpenCode 1.18.29+) accepts an object
// default export and calls server(input, options) for the hook map; v2 validates
// { id, setup } on the default export and calls setup(ctx), ignoring server.
// Plugin.define in @opencode/plugin is an identity function, so no import is needed
// and a bare local file path keeps working without npm.
export default {
  id: "opencode-context-guru",
  server: ContextGuruPlugin,
  setup: setupV2,
}
