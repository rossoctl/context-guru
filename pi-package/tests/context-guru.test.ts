// Unit tests for the extension's pure helpers. Run: node --test tests/*.test.ts (Node 22.18+ strips the types).
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { createServer } from "node:http";
import type { AddressInfo } from "node:net";

import {
  checkAtStart, downNotice, downReason, endKind, forChatGPTSignIn, isRouted, needsStartCheck, parseArgs, parseFacts, proxyAnswers,
  readInstall, START_CHECK_TIMEOUT_MS, StartChecks, type StartCheck,
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

test("a session start checks the proxy only when installed and some model is routed", () => {
  assert.equal(needsStartCheck(undefined, ["http://127.0.0.1:8800/anthropic"]), false);
  assert.equal(needsStartCheck({ ...install, port: undefined }, ["http://127.0.0.1:8800/anthropic"]), false);
  assert.equal(needsStartCheck(install, []), false);
  assert.equal(needsStartCheck(install, [undefined, "https://api.anthropic.com"]), false);
  // The session's model need not be routed: any routed provider is enough.
  assert.equal(needsStartCheck(install, [undefined, "https://api.x.ai/v1", "http://127.0.0.1:8800/openai/v1"]), true);
});

/** A StartCheck whose answers come from a list, and that records what it did. */
function fake(answers: boolean[], over: Partial<StartCheck> = {}) {
  const log = { checks: 0, restarts: 0, notices: [] as string[] };
  const check: StartCheck = {
    port: 8800,
    hatch: "/state/context-guru-reset",
    answers: async () => answers[log.checks++] ?? false,
    restart: async () => {
      log.restarts++;
      return { result: "down", service: "restarted" };
    },
    active: () => true,
    notify: (text) => log.notices.push(text),
    ...over,
  };
  return { check, log };
}

test("a proxy that answers is not restarted and nothing is said", async () => {
  const { check, log } = fake([true]);
  assert.equal(await checkAtStart(check), "up");
  assert.deepEqual(log, { checks: 1, restarts: 0, notices: [] });
});

test("a proxy that comes back after one restart gives no notice", async () => {
  const { check, log } = fake([false, true]);
  assert.equal(await checkAtStart(check), "restarted");
  assert.deepEqual(log, { checks: 2, restarts: 1, notices: [] });
});

test("a proxy still down after one restart gives exactly one notice, and no loop", async () => {
  const { check, log } = fake([false, false, false, false]);
  assert.equal(await checkAtStart(check), "down");
  assert.equal(log.checks, 2);
  assert.equal(log.restarts, 1);
  assert.equal(log.notices.length, 1);
  assert.match(log.notices[0], /port 8800 does not answer: one restart did not help\./);
  assert.match(log.notices[0], /\/context-guru status/);
  assert.match(log.notices[0], /\/state\/context-guru-reset/);
});

test("the notice names the reset command the controller reports, and a failed restart still warns", async () => {
  const reported = fake([false, false], { restart: async () => ({ result: "down", reset_hatch: "/x/context-guru-reset" }) });
  await checkAtStart(reported.check);
  assert.match(reported.log.notices[0], /run \/x\/context-guru-reset from a shell/);
  const thrown = fake([false, false], { restart: async () => { throw new Error("stale"); } });
  assert.equal(await checkAtStart(thrown.check), "down");
  assert.match(thrown.log.notices[0], /\/state\/context-guru-reset/);
});

test("a session that ended during the check gets no restart and no notice", async () => {
  let active = true;
  const early = fake([false], { active: () => false });
  assert.equal(await checkAtStart(early.check), "ended");
  assert.deepEqual(early.log, { checks: 1, restarts: 0, notices: [] });
  const late = fake([false, false], { restart: async () => { active = false; return {}; }, active: () => active });
  assert.equal(await checkAtStart(late.check), "ended");
  assert.deepEqual(late.log.notices, []);
});

test("the notice text is one line", () => {
  assert.equal(downNotice(8800, { service: "restarted" }, "/h").includes("\n"), false);
});

test("the notice names the real reason the proxy is still down", () => {
  assert.equal(downReason({ result: "down", service: "restarted" }), "one restart did not help");
  assert.match(downReason({ result: "down", service: "no_service" }), /service file is missing, so nothing was restarted.*\/context-guru setup/);
  assert.match(downReason({ result: "down", service: "not_owned" }), /not written by context-guru/);
  assert.match(downReason({ result: "down", service: "unsupported" }), /no user service/);
  assert.match(downReason({ result: "down", service: "failed" }), /restart command failed/);
  assert.match(downReason({ result: "not_installed" }), /no install record/);
  assert.match(downReason({ result: "already_up" }), /answered the restart check/);
  assert.equal(downReason({}), "the restart did not run");
  assert.match(downNotice(8800, { service: "no_service" }), /port 8800 does not answer: its service file is missing/);
});

test("the notice names the reset command only when there is one", () => {
  assert.match(downNotice(8800, {}, "/h/context-guru-reset"), /run \/h\/context-guru-reset from a shell/);
  assert.doesNotMatch(downNotice(8800, {}), /from a shell|undefined/);
  const none = fake([false, false], { hatch: undefined, restart: async () => ({ result: "down", service: "restarted" }) });
  return checkAtStart(none.check).then(() => assert.doesNotMatch(none.log.notices[0], /from a shell|undefined/));
});

test("a session's end stops only that session's check", () => {
  const checks = new StartChecks();
  const a = checks.begin("session-a");
  const b = checks.begin("session-b");
  checks.end("session-a");
  assert.equal(a.active(), false);
  assert.equal(a.signal.aborted, true);
  assert.equal(b.active(), true, "the other session's check keeps running");
  assert.equal(b.signal.aborted, false);
  checks.end("session-unknown"); // nothing to stop
  assert.equal(b.active(), true);
});

test("a session that starts again replaces its own check, and a finished check is forgotten", () => {
  const checks = new StartChecks();
  const first = checks.begin("s");
  const second = checks.begin("s");
  assert.equal(first.active(), false);
  assert.equal(second.active(), true);
  checks.done("s", first.signal); // a late finish of the old check does not drop the new one
  checks.end("s");
  assert.equal(second.active(), false);
  const third = checks.begin("s");
  checks.done("s", third.signal);
  checks.end("s"); // already forgotten: nothing aborts
  assert.equal(third.active(), true);
});

test("the health check is a GET to localhost /healthz, true only on 200", async () => {
  const seen: string[] = [];
  let status = 200;
  const server = createServer((req, res) => {
    seen.push(`${req.method} ${req.url}`);
    res.statusCode = status;
    res.end("ok");
  });
  await new Promise<void>((done) => server.listen(0, "127.0.0.1", done));
  const port = (server.address() as AddressInfo).port;
  try {
    assert.equal(await proxyAnswers(port), true);
    status = 503;
    assert.equal(await proxyAnswers(port), false);
    assert.deepEqual(seen, ["GET /healthz", "GET /healthz"]);
  } finally {
    server.close();
  }
  // Nothing listens there now.
  assert.equal(await proxyAnswers(port, 1000), false);
});

test("a proxy that accepts but never answers fails the check at the time limit", async () => {
  const server = createServer(() => undefined);
  await new Promise<void>((done) => server.listen(0, "127.0.0.1", done));
  const port = (server.address() as AddressInfo).port;
  try {
    const start = Date.now();
    assert.equal(await proxyAnswers(port, 200), false);
    assert.ok(Date.now() - start < 2000);
  } finally {
    server.closeAllConnections();
    server.close();
  }
});

test("each check has a time limit, 5 seconds by default", async () => {
  assert.equal(START_CHECK_TIMEOUT_MS, 5000);
  let signal: AbortSignal | undefined;
  const fetcher = (async (_url: string | URL | Request, init?: RequestInit) => {
    signal = init?.signal ?? undefined;
    return new Response("ok");
  }) as typeof fetch;
  assert.equal(await proxyAnswers(8800, undefined, fetcher), true);
  assert.ok(signal, "the request carries a time limit");
});

test("a stopped check ends at once, before its time limit", async () => {
  const server = createServer(() => undefined);
  await new Promise<void>((done) => server.listen(0, "127.0.0.1", done));
  const port = (server.address() as AddressInfo).port;
  try {
    const stop = new AbortController();
    const start = Date.now();
    const pending = proxyAnswers(port, 5000, fetch, stop.signal);
    setTimeout(() => stop.abort(), 50);
    assert.equal(await pending, false);
    assert.ok(Date.now() - start < 1000);
  } finally {
    server.closeAllConnections();
    server.close();
  }
});
