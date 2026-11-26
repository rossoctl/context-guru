package predictive

import "testing"

// The three cost forms that the model reads (the Decide stake, and the stake, read_cost and rebuild_cost
// features) come from the accountant now. They must give the exact bits of the formulas that the model
// was trained on: prefix x (write - read), prefix x read, prefix x write.
func TestModelCostFormsAreBitIdentical(t *testing.T) {
	prefixes := []int64{1, 7, 1000, 48_123, 80_000, 271_993, 272_001, 1_000_000}
	rates := [][2]float64{{0.3e-6, 3.75e-6}, {1e-7, 1.25e-6}, {0.5e-6, 5e-6}, {1e-6, 10e-6}, {0.1e-6, 1.25e-6}, {1.5e-7, 6.25e-7}}
	for _, p := range prefixes {
		for _, r := range rates {
			read, write := r[0], r[1]
			if got, want := missUSD(p, read, write), float64(p)*(write-read); got != want {
				t.Errorf("stake(%d, %v, %v) = %v, want %v", p, read, write, got, want)
			}
			if got, want := readUSD(p, read), float64(p)*read; got != want {
				t.Errorf("read_cost(%d, %v) = %v, want %v", p, read, got, want)
			}
			if got, want := rebuildUSD(p, write), float64(p)*write; got != want {
				t.Errorf("rebuild_cost(%d, %v) = %v, want %v", p, write, got, want)
			}
		}
	}
}

// A row with no price has no stake. Rows that reach the model are priced (write above read), so the
// floor at 0 never changes a value the model reads.
func TestModelStakeFloorsAtZero(t *testing.T) {
	if got := missUSD(1000, 3e-6, 1e-6); got != 0 {
		t.Errorf("write below read: %v, want 0", got)
	}
}
