package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// notes are the caveats a reader of the report needs, stated once.
func notes(r *Report) []string {
	n := []string{
		"Every figure is core's own replay (dash.KVCacheSuggest / KVCacheSuggestBy / KVCacheSuggestHoldout). " +
			"This tool only sums cells.",
		"Rows are Sunday-Thursday, hours and weekdays are UTC: the suggester's own filter.",
		"Each cell is replayed on its own rows only. A naive policy is one arm in every cell, so all " +
			"policies are measured on the same cells.",
		"optimal reads each request's true next-request time. No policy can reach it; it is the ceiling.",
		"observed-policy replays the cache tier each request asked for. The proxy's own keep-alive pings are " +
			"measured separately, in current_keepalive.",
		"campaign-by-<key> picks a winner per (account, hour, key). The upload format has no such key, " +
			"so it shows what that key is worth, not something the dashboard can enforce today.",
	}
	if r.Holdout != nil && r.Holdout.RetentionKnown {
		n = append(n, fmt.Sprintf("Out of sample, the campaign kept %.0f%% of its in-sample saving "+
			"(%d comparable cells).", r.Holdout.RetentionPct, r.Holdout.ComparableCells))
	}
	return n
}

// Markdown renders the report for a person.
func Markdown(r *Report) string {
	var b strings.Builder
	day := func(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.DateOnly) }
	from, to := r.Since, r.Until
	if r.HistorySince > 0 {
		from, to = r.HistorySince, r.HistoryUntil // what was actually read, after retention
	}
	fmt.Fprintf(&b, "# Cache campaign report\n\n%s to %s (history actually retained) · %d requests · %d accounts · baseline `%s`\n\n",
		day(from), day(to), r.Requests, len(r.Accounts), r.Baseline)

	b.WriteString("## Every policy\n\n| Policy | Kind | Cost | Saving vs baseline | % | Of optimal |\n|---|---|---:|---:|---:|---:|\n")
	for _, p := range r.Policies {
		fmt.Fprintf(&b, "| `%s` | %s | $%.2f | $%.2f | %.1f%% | %.0f%% |\n",
			p.Name, p.Kind, p.CostUSD, p.SavingUSD, p.SavingPct, p.OfOptimalPct)
	}

	c := r.Campaign
	fmt.Fprintf(&b, "\n## The upload\n\n%d cells. The dashboard will enforce %d of them, predicted saving **$%.2f**.\n\n",
		c.Cells, c.Enforceable, c.PredictedSavingUSD)
	b.WriteString("| Arm | Cells |\n|---|---:|\n")
	for _, k := range sortedKeys(c.ByArm) {
		fmt.Fprintf(&b, "| `%s` | %d |\n", k, c.ByArm[k])
	}
	if len(c.NotEnforced) > 0 {
		b.WriteString("\nRecorded but not enforced:\n\n")
		for _, k := range sortedKeys(c.NotEnforced) {
			fmt.Fprintf(&b, "- %d × %s\n", c.NotEnforced[k], k)
		}
	}

	if k := r.Current; k != nil {
		fmt.Fprintf(&b, "\n## What ran: the live keep-alive\n\nMeasured by the proxy over the same span "+
			"(core's keep-alive ledger): %d pings cost $%.2f, prevented up to %d misses worth $%.2f, net **$%.2f**. "+
			"`observed-policy` above replays the tier each request asked for; it does not include these pings.\n",
			k.Pings, k.PingUSD, k.MissesAvoided, k.SavedUSD, k.NetUSD)
	}

	if h := r.Holdout; h != nil {
		fmt.Fprintf(&b, "\n## Out of sample\n\nChosen on the first part of the window, scored on the rest: "+
			"in-sample $%.2f, out of sample $%.2f", h.TotalTrainSavingUSD, h.TotalTestSavingUSD)
		if h.RetentionKnown {
			fmt.Fprintf(&b, " (**%.0f%%** kept, %d comparable cells)", h.RetentionPct, h.ComparableCells)
		}
		b.WriteString(".\n")
	}

	for _, dim := range []string{"account", "hour_utc", "weekday", "model", "agent"} {
		rows := r.Breakdowns[dim]
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## By %s\n\n| %s | Requests | Baseline | Campaign | Observed | Optimal | Best single arm |\n|---|---:|---:|---:|---:|---:|---|\n", dim, dim)
		for _, x := range rows {
			fmt.Fprintf(&b, "| %s | %d | $%.2f | $%.2f | $%.2f | $%.2f | `%s` $%.2f |\n",
				x.Key, x.Requests, x.BaselineUSD, x.CampaignUSD, x.ObservedUSD, x.OptimalUSD, x.BestArm, x.BestArmUSD)
		}
	}

	b.WriteString("\n## Notes\n\n")
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	return b.String()
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
