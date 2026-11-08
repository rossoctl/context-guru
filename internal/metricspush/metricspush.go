// Package metricspush starts the OpenTelemetry SDK and the OTLP exporter that send a proxy's metrics to the
// central collector (R15). It is the only package that imports the SDK, so the metrics package
// stays API-only. Each proxy holds its own copy of this setup: the URL, the token and the exporter are
// settings of the host, not of the accountant.
//
// The push is asynchronous (R16). The SDK aggregates in its own memory and exports from its own
// goroutine. A recording call never waits for the collector, and a failed push loses no count: the
// next push carries the totals (R20).
package metricspush

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// DefaultURL is the collector a proxy pushes to when the user sets nothing (R19). Data leaves the
// machine from the first run, so the host names this in its release notes.
// TODO: replace with the real collector address before release.
const DefaultURL = "https://metrics.example.invalid"

// Defaults.
const (
	DefaultInterval = 30 * time.Second
	exportTimeout   = 10 * time.Second
)

// Config is what a proxy gives [Start].
type Config struct {
	// URL is the collector base URL. Empty means "use DefaultURL" unless Disabled is set.
	URL string
	// Token is the per-install access token, sent as a bearer token (R44).
	Token string
	// Interval is the time between pushes. Zero means DefaultInterval.
	Interval time.Duration
	// Disabled turns the push off. The proxy then makes no connection (R19).
	Disabled bool

	// Resource attributes (R30, R43). The collector turns them into the labels proxy and machine.
	Proxy   string // "cortex" or "context-guru"
	Machine string // random install ID
	Version string
	Preset  string

	// Logger receives the one start line and the first failure of a run. Nil means slog.Default().
	Logger *slog.Logger
}

// Shutdown flushes the last totals and stops the exporter. Give it a short timeout.
type Shutdown func(context.Context) error

// Start builds the provider. It returns a no-op provider when the push is off, so a caller never
// needs a nil check. It never returns an error for a bad collector: that is a runtime failure, and
// it is counted in llm_proxy_export_failures_total (R20, fail open).
func Start(ctx context.Context, cfg Config) (metric.MeterProvider, Shutdown, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	endpoint := cfg.URL
	if endpoint == "" && !cfg.Disabled {
		endpoint = DefaultURL
	}
	if cfg.Disabled || endpoint == "" {
		log.Info("metrics push is off: no connection will be made")
		return noopmetric.NewMeterProvider(), func(context.Context) error { return nil }, nil
	}
	if err := checkURL(endpoint); err != nil {
		return nil, nil, err
	}

	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(strings.TrimRight(endpoint, "/") + "/v1/metrics"),
		otlpmetrichttp.WithTimeout(exportTimeout),
	}
	if cfg.Token != "" {
		opts = append(opts, otlpmetrichttp.WithHeaders(map[string]string{"Authorization": "Bearer " + cfg.Token}))
	}
	inner, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("push: build exporter: %w", err)
	}

	fails := &failureCounter{log: log}
	exp := &countingExporter{Exporter: inner, fails: fails}

	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	res := resource.NewSchemaless(
		attribute.String("proxy", cfg.Proxy),
		attribute.String("service.instance.id", cfg.Machine),
		attribute.String("service.version", cfg.Version),
		attribute.String("preset", cfg.Preset),
	)
	provider := sdk.NewMeterProvider(
		sdk.WithResource(res),
		sdk.WithReader(sdk.NewPeriodicReader(exp, sdk.WithInterval(interval), sdk.WithTimeout(exportTimeout))),
	)
	c, err := provider.Meter("accountant/metrics/push").Int64Counter("llm.proxy.export_failures",
		metric.WithDescription("Failed pushes to the collector. The next push carries the totals, so a failure loses no count."))
	if err != nil {
		return nil, nil, fmt.Errorf("push: build failure counter: %w", err)
	}
	fails.counter = c

	log.Info("metrics push is on", "collector", host(endpoint), "interval", interval.String())
	return provider, provider.Shutdown, nil
}

// checkURL accepts an https URL. It accepts http only for a loopback host, so a test collector
// works and a token never crosses the network in clear text (R44).
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("push: %q is not a collector URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("push: the collector URL must use https (R44): %q", host(raw))
}

// host drops the path and the query, so a log line never carries a secret.
func host(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Scheme + "://" + u.Host
	}
	return ""
}

// failureCounter counts failed exports by reason. It logs the first failure of each reason only.
type failureCounter struct {
	counter metric.Int64Counter
	log     *slog.Logger
	logged  map[string]bool
}

func (f *failureCounter) record(err error) {
	reason := reasonOf(err)
	if f.counter != nil {
		f.counter.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
	// Export runs on the reader's single goroutine, so logged needs no lock.
	if f.logged == nil {
		f.logged = map[string]bool{}
	}
	if !f.logged[reason] {
		f.logged[reason] = true
		f.log.Warn("metrics push failed; the next push carries the totals", "reason", reason)
	}
}

// reasonOf maps an export error to a small fixed set of label values.
func reasonOf(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case strings.Contains(err.Error(), "401"), strings.Contains(err.Error(), "403"):
		return "unauthorized"
	case strings.Contains(err.Error(), "status 5"), strings.Contains(err.Error(), "503"):
		return "collector_error"
	case errors.As(err, new(*net.OpError)), strings.Contains(err.Error(), "connection"):
		return "connect"
	}
	return "other"
}

// countingExporter wraps the exporter and counts each failed export.
type countingExporter struct {
	sdk.Exporter
	fails *failureCounter
}

func (e *countingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err != nil {
		e.fails.record(err)
	}
	return err
}

// FromEnv fills Config from the environment for a proxy that wants no flag of its own. The names
// are a proposal; the PR may rename them.
func FromEnv(c Config) Config {
	if v, ok := os.LookupEnv("METRICS_PUSH_URL"); ok {
		c.URL = v
	}
	if v := os.Getenv("METRICS_PUSH_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("METRICS_PUSH"); v == "off" || v == "0" || v == "false" {
		c.Disabled = true
	}
	if v := os.Getenv("METRICS_PUSH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Interval = d
		}
	}
	return c
}
