package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/buildinfo"
	"github.com/rossoctl/context-guru/internal/metricspush"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/proxy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// startMetricsPush starts the push of metrics to the central collector and returns the sink the
// proxy hands each priced request to, and a function that flushes and stops the push.
//
// It never fails the proxy (R20). A bad setting, or an error building the exporter, logs one line
// and leaves the sink nil: the proxy then runs with no metrics. The dashboard does not depend on
// this (R8).
//
// There is no default collector URL. With no URL, or with the push off, it logs one line and returns
// a nil sink: no machine-ID file is made, no provider starts, and no request pays for a sink. The
// push needs the accountant, which needs the dashboard, so a run with no dashboard has no sink.
func startMetricsPush(cfg metricsPushSettings, prices modelinfo.Pricer, preset string) (metricsSinks, func()) {
	nop := func() {}
	switch {
	case cfg.off:
		slog.Info("context-guru: metrics push is off (--metrics-push=false); no connection is made")
		return metricsSinks{}, nop
	case strings.TrimSpace(cfg.url) == "":
		// No default URL: nothing is sent unless the operator names a collector.
		slog.Warn("context-guru: no collector URL is set (--metrics-push-url or METRICS_PUSH_URL); " +
			"no metrics are reported")
		return metricsSinks{}, nop
	case !cfg.dashboard:
		slog.Warn("context-guru: metrics need --dashboard, which is off; no metrics are reported")
		return metricsSinks{}, nop
	}
	// The flags already read the environment (METRICS_PUSH_*), so there is one reader, and an explicit
	// flag wins over the environment.
	push := metricspush.Config{
		URL: cfg.url, Token: cfg.token, Interval: cfg.interval, Disabled: cfg.off,
		Proxy: "context-guru", Version: buildinfo.Version, Preset: preset,
	}
	id, err := metricspush.MachineID(machineIDPath(cfg.dashboardDB))
	if err != nil {
		slog.Warn("context-guru: the metrics machine ID is not kept; this run uses a new one", "err", err)
	}
	push.Machine = id

	provider, shutdown, err := metricspush.Start(context.Background(), push)
	if err != nil {
		slog.Warn("context-guru: metrics push is off; the proxy runs without it", "err", err)
		return metricsSinks{}, nop
	}
	sink, err := proxy.NewMetricsSink(provider.Meter("context-guru"), prices, components.Names()...)
	if err != nil {
		slog.Warn("context-guru: metrics are off; could not build the sink", "err", err)
		stopProvider(shutdown)
		return metricsSinks{}, nop
	}
	inf, err := proxy.NewInferenceMetrics(provider.Meter("context-guru"), prices, components.Names()...)
	if err != nil {
		slog.Warn("context-guru: metrics are off; could not build the inference set", "err", err)
		stopProvider(shutdown)
		return metricsSinks{}, nop
	}
	if err := registerProxyInfo(provider.Meter("context-guru"), buildinfo.Version, preset); err != nil {
		slog.Warn("context-guru: the llm_proxy_info gauge is off", "err", err)
	}
	return metricsSinks{savings: sink, inference: inf}, func() { stopProvider(shutdown) }
}

// metricsPushSettings are the flag values that control the push.
type metricsPushSettings struct {
	dashboard   bool
	dashboardDB string
	url, token  string
	interval    time.Duration
	off         bool
}

// machineIDPath puts the install ID beside the dashboard database. An in-memory database has no
// folder, so it gets no kept ID.
func machineIDPath(dashDB string) string {
	if dashDB == "" || strings.HasPrefix(dashDB, ":memory:") || strings.HasPrefix(dashDB, "file::memory:") {
		return ""
	}
	return filepath.Join(filepath.Dir(dashDB), "machine-id")
}

// metricsSinks are the two things the proxy books a finished request into. Both are nil when the
// push is off.
type metricsSinks struct {
	savings   acmetrics.Observer
	inference *proxy.InferenceMetrics
}

// registerProxyInfo adds llm_proxy_info: a gauge that is always 1 and carries the build and the
// preset as labels (metrics spec 5.5). Alerts and dashboards join on it to see which version runs.
func registerProxyInfo(m metric.Meter, version, preset string) error {
	g, err := m.Int64ObservableGauge("llm.proxy.info", metric.WithDescription("Always 1. Shows the build and the preset."))
	if err != nil {
		return err
	}
	attrs := metric.WithAttributes(attribute.String("version", version), attribute.String("preset", preset))
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(g, 1, attrs)
		return nil
	}, g)
	return err
}

// stopProvider flushes the last totals and stops the push. A hung collector delays it by 3 seconds
// at most, on every path that stops the provider.
func stopProvider(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}
