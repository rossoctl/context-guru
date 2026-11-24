// The #1341 wiring: the real extension, loaded with its state folder in a temporary place, driven through
// its own handlers. Its own file, because the extension reads XDG_STATE_HOME when it loads.
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

const stateHome = mkdtempSync(join(tmpdir(), "cg-recover-"));
process.env.XDG_STATE_HOME = stateHome;
const stateDir = join(stateHome, "context-guru-pi");
const record = join(stateDir, "install.json");
const PROXY = "http://127.0.0.1:8800/anthropic";
const GATEWAY = "https://gw.example.com";
const { default: contextGuru, LOADED } = await import("../extensions/context-guru.ts");

function writeRecord() {
  mkdirSync(stateDir, { recursive: true });
  writeFileSync(record, JSON.stringify({ port: 8800, base_urls: { anthropic: PROXY } }));
}

/** A Pi with one session. `onDisk` is the baseUrl that models.json names now. Handlers are awaited. */
function world(startUrl: string, over: { setModel?: (m: unknown) => Promise<boolean> } = {}) {
  const handlers: Record<string, Array<(event: unknown, ctx: unknown) => unknown>> = {};
  const commands: Record<string, { handler: (args: string, ctx: unknown) => Promise<unknown> }> = {};
  const seen = { set: [] as string[], models: [] as Array<{ provider: string; id: string }>, notices: [] as string[],
    onDisk: GATEWAY };
  const ctx = {
    hasUI: true,
    model: { provider: "anthropic", id: "aws/claude-opus-5-5", baseUrl: startUrl } as { provider: string; id: string; baseUrl: string },
    ui: { notify: (text: string) => seen.notices.push(text), confirm: async () => true },
    modelRegistry: {
      getAll: () => [],
      refresh: async () => undefined,
      find: (provider: string, id: string) => ({ provider, id, baseUrl: seen.onDisk }),
    },
    sessionManager: { getSessionId: () => "s1" },
  };
  const pi = {
    on: (name: string, fn: (event: unknown, c: unknown) => unknown) => (handlers[name] ??= []).push(fn),
    registerCommand: (name: string, spec: { handler: (args: string, c: unknown) => Promise<unknown> }) => {
      commands[name] = spec;
    },
    exec: async (_cmd: string, args: string[]) => ({
      stdout: args[1] === "uninstall" ? "models_restore=restored\nservice=stopped\nresult=removed\n" : "",
      stderr: "", code: 0, killed: false, truncated: false,
    }),
    setModel: over.setModel ?? (async (m: { provider: string; id: string; baseUrl: string }) => {
      seen.set.push(m.baseUrl);
      seen.models.push({ provider: m.provider, id: m.id });
      ctx.model = { ...ctx.model, baseUrl: m.baseUrl };
      return true;
    }),
  };
  delete (globalThis as Record<symbol, unknown>)[LOADED];
  contextGuru(pi as never);
  const emit = async (name: string, event: unknown = {}) => {
    const results = [];
    for (const fn of handlers[name] ?? []) results.push(await fn(event, ctx));
    return results;
  };
  return { ctx, seen, emit, command: (args: string) => commands["context-guru"].handler(args, ctx) };
}

test("a window routed at load switches to models.json's endpoint after the record is gone", async () => {
  writeRecord();
  const w = world(PROXY);
  rmSync(record);
  assert.deepEqual(await w.emit("input", { text: "hi" }), [{ action: "continue" }]);
  assert.deepEqual(w.seen.set, [GATEWAY]);
  // Exactly the same model: the same provider and id. Only the address is read again from models.json.
  assert.deepEqual(w.seen.models, [{ provider: "anthropic", id: "aws/claude-opus-5-5" }]);
  assert.equal(w.ctx.model.id, "aws/claude-opus-5-5");
  assert.equal(w.seen.notices.length, 1);
  assert.match(w.seen.notices[0], /context-guru was removed/);
});

test("a second prompt after the switch changes nothing and says nothing more", async () => {
  writeRecord();
  const w = world(PROXY);
  rmSync(record);
  await w.emit("input", { text: "one" });
  await w.emit("input", { text: "two" });
  await w.emit("before_agent_start", {});
  assert.deepEqual(w.seen.set, [GATEWAY]);
  assert.equal(w.seen.notices.length, 1);
});

test("an uninstall from this same window recovers on the next prompt", async () => {
  writeRecord();
  const w = world(PROXY);
  rmSync(record);         // what pi_plugin.py uninstall does
  await w.command("uninstall"); // the command reloads the record (now gone) in the extension
  await w.emit("input", { text: "hi" });
  assert.deepEqual(w.seen.set, [GATEWAY]);
});

test("a window that took the route only after it started (via /model) recovers too", async () => {
  rmSync(record, { force: true });
  const w = world(GATEWAY);          // started before context-guru was on
  writeRecord();                     // then context-guru was set up
  w.ctx.model = { ...w.ctx.model, baseUrl: PROXY }; // and /model picked up the route
  await w.emit("input", { text: "while routed" });
  assert.deepEqual(w.seen.set, []);  // still installed: nothing to do
  rmSync(record);                    // the uninstall
  await w.emit("input", { text: "after" });
  assert.deepEqual(w.seen.set, [GATEWAY]);
});

test("a setModel that throws leaves the prompt alive (fail open)", async () => {
  writeRecord();
  const w = world(PROXY, { setModel: async () => { throw new Error("No API key for anthropic/m"); } });
  rmSync(record);
  assert.deepEqual(await w.emit("input", { text: "hi" }), [{ action: "continue" }]);
  assert.deepEqual(await w.emit("before_agent_start", {}), [undefined]);
});

test("while installed, a window on the proxy is left alone", async () => {
  writeRecord();
  const w = world(PROXY);
  await w.emit("input", { text: "hi" });
  assert.deepEqual(w.seen.set, []);
  assert.deepEqual(w.seen.notices, []);
  rmSync(record);
});

test("a re-run that moved context-guru to another port moves this window there too", async () => {
  writeRecord();
  const w = world(PROXY);
  writeFileSync(record, JSON.stringify({ port: 8801, base_urls: { anthropic: "http://127.0.0.1:8801/anthropic" } }));
  w.seen.onDisk = "http://127.0.0.1:8801/anthropic";
  await w.emit("input", { text: "hi" });
  assert.deepEqual(w.seen.set, ["http://127.0.0.1:8801/anthropic"]);
  assert.match(w.seen.notices[0], /context-guru moved to 127\.0\.0\.1:8801/);
  rmSync(record);
});
