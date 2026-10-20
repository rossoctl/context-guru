import assert from "node:assert/strict"
import test from "node:test"

import plugin from "../index.js"

function context() {
  return {
    directory: "/tmp/context-guru-opencode-test",
    client: { app: { log: async () => {} } },
  }
}

test("exposes the OpenCode hooks", async () => {
  const hooks = await plugin(context())

  assert.equal(typeof hooks.config, "function")
  assert.equal(typeof hooks["chat.headers"], "function")
})

test("adds the OpenCode session header", async () => {
  const hooks = await plugin(context())
  const output = { headers: {} }

  await hooks["chat.headers"]({ sessionID: "session-test" }, output)

  assert.equal(output.headers["x-context-guru-session"], "session-test")
})

test("leaves unsupported providers unchanged", async () => {
  const hooks = await plugin(context())
  const config = {
    model: "custom/model",
    provider: { custom: { options: { baseURL: "https://example.test" } } },
  }

  await hooks.config(config)

  assert.deepEqual(config, {
    model: "custom/model",
    provider: { custom: { options: { baseURL: "https://example.test" } } },
  })
})
