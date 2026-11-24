package accrec

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
)

// Every registered component is on the list, and no name outside the registry has since been
// registered.
func TestMetricComponentsHoldTheRegistry(t *testing.T) {
	got, names := MetricComponents(), components.Names()
	if len(names) == 0 {
		t.Fatal("the registry is empty; the test imports components/all to fill it")
	}
	for _, n := range names {
		if !slices.Contains(got, n) {
			t.Errorf("registered component %q is not on the metrics allow-list", n)
		}
	}
	for _, n := range accwire.OperatorsOutsideRegistry() {
		if slices.Contains(names, n) {
			t.Errorf("%q is now a registered component; drop it from accwire.OperatorsOutsideRegistry", n)
		}
	}
}

var (
	// An operator name written as a literal instead of through accwire.Operator or an accwire.Op* constant.
	literalOperator = regexp.MustCompile(`"context-guru\.[a-z_]+"|Operator:\s*"[^"]+"`)
	// A bare name given to a helper that prefixes it.
	literalHelperArg = regexp.MustCompile(`(?:envelopeOpID?|MessageOps|accwire\.Operator|[^.]\bOperator)\("([a-z_]+)"`)
	// An accwire.Op* constant.
	opConst = regexp.MustCompile(`(?m)^\s*(Op[A-Z]\w*)\s*=\s*"([^"]+)"`)
)

// Each operator the code books is on the allow-list. The emit sites use accwire's constants, and
// this test fails when a site writes a name by hand, or when a new accwire.Op* constant is left off
// OperatorsOutsideRegistry. Without it, a new operator would reach Prometheus as component="other".
func TestEveryBookedOperatorIsOnTheAllowList(t *testing.T) {
	allowed := MetricComponents()
	root := filepath.Join("..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n != ".." && (strings.HasPrefix(n, ".") || n == "vendor" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := literalOperator.FindString(line); m != "" {
				t.Errorf("%s: operator written by hand (%s); use accwire.Operator and an accwire.Op* constant", path, m)
			}
			for _, m := range literalHelperArg.FindAllStringSubmatch(line, -1) {
				if !slices.Contains(allowed, m[1]) {
					t.Errorf("%s: operator %q is booked but not on the metrics allow-list", path, m[1])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("..", "accwire", "operators.go"))
	if err != nil {
		t.Fatal(err)
	}
	consts := opConst.FindAllStringSubmatch(string(src), -1)
	if len(consts) == 0 {
		t.Fatal("no accwire.Op* constant found; the pattern is out of date")
	}
	for _, m := range consts {
		if !slices.Contains(accwire.OperatorsOutsideRegistry(), m[2]) {
			t.Errorf("accwire.%s = %q is not in OperatorsOutsideRegistry", m[1], m[2])
		}
	}
}

// A sink built with MetricComponents labels each operator outside the registry with its own name,
// not "other".
func TestOperatorsOutsideRegistryKeepTheirComponentLabel(t *testing.T) {
	reader := sdk.NewManualReader()
	sink, err := acmetrics.New(sdk.NewMeterProvider(sdk.WithReader(reader)).Meter("t"), zeroPricer{}, acmetrics.WithComponents(MetricComponents()...))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range accwire.OperatorsOutsideRegistry() {
		sink.Observe(acmetrics.Observation{
			Meta: accountant.RequestMeta{Model: "m", Billing: accountant.Billing{Mode: accountant.BillingAPI}},
			Result: accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: accwire.Operator(name),
				Saved: accountant.TierTokens{Input: 1}, Caused: accountant.TierTokens{Input: 1}}}},
		})
	}
	labels := componentLabels(t, reader)
	for _, name := range accwire.OperatorsOutsideRegistry() {
		if !labels[name] {
			t.Errorf("operator %q has no component=%q series; labels seen: %v", name, name, labels)
		}
	}
	if labels["other"] {
		t.Errorf("an operator on the allow-list was pushed as component=\"other\": %v", labels)
	}
}

// zeroPricer prices every tier at 0, marked estimated: the test reads labels, not money.
type zeroPricer struct{}

func (zeroPricer) Price(string, time.Time, acmetrics.Tier) (float64, bool) { return 0, true }

func componentLabels(t *testing.T, r *sdk.ManualReader) map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if d, ok := m.Data.(metricdata.Sum[float64]); ok {
				for _, dp := range d.DataPoints {
					if v, ok := dp.Attributes.Value("component"); ok {
						out[v.AsString()] = true
					}
				}
			}
			if d, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range d.DataPoints {
					if v, ok := dp.Attributes.Value("component"); ok {
						out[v.AsString()] = true
					}
				}
			}
		}
	}
	return out
}
