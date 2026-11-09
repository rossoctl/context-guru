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
)

// startMetricsPush starts the push of metrics to the central collector and returns the sink the
// proxy hands each priced request to, and a function that flushes and stops the push.
//
// It never fails the proxy (R20). A bad setting, or an error building the exporter, logs one line
// and leaves the sink nil: the proxy then runs with no metrics. The dashboard does not depend on
// this (R8).
//
// The push needs the accountant, which needs the dashboard, so a run with no dashboard has no sink
// and makes no connection.
func startMetricsPush(cfg metricsPushSettings, prices modelinfo.Pricer, preset string) (acmetrics.Observer, func()) {
	nop := func() {}
	if !cfg.dashboard {
		return nil, nop
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
		return nil, nop
	}
	sink, err := proxy.NewMetricsSink(provider.Meter("context-guru"), prices, components.Names()...)
	if err != nil {
		slog.Warn("context-guru: metrics are off; could not build the sink", "err", err)
		_ = shutdown(context.Background())
		return nil, nop
	}
	return sink, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = shutdown(ctx) // a hung collector delays the exit by 3 seconds at most
	}
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
