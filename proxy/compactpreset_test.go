package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// TestCompactRowNamesThePresetThatRan.
//
// Observed live: POST /compact?preset=codesafe ran codesafe's pipeline — `collapse` fired,
// and collapse is in no other preset — while both /api/requests and the request drawer
// labelled the row "codesmart", the process default. /compact is THE offline replay and
// eval endpoint (deploy/harbor/replay.py drives it), and the point of an eval sweep is
// comparing presets, so every row carrying the default silently blended an A/B into one
// bucket with nothing to show it had.
func TestCompactRowNamesThePresetThatRan(t *testing.T) {
	agg := metrics.NewAggregator()
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{DBPath: t.TempDir() + "/d.db",
		BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		Preset: cfg.Preset, Dashboard: rec, Prices: fixedPricer{},
		// Mirrors cmd/context-guru-proxy's builder, names before preset.
		PipelineFor: func(preset string, names []string) (*components.Pipeline, error) {
			doc := "preset: " + preset + "\n"
			if len(names) > 0 {
				doc = "pipeline: [" + strings.Join(names, ",") + "]\n"
			}
			c, err := config.LoadBytes([]byte(doc))
			if err != nil {
				return nil, err
			}
			return c.Build(agg)
		},
	})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// One request per override shape, each tagged by its model so the rows can be told
	// apart without depending on insertion order.
	want := map[string]string{
		"m-default": "codesafe",  // no override: the tenant default stands
		"m-preset":  "codesmart", // ?preset= actually swapped the pipeline
		"m-header":  "custom",    // an explicit list has no preset name
		"m-unknown": "codesafe",  // build failed => the configured pipeline ran, and IS the label
	}
	for _, tc := range []struct{ model, query, header string }{
		{"m-default", "", ""},
		{"m-preset", "&preset=codesmart", ""},
		{"m-header", "", "format,textclean"},
		{"m-unknown", "&preset=nosuchpreset", ""},
	} {
		req, _ := http.NewRequest(http.MethodPost,
			srv.URL+"/compact?provider=anthropic"+tc.query,
			strings.NewReader(string(anthropicRequest(tc.model))))
		if tc.header != "" {
			req.Header.Set("x-context-guru-pipeline", tc.header)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	waitForRows(t, rec, int64(len(want)))

	page, _ := rec.DB().Requests(dash.Filter{}, 0, 10)
	for _, e := range page.Requests {
		if got := e.Preset; got != want[e.Model] {
			t.Errorf("%s: row preset = %q; want %q — the dashboard names a pipeline that did not run",
				e.Model, got, want[e.Model])
		}
	}
}
