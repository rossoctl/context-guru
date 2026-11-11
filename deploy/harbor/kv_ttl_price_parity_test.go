package harbor

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The Python price reader is a port of modelinfo.Table (issue #856). This test gives the same
// price file and the same model list to both and fails on any difference in the cache tiers, the write tier included.
func TestPythonPriceReaderMatchesGoTable(t *testing.T) {
	py := pythonBinFor(t, "kv_ttl_cost_model")
	if py == "" {
		t.Skip("no usable Python")
	}
	if out, err := exec.Command(py, "-c", "import yaml").CombinedOutput(); err != nil {
		t.Skipf("PyYAML missing: %s", out)
	}
	const models = `models:
  - {match: "claude-opus*", in: 5.0, out: 25.0}
  - {match: "aws/claude-sonnet-5", in: 3.0, out: 15.0}
  - {match: "aws/claude-fable-5-1*", in: 10.0, out: 50.0, cache_read: 0.7}
  - {match: "claude-haiku*", in: 1.0, out: 5.0, cache_write: 1.4}
  - {match: "gemini*", in: 2.0, out: 12.0}
  - {match: "azure/gpt-5.5", in: 2.5, out: 15.0}
  - {match: "premium", in: 4.0, out: 20.0, cache_write_1h_frac: 1.0}
  - {match: "claude-sonnet-4*", in: 3.0, out: 15.0, cache_write_1h_frac: 1.0}
  - {match: "mythos*", in: 8.0, out: 40.0, cache_read: 0.4, cache_write: 9.5}
`
	ids := []string{
		"claude-opus-5-5", "claude-opus-5", "claude-opus-5-5[1m]", "claude-opus-5-50",
		"aws/claude-sonnet-5", "claude-sonnet-5", "vertex/claude-sonnet-5", "aws/claude-sonnet-5-5", "aws/claude-fable-5-1",
		"aws/claude-fable-5-1[1m]", "claude-haiku-4-5", "claude-mythos-5-1", "gemini-3-flash-preview",
		"azure/gpt-5.5", "premium", "claude-sonnet-4-6", "nobody-knows-this-one", "",
	}
	idJSON, _ := json.Marshal(ids)
	script := `
import json, sys
sys.path.insert(0, '.')
import kv_ttl_cost_model as m
b = m.PriceBook.from_operator_file(sys.argv[1])
out = {}
for model in json.loads(sys.argv[2]):
    p = b.for_model(model)
    out[model] = [p.known, p.input * m.PER_MTOK, p.cache_read * m.PER_MTOK, p.write_5m * m.PER_MTOK, p.write_1h * m.PER_MTOK]
print(json.dumps(out))
`
	for name, head := range map[string]string{
		"plain":     "",
		"readfrac":  "cache_read_frac: 0.2\n",
		"writefrac": "cache_write_frac: 1.5\n",
		"both":      "cache_read_frac: 0.3\ncache_write_frac: 1.1\n",
	} {
		doc := head + models
		path := filepath.Join(t.TempDir(), "prices.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(py, "-c", script, path, string(idJSON)).Output()
		if err != nil {
			t.Fatalf("%s: python: %v", name, err)
		}
		var theirs map[string][]float64
		var raw map[string][]any
		if err := json.Unmarshal(out, &raw); err != nil {
			t.Fatal(err)
		}
		theirs = map[string][]float64{}
		for k, v := range raw {
			row := []float64{0}
			if v[0] == true {
				row[0] = 1
			}
			for _, x := range v[1:] {
				row = append(row, x.(float64))
			}
			theirs[k] = row
		}
		tb, err := modelinfo.ParseTable([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			p, ok := tb.Price(context.Background(), id)
			want := []float64{0, 0, 0, 0, 0}
			if ok {
				want = []float64{1, p.Input * 1e6, p.CacheRead * 1e6, p.CacheWrite * 1e6, p.CacheWrite1h() * 1e6}
			}
			got := theirs[id]
			for i := range want {
				if math.Abs(got[i]-want[i]) > 1e-9 {
					t.Errorf("%s %q field %d: python %v, go %v", name, id, i, got[i], want[i])
				}
			}
		}
	}
}
