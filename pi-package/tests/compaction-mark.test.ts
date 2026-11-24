// The request-kind mark on Pi's own summary request. Kept in its own file: the install record is read from
// XDG_STATE_HOME when the extension loads, so the variable is set before the extension is imported.
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

const state = mkdtempSync(join(tmpdir(), "cg-pi-mark-"));
mkdirSync(join(state, "context-guru-pi"), { recursive: true });
writeFileSync(join(state, "context-guru-pi", "install.json"),
  JSON.stringify({ port: 1, base_urls: { anthropic: "http://127.0.0.1:1/anthropic" } }));
process.env.XDG_STATE_HOME = state;
const { default: contextGuru } = await import("../extensions/context-guru.ts");

function loaded() {
  const handlers: Record<string, Array<(event: unknown, ctx: unknown) => unknown>> = {};
  const pi = {
    on: (name: string, fn: (event: unknown, ctx: unknown) => unknown) => (handlers[name] ??= []).push(fn),
    registerCommand: () => undefined,
    exec: async () => ({ stdout: "", stderr: "", code: 0, killed: false, truncated: false }),
  };
  contextGuru(pi as never);
  const ctx = {
    model: { baseUrl: "http://127.0.0.1:1/anthropic" },
    sessionManager: { getSessionId: () => "s1" },
    hasUI: false,
  };
  const emit = (name: string, event: unknown = {}) => (handlers[name] ?? []).forEach((fn) => fn(event, ctx));
  const headers = () => {
    const event = { headers: {} as Record<string, string> };
    emit("before_provider_headers", event);
    return event.headers;
  };
  return { emit, headers };
}

test("the summary request carries the compaction mark, and no other request does", () => {
  const g = globalThis as Record<symbol, unknown>;
  delete g[Symbol.for("rossoctl.context-guru.pi.loaded")];
  const pi = loaded();
  assert.equal(pi.headers()["x-context-guru-request-kind"], undefined);
  assert.equal(pi.headers()["x-context-guru-session"], "s1");
  pi.emit("session_before_compact");
  assert.equal(pi.headers()["x-context-guru-request-kind"], "compaction");
  pi.emit("session_compact");
  assert.equal(pi.headers()["x-context-guru-request-kind"], undefined);
  pi.emit("session_before_compact");
  pi.emit("session_compact_failed");
  assert.equal(pi.headers()["x-context-guru-request-kind"], undefined);
});
