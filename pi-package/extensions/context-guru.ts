// context-guru for Pi.
//
// Routing lives in Pi's models.json (written by scripts/pi_plugin.py after the user says yes), and the
// proxy runs as a user service. This extension sends no request of its own when Pi sends a request.
// It only:
//   0. checks the proxy once when a session starts (a localhost /healthz, in the background, never
//      awaited by Pi), restarts the service once if it does not answer, and warns once if it is still down.
//      The session's end stops its own check, the restart child included;
//   1. tags each routed request with Pi's own session id (header x-context-guru-session), so the proxy
//      keeps one session across a compaction instead of splitting it in two;
//   2. stops Pi's own cache warming for routed models while the proxy's keep-alive is on, so the cache
//      is pinged by one owner, not two;
//   3. tells the proxy when a session ends or compacts (content-free: id, event, reason), so the
//      keep-alive stops pinging a cache no later request will read; sent after the fact, never awaited;
//   4. keeps Sign in with ChatGPT working on a routed `openai` model: Pi leaves out the request fields
//      ChatGPT tokens reject only when the baseUrl is api.openai.com, so the extension does it here;
//   5. adds /context-guru <setup|status|preset|cache-strategy|update|uninstall>.
import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import type { ExtensionAPI, ExtensionCommandContext, ExtensionContext } from "@earendil-works/pi-coding-agent";

const STATE_DIR = join(process.env.XDG_STATE_HOME || join(homedir(), ".local", "state"), "context-guru-pi");
const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), "..", "scripts", "pi_plugin.py");
const SESSION_HEADER = "x-context-guru-session";
const SESSION_ID = /^[A-Za-z0-9._:-]{1,128}$/;
const PRESETS = ["off", "conservative", "medium", "high", "xhigh", "ultrahigh"];
const STRATEGIES = ["auto", "1-ping", "2-ping", "none"];
const SUBCOMMANDS = ["setup", "status", "preset", "cache-strategy", "update", "uninstall"];
/** The time limit of each /healthz check at session start (the user's choice). */
export const START_CHECK_TIMEOUT_MS = 5000;

interface Install {
  port?: number;
  base_urls?: Record<string, string>;
  cache_strategy?: string;
  /** Providers chained behind the endpoint models.json named before setup: {provider: proxy upstream}. */
  upstreams?: Record<string, string>;
}

/** The install record pi_plugin.py writes. A local file read; undefined when not installed. */
export function readInstall(dir: string = STATE_DIR): Install | undefined {
  try {
    const value = JSON.parse(readFileSync(join(dir, "install.json"), "utf8"));
    return value && typeof value === "object" ? (value as Install) : undefined;
  } catch {
    return undefined;
  }
}

/** True when this model's requests go to our proxy: its baseUrl is one of the URLs setup wrote. */
export function isRouted(install: Install | undefined, baseUrl: string | undefined): boolean {
  if (!install?.base_urls || !baseUrl) return false;
  const want = baseUrl.replace(/\/+$/, "");
  return Object.values(install.base_urls).some((url) => url.replace(/\/+$/, "") === want);
}

/** True when a session start must check the proxy: context-guru is installed and at least one of these
 *  model URLs (the session's model, and every model Pi knows) goes to our proxy. */
export function needsStartCheck(install: Install | undefined, baseUrls: Array<string | undefined>): boolean {
  if (!install?.port) return false;
  return baseUrls.some((url) => isRouted(install, url));
}

/** One GET /healthz on localhost: true only on HTTP 200 within the time limit. No model call, no tokens.
 *  `stop` ends the check at once, so an open check does not keep a finished `pi -p` alive. */
export async function proxyAnswers(port: number, timeoutMs: number = START_CHECK_TIMEOUT_MS,
  fetcher: typeof fetch = fetch, stop?: AbortSignal): Promise<boolean> {
  const limit = AbortSignal.timeout(timeoutMs);
  try {
    const response = await fetcher(`http://127.0.0.1:${port}/healthz`,
      { signal: stop ? AbortSignal.any([limit, stop]) : limit });
    await response.arrayBuffer().catch(() => undefined);
    return response.status === 200;
  } catch {
    return false;
  }
}

/** Why the proxy is still down, from the facts `pi_plugin.py restart` printed (empty when it did not run). */
export function downReason(facts: Record<string, string>): string {
  if (facts.result === "not_installed") return "context-guru has no install record, so nothing was restarted";
  switch (facts.service) {
    case "restarted":
      return "one restart did not help";
    case "no_service":
      return "its service file is missing, so nothing was restarted. Run /context-guru setup to install it again";
    case "not_owned":
      return "its service file was not written by context-guru, so context-guru did not restart it";
    case "unsupported":
      return "this platform has no user service for it, so nothing was restarted";
    case "failed":
      return "the restart command failed";
  }
  if (facts.result === "already_up") return "it answered the restart check, but not the check after it";
  return "the restart did not run";
}

/** The one warning when the proxy is still down. Names the reset command only when there is one. */
export function downNotice(port: number, facts: Record<string, string>, hatch?: string): string {
  return `context-guru: the proxy on port ${port} does not answer: ${downReason(facts)}. ` +
    "Requests routed through it will fail. Run /context-guru status." +
    (hatch ? ` If Pi cannot talk, run ${hatch} from a shell.` : "");
}

/** The session-start checks that are still running, by Pi session id. `session_shutdown` stops only the
 *  check of the session that ended: it aborts its open /healthz and its restart, and keeps it quiet. */
export class StartChecks {
  private running = new Map<string, AbortController>();

  begin(session: string): { signal: AbortSignal; active: () => boolean } {
    this.end(session); // a session that starts again (a reload) replaces its own earlier check
    const stop = new AbortController();
    this.running.set(session, stop);
    return { signal: stop.signal, active: () => !stop.signal.aborted };
  }

  end(session: string): void {
    this.running.get(session)?.abort();
    this.running.delete(session);
  }

  /** Forget a check that finished, without stopping anything. */
  done(session: string, signal: AbortSignal): void {
    if (this.running.get(session)?.signal === signal) this.running.delete(session);
  }
}

export interface StartCheck {
  port: number;
  /** The reset command to name when the restart does not report one. Undefined when it does not exist. */
  hatch?: string;
  answers: () => Promise<boolean>;
  /** Runs `pi_plugin.py restart` once; returns its facts. */
  restart: () => Promise<Record<string, string>>;
  /** False once the session has ended: a check for an ended session restarts and says nothing. */
  active: () => boolean;
  notify: (text: string) => void;
}

/** The session-start check: one check, at most one restart, one more check, at most one notice. No loop. */
export async function checkAtStart(check: StartCheck): Promise<"up" | "restarted" | "down" | "ended"> {
  if (await check.answers()) return "up";
  if (!check.active()) return "ended";
  let facts: Record<string, string> = {};
  try {
    facts = await check.restart();
  } catch {
    // A failed restart is the same as a restart that did not help: check again, then warn.
  }
  if (await check.answers()) return "restarted";
  if (!check.active()) return "ended";
  check.notify(downNotice(check.port, facts, facts.reset_hatch || check.hatch));
  return "down";
}

// The fields Pi's openai-responses API leaves out for a Sign in with ChatGPT token (pi-ai 1.5.0,
// api/openai-responses.js `omitUnsupportedFields`). Pi decides that by `baseUrl === api.openai.com/v1`,
// which a routed model no longer has.
const CHATGPT_REJECTED = ["prompt_cache_retention", "prompt_cache_options", "max_output_tokens", "temperature"];

/** The payload without the fields a Sign in with ChatGPT token rejects, or undefined when it has none.
 *  A new object: the request Pi built stays as it was. */
export function forChatGPTSignIn(payload: unknown): Record<string, unknown> | undefined {
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return undefined;
  const body = payload as Record<string, unknown>;
  if (!CHATGPT_REJECTED.some((key) => key in body)) return undefined;
  const out = { ...body };
  for (const key of CHATGPT_REJECTED) delete out[key];
  return out;
}

export const CHAIN = "Chain (recommended): Pi -> context-guru -> the endpoint you use now";
export const REPLACE = "Replace: Pi -> context-guru -> the provider's public API";
export const KEEP = "Don't change anything";

/** The plan's `existing_base_url` ({provider: url} as JSON) in plain words: "anthropic: https://gw". */
export function conflictWords(json: string | undefined): string {
  try {
    const value = JSON.parse(json ?? "");
    if (value && typeof value === "object" && !Array.isArray(value)) {
      const words = Object.entries(value).map(([provider, url]) => `${provider}: ${String(url)}`);
      if (words.length) return words.join(", ");
    }
  } catch {
    // fall through: show the text as it came
  }
  return json ?? "unknown";
}

/** True when the ChatGPT sign-in field removal applies to a routed `openai` model. A chained `openai`
 *  went to another endpoint before setup, where Pi left those fields in, so it keeps them. */
export function stripsChatGPTFields(install: Install | undefined): boolean {
  return !install?.upstreams?.openai;
}

/** The one question for a models.json that already sends a provider elsewhere, built from the plan's
 *  paired answers. `answers` maps each option to its --on-conflict value; KEEP has none. Chain is
 *  offered only when the plan printed it (the proxy can reproduce that URL). */
export function conflictPrompt(f: Record<string, string>): { title: string; options: string[]; answers: Record<string, string> } {
  const options: string[] = [];
  const answers: Record<string, string> = {};
  const parts = [`models.json already sends a provider somewhere else: ${conflictWords(f.existing_base_url)}.`];
  if (f.consent_question_chain) {
    options.push(CHAIN);
    answers[CHAIN] = "chain";
    parts.push(`Chain (usually right): ${f.consent_question_chain}`);
  } else if (f.chain_refused) {
    parts.push(`Chain is not offered: context-guru cannot forward to that URL the way Pi does (${f.chain_refused}).`);
  }
  if (f.consent_question_replace) {
    options.push(REPLACE);
    answers[REPLACE] = "replace";
    parts.push(`Replace: ${f.consent_question_replace}`);
  }
  options.push(KEEP);
  return { title: parts.join("\n\n"), options, answers };
}

/** Parse the `key=value` lines pi_plugin.py prints. Later keys win. */
export function parseFacts(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const at = line.indexOf("=");
    if (at > 0) out[line.slice(0, at)] = line.slice(at + 1);
  }
  return out;
}

/** Split `/context-guru` arguments. A bare command (or only spaces) means `status`. */
export function parseArgs(args: string): { sub: string; value: string } {
  const [sub = "status", value = ""] = args.trim().split(/\s+/).filter(Boolean);
  return { sub, value };
}

/** Pi's shutdown reason as the proxy's SessionEnd kind. `reload` keeps the session, so it sends nothing. */
export function endKind(reason: string): string | undefined {
  return ({ quit: "prompt_input_exit", new: "clear", resume: "resume", fork: "other" } as Record<string, string>)[reason];
}

export default function contextGuru(pi: ExtensionAPI) {
  let install = readInstall();

  const routed = (ctx: ExtensionContext) => isRouted(install, ctx.model?.baseUrl);

  const signal = (ctx: ExtensionContext, event: string, kind: string) => {
    const port = install?.port;
    const session = ctx.sessionManager.getSessionId();
    if (!port || !routed(ctx) || !SESSION_ID.test(session)) return;
    // Fire and forget: a lost signal only means the keep-alive behaves as it would without signals.
    void fetch(`http://127.0.0.1:${port}/signal`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ session, event, kind }),
      signal: AbortSignal.timeout(1000),
    }).catch(() => undefined);
  };

  const run = async (args: string[], timeout = 180_000, signal?: AbortSignal) => {
    const result = await pi.exec("python3", [SCRIPT, ...args], { timeout, signal });
    return { ...result, facts: parseFacts(result.stdout) };
  };

  const checks = new StartChecks();

  pi.on("session_start", (_event, ctx) => {
    install = readInstall();
    const port = install?.port;
    let urls: Array<string | undefined> = [ctx.model?.baseUrl];
    try {
      urls = urls.concat(ctx.modelRegistry.getAll().map((model) => model.baseUrl));
    } catch {
      // Without the registry, the session's own model decides.
    }
    if (!port || !needsStartCheck(install, urls)) return;
    // Read now: ctx throws once the session is replaced, and the notice may come seconds later.
    const hasUI = ctx.hasUI;
    const session = ctx.sessionManager.getSessionId();
    const hatch = join(STATE_DIR, "context-guru-reset");
    // The session's end aborts both the /healthz and the restart child, so a finished `pi -p` exits at once.
    const current = checks.begin(session);
    // Not awaited: Pi awaits session_start handlers, and this check must not delay the session.
    void checkAtStart({
      port,
      hatch: existsSync(hatch) ? hatch : undefined,
      answers: () => proxyAnswers(port, START_CHECK_TIMEOUT_MS, fetch, current.signal),
      // 60 s: a restart can wait for another session's restart to finish (a lock in pi_plugin.py).
      restart: async () => (await run(["restart"], 60_000, current.signal)).facts,
      active: current.active,
      notify: (text) => {
        try {
          // Print and JSON modes have no UI. stdout carries the answer there, so the warning goes to stderr.
          if (hasUI) ctx.ui.notify(text, "warning");
          else console.error(text);
        } catch {
          // The session was replaced; its successor runs its own check.
        }
      },
    }).catch(() => undefined).finally(() => checks.done(session, current.signal));
  });

  pi.on("before_provider_headers", (event, ctx) => {
    if (!routed(ctx)) return;
    const session = ctx.sessionManager.getSessionId();
    if (SESSION_ID.test(session)) event.headers[SESSION_HEADER] = session;
  });

  // Only a routed built-in `openai` Responses model with a stored ChatGPT login, and only for a request
  // to that same model. An API key (or any other provider) passes through unchanged.
  pi.on("before_provider_request", (event, ctx) => {
    const model = ctx.model;
    if (!model || model.provider !== "openai" || model.api !== "openai-responses" || !routed(ctx)) return undefined;
    if (!stripsChatGPTFields(install)) return undefined;
    const payload = event.payload as { model?: unknown } | undefined;
    if (payload?.model !== model.id || !ctx.modelRegistry.isUsingOAuth(model)) return undefined;
    return forChatGPTSignIn(payload);
  });

  pi.on("cache_warming_decision", (_event, ctx) => {
    if (routed(ctx) && install?.cache_strategy && install.cache_strategy !== "none") return { action: "stop" as const };
    return undefined;
  });

  pi.on("session_compact", (_event, ctx) => signal(ctx, "SessionStart", "compact"));

  pi.on("session_shutdown", (event, ctx) => {
    try {
      checks.end(ctx.sessionManager.getSessionId());
    } catch {
      // No session id: nothing of ours to stop.
    }
    const kind = endKind(event.reason);
    if (kind) signal(ctx, "SessionEnd", kind);
  });

  const say = (ctx: ExtensionCommandContext, text: string, level: "info" | "warning" | "error" = "info") => {
    if (ctx.hasUI) ctx.ui.notify(text, level);
    else console.log(text);
  };

  const reloadModels = async (ctx: ExtensionCommandContext) => {
    install = readInstall();
    try {
      await ctx.modelRegistry.refresh();
    } catch {
      // Pi also reloads models.json when /model opens and at the next start.
    }
  };

  const setup = async (ctx: ExtensionCommandContext) => {
    const plan = await run(["setup", "--plan"]);
    const f = plan.facts;
    const args = ["setup", "--i-consent-to-traffic-interception"];
    if (f.result === "needs_decision") {
      // One question with three answers. Print mode has no dialog, and no answer changes nothing.
      const hatch = f.reset_hatch ? ` If an earlier context-guru install wrote that URL, run ${f.reset_hatch} from a shell, then setup again.` : "";
      const prompt = conflictPrompt(f);
      if (!ctx.hasUI) {
        say(ctx, `context-guru setup needs an interactive choice, so nothing changed. ${prompt.title}${hatch}`, "warning");
        return;
      }
      const choice = await ctx.ui.select(prompt.title, prompt.options);
      const answer = choice ? prompt.answers[choice] : undefined;
      if (!answer) {
        say(ctx, `Nothing changed.${hatch}`);
        return;
      }
      args.push("--on-conflict", answer);
    } else if (f.result !== "planned") {
      say(ctx, `context-guru setup: ${f.reason ?? f.result ?? plan.stderr.trim()}`, "error");
      return;
    } else {
      // Consent comes from a person. Print mode has no dialog, and no answer is a no.
      if (!ctx.hasUI) {
        say(ctx, `context-guru setup needs an interactive yes. Question: ${f.consent_question}`, "warning");
        return;
      }
      const yes = await ctx.ui.confirm("Route Pi through context-guru?", f.consent_question);
      if (!yes) {
        say(ctx, "Nothing changed.");
        return;
      }
    }
    const done = await run(args, 300_000);
    const r = done.facts;
    if (r.result !== "routed") {
      say(ctx, `context-guru setup failed: ${r.reason ?? r.result ?? done.stderr.trim()}` +
        (r.log ? `. Log: ${r.log}` : "") + (r.routing_written === "false" ? ". Pi was not routed." : ""), "error");
      return;
    }
    await reloadModels(ctx);
    say(ctx, [
      `context-guru routes Pi's Anthropic and OpenAI models through port ${r.port}: ${r.routes}.`,
      `Preset ${r.preset}, cache strategy ${r.cache_strategy}. Dashboard: ${r.dashboard}`,
      "Re-select the model with /model (or restart Pi) so this session uses the new route.",
      `If Pi ever stops being able to talk, run: ${r.reset_hatch}`,
    ].join("\n"));
  };

  const status = async (ctx: ExtensionCommandContext) => {
    const out = await run(["status"], 10_000);
    const f = out.facts;
    if (f.result === "not_installed") {
      say(ctx, "context-guru is not set up for Pi. Run /context-guru setup.");
      return;
    }
    say(ctx, [
      `proxy ${f.proxy_up === "true" ? "up" : "DOWN"} on port ${f.port} · routed ${f.routed}`,
      ...(f.routes ? [`routes: ${f.routes}`] : []),
      `this model routed: ${routed(ctx) ? "yes" : "no"} (${ctx.model?.provider ?? "no model"})`,
      `preset ${f.preset}${f.running_preset && f.running_preset !== f.preset ? ` (proxy runs ${f.running_preset})` : ""}` +
        ` · cache strategy ${f.cache_strategy} · requests ${f.requests ?? "?"}`,
      `dashboard: ${f.dashboard}`,
      ...(f.proxy_up === "true" && f.routed?.includes("false") ? ["A provider is not routed. Run /context-guru setup to route it."] : []),
    ].join("\n"), f.result === "ok" ? "info" : "warning");
  };

  const pick = async (ctx: ExtensionCommandContext, title: string, options: string[], given: string) => {
    if (given) return options.includes(given) ? given : undefined;
    return ctx.hasUI ? ctx.ui.select(title, options) : undefined;
  };

  /** One yes for a command that writes or restarts. Print mode has no dialog, and no answer is a no. */
  const agree = async (ctx: ExtensionCommandContext, title: string, message: string) => {
    if (!ctx.hasUI) {
      say(ctx, `${title} needs an interactive yes, so nothing changed. ${message}`, "warning");
      return false;
    }
    if (await ctx.ui.confirm(title, message)) return true;
    say(ctx, "Nothing changed.");
    return false;
  };

  const configure = async (ctx: ExtensionCommandContext, flag: "--preset" | "--cache-strategy", value: string) => {
    if (!(await agree(ctx, "Restart the proxy?",
      "The new setting applies after a restart. A restart costs one cache rewrite in every open Pi session."))) return;
    const out = await run(["configure", flag, value], 30_000);
    install = readInstall();
    const f = out.facts;
    say(ctx, f.result === "configured"
      ? `context-guru now runs preset ${f.preset}, cache strategy ${f.cache_strategy}.` + (f.probe ? ` Tool deferral probe: ${f.probe}.` : "")
      : `context-guru: ${f.result ?? "error"} ${f.detail ?? out.stderr.trim()}`, f.result === "configured" ? "info" : "error");
  };

  pi.registerCommand("context-guru", {
    description: "context-guru: setup, status, preset, cache-strategy, update, uninstall",
    getArgumentCompletions: (prefix) => {
      const [sub, rest] = prefix.split(/\s+/, 2);
      if (rest === undefined) return SUBCOMMANDS.filter((s) => s.startsWith(sub)).map((s) => ({ value: s, label: s }));
      const values = sub === "preset" ? PRESETS : sub === "cache-strategy" ? STRATEGIES : [];
      return values.filter((v) => v.startsWith(rest)).map((v) => ({ value: `${sub} ${v}`, label: v }));
    },
    handler: async (args, ctx) => {
      const { sub, value } = parseArgs(args);
      switch (sub) {
        case "setup":
          return setup(ctx);
        case "status":
          return status(ctx);
        case "preset": {
          const name = await pick(ctx, "Preset (each one includes the one above it)", PRESETS, value);
          if (name) return configure(ctx, "--preset", name);
          return say(ctx, `Presets: ${PRESETS.join(", ")}`);
        }
        case "cache-strategy": {
          const name = await pick(ctx, "Cache strategy", STRATEGIES, value);
          if (name) return configure(ctx, "--cache-strategy", name);
          return say(ctx, `Cache strategies: ${STRATEGIES.join(", ")}`);
        }
        case "update": {
          const check = (await run(["update", "--check"], 30_000)).facts;
          if (check.update_available !== "true") return say(ctx, `context-guru proxy ${check.installed_version ?? "?"} is current.`);
          if (!(await agree(ctx, "Update the proxy?",
            `Install ${check.latest_version} (now ${check.installed_version}) and restart the proxy?`))) return;
          const f = (await run(["update", "--install"], 300_000)).facts;
          return say(ctx, `context-guru update: ${f.result} ${f.version ?? f.reason ?? ""}`.trim());
        }
        case "uninstall": {
          if (!(await agree(ctx, "Remove context-guru?",
            "Restore models.json, stop and remove the proxy service, and delete the downloaded binary."))) return;
          const f = (await run(["uninstall"], 60_000)).facts;
          await reloadModels(ctx);
          return say(ctx, `context-guru removed (models.json: ${f.models_restore}, service: ${f.service}). ` +
            "Re-select the model with /model, or restart Pi.");
        }
        default:
          return say(ctx, `Unknown subcommand ${sub}. Use one of: ${SUBCOMMANDS.join(", ")}`, "warning");
      }
    },
  });
}
