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
export function stubProxyBinary() {
  const stubDirectory = mkdtempSync(join(tmpdir(), "context-guru-stub-"))
  const stub = join(stubDirectory, "context-guru-proxy")
  writeFileSync(stub, `#!/usr/bin/env node
const { createServer } = require("node:http")
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
