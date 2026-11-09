import assert from "node:assert/strict"
import { chmodSync, mkdtempSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

export async function waitUntilUnhealthy(port) {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    try {
      await fetch(`http://127.0.0.1:${port}/healthz`, { signal: AbortSignal.timeout(500) })
      await new Promise((resolve) => setTimeout(resolve, 100))
    } catch {
      return
    }
  }
  assert.fail(`proxy on port ${port} did not stop`)
}

// A stub proxy binary: a node script named context-guru-proxy so the pid-guarded
// kills in the scripts recognize it, serving /healthz like the real proxy.
// The strategy table the real proxy prints (shape of config.StrategiesJSON). Rows are trimmed.
export const FAKE_STRATEGIES = {
  default: "auto",
  aliases: { "5-min-ping": "auto" },
  presets: ["off", "conservative", "medium", "high", "xhigh", "ultrahigh"],
  strategies: [
    { name: "none", spends: false, desc: "no cache strategy", providers: ["any"], cache: {} },
    { name: "auto", spends: true, desc: "idle pings that follow the cache lifetime", providers: ["anthropic"], cache: { keepalive: true } },
    { name: "1-ping", spends: true, desc: "at most 1 idle ping", providers: ["anthropic"], cache: { keepalive: true } },
    { name: "2-ping", spends: true, desc: "at most 2 idle pings", providers: ["anthropic"], cache: { keepalive: true } },
    { name: "1-hour-head", spends: false, desc: "asks for the 1-hour tier", providers: ["anthropic"], cache: { head_ttl_1h: true } },
    { name: "30-min-ping", spends: true, desc: "refreshes OpenAI caches", providers: ["openai"], cache: { keepalive: true } },
  ],
}

export function stubProxyBinary(table = FAKE_STRATEGIES) {
  const stubDirectory = mkdtempSync(join(tmpdir(), "context-guru-stub-"))
  const stub = join(stubDirectory, "context-guru-proxy")
  writeFileSync(stub, `#!/usr/bin/env node
const { createServer } = require("node:http")
if (process.argv[2] === "strategies") { console.log(${JSON.stringify(JSON.stringify(table))}); process.exit(0) }
const listen = process.argv.indexOf("--listen")
const port = Number(process.argv[listen + 1].split(":")[1])
createServer((request, response) => {
  if (request.url === "/healthz") response.end("ok")
  else { response.statusCode = 404; response.end() }
}).listen(port, "127.0.0.1")
`)
  chmodSync(stub, 0o755)
  return stub
}

// A proxy that lists its strategies like the real one, then exits at once when asked to serve.
export function deadProxyBinary() {
  const dir = mkdtempSync(join(tmpdir(), "context-guru-dead-"))
  const path = join(dir, "context-guru-proxy")
  writeFileSync(path, `#!/bin/sh
if [ "$1" = strategies ]; then echo '${JSON.stringify(FAKE_STRATEGIES)}'; exit 0; fi
exit 1
`)
  chmodSync(path, 0o755)
  return path
}

// A stub proxy like the real one in one way: it reads --config, and it exits at once, as the real proxy does
// for an unknown preset or strategy name, when the file holds a name the real proxy would refuse.
export function pickyProxyBinary(table = FAKE_STRATEGIES, { refuse = [] } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "context-guru-picky-"))
  const path = join(dir, "context-guru-proxy")
  const presets = ["off", "conservative", "medium", "high", "xhigh", "ultrahigh"]
  writeFileSync(path, `#!/usr/bin/env node
const { createServer } = require("node:http")
const { readFileSync } = require("node:fs")
if (process.argv[2] === "strategies") { console.log(${JSON.stringify(JSON.stringify(table))}); process.exit(0) }
const arg = (name) => process.argv[process.argv.indexOf(name) + 1]
const text = readFileSync(arg("--config"), "utf8")
const preset = /^preset: (.+)$/m.exec(text)?.[1]
const strategy = /^  strategy: (.+)$/m.exec(text)?.[1]
const names = ${JSON.stringify(table.strategies.map((row) => row.name))}
const refuse = ${JSON.stringify(refuse)}
if (!${JSON.stringify(presets)}.includes(preset) || (strategy && !names.includes(strategy)) || refuse.includes(strategy) || refuse.includes(preset)) {
  console.error("config: unknown preset or strategy: " + preset + " " + strategy)
  process.exit(2)
}
const port = Number(arg("--listen").split(":")[1])
createServer((request, response) => {
  if (request.url === "/healthz") response.end("ok")
  else { response.statusCode = 404; response.end() }
}).listen(port, "127.0.0.1")
`)
  chmodSync(path, 0o755)
  return path
}
