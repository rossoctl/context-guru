package dash

import (
	"os"
	"testing"
)

// TestWriteDemoDB writes the golden fixture to $CG_DEMO_DB so a dashboard can be started on
// synthetic data (screenshots for a public change). It does nothing unless the variable is set.
func TestWriteDemoDB(t *testing.T) {
	path := os.Getenv("CG_DEMO_DB")
	if path == "" {
		t.Skip("set CG_DEMO_DB to write the synthetic fixture database")
	}
	goldenAPIAt(t, path)
}
