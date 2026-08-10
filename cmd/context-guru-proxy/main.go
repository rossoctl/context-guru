// Command context-guru-proxy is the LLM proxy integration and the
// eval-containers gateway. It runs the context-guru component pipeline on
// inbound chat requests, then forwards them to the configured upstream
// provider, exposing /openai + /anthropic on one port plus /healthz, /stats,
// and /expand.
//
// Config is loaded from --config (YAML); upstreams and listen address from
// flags/env. Fail open: on any pipeline trouble the original request is
// forwarded untouched.
package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/proxy"
)

func main() {
	var (
		addr      = envOr("LISTEN_ADDR", ":4000")
		cfgPath   = flag.String("config", envOr("CONFIG", ""), "path to context-guru YAML config")
		preset    = flag.String("preset", envOr("PRESET", "codesmart"), "preset to use when --config is absent (codesmart = the SWE-bench-winning cache-aware config; codesafe = deterministic-only)")
		openai    = flag.String("openai-upstream", envOr("OPENAI_UPSTREAM", "https://api.openai.com"), "OpenAI upstream base URL")
		anthropic = flag.String("anthropic-upstream", envOr("ANTHROPIC_UPSTREAM", "https://api.anthropic.com"), "Anthropic upstream base URL")
		bob       = flag.String("bob-upstream", envOr("BOB_UPSTREAM", ""), "Bob (BobShell) backend base URL; enables the Bob gateway routes when set (e.g. https://api.us-east.bob.ibm.com)")
		storeFlag = flag.String("store", envOr("STORE", ""), "override state store: true|false (default: config store.enabled, else on)")
		modeFlag  = flag.String("mode", envOr("MODE", ""), "operating mode: sync (default) | async | observe (overrides the config's mode:)")
	)
	flag.Parse()

	cfg, err := loadConfig(*cfgPath, *preset)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *modeFlag != "" {
		cfg.Mode = *modeFlag // flag/env wins over the config file when set
	}
	mode, err := cfg.OperatingMode()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if v, ok := parseBool(*storeFlag); ok {
		cfg.Store.Enabled = &v // flag/env wins over the config file when set
	}

	agg := metrics.NewAggregator()
	emitter := metrics.Tee{agg, metrics.Slog{L: slog.Default()}}
	pipe, err := cfg.Build(emitter)
	if err != nil {
		log.Fatalf("build pipeline: %v", err)
	}

	h := proxy.New(pipe, cfg.NewStore(), agg, proxy.Options{
		OpenAIUpstream:    *openai,
		AnthropicUpstream: *anthropic,
		BobUpstream:       *bob, // enables the Bob gateway routes when set (BOB_UPSTREAM)
		// Gateway mode: real provider keys live here (eval-containers passes them
		// via env); the agent holds only a placeholder. Empty => pass client auth.
		OpenAIKey:    os.Getenv("OPENAI_API_KEY"),
		AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
		ForceModel:   os.Getenv("FORCE_MODEL"),   // eval-containers pins EVAL_MODEL's model here
		CheapModel:   cheapModelFromEnv(),        // static "config"-source LLM for NeedsModel components
		InjectExpand: os.Getenv("INJECT_EXPAND"), // auto (default) | always | never
		CacheMode:    os.Getenv("CACHE_MODE"),    // auto (default) | on | off — cache-aware compaction
		Windows:      modelWindows(),             // dynamic context-window resolver (fraction triggers)
		Mode:         mode,                       // sync (default) | async | observe — explicit, never inferred
		Async: proxy.AsyncOptions{
			CacheUncompactedTail:   cfg.Async.CacheUncompactedTail,
			StripCallerBreakpoints: cfg.Async.StripCallerBreakpoints,
			MaxQueue:               cfg.Async.MaxQueue,
			Workers:                cfg.Async.Workers,
		},

		// Per-request /compact override: swap the pipeline (?preset / header) while
		// keeping this config's component blocks. nil-safe in the handler.
		PipelineFor: func(preset string, names []string) (*components.Pipeline, error) {
			oc := *cfg // override Pipeline only; component blocks + store carry over
			switch {
			case len(names) > 0:
				oc.Pipeline = names
			case preset != "":
				p, ok := config.PresetPipeline(preset) // map lookup, not YAML from request input
				if !ok {
					return nil, fmt.Errorf("unknown preset %q", preset)
				}
				oc.Pipeline = p
			}
			return oc.Build(emitter)
		},
	})

	defer h.Close() // stop the off-path worker pool cleanly (no-op in sync mode)
	if mode == components.ModeObserve {
		slog.Warn("context-guru: OBSERVE MODE — requests are forwarded UNMODIFIED; " +
			"/stats reports what compaction WOULD have saved under potential_*/projected_* keys")
	}
	slog.Info("context-guru-proxy listening", "addr", addr, "pipeline", cfg.Pipeline, "mode", mode)
	if err := http.ListenAndServe(addr, h.Mux()); err != nil {
		log.Fatal(err)
	}
}

func loadConfig(path, preset string) (*config.Config, error) {
	if path != "" {
		return config.Load(path)
	}
	return config.LoadBytes([]byte("preset: " + preset + "\n"))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseBool reads a permissive bool override; ok=false for an empty/unknown
// value so the config file's setting is left untouched.
func parseBool(s string) (v, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "on", "yes":
		return true, true
	case "false", "0", "off", "no":
		return false, true
	}
	return false, false
}

// modelWindows builds the dynamic context-window resolver used for fraction-based
// triggers. Default chain: LiteLLM's public prices map (cached) -> small embedded
// fallback. MODEL_INFO_URL overrides the map source; MODEL_INFO=off disables it
// (windows unknown => fraction triggers ignored, absolutes apply).
func modelWindows() modelinfo.Resolver {
	if strings.EqualFold(os.Getenv("MODEL_INFO"), "off") {
		return nil
	}
	return modelinfo.Chain{
		modelinfo.NewLiteLLM(os.Getenv("MODEL_INFO_URL"), nil, 0),
		modelinfo.DefaultStatic(),
	}
}

// cheapModelFromEnv builds the static "config"-source LLM client for NeedsModel
// components (extract code/rlm, summarize with model.source=config). Returns nil
// when CHEAP_MODEL is unset, so those components fall back / no-op.
//
//	CHEAP_MODEL           model id (e.g. claude-haiku-4-5); unset => no client
//	CHEAP_MODEL_PROVIDER  anthropic (default) | openai
//	CHEAP_MODEL_BASE      upstream base URL (default: the matching provider default)
//	CHEAP_MODEL_KEY       API key (default: ANTHROPIC_API_KEY / OPENAI_API_KEY)
//	CHEAP_MODEL_AUTH      anthropic auth scheme: x-api-key (default) | bearer
func cheapModelFromEnv() components.Model {
	model := os.Getenv("CHEAP_MODEL")
	if model == "" {
		return nil
	}
	switch envOr("CHEAP_MODEL_PROVIDER", "anthropic") {
	case "openai":
		return cheapmodel.OpenAI{
			BaseURL: os.Getenv("CHEAP_MODEL_BASE"), Model: model,
			APIKey: envOr("CHEAP_MODEL_KEY", os.Getenv("OPENAI_API_KEY")),
		}
	default:
		return cheapmodel.Anthropic{
			BaseURL: os.Getenv("CHEAP_MODEL_BASE"), Model: model,
			APIKey:     envOr("CHEAP_MODEL_KEY", os.Getenv("ANTHROPIC_API_KEY")),
			AuthScheme: os.Getenv("CHEAP_MODEL_AUTH"),
		}
	}
}
