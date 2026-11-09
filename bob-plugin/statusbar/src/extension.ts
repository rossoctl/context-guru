// The VS Code part: one status bar item, a timer, and the click menu.
// All logic is in model.ts and client.ts. This file only wires them to the editor.

import * as os from 'node:os'
import * as vscode from 'vscode'
import { poll, stateDir } from './client'
import { DEFAULT_POLL_S, LOOPBACK, loopbackUrl, MENU, nextPollMs, renderView, Snapshot } from './model'

const MENU_COMMAND = 'contextGuru.bob.menu'

export function activate(context: vscode.ExtensionContext): void {
  const item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 100)
  item.command = MENU_COMMAND
  item.text = 'CG …'
  item.show()

  let snap: Snapshot | undefined
  let pollTimer: NodeJS.Timeout | undefined
  let stopped = false

  let polled = false
  const draw = () => {
    if (!polled) return
    const view = renderView(snap, Date.now())
    item.text = view.text
    item.tooltip = view.tooltip
  }

  const pollSeconds = () => vscode.workspace.getConfiguration('contextGuru.bob')
    .get<number>('pollSeconds', DEFAULT_POLL_S)

  const tick = async () => {
    try {
      snap = await poll(stateDir(process.env, os.homedir()))
    } catch {
      snap = undefined
    }
    if (stopped) return
    polled = true
    draw()
    pollTimer = setTimeout(tick, nextPollMs(snap?.state ?? 'down', pollSeconds()))
  }

  // The clock counts down every second from the last poll, with no call to the proxy.
  const clockTimer = setInterval(draw, 1000)

  context.subscriptions.push(item, vscode.commands.registerCommand(MENU_COMMAND, () => showMenu(snap)),
    { dispose: () => {
      stopped = true
      clearInterval(clockTimer)
      if (pollTimer) clearTimeout(pollTimer)
    } })
  void tick()
}

async function showMenu(snap: Snapshot | undefined): Promise<void> {
  const pick = await vscode.window.showQuickPick(
    MENU.map(entry => ({ label: entry.label, detail: entry.detail, entry })),
    { placeHolder: 'context-guru' })
  if (!pick) return
  if (pick.entry.dashboard) {
    if (!snap?.port) {
      void vscode.window.showInformationMessage('context-guru is not set up for Bob.')
      return
    }
    void vscode.env.openExternal(vscode.Uri.parse(loopbackUrl(LOOPBACK, snap.port, '/dashboard/')))
    return
  }
  if (pick.entry.command) {
    const terminal = vscode.window.createTerminal('context-guru')
    terminal.show()
    terminal.sendText(pick.entry.command)
  }
}

export function deactivate(): void {
  // The timers are in context.subscriptions; VS Code disposes them.
}
