import assert from "node:assert/strict"
import test from "node:test"
import { mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

process.env.CONTEXT_GURU_STATE = mkdtempSync(join(tmpdir(), "context-guru-tui-test-"))
process.env.CONTEXT_GURU_PORT_START = "9900"

import plugin, { stateDirectory } from "../index.js"
import tuiModule, { pickAndSwitch, registerPickerCommands } from "../tui.js"
import { stubProxyBinary, waitUntilUnhealthy } from "./helpers.mjs"

const server = (directory) => ({ directory, client: { app: { log: async () => {} } } })
const settle = () => new Promise((resolve) => setTimeout(resolve, 600))

// A fake api.ui that records each dialog and lets the test answer it.
function fakeApi(directory, { keymap = true, legacy = false } = {}) {
  const shown = []
  const toasts = []
  const registered = { layers: [], legacy: [] }
  const api = {
    state: { path: { directory } },
    ui: {
      DialogSelect: (props) => ({ kind: "select", props }),
      DialogConfirm: (props) => ({ kind: "confirm", props }),
      toast: (input) => toasts.push(input),
      dialog: { replace: (render) => shown.push(render()), clear: () => shown.push({ kind: "cleared" }) },
    },
  }
  if (keymap) api.keymap = { registerLayer: (layer) => { registered.layers.push(layer); return () => {} } }
  if (legacy) api.command = { register: (cb) => { registered.legacy.push(cb); return () => {} } }
  return { api, shown, toasts, registered }
}

async function setup(directory) {
  process.env.CONTEXT_GURU_BIN = stubProxyBinary()
  const hooks = await plugin.server(server(directory))
  await hooks.config({ provider: { anthropic: { options: { baseURL: "https://api.anthropic.com" } } } })
  return stateDirectory(directory)
}

test("the TUI module exports id and tui, and no server", () => {
  assert.equal(typeof tuiModule.tui, "function")
  assert.equal(typeof tuiModule.id, "string")
  assert.equal(tuiModule.server, undefined)
})

test("registers both slash commands through keymap.registerLayer", async () => {
  const { api, registered } = fakeApi("/tmp/context-guru-tui-reg")
  await tuiModule.tui(api)
  assert.equal(registered.layers.length, 1)
  const commands = registered.layers[0].commands
  assert.deepEqual(commands.map((command) => command.slash.name).sort(), ["context-guru-preset", "context-guru-strategy"])
  for (const command of commands) assert.equal(typeof command.run, "function")
})

test("falls back to the legacy api.command when there is no keymap, and to nothing when neither exists", () => {
  const legacy = fakeApi("/tmp/x", { keymap: false, legacy: true })
  assert.equal(registerPickerCommands(legacy.api, "/tmp/x"), "legacy")
  const items = legacy.registered.legacy[0]()
  assert.deepEqual(items.map((item) => item.slash.name).sort(), ["context-guru-preset", "context-guru-strategy"])
  const none = fakeApi("/tmp/x", { keymap: false })
  assert.equal(registerPickerCommands(none.api, "/tmp/x"), "none")
})

test("native preset flow: select, confirm switch, confirm restart, then the proxy restarts", async () => {
  const directory = "/tmp/context-guru-tui-flow"
  const state = await setup(directory)
  const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
  try {
    const { api, shown, toasts } = fakeApi(directory)
    await pickAndSwitch(api, directory, "preset")
    const select = shown.at(-1)
    assert.equal(select.kind, "select")
    assert.deepEqual(select.props.options.map((option) => option.value), ["off", "conservative", "medium", "high", "xhigh", "ultrahigh"])
    assert.equal(select.props.current, "off")
    assert.ok(select.props.options.find((option) => option.value === "off").title.includes("(current)"))
    assert.ok(select.props.options.every((option) => option.description.length > 0))

    select.props.onSelect({ value: "medium" })
    const confirmSwitch = shown.at(-1)
    assert.equal(confirmSwitch.kind, "confirm")
    // Nothing is saved before the first yes.
    assert.ok(!(await import("node:fs")).existsSync(join(state, "settings.env")))

    await confirmSwitch.props.onConfirm()
    assert.match(readFileSync(join(state, "settings.env"), "utf8"), /preset=medium/)
    const confirmRestart = shown.at(-1)
    assert.equal(confirmRestart.kind, "confirm")
    assert.match(confirmRestart.props.message, /one cache rewrite/)
    // The proxy keeps running with the old setting until the second yes.
    assert.equal(JSON.parse(readFileSync(join(state, "install.json"), "utf8")).pid, before.pid)

    await confirmRestart.props.onConfirm()
    assert.match(readFileSync(join(state, "proxy.yaml"), "utf8"), /preset: medium/)
    assert.notEqual(Number(readFileSync(join(state, "proxy.pid"), "utf8")), before.pid)
    assert.equal(toasts.at(-1).variant, "success")
    process.kill(Number(readFileSync(join(state, "proxy.pid"), "utf8")), "SIGTERM")
    await waitUntilUnhealthy(before.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("cancelling the first confirm saves nothing, and cancelling the restart saves but keeps the proxy", async () => {
  const directory = "/tmp/context-guru-tui-cancel"
  const state = await setup(directory)
  const before = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
  try {
    const { api, shown, toasts } = fakeApi(directory)
    await pickAndSwitch(api, directory, "strategy")
    const select = shown.at(-1)
    assert.deepEqual(select.props.options.map((option) => option.value), ["none", "auto", "1-ping", "2-ping", "1-hour-head"])
    assert.equal(select.props.current, "auto")

    select.props.onSelect({ value: "2-ping" })
    const first = shown.at(-1)
    assert.match(first.props.message, /spends your own credential/)
    first.props.onCancel()
    assert.equal(shown.at(-1).kind, "cleared")
    await settle()
    assert.ok(!(await import("node:fs")).existsSync(join(state, "settings.env")))

    select.props.onSelect({ value: "none" })
    const second = shown.at(-1)
    assert.ok(!second.props.message.includes("spends your own credential"))
    await second.props.onConfirm()
    shown.at(-1).props.onCancel()
    await settle()
    assert.match(readFileSync(join(state, "settings.env"), "utf8"), /strategy=none/)
    assert.equal(JSON.parse(readFileSync(join(state, "install.json"), "utf8")).pid, before.pid)
    assert.match(toasts.at(-1).message, /next OpenCode start/)
    process.kill(before.pid, "SIGTERM")
    await waitUntilUnhealthy(before.port)
  } finally {
    delete process.env.CONTEXT_GURU_BIN
  }
})

test("choosing the current value shows no confirm", async () => {
  const directory = "/tmp/context-guru-tui-same"
  await setup(directory).then(async (state) => {
    const record = JSON.parse(readFileSync(join(state, "install.json"), "utf8"))
    const { api, shown, toasts } = fakeApi(directory)
    await pickAndSwitch(api, directory, "preset")
    shown.at(-1).props.onSelect({ value: "off" })
    assert.equal(shown.at(-1).kind, "cleared")
    assert.match(toasts.at(-1).message, /already saved/)
    process.kill(record.pid, "SIGTERM")
    await waitUntilUnhealthy(record.port)
  })
  delete process.env.CONTEXT_GURU_BIN
})

test("without an installed script the picker shows a toast and no dialog", async () => {
  const { api, shown, toasts } = fakeApi("/tmp/context-guru-tui-missing")
  await pickAndSwitch(api, "/tmp/context-guru-tui-missing", "preset")
  assert.equal(shown.length, 0)
  assert.equal(toasts.at(-1).variant, "error")
})

test("the chat fallback still registers both pickers and runs the same script", async () => {
  const hooks = await plugin.server(server("/tmp/context-guru-tui-chat"))
  const config = { provider: { custom: { options: { baseURL: "https://example.test" } } } }
  await hooks.config(config)
  for (const name of ["context-guru-preset", "context-guru-strategy"]) {
    const template = config.command[name].template
    assert.match(template, new RegExp(`context-guru-${name.split("-").pop()}`))
    assert.match(template, /set NAME --restart/)
    assert.match(template, /Never restart without the yes/)
  }
})
