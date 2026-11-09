// Native pickers for the OpenCode TUI. OpenCode loads a TUI plugin from tui.json, as a module
// with a "tui" export; that module cannot also export "server", so this is a second file.
// It only draws dialogs. Every switch runs the same "set NAME [--restart]" script as the chat
// commands, through runCommandScript, so the native path and the chat path cannot differ.
import { parsePickerOutput, runCommandScript, stateDirectory } from "./index.js"

const KINDS = {
  preset: {
    script: "context-guru-preset",
    slash: "context-guru-preset",
    title: "Context Guru: preset",
    what: "preset",
  },
  strategy: {
    script: "context-guru-strategy",
    slash: "context-guru-strategy",
    title: "Context Guru: cache strategy",
    what: "cache strategy",
  },
}

const RESTART_WARNING = "A restart applies it now. It costs one cache rewrite in every open session of this project."

function toast(api, variant, message) {
  try { api.ui.toast({ variant, title: "Context Guru", message }) } catch {}
}

// Select list, then a confirm to switch (saves the choice), then a confirm to restart.
export async function pickAndSwitch(api, directory, kind) {
  const spec = KINDS[kind]
  const state = stateDirectory(directory)
  const shown = await runCommandScript(state, spec.script)
  const { facts, choices } = parsePickerOutput(shown.output)
  if (shown.status !== 0 || !choices.length) {
    toast(api, "error", `No ${spec.what} script found. Start OpenCode once with the Context Guru plugin enabled.`)
    return
  }
  const current = facts.saved
  const options = choices.map((choice) => ({
    title: choice.name === current ? `${choice.name} (current)` : choice.name,
    value: choice.name,
    description: choice.help,
  }))
  const gap = facts.gap ? ` ${facts.gap}.` : ""
  api.ui.dialog.replace(() => api.ui.DialogSelect({
    title: `${spec.title} (saved: ${current}).${gap}`.trim(),
    current,
    options,
    onSelect: (option) => {
      if (option.value === current) {
        api.ui.dialog.clear()
        toast(api, "info", `${option.value} is already saved.`)
        return
      }
      confirmSwitch(api, state, kind, option.value)
    },
  }))
}

function confirmSwitch(api, state, kind, name) {
  const spec = KINDS[kind]
  const spends = kind === "strategy" && ["auto", "1-ping", "2-ping"].includes(name)
  const message = `Save ${spec.what} "${name}" for this project?` + (spends ? " Pings spend your own credential while nobody is at the keyboard, on every session routed through this proxy." : "")
  api.ui.dialog.replace(() => api.ui.DialogConfirm({
    title: `Switch ${spec.what}?`,
    message,
    onCancel: () => api.ui.dialog.clear(),
    onConfirm: async () => {
      const saved = await runCommandScript(state, spec.script, ["set", name])
      if (saved.status !== 0) {
        api.ui.dialog.clear()
        toast(api, "error", `Could not save ${name}: ${saved.output.trim().split("\n").slice(-2).join(" ")}`)
        return
      }
      confirmRestart(api, state, kind, name)
    },
  }))
}

function confirmRestart(api, state, kind, name) {
  const spec = KINDS[kind]
  api.ui.dialog.replace(() => api.ui.DialogConfirm({
    title: "Restart the proxy now?",
    message: `Saved ${spec.what} "${name}". ${RESTART_WARNING} If you say no, it applies at the next OpenCode start.`,
    onCancel: () => {
      api.ui.dialog.clear()
      toast(api, "info", `Saved. ${name} applies at the next OpenCode start.`)
    },
    onConfirm: async () => {
      api.ui.dialog.clear()
      const restarted = await runCommandScript(state, spec.script, ["set", name, "--restart"])
      const ok = restarted.status === 0 && /result=restarted/.test(restarted.output)
      toast(api, ok ? "success" : "error", ok ? `Proxy restarted with ${spec.what} "${name}".` : `Restart not applied. See proxy.log in ${state}`)
    },
  }))
}

// keymap.registerLayer is the current API. api.command.register is deprecated and is the
// fallback for a host that has no keymap. With neither, the chat commands from the server
// plugin stay the only path.
export function registerPickerCommands(api, directory) {
  const entries = Object.entries(KINDS)
  if (typeof api.keymap?.registerLayer === "function") {
    api.keymap.registerLayer({
      commands: entries.map(([kind, spec]) => ({
        name: `context-guru.${kind}`,
        title: spec.title,
        category: "Context Guru",
        slash: { name: spec.slash },
        run: () => { pickAndSwitch(api, directory, kind).catch(() => toast(api, "error", "Picker failed.")) },
      })),
    })
    return "keymap"
  }
  if (typeof api.command?.register === "function") {
    api.command.register(() => entries.map(([kind, spec]) => ({
      title: spec.title,
      value: `context-guru.${kind}`,
      category: "Context Guru",
      slash: { name: spec.slash },
      onSelect: () => { pickAndSwitch(api, directory, kind).catch(() => toast(api, "error", "Picker failed.")) },
    })))
    return "legacy"
  }
  return "none"
}

async function tui(api) {
  const directory = api.state?.path?.directory
  if (!directory) return
  registerPickerCommands(api, directory)
}

export default {
  id: "opencode-context-guru-tui",
  tui,
}
