import assert from "node:assert/strict"
import test from "node:test"
import { mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-plugin-test-"))

process.env.CONTEXT_GURU_PORT_START = "9300"

import plugin, { routeProviders, stateDirectory } from "../index.js"
import { stubProxyBinary } from "./helpers.mjs"

function context() {
  return {
    directory: "/tmp/context-guru-opencode-test",
    client: { app: { log: async () => {} } },
  }
}

test("exposes the dual v1/v2 entrypoint", async () => {
  assert.equal(plugin.id, "opencode-context-guru")
  assert.equal(typeof plugin.server, "function")
  assert.equal(typeof plugin.setup, "function")
})

test("exposes the OpenCode hooks", async () => {
  const hooks = await plugin.server(context())

  assert.equal(typeof hooks.config, "function")
  assert.equal(typeof hooks["chat.headers"], "function")
})

test("adds the OpenCode session header", async () => {
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  try {
    const hooks = await plugin.server({ directory: "/tmp/context-guru-header-test", client: context().client })
    await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } }, custom: { options: { baseURL: "https://example.test" } } } })

    const routed = { headers: {} }
    await hooks["chat.headers"]({ sessionID: "session-test", provider: { info: { id: "anthropic" } } }, routed)
    assert.equal(routed.headers["x-context-guru-session"], "session-test")

    // A provider the plugin did not route does not send traffic to the proxy: no header.
    const other = { headers: {} }
    await hooks["chat.headers"]({ sessionID: "session-test", provider: { info: { id: "custom" } } }, other)
    assert.equal(other.headers["x-context-guru-session"], undefined)
  } finally {
    const record = JSON.parse(readFileSync(join(stateDirectory("/tmp/context-guru-header-test"), "install.json"), "utf8"))
    process.kill(record.pid, "SIGTERM")
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("leaves unsupported providers unchanged", async () => {
  const hooks = await plugin.server(context())
  const config = {
    model: "custom/model",
    provider: { custom: { options: { baseURL: "https://example.test" } } },
  }

  await hooks.config(config)

  assert.deepEqual(config.provider, { custom: { options: { baseURL: "https://example.test" } } })
  assert.equal(config.model, "custom/model")
})

test("registers the context-guru commands", async () => {
  const hooks = await plugin.server(context())
  const config = { provider: { custom: { options: { baseURL: "https://example.test" } } } }

  await hooks.config(config)

  assert.deepEqual(Object.keys(config.command).sort(), [
    "context-guru-install",
    "context-guru-preset",
    "context-guru-status",
    "context-guru-strategy",
    "context-guru-uninstall",
    "context-guru-update",
  ])
  for (const command of Object.values(config.command)) {
    assert.equal(typeof command.template, "string")
    assert.equal(typeof command.description, "string")
  }
})

test("rewrites routed provider base URLs to the paths the proxy serves", () => {
  const config = {
    provider: {
      anthropic: { options: { baseURL: "https://api.anthropic.com" } },
      openai: { options: { baseURL: "https://api.openai.com/v1" } },
    },
  }

  routeProviders(config, 8787)

  // @ai-sdk/anthropic appends only /messages to the base URL (its own default
  // base is https://api.anthropic.com/v1), so the base must include /v1 to hit
  // the proxy's POST /anthropic/v1/messages route. @ai-sdk/openai appends
  // /responses or /chat/completions, which the proxy serves under /openai/v1.
  assert.equal(config.provider.anthropic.options.baseURL, "http://127.0.0.1:8787/anthropic/v1")
  assert.equal(config.provider.openai.options.baseURL, "http://127.0.0.1:8787/openai/v1")
})

test("preserves a local gateway as the upstream instead of replacing it with the public API", () => {
  const config = { provider: { anthropic: { options: { baseURL: "http://localhost:4000/anthropic" } } } }

  const routes = routeProviders(config, 8787)

  assert.equal(routes.length, 1)
  assert.equal(routes[0].upstream, "http://localhost:4000/anthropic")
  assert.equal(config.provider.anthropic.options.baseURL, "http://127.0.0.1:8787/anthropic/v1")
})

test("recovers the recorded upstream when a baseURL points at this plugin's own proxy", () => {
  const config = { provider: { anthropic: { options: { baseURL: "http://127.0.0.1:8787/anthropic/v1" } } } }
  const own = { port: 8787, upstreams: new Map([["anthropic", "https://gw.example.test"]]) }

  const routes = routeProviders(config, 8787, own)

  assert.equal(routes.length, 1)
  assert.equal(routes[0].upstream, "https://gw.example.test")
})
