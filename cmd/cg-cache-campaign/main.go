// cg-cache-campaign reads a dashboard database READ-ONLY and writes two files:
//
//   - a campaign file the dashboard's Campaigns tab uploads as-is: the best keep-alive arm for
//     every (account, hour of day), in exactly the GET /api/kvcache/suggest format;
//   - a report: what every policy would have cost over the same history — the per-cell winners
//     (the campaign), every single arm applied to everything (the naive policies), what actually
//     ran (observed-policy), and the exact ceiling (optimal) — plus the same comparison broken
//     down by account, hour, weekday, model and agent, and an out-of-sample check of the campaign.
//
// NOTHING HERE DECIDES OR PRICES ANYTHING. Every number comes from core: dash.OpenReadOnly reads the
// file, dash.DB.KVCacheSuggest picks the per-cell winners, dash.DB.KVCacheSuggestHoldout scores them
// out of sample, and kvcache.Simulate replays every arm with kvcache's own registry and price list.
// A second copy of any of that here would agree with itself rather than with what the dashboard and
// the live campaign enforce.
//
// SAFE BESIDE A LIVE PROXY. OpenReadOnly opens the file with SQLite mode=ro and query_only, runs no
// migration and no checkpoint, and holds at most 4 connections.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/proxyapp"
)

func main() {
	dbPath := flag.String("db", "", "dashboard database (cg.db); opened read-only")
	days := flag.Int("days", 30, "history to read, in days back from now")
	out := flag.String("out", "campaign.json", "campaign file to upload in the dashboard's Campaigns tab")
	report := flag.String("report", "report.json", "report with every policy's total and the breakdowns")
	md := flag.String("md", "", "also write the report as Markdown here (optional)")
	baseline := flag.String("baseline", "", "arm every saving is measured against (default: the registry's baseline, fixed-5m)")
	holdout := flag.Float64("holdout", 0.3, "share of the window, at its end, held out to score the campaign out of sample (0 = skip)")
	prices := flag.String("prices", "", "MODEL_PRICES table, as the proxy reads it (optional)")
	offline := flag.Bool("offline", false, "do not fetch the public model price map; use the compiled-in table only")
	flag.Parse()
	if *dbPath == "" {
		log.Fatal("-db is required")
	}

	db, err := dash.OpenReadOnly(*dbPath)
	if err != nil {
		log.Fatalf("open %s read-only: %v", *dbPath, err)
	}
	defer db.Close()

	now := time.Now()
	in := Inputs{
		DB:       db,
		Pricer:   pricer(*prices, *offline),
		Since:    now.Add(-time.Duration(*days) * 24 * time.Hour).UnixMilli(),
		Until:    now.UnixMilli(),
		Baseline: *baseline,
		Holdout:  *holdout,
	}
	res, err := Run(in)
	if err != nil {
		log.Fatal(err)
	}
	b, err := json.Marshal(res.Campaign)
	must(err)
	if len(b) > MaxUpload {
		log.Fatalf("the campaign file is %d KiB and the dashboard accepts at most %d KiB; read fewer days "+
			"or split it by account", len(b)>>10, MaxUpload>>10)
	}
	must(writeJSON(*out, res.Campaign))
	must(writeJSON(*report, res.Report))
	if *md != "" {
		must(os.WriteFile(*md, []byte(Markdown(res.Report)), 0o600))
	}
	r := res.Report
	fmt.Printf("read %d requests (%d accounts, %d cells) from %s to %s\n", r.Requests, len(r.Accounts),
		len(res.Campaign.Cells), time.UnixMilli(r.Since).UTC().Format(time.DateOnly), time.UnixMilli(r.Until).UTC().Format(time.DateOnly))
	for _, p := range r.Policies {
		fmt.Printf("  %-26s $%10.2f  saves $%9.2f vs %s\n", p.Name, p.CostUSD, p.SavingUSD, r.Baseline)
	}
	fmt.Printf("wrote %s (upload this) and %s\n", *out, *report)
}

// pricer builds the same resolver chain the proxy does: an operator's MODEL_PRICES table first,
// then the public LiteLLM map, then the compiled-in table. -offline drops the network fetch.
func pricer(table string, offline bool) proxyapp.Pricer {
	chain := proxyapp.Chain{}
	if strings.TrimSpace(table) != "" {
		t, err := proxyapp.LoadTable(table)
		if err != nil {
			log.Fatalf("-prices %s: %v", table, err)
		}
		chain = append(chain, t)
	}
	if !offline {
		// LOADED BEFORE USE, not in the background. The resolver normally fetches the public map
		// asynchronously, and this tool asks its first question at once: it priced every cell of
		// the first run on the compiled-in table alone, which does not know the gateway's model
		// names, and wrote an upload of 121 cells that were all unpriced.
		live := proxyapp.NewLiteLLM("", nil, 0)
		if err := live.Load(context.Background()); err != nil {
			log.Fatalf("could not load the public model price map (%v); retry, or pass -prices with "+
				"a MODEL_PRICES table, or -offline to price on the compiled-in table only", err)
		}
		chain = append(chain, live)
	}
	return append(chain, proxyapp.DefaultStatic())
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
