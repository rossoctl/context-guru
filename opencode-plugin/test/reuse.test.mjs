import assert from "node:assert/strict"
import test from "node:test"
import { mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-reuse-test-"))
process.env.CONTEXT_GURU_PORT_START = "9000"

import plugin, { stateDirectory } from "../index.js"
import { stubProxyBinary } from "./helpers.mjs"

const context = (directory, logs) => ({ directory, client: { app: { log: async ({ body }) => { logs.push(body.message) } } } })
const anthropic = () => ({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })

// Issue #1237: a second instance on the same project reuses the running proxy and must say so in its log.
test("a second instance that reuses the running proxy logs the port and the recovery path", async () => {
  const directory = "/tmp/context-guru-reuse-log"
  const state = stateDirectory(directory)
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const first = []
    await (await plugin.server(context(directory, first))).config(anthropic())
    const { port } = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    assert.ok(first.includes(`proxy running on 127.0.0.1:${port}; recovery: ${join(state, "context-guru-reset")}`), first.join("\n"))

    const second = []
    await (await plugin.server(context(directory, second))).config(anthropic())

    assert.equal(JSON.parse(readFileSync(join(state, "install.json"), "utf8")).port, port)
    assert.ok(second.includes(`proxy reused on 127.0.0.1:${port}; recovery: ${join(state, "context-guru-reset")}`), second.join("\n"))
  } finally {
    process.kill(JSON.parse(readFileSync(join(state, "install.json"), "utf8")).pid, "SIGTERM")
    delete process.env.CONTEXT_GURU_BIN
  }
})
