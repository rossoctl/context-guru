package predictive

import (
	"os"
	"testing"
)

// BenchmarkDecideLGBFull times a decision with a full-size model (set CG_BENCH_MODEL to a built model.json).
func BenchmarkDecideLGBFull(b *testing.B) {
	p := os.Getenv("CG_BENCH_MODEL")
	if p == "" {
		b.Skip("CG_BENCH_MODEL not set")
	}
	m, err := LoadModel(p)
	if err != nil {
		b.Fatal(err)
	}
	fx, _ := loadLGBFixture(b)
	e := &fx.Events[100]
	row := NewRow()
	fillFromFixture(row, fx, e)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Decide(m, row, e.econ(1))
	}
}
