// context-guru for Pi.
//
// Routing lives in Pi's models.json (written by scripts/pi_plugin.py after the user says yes), and the
// proxy runs as a user service. So this extension does NO network work when a session starts or a
// request is sent. It only:
//   1. tags each routed request with Pi's own session id (header x-context-guru-session), so the proxy
//      keeps one session across a compaction instead of splitting it in two;
//   2. stops Pi's own cache warming for routed models while the proxy's keep-alive is on, so the cache
//      is pinged by one owner, not two;
//   3. tells the proxy when a session ends or compacts (content-free: id, event, reason), so the
//      keep-alive stops pinging a cache no later request will read; sent after the fact, never awaited;
//   4. adds /context-guru <setup|status|preset|cache-strategy|update|uninstall>.
import { readFileSync } from "node:fs";
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

interface Install {
  port?: number;
  base_urls?: Record<string, string>;
  cache_strategy?: string;
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

/** Parse the `key=value` lines pi_plugin.py prints. Later keys win. */
export function parseFacts(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const at = line.indexOf("=");
    if (at > 0) out[line.slice(0, at)] = line.slice(at + 1);
  }
  return out;
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

  pi.on("session_start", () => {
    install = readInstall();
  });

  pi.on("before_provider_headers", (event, ctx) => {
    if (!routed(ctx)) return;
    const session = ctx.sessionManager.getSessionId();
    if (SESSION_ID.test(session)) event.headers[SESSION_HEADER] = session;
  });

  pi.on("cache_warming_decision", (_event, ctx) => {
    if (routed(ctx) && install?.cache_strategy && install.cache_strategy !== "none") return { action: "stop" as const };
    return undefined;
  });

  pi.on("session_compact", (_event, ctx) => signal(ctx, "SessionStart", "compact"));

  pi.on("session_shutdown", (event, ctx) => {
    const kind = endKind(event.reason);
    if (kind) signal(ctx, "SessionEnd", kind);
  });

  const run = async (args: string[], timeout = 180_000) => {
    const result = await pi.exec("python3", [SCRIPT, ...args], { timeout });
    return { ...result, facts: parseFacts(result.stdout) };
  };

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
    if (f.result === "needs_decision") {
      say(ctx, `models.json already sends ${f.existing_base_url} somewhere else. context-guru did not change it. ` +
        "Remove that baseUrl first, or wait for chain support.", "warning");
      return;
    }
    if (f.result !== "planned") {
      say(ctx, `context-guru setup: ${f.reason ?? f.result ?? plan.stderr.trim()}`, "error");
      return;
    }
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
    const done = await run(["setup", "--i-consent-to-traffic-interception"], 300_000);
    const r = done.facts;
    if (r.result !== "routed") {
      say(ctx, `context-guru setup failed: ${r.reason ?? r.result ?? done.stderr.trim()}` +
        (r.log ? `. Log: ${r.log}` : "") + (r.routing_written === "false" ? ". Pi was not routed." : ""), "error");
      return;
    }
    await reloadModels(ctx);
    say(ctx, [
      `context-guru routes Pi's Anthropic models through port ${r.port}.`,
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
      `this model routed: ${routed(ctx) ? "yes" : "no"} (${ctx.model?.provider ?? "no model"})`,
      `preset ${f.preset}${f.running_preset && f.running_preset !== f.preset ? ` (proxy runs ${f.running_preset})` : ""}` +
        ` · cache strategy ${f.cache_strategy} · requests ${f.requests ?? "?"}`,
      `dashboard: ${f.dashboard}`,
    ].join("\n"), f.result === "ok" ? "info" : "warning");
  };

  const pick = async (ctx: ExtensionCommandContext, title: string, options: string[], given: string) => {
    if (given) return options.includes(given) ? given : undefined;
    return ctx.hasUI ? ctx.ui.select(title, options) : undefined;
  };

  const configure = async (ctx: ExtensionCommandContext, flag: "--preset" | "--cache-strategy", value: string) => {
    if (ctx.hasUI) {
      const ok = await ctx.ui.confirm("Restart the proxy?",
        "The new setting applies after a restart. A restart costs one cache rewrite in every open Pi session.");
      if (!ok) {
        say(ctx, "Nothing changed.");
        return;
      }
    }
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
      const [sub = "status", value = ""] = args.trim().split(/\s+/);
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
          if (ctx.hasUI && !(await ctx.ui.confirm("Update the proxy?",
            `Install ${check.latest_version} (now ${check.installed_version}) and restart the proxy?`))) return say(ctx, "Nothing changed.");
          const f = (await run(["update", "--install"])).facts;
          return say(ctx, `context-guru update: ${f.result} ${f.version ?? f.reason ?? ""}`.trim());
        }
        case "uninstall": {
          if (ctx.hasUI && !(await ctx.ui.confirm("Remove context-guru?",
            "Restore models.json, stop and remove the proxy service, and delete the downloaded binary."))) return say(ctx, "Nothing changed.");
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
