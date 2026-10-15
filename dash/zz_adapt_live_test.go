package dash

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The adapt report against a COPY of a real dashboard database, priced from an operator price
// list. Skipped unless all of these are set:
//
//	CG_ADAPT_DB=/path/to/copy.db CG_ADAPT_TENANT=<id> CG_ADAPT_CONFIG=/path/to/config.yaml \
//	CG_ADAPT_PRICES=/path/to/prices.yaml CG_ADAPT_SINCE=<ms> CG_ADAPT_UNTIL=<ms> \
//	go test ./dash -run AdaptLive -v
//
// The database is opened in place, so point it at a copy.
func TestAdaptLive(t *testing.T) {
	path, tenant, cfg, prices := os.Getenv("CG_ADAPT_DB"), os.Getenv("CG_ADAPT_TENANT"),
		os.Getenv("CG_ADAPT_CONFIG"), os.Getenv("CG_ADAPT_PRICES")
	if path == "" || tenant == "" || cfg == "" || prices == "" {
		t.Skip("set CG_ADAPT_DB, CG_ADAPT_TENANT, CG_ADAPT_CONFIG, CG_ADAPT_PRICES")
	}
	doc, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := modelinfo.LoadTable(prices)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	since, _ := strconv.ParseInt(os.Getenv("CG_ADAPT_SINCE"), 10, 64)
	until, _ := strconv.ParseInt(os.Getenv("CG_ADAPT_UNTIL"), 10, 64)
	rep, err := db.AdaptCompaction(context.Background(), Filter{Tenant: tenant, Since: since, Until: until},
		p, string(doc), defaultAdaptDocForTest)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	if o := os.Getenv("CG_ADAPT_OUT"); o != "" {
		_ = os.WriteFile(o, out, 0o600)
	}
	t.Logf("%s", out)
}

// defaultAdaptDocForTest is tenant.DefaultConfigYAML; dash cannot import tenant.
const defaultAdaptDocForTest = `pipeline: [format, dedup, toon, cmdfilter, searchfold, textclean, extract, cachesplit, toolfilter]
components:
  extract:
    min_tokens: 400
mode: sync
`
