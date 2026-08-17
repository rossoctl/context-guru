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
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/buildinfo"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/tenant"
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
		modeFlag  = flag.String("mode", envOr("MODE", ""), "operating mode: sync (default) | observe (overrides the config's mode:)")

		// Dashboard. Off by default so an existing deployment's behavior and route
		// table are unchanged until asked for; on, it adds /dashboard/ + /api/*.
		// NOTE: deliberately NO "disable observability in production" gate — for a
		// tool whose value IS observability, that would be backwards.
		dashOn = flag.Bool("dashboard", envBool("DASHBOARD", false),
			"enable the persistent dashboard (embedded UI at /dashboard/, JSON+SSE at /api/*)")
		dashDB = flag.String("dashboard-db", envOr("DASHBOARD_DB", "./context-guru-dashboard.db"),
			"dashboard SQLite path; ':memory:' keeps history in RAM only (lost on restart)")
		dashRetain = flag.Duration("dashboard-retention", envDuration("DASHBOARD_RETENTION", 7*24*time.Hour),
			"drop dashboard rows older than this (0 = no age limit)")
		dashMaxBytes = flag.Int64("dashboard-max-bytes", int64(envInt("DASHBOARD_MAX_BYTES", 512<<20)),
			"cap the dashboard database size, dropping oldest rows first (0 = no size limit)")
		// Content capture is opt-IN, not opt-out. The before/after diff is the dashboard's
		// best view, but it is the one path that writes ARBITRARY agent output to disk, and
		// arbitrary output cannot be allowlisted the way headers and config keys are — it
		// gets pattern scrubbing, and a pattern denylist is always one unseen credential
		// shape behind reality (a review of 22 realistic shapes found 11 leaking). So the
		// default is the safe one and the operator turns it on for their own transcripts.
		dashContent = flag.Bool("dashboard-content", envBool("DASHBOARD_CONTENT", false),
			"capture before/after message text for the diff view; stores arbitrary agent output on disk (scrubbed of known credential shapes and size-capped first), so it is opt-in")
		dashContentCap = flag.Int("dashboard-content-cap", envInt("DASHBOARD_CONTENT_CAP", 16<<10),
			"maximum bytes stored per captured before/after blob")
		dashQueue = flag.Int("dashboard-queue", envInt("DASHBOARD_QUEUE", 4096),
			"capture channel depth; a full channel DROPS events (counted, and shown in the UI) rather than delaying a request")
		dashCIDRs = flag.String("dashboard-trusted-cidrs", envOr("DASHBOARD_TRUSTED_CIDRS", ""),
			"comma-separated CIDRs allowed to view per-request CONTENT and the effective config (loopback always is; aggregates are open)")
		// Hosted (multi-tenant) mode. Off by default: without --upstreams this binary
		// behaves exactly as it always has, which keeps every existing deployment and
		// every benchmark harness working unchanged.
		upstreamsPath = flag.String("upstreams", envOr("UPSTREAMS", ""),
			"path to the upstream allow-list YAML; SET THIS TO ENABLE HOSTED MULTI-TENANT MODE "+
				"(every request then needs a context-guru token, and each tenant's own config applies)")
		controlDB = flag.String("control-db", envOr("CONTROL_DB", "./context-guru-control.db"),
			"hosted mode: path to the control database (tenants, tokens, per-tenant config). "+
				"Kept separate from the dashboard DB, which is a derived view that may be rebuilt or pruned")
		managerEmail = flag.String("manager-email", envOr("MANAGER_EMAIL", ""),
			"hosted mode: the email that becomes the manager account on registration (sees and edits every tenant)")
		registerDomains = flag.String("register-domains", envOr("REGISTER_DOMAINS", ""),
			"hosted mode: comma-separated email domains allowed to self-register. Applies only "+
				"when CG_REGISTER=open or invite; registration is CLOSED unless CG_REGISTER "+
				"says otherwise (invite also needs CG_REGISTER_CODE). Matching is exact-domain "+
				"or a subdomain of it, but the address itself is UNVERIFIED — nobody proves "+
				"they own it")
		maxTenancies = flag.Int("max-tenancies", envInt("MAX_TENANCIES", proxy.DefaultMaxTenancies),
			"hosted mode: how many tenants keep live pipelines and compaction state in memory; "+
				"evicting a tenant costs it one cold cache on its next turn")

		// Disk-pressure eviction. The byte budget above bounds THIS database; these
		// bound the FILESYSTEM, which on a shared box is mostly filled by other things.
		dashDiskHigh = flag.Float64("dashboard-disk-high", envFloat("DASHBOARD_DISK_HIGH", 0.90),
			"evict the oldest SESSIONS while this fraction of the filesystem is in use (negative = disable)")
		dashDiskLow = flag.Float64("dashboard-disk-low", envFloat("DASHBOARD_DISK_LOW", 0.85),
			"stop evicting once usage falls to this fraction; the gap from --dashboard-disk-high is what stops the janitor grinding when the host is full for other reasons")
		dashMinKeep = flag.Int64("dashboard-min-keep-bytes", int64(envInt("DASHBOARD_MIN_KEEP_BYTES", 1<<30)),
			"never shrink the dashboard database below this under disk pressure; below it the pressure is not ours to relieve")
		dashMaxRowsPerTenant = flag.Int64("dashboard-max-rows-per-tenant", int64(envInt("DASHBOARD_MAX_ROWS_PER_TENANT", 0)),
			"hosted mode: cap one tenant's retained request rows, trimmed before the disk rule so a heavy user cannot evict everyone else (0 = no cap)")

		// Cold storage (Box via rclone). When set, eviction becomes MIGRATION: a
		// session is uploaded and verified before its local rows are deleted, so
		// retention is bounded by Box rather than by this filesystem. rclone is driven
		// as a subprocess and nothing is mounted — see dash/remote.go for why a FUSE
		// mount is the wrong home for a SQLite database.
		archiveRemote = flag.String("archive-remote", envOr("ARCHIVE_REMOTE", ""),
			"rclone remote path for cold storage, e.g. box:context-guru. SET THIS to make eviction archive instead of delete")
		rclonePath = flag.String("rclone", envOr("RCLONE", "rclone"), "path to the rclone binary")
		rcloneConf = flag.String("rclone-config", envOr("RCLONE_CONFIG", ""),
			"rclone config file holding the remote's OAuth token; set it explicitly for a service (under systemd $HOME is not the shell's)")
		rcloneBW = flag.String("archive-bwlimit", envOr("ARCHIVE_BWLIMIT", ""),
			"rclone --bwlimit for archiving (e.g. 8M); empty = unlimited, which will use all the upload bandwidth this box has")
		archiveContentAfter = flag.Duration("archive-content-after", envDuration("ARCHIVE_CONTENT_AFTER", 24*time.Hour),
			"move a session's TRANSCRIPTS to cold storage once idle this long (0 = never). This is where the bytes are")
		archiveSessionAfter = flag.Duration("archive-session-after", envDuration("ARCHIVE_SESSION_AFTER", 30*24*time.Hour),
			"move a WHOLE session to cold storage once idle this long (0 = never)")
		archiveInterval = flag.Duration("archive-interval", envDuration("ARCHIVE_INTERVAL", 15*time.Minute),
			"how often the archiver runs")
		archiveBatch = flag.Int("archive-batch", envInt("ARCHIVE_BATCH", 50),
			"maximum sessions archived per pass, so one catch-up cycle cannot exhaust the remote's API quota")
		archiveRequired = flag.Bool("archive-required", envBool("ARCHIVE_REQUIRED", false),
			"under disk pressure, refuse to delete a session that could not be archived. Safer for data; lets the filesystem fill if the remote is down, which takes every user's agent with it")

		// Prometheus, for Grafana. Loopback needs no token; a scraper on another host
		// does, because /metrics carries per-tenant cost.
		metricsToken = flag.String("metrics-token", envOr("METRICS_TOKEN", ""),
			"bearer token allowing a remote Prometheus to scrape /metrics (loopback never needs one)")
		// Per-tenant limits on the shared box.
		rpm = flag.Int("tenant-rpm", envInt("TENANT_RPM", 0),
			"hosted mode: max requests per minute per tenant (0 = unlimited)")
		tenantConcurrent = flag.Int("tenant-concurrent", envInt("TENANT_CONCURRENT", 0),
			"hosted mode: max in-flight requests per tenant (0 = unlimited)")
		cheapConcurrent = flag.Int("cheap-model-concurrent", envInt("CHEAP_MODEL_CONCURRENT", 4),
			"process-wide cap on concurrent compaction-model calls, so one tenant's extract_llm cannot stall everyone's agents (0 = unlimited)")

		dashBench = flag.String("dashboard-bench-dirs", envOr("DASHBOARD_BENCH_DIRS", ""),
			"comma-separated directories of benchmark runs (each with summary.json + rows-*.json) to ingest")
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

	windows := modelWindows()

	// Cold storage, verified at boot: a remote that cannot be reached should be a log
	// line now, not a stream of "archiving failed" hours later with no hint that the
	// remote was never configured properly in the first place.
	var remote dash.Remote
	if *archiveRemote != "" {
		rc := &dash.Rclone{Bin: *rclonePath, Base: *archiveRemote,
			ConfigPath: *rcloneConf, BWLimit: *rcloneBW}
		if err := rc.Check(context.Background()); err != nil {
			// Not fatal. Cold storage being unreachable must not stop the proxy from
			// serving traffic — the same reasoning that makes the dashboard fall back to
			// memory rather than refusing to boot.
			slog.Error("context-guru: cold storage is not reachable; archiving is DISABLED "+
				"and eviction will delete instead of migrate", "remote", *archiveRemote, "err", err)
		} else {
			remote = rc
			slog.Info("context-guru: cold storage ready", "remote", *archiveRemote,
				"content_after", *archiveContentAfter, "session_after", *archiveSessionAfter)
			// No spend warning here any more: month-to-date spend comes from the
			// tenant_spend rollup (dash/spend.go), which retention and archiving never
			// touch, so archiving sessions inside the calendar month no longer makes the
			// cap under-count.
		}
	}

	var rec *dash.Recorder
	if *dashOn {
		opts := dash.Options{
			DBPath:         *dashDB,
			RetentionAge:   *dashRetain,
			RetentionBytes: *dashMaxBytes,
			CaptureContent: *dashContent,
			ContentCap:     *dashContentCap,
			QueueSize:      *dashQueue,
			TrustedCIDRs:   splitComma(*dashCIDRs),
			BenchDirs:      splitComma(*dashBench),

			DiskHighWatermark: *dashDiskHigh,
			DiskLowWatermark:  *dashDiskLow,
			MinKeepBytes:      *dashMinKeep,
			MaxRowsPerTenant:  *dashMaxRowsPerTenant,

			Remote: remote,
			// The CONFIGURED name, whether or not the probe above succeeded: `remote` is
			// nil when cold storage is unreachable, and the dashboard must still be able
			// to say "configured but unreachable" instead of "not configured".
			RemoteName:          *archiveRemote,
			ArchiveContentAfter: *archiveContentAfter,
			ArchiveSessionAfter: *archiveSessionAfter,
			ArchiveInterval:     *archiveInterval,
			ArchiveBatch:        *archiveBatch,
			ArchiveRequired:     *archiveRequired,
			// The REAL mode, not a hardcoded "active". In observe mode nothing context-guru
			// computed was ever enforced, so the dashboard must say so unmissably rather than
			// present projections as achieved savings.
			Mode:      dashMode(mode),
			Effective: effectiveConfig(cfg, addr, *openai, *anthropic, *bob, *dashDB, *dashContent, *dashCIDRs),
		}
		// A negative retention means "no limit"; a zero means "use the default". Map
		// an explicit 0 from the flag to "no limit", which is what a user typing 0 means.
		if *dashRetain == 0 {
			opts.RetentionAge = -1
		}
		if *dashMaxBytes == 0 {
			opts.RetentionBytes = -1
		}
		r, err := dash.NewRecorder(opts)
		if err != nil {
			log.Fatalf("dashboard: %v", err)
		}
		rec = r
		defer rec.Close()
		if runs, tasks := rec.DB().IngestBenchRoots(opts.BenchDirs); runs > 0 {
			slog.Info("dashboard: ingested benchmark runs", "runs", runs, "tasks", tasks)
		} else if len(opts.BenchDirs) > 0 {
			// Asked to ingest and found nothing. Say so: ingestion runs ONCE at startup,
			// so a silent zero means the Benchmarks tab stays empty forever with no clue
			// why, and the most likely cause is invisible from inside the process.
			//
			// That cause is PrivateTmp. The shipped unit sets PrivateTmp=true, so the
			// service gets its OWN empty /tmp — a directory that plainly exists in the
			// operator's shell is simply not there for this process. Pointing
			// DASHBOARD_BENCH_DIRS at /tmp/<anything> therefore looks correct, changes
			// nothing, and reports nothing. Naming the path we actually looked at is what
			// makes that diagnosable in one glance at the log.
			hint := ""
			for _, d := range opts.BenchDirs {
				if strings.HasPrefix(filepath.Clean(d), "/tmp/") {
					hint = "a /tmp path is NOT visible to this process when the unit sets " +
						"PrivateTmp=true; copy the run directory somewhere readable " +
						"(e.g. under the state dir) and point DASHBOARD_BENCH_DIRS there"
					break
				}
			}
			slog.Warn("dashboard: no benchmark runs ingested; the Benchmarks tab will be empty",
				"dirs", strings.Join(opts.BenchDirs, ","), "hint", hint)
		}
		slog.Info("dashboard enabled", "url", "http://"+addr+"/dashboard/", "db", rec.DB().Path(),
			"content_capture", *dashContent)
	}

	// Hosted mode. Everything below is nil/empty unless --upstreams was given, and the
	// proxy checks exactly one field (Options.Tenants) to decide which world it is in.
	var tenants *proxy.TenantSource
	var upstreams map[string]proxy.Upstream
	var reg *tenant.Registry
	if *upstreamsPath != "" {
		list, err := config.LoadUpstreams(*upstreamsPath)
		if err != nil {
			// Deliberately fatal. A hosted proxy with an unusable allow-list would
			// either refuse every request or, worse, forward a client's token to a
			// third party for want of a key to inject.
			log.Fatalf("upstreams: %v", err)
		}
		upstreams = make(map[string]proxy.Upstream, len(list))
		for _, u := range list {
			upstreams[u.Name] = proxy.Upstream{Dialect: u.Dialect, BaseURL: u.BaseURL,
				KeyEnv: u.KeyEnv, Header: u.Header}
		}
		defAnthropic, defOpenAI, defBob := defaultUpstreams(list)
		reg, err = tenant.Open(*controlDB, tenant.Options{
			ManagerEmail:       *managerEmail,
			EmailDomains:       splitComma(*registerDomains),
			DefaultUpAnthropic: defAnthropic,
			DefaultUpOpenAI:    defOpenAI,
			DefaultUpBob:       defBob,
			// Reject a bad configuration when a user SAVES it, so the failure is a 400
			// on their settings page instead of a silent pass-through on their next turn.
			Validate: config.Validate,
		})
		if err != nil {
			log.Fatalf("control database: %v", err)
		}
		defer reg.Close()
		tenants = proxy.NewTenantSource(reg, emitter, buildTenantConfig, *maxTenancies)
		// Make the per-tenant row quota a manager can set actually bind. Wired here
		// rather than in dash.Options because the recorder starts before the control
		// database opens, and because the value has to be read fresh — a manager can
		// change it at any time. 0 (or an unknown tenant) falls back to the server-wide
		// --dashboard-max-rows-per-tenant.
		rec.SetTenantQuota(func(id string) int64 {
			t, err := reg.Get(id)
			if err != nil {
				return 0
			}
			return t.MaxRows
		})
		// A tenant's own configuration must never be able to spend the server's ambient
		// provider credential: a `model:` block that names a model but no api_key would
		// otherwise fall back to this process's ANTHROPIC_API_KEY / OPENAI_API_KEY. In
		// hosted mode that is exactly the billing defect this deployment removes, so the
		// fallback is switched off and such a block simply has no client (fail open).
		offload.AllowEnvModelKey = false
		slog.Info("context-guru: HOSTED multi-tenant mode",
			"control_db", *controlDB, "upstreams", len(upstreams),
			"register_domains", *registerDomains, "manager", *managerEmail != "")
		if *managerEmail == "" {
			slog.Warn("context-guru: no --manager-email set; no account will be able to " +
				"administer other tenants until one is configured")
		}
		// Registration mode lives in the environment (CG_REGISTER) and is re-read by the
		// control plane on every request, so an operator can open or close registration
		// without a restart. That means this log reports what the mode is NOW, not a value
		// pinned for the process lifetime — which is also why it is not a flag.
		//
		// The warning is mode-dependent on purpose. It used to fire on an empty
		// --register-domains alone, which after registration became closed-by-default was
		// worse than no warning: it told the operator anyone could create an account at a
		// moment when nobody could.
		//
		// The mode comes from proxy.RegisterMode(), NOT from reading CG_REGISTER here:
		// the control plane trims and lower-cases the value, so a banner that switched on
		// the raw string reported "off" for CG_REGISTER=Open while registration was open.
		// Whether a code can be DELIVERED is a boot-time fact worth reporting: registration
		// and password sign-in cannot complete without it, and an operator should learn
		// that here rather than from the first user's bug report.
		if ok, how := proxy.MailConfigured(); !ok {
			slog.Warn("context-guru: no email path configured (" + how + "); verification " +
				"codes cannot be delivered, so NOBODY can create an account or sign in " +
				"with a password")
		} else {
			slog.Info("context-guru: verification email path", "via", how)
		}
		switch mode := proxy.RegisterMode(); mode {
		case "open":
			if *registerDomains == "" {
				slog.Warn("context-guru: CG_REGISTER=open with no --register-domains; anyone " +
					"who can receive mail at any address may create an account here")
			} else {
				// The match itself is sound (exact domain or a subdomain of it, so
				// notibm.com does not match ibm.com), and the address is now PROVEN by a
				// mailed code rather than merely claimed — which is what the old warning
				// here said was missing. What remains is that reachability is not
				// entitlement: anyone with a mailbox in the domain may register.
				slog.Info("context-guru: CG_REGISTER=open with verified email addresses; "+
					"anyone with a mailbox in these domains may self-register",
					"domains", *registerDomains)
			}
		case "invite":
			if os.Getenv("CG_REGISTER_CODE") == "" {
				slog.Warn("context-guru: CG_REGISTER=invite but CG_REGISTER_CODE is empty; " +
					"registration will refuse EVERYONE until a code is set")
			}
		default:
			// Deliberately says how to BOOTSTRAP: /api/register is the only route that
			// creates an account (a manager can reissue tokens for tenants that exist, but
			// cannot create one), so a fresh control database with registration closed has
			// no path to a first account at all. An operator who is told only "it is off"
			// discovers that the hard way.
			slog.Info("context-guru: self-registration is off (CG_REGISTER=" + mode +
				"); /api/register is the only way an account is created, so bootstrap the " +
				"first one with CG_REGISTER=invite plus CG_REGISTER_CODE (matching " +
				"--manager-email), then close it again")
		}
	}

	h := proxy.New(pipe, cfg.NewStore(), agg, proxy.Options{
		Tenants:      tenants,   // nil => single-tenant, unchanged behaviour
		Upstreams:    upstreams, // the allow-list; a tenant picks a name, never a URL
		MetricsToken: *metricsToken,
		// Both come from the dashboard's store, so both are nil when the dashboard is
		// off — with no request rows there is nothing to price a cap against and nothing
		// to roll up per tenant.
		Spend:          spendChecker(rec),
		TenantMetrics:  tenantMetrics(rec),
		Version:        buildinfo.Version,
		PresetNames:    config.PresetNames(),
		ComponentNames: components.Names(),
		Limits: proxy.Limits{
			RequestsPerMinute:    *rpm,
			Concurrent:           *tenantConcurrent,
			CheapModelConcurrent: *cheapConcurrent,
		},
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
		Windows:      windows,                    // dynamic context-window resolver (fraction triggers)
		Prices:       priceResolver(windows),     // per-token rates, so each captured request is priced at write time
		Preset:       cfg.Preset,
		Dashboard:    rec,  // nil unless --dashboard
		Mode:         mode, // sync (default) | observe — explicit, never inferred
		Observe: proxy.ObserveOptions{
			MaxQueue: cfg.Observe.MaxQueue,
			Workers:  cfg.Observe.Workers,
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

	// One identity resolver for both halves of the dashboard: the read routes (dash)
	// and the write routes (control plane) must never disagree about who the caller is.
	if tenants != nil && rec != nil {
		h.API().SetAuth(h.DashAuth())
		h.API().SetWhoami(h.DashWhoami())
		// The per-tenant half of the capture decision, read fresh because a tenant can
		// toggle its consent at any time. Without it the dashboard reported the
		// process-global flag to every tenant: "captured" to accounts that never
		// consented, and "not captured, go and enable it" when the operator's gate was
		// the one that was off. Mirrors proxy's own captureContentFor.
		h.API().SetTenantCapture(func(id string) bool {
			t, err := reg.Get(id)
			return err == nil && t.CaptureContent
		})
	}

	defer h.Close() // stop the off-path worker pool cleanly (no-op in sync mode)
	if mode == components.ModeObserve {
		slog.Warn("context-guru: OBSERVE MODE — requests are forwarded UNMODIFIED; " +
			"/stats reports what compaction WOULD have saved under potential_*/projected_* keys")
	}
	slog.Info("context-guru-proxy listening", "addr", addr, "pipeline", cfg.Pipeline, "mode", mode)

	srv := &http.Server{
		Addr:    addr,
		Handler: h.Mux(),
		// ReadHeaderTimeout is the one that matters for a service on a network: without
		// it, a client that opens a connection and never finishes its headers holds a
		// goroutine and a file descriptor indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// NO WriteTimeout, and no ReadTimeout, deliberately. Both are absolute deadlines
		// on the whole request, and this proxy has two kinds of traffic that legitimately
		// outlive any fixed budget: an SSE stream that stays open for hours, and an agent
		// turn on a large transcript. A WriteTimeout here would sever the dashboard's live
		// feed and truncate long completions mid-stream — which would look like
		// context-guru corrupting responses. Per-write deadlines are applied where they
		// belong instead, via http.ResponseController in dash/sse.go.
	}

	armShutdown(srv, rec)

	// Graceful shutdown, so the dashboard's writer goroutine flushes its batch and any
	// in-flight archive upload is not abandoned halfway. Without this, a restart loses
	// the last few hundred milliseconds of captured requests every time.
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		s := <-sig
		slog.Info("context-guru: shutting down", "signal", s.String())
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("context-guru: graceful shutdown did not finish in time", "err", err)
		}
		close(idle)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-idle
	// The deferred Close calls (recorder, registry) run after this, which is what
	// actually flushes the capture batch and closes the databases cleanly.
}

// armShutdown makes graceful shutdown able to finish while the dashboard's live
// feed is connected.
//
// http.Server.Shutdown closes the listeners and then WAITS for every in-flight
// request to return. An SSE stream never returns on its own — that is the point of
// it — so a single open /api/events connection held Shutdown for its whole deadline
// (measured: 15 ms with no viewer, 25 s with one), which is 25 s of downtime on
// every restart and 25 s in which an in-flight archive upload is neither finished
// nor cancelled. The hook runs as shutdown begins, after the listeners are closed,
// so the ordering is: stop accepting, disconnect the streams, drain the rest — and
// only then do the deferred Close calls flush the capture batch and close the
// databases.
func armShutdown(srv *http.Server, rec *dash.Recorder) {
	if rec == nil {
		return
	}
	srv.RegisterOnShutdown(rec.Hub().Close)
}

// buildTenantConfig expands one tenant's configuration document into a runnable
// pipeline and its own state store. Handed to the proxy as a function so that
// package stays decoupled from `config`, exactly as PipelineFor already is.
//
// The store is built per tenant, not shared: it holds the FROZEN compaction
// decisions that make savings compound turn over turn, and a shared LRU means one
// busy tenant evicts another's, which re-writes that tenant's whole cached prefix
// at roughly 11.5x the read price.
// spendChecker adapts the recorder to the spend interface, returning nil (cap
// disabled) when there is no dashboard — the month-to-date figure lives in the
// dashboard's rows, so without it there is nothing to enforce against.
func spendChecker(rec *dash.Recorder) proxy.SpendChecker {
	if rec == nil {
		return nil
	}
	return rec.DB()
}

// tenantMetrics adapts the recorder's per-tenant rollup for the Prometheus exporter.
// The two row types are structurally identical but declared in different packages
// (dash must not import proxy), so this converts.
func tenantMetrics(rec *dash.Recorder) proxy.TenantMetricsSource {
	if rec == nil {
		return nil
	}
	return tenantMetricsAdapter{rec}
}

type tenantMetricsAdapter struct{ rec *dash.Recorder }

func (a tenantMetricsAdapter) TenantMetrics(since int64) ([]proxy.TenantMetricRow, error) {
	rows, err := a.rec.DB().TenantMetrics(since)
	if err != nil {
		return nil, err
	}
	out := make([]proxy.TenantMetricRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, proxy.TenantMetricRow{
			TenantID: r.TenantID, Label: r.Label, Requests: r.Requests,
			TokensBefore: r.TokensBefore, TokensAfter: r.TokensAfter,
			SavedUnique: r.SavedUnique, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
			FreshInput: r.FreshInput, OutputTokens: r.OutputTokens,
			CostUSD: r.CostUSD, BaselineUSD: r.BaselineUSD, CGLLMCostUSD: r.CGLLMCostUSD,
			CGLatencyMs: r.CGLatencyMs, UpstreamMs: r.UpstreamMs, Sessions: r.Sessions,
			ArchivedCount: r.ArchivedCount, ArchivedBytes: r.ArchivedBytes,
		})
	}
	return out, nil
}

func buildTenantConfig(doc []byte, e components.Emitter) (proxy.BuiltConfig, error) {
	cfg, err := config.LoadBytes(doc)
	if err != nil {
		return proxy.BuiltConfig{}, err
	}
	mode, err := cfg.OperatingMode()
	if err != nil {
		return proxy.BuiltConfig{}, err
	}
	pipe, err := cfg.Build(e)
	if err != nil {
		return proxy.BuiltConfig{}, err
	}
	preset := cfg.Preset
	if preset == "" {
		preset = "custom" // an explicit pipeline list; labelled so the dashboard can group it
	}
	return proxy.BuiltConfig{Pipe: pipe, Store: cfg.NewStore(), Mode: mode, Preset: preset}, nil
}

// defaultUpstreams picks the first entry of each dialect as the default a newly
// registered tenant starts on, so registration needs no choices and the common case
// (one gateway, one Bob region) needs no configuration at all.
func defaultUpstreams(list []config.Upstream) (anthropic, openai, bob string) {
	for _, u := range list {
		switch u.Dialect {
		case config.DialectAnthropic:
			if anthropic == "" {
				anthropic = u.Name
			}
		case config.DialectOpenAI:
			if openai == "" {
				openai = u.Name
			}
		case config.DialectBob:
			if bob == "" {
				bob = u.Name
			}
		}
	}
	return
}

func loadConfig(path, preset string) (*config.Config, error) {
	if path != "" {
		return config.Load(path)
	}
	return config.LoadBytes([]byte("preset: " + preset + "\n"))
}

// splitComma splits a comma-separated flag value into trimmed, non-empty items.
func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// priceResolver returns the Pricer side of the window resolver, when it has one.
// A nil Pricer means "no rates known", and every captured row is then marked
// partially accounted rather than priced as free.
func priceResolver(r modelinfo.Resolver) modelinfo.Pricer {
	p, _ := r.(modelinfo.Pricer)
	return p
}

// effectiveConfig assembles the RESOLVED configuration for the dashboard's config
// view — preset expanded, pipeline as actually built, upstream bases and dashboard
// settings included. It is key-allowlisted by dash.RedactConfig before serving, and
// deliberately carries no credential: keys are read from the environment at use
// time and never copied into this map.
func effectiveConfig(cfg *config.Config, addr, openai, anthropic, bob, dbPath string, content bool, cidrs string) map[string]any {
	comps := map[string]any{}
	for name, node := range cfg.Components {
		var v any
		if err := node.Decode(&v); err == nil {
			comps[name] = v
		}
	}
	return map[string]any{
		"preset":               cfg.Preset,
		"pipeline":             cfg.Pipeline,
		"components":           comps,
		"listen_addr":          addr,
		"openai_upstream":      openai,
		"anthropic_upstream":   anthropic,
		"bob_upstream":         bob,
		"force_model":          os.Getenv("FORCE_MODEL"),
		"cache_mode":           envOr("CACHE_MODE", "auto"),
		"inject_expand":        envOr("INJECT_EXPAND", "auto"),
		"cheap_model":          os.Getenv("CHEAP_MODEL"),
		"cheap_model_provider": envOr("CHEAP_MODEL_PROVIDER", "anthropic"),
		"store":                map[string]any{"ttl_seconds": cfg.Store.TTLSeconds, "max_entries": cfg.Store.MaxEntries},
		"dashboard":            map[string]any{"db_path": dbPath, "capture_content": content, "trusted_cidrs": cidrs},
		"build_version":        buildinfo.Version,
		"build_commit":         buildinfo.Commit,
	}
}

// envBool reads a permissive boolean environment variable.
func envBool(key string, def bool) bool {
	if v, ok := parseBool(os.Getenv(key)); ok {
		return v
	}
	return def
}

// envInt reads an integer environment variable, falling back on anything unparseable.
func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64); err == nil {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return def
}

// envDuration reads a Go duration environment variable (e.g. "72h").
func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(key))); err == nil {
		return d
	}
	return def
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

// dashMode maps the operating mode onto the dashboard's own label. Two vocabularies
// exist because they answer different questions: `components.Mode` is "what does the
// pipeline do", while the dashboard's per-row mode also has to express `bypass`, which
// is a property of one request rather than of the deployment.
func dashMode(m components.Mode) string {
	if m == components.ModeObserve {
		return dash.ModeObserve
	}
	return dash.ModeActive
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
