// Unit tests for the extension's pure helpers. Run: node --test tests/*.test.ts (Node 22.18+ strips the types).
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import {
  CHAIN, conflictPrompt, endKind, forChatGPTSignIn, isRouted, KEEP, parseArgs, parseFacts, readInstall, REPLACE,
} from "../extensions/context-guru.ts";

const install = {
  port: 8800,
  base_urls: { anthropic: "http://127.0.0.1:8800/anthropic", openai: "http://127.0.0.1:8800/openai/v1" },
  cache_strategy: "auto",
};

test("a model is routed only when its baseUrl is one setup wrote", () => {
  assert.equal(isRouted(install, "http://127.0.0.1:8800/anthropic"), true);
  assert.equal(isRouted(install, "http://127.0.0.1:8800/anthropic/"), true);
  assert.equal(isRouted(install, "https://api.anthropic.com"), false);
  assert.equal(isRouted(install, "http://127.0.0.1:8801/anthropic"), false);
  assert.equal(isRouted(undefined, "http://127.0.0.1:8800/anthropic"), false);
  assert.equal(isRouted(install, undefined), false);
});

test("an OpenAI model is routed by its own URL, and OpenAI's own URL is not", () => {
  assert.equal(isRouted(install, "http://127.0.0.1:8800/openai/v1"), true);
  assert.equal(isRouted(install, "http://127.0.0.1:8800/openai/v1/"), true);
  assert.equal(isRouted(install, "https://api.openai.com/v1"), false);
  assert.equal(isRouted(install, "http://127.0.0.1:8800/openai"), false);
  // An install from the Anthropic-only version routes no OpenAI model.
  assert.equal(isRouted({ ...install, base_urls: { anthropic: install.base_urls.anthropic } }, "http://127.0.0.1:8800/openai/v1"), false);
});

test("a ChatGPT sign-in request loses the fields ChatGPT rejects, and keeps everything else", () => {
  const payload = {
    model: "gpt-5.6-luna", input: [{ role: "user" }], stream: true, prompt_cache_key: "s1", store: false,
    prompt_cache_retention: "24h", prompt_cache_options: { ttl: "30m" }, max_output_tokens: 1000, temperature: 0.2,
  };
  const out = forChatGPTSignIn(payload);
  assert.deepEqual(out, { model: "gpt-5.6-luna", input: [{ role: "user" }], stream: true, prompt_cache_key: "s1", store: false });
  assert.equal(payload.max_output_tokens, 1000, "the payload Pi built is not changed");
  assert.equal(forChatGPTSignIn({ model: "gpt-5.6-luna", input: [] }), undefined);
  assert.equal(forChatGPTSignIn(undefined), undefined);
  assert.equal(forChatGPTSignIn([1, 2]), undefined);
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

test("a bare /context-guru runs status, and the first two words are the subcommand and its value", () => {
  assert.deepEqual(parseArgs(""), { sub: "status", value: "" });
  assert.deepEqual(parseArgs("   "), { sub: "status", value: "" });
  assert.deepEqual(parseArgs("preset"), { sub: "preset", value: "" });
  assert.deepEqual(parseArgs("  preset   medium "), { sub: "preset", value: "medium" });
});

test("a conflict is one question: chain, replace, or change nothing", () => {
  const p = conflictPrompt({
    result: "needs_decision", conflicts: "anthropic", existing_base_url: '{"anthropic":"https://gw.example"}',
    consent_question_chain: "CHAIN-TEXT", consent_question_replace: "REPLACE-TEXT",
  });
  assert.deepEqual(p.options, [CHAIN, REPLACE, KEEP]);
  assert.deepEqual(p.answers, { [CHAIN]: "chain", [REPLACE]: "replace" });
  assert.equal(p.answers[KEEP], undefined, "no answer means nothing changes");
  assert.match(p.title, /anthropic somewhere else: \{"anthropic":"https:\/\/gw.example"\}/);
  assert.match(p.title, /CHAIN-TEXT/);
  assert.match(p.title, /REPLACE-TEXT/);
});

test("a URL the proxy cannot reproduce offers no chain, and says why", () => {
  const p = conflictPrompt({ conflicts: "openai", existing_base_url: "x", chain_refused: "openai:openai_path_not_v1",
    consent_question_replace: "REPLACE-TEXT" });
  assert.deepEqual(p.options, [REPLACE, KEEP]);
  assert.match(p.title, /Chain is not offered.*openai_path_not_v1/);
});
