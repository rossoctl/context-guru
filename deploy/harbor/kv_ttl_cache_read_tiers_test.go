package harbor

import (
	"os/exec"
	"testing"
)

// The Python port of the cache-read tiers carries its own unit test. Run it here so CI reaches it.
// It skips when Python or PyYAML is missing, as the drift tests do.
func TestCacheReadTiersPythonTestPasses(t *testing.T) {
	py := pythonBinFor(t, "kv_ttl_cost_model")
	if py == "" {
		t.Skip("no usable Python")
	}
	if out, err := exec.Command(py, "-c", "import yaml").CombinedOutput(); err != nil {
		t.Skipf("PyYAML missing: %s", out)
	}
	if out, err := exec.Command(py, "test_cache_read_tiers.py").CombinedOutput(); err != nil {
		t.Fatalf("test_cache_read_tiers.py failed: %v\n%s", err, out)
	}
}
