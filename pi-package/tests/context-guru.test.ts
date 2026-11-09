// Unit tests for the extension's pure helpers. Run: node --test tests/*.test.ts (Node 22.18+ strips the types).
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { endKind, isRouted, parseFacts, readInstall } from "../extensions/context-guru.ts";

const install = { port: 8800, base_urls: { anthropic: "http://127.0.0.1:8800/anthropic" }, cache_strategy: "auto" };

test("a model is routed only when its baseUrl is one setup wrote", () => {
  assert.equal(isRouted(install, "http://127.0.0.1:8800/anthropic"), true);
  assert.equal(isRouted(install, "http://127.0.0.1:8800/anthropic/"), true);
  assert.equal(isRouted(install, "https://api.anthropic.com"), false);
  assert.equal(isRouted(install, "http://127.0.0.1:8801/anthropic"), false);
  assert.equal(isRouted(undefined, "http://127.0.0.1:8800/anthropic"), false);
  assert.equal(isRouted(install, undefined), false);
});

test("facts parse one key per line and keep '=' inside values", () => {
  assert.deepEqual(parseFacts("result=routed\nconsent_question=a=b\nnoise\n"), { result: "routed", consent_question: "a=b" });
});

test("a reload keeps the session, so it sends no end signal", () => {
  assert.equal(endKind("reload"), undefined);
  assert.equal(endKind("quit"), "prompt_input_exit");
  assert.equal(endKind("new"), "clear");
  assert.equal(endKind("resume"), "resume");
  assert.equal(endKind("fork"), "other");
});

test("the install record is read from disk, and a missing or broken one is undefined", () => {
  const dir = mkdtempSync(join(tmpdir(), "cg-pi-"));
  assert.equal(readInstall(dir), undefined);
  writeFileSync(join(dir, "install.json"), "{broken");
  assert.equal(readInstall(dir), undefined);
  writeFileSync(join(dir, "install.json"), JSON.stringify(install));
  assert.deepEqual(readInstall(dir), install);
});
