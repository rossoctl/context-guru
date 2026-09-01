package dash

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Benchmark ingestion reads the artifacts deploy/harbor/*.py already writes —
// summary.json plus one rows-<arm>.json per config — so there is no new export
// format to maintain and every historical run in /tmp/*-runs is already ingestible.
//
// A run is identified by its DIRECTORY NAME, and re-ingesting replaces it. That
// makes ingestion idempotent: pointing the proxy at a jobs root and restarting it
// cannot double-count a run.

// harborSummary is the subset of summary.json we read. Unknown fields are kept
// verbatim in the stored blob so the UI can show anything the harness reports
// without a schema change here.
type harborSummary struct {
	Model   string            `json:"model"`
	Dataset string            `json:"dataset"`
	Configs []json.RawMessage `json:"configs"`
}

// harborRow is one task's trial row from rows-<arm>.json.
type harborRow struct {
	Task             string  `json:"task"`
	Reward           float64 `json:"reward"`
	Steps            int     `json:"steps"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheRead        int64   `json:"cache_read"`
	CacheWrite       int64   `json:"cache_write"`
	FreshInput       int64   `json:"fresh_input"`
	AgentCost        float64 `json:"agent_cost"`
	NormCost         float64 `json:"norm_cost"`
	WallS            float64 `json:"wall_s"`
	Exception        bool    `json:"exception"`
}

// IngestBenchDir ingests one run directory (summary.json + rows-*.json). A
// directory with neither is skipped without error — scanning a jobs root full of
// in-progress runs must not fail.
func (d *DB) IngestBenchDir(dir string) (tasks int, err error) {
	name := filepath.Base(strings.TrimRight(dir, string(os.PathSeparator)))
	rowFiles, _ := filepath.Glob(filepath.Join(dir, "rows-*.json"))
	summaryPath := filepath.Join(dir, "summary.json")
	sumBytes, sumErr := os.ReadFile(summaryPath)
	if sumErr != nil && len(rowFiles) == 0 {
		return 0, nil // not a run directory
	}

	var sum harborSummary
	if len(sumBytes) > 0 {
		if err := json.Unmarshal(sumBytes, &sum); err != nil {
			return 0, fmt.Errorf("dash: %s/summary.json: %w", name, err)
		}
	} else {
		sumBytes = []byte("{}")
	}
	ts := time.Now().UnixMilli()
	if fi, err := os.Stat(summaryPath); err == nil {
		ts = fi.ModTime().UnixMilli()
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// Replace-on-reingest: delete the old run (cascading its tasks) then insert.
	if _, err := tx.Exec(`DELETE FROM bench_runs WHERE name = ?`, name); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO bench_runs(name, ts, dataset, model, summary) VALUES (?,?,?,?,?)`,
		name, ts, sum.Dataset, sum.Model, string(sumBytes))
	if err != nil {
		return 0, err
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT INTO bench_tasks(run_id, arm, task, reward, steps,
		prompt_tokens, completion_tokens, cache_read, cache_write, fresh_input,
		cost_usd, norm_cost_usd, wall_s, exception) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	sort.Strings(rowFiles)
	for _, rf := range rowFiles {
		arm := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(rf), "rows-"), ".json")
		b, err := os.ReadFile(rf)
		if err != nil {
			continue
		}
		var rows []harborRow
		if err := json.Unmarshal(b, &rows); err != nil {
			continue // a partially-written rows file must not abort the whole ingest
		}
		for _, r := range rows {
			if _, err := stmt.Exec(runID, arm, r.Task, r.Reward, r.Steps,
				r.PromptTokens, r.CompletionTokens, r.CacheRead, r.CacheWrite, r.FreshInput,
				r.AgentCost, r.NormCost, r.WallS, boolInt(r.Exception)); err != nil {
				return 0, err
			}
			tasks++
		}
	}
	// Commit the run row ONLY if it actually gained tasks. Committing first and then
	// returning tasks=0 is how the log line ("ingested benchmark runs runs=17") came to
	// disagree with the API (42 rows, 25 of them with no arms at all): the callers below
	// count a run only when tasks>0, but the row was already committed, so the
	// Benchmarks tab filled up with contentless shells. The deferred Rollback discards
	// it, which also means a directory that stops parsing does not silently replace a
	// previously-good run with an empty one.
	if tasks == 0 {
		return 0, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return tasks, nil
}

// IngestBenchRoots scans each root one level deep for run directories and ingests
// every one it finds. Errors are collected per-directory and do not abort the scan.
func (d *DB) IngestBenchRoots(roots []string) (runs, tasks int) {
	for _, root := range roots {
		// A root may itself be a run directory.
		if n, err := d.IngestBenchDir(root); err == nil && n > 0 {
			runs++
			tasks += n
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			n, err := d.IngestBenchDir(filepath.Join(root, e.Name()))
			if err != nil || n == 0 {
				continue
			}
			runs++
			tasks += n
		}
	}
	return runs, tasks
}

// BenchRun is one ingested run as served to the UI.
type BenchRun struct {
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	TS      int64           `json:"ts"`
	Dataset string          `json:"dataset"`
	Model   string          `json:"model"`
	Summary json.RawMessage `json:"summary"`
	Arms    []BenchArm      `json:"arms"`
}

// BenchArm aggregates one arm (baseline / context-guru / headroom / rtk) of a run.
// This is the cost-vs-reward view: an arm that saves money by failing tasks is not
// saving money, so reward sits beside cost in the same row.
type BenchArm struct {
	Arm              string  `json:"arm"`
	Tasks            int64   `json:"tasks"`
	Scored           int64   `json:"scored"`
	Solved           int64   `json:"solved"`
	SolveRate        float64 `json:"solve_rate"`
	MeanReward       float64 `json:"mean_reward"`
	MeanSteps        float64 `json:"mean_steps"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	MeanCostUSD      float64 `json:"mean_cost_usd"`
	TotalNormCostUSD float64 `json:"total_norm_cost_usd"`
	CacheRead        int64   `json:"cache_read"`
	CacheWrite       int64   `json:"cache_write"`
	FreshInput       int64   `json:"fresh_input"`
	Completion       int64   `json:"completion_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	MeanWallS        float64 `json:"mean_wall_s"`
	Exceptions       int64   `json:"exceptions"`
	// CostRows is how many of this arm's tasks carry a non-zero cost, and it exists so the UI
	// can tell "this arm was free" apart from "the ingester could not find a cost in the file".
	//
	// harborRow.AgentCost binds to `agent_cost`. A rows file that names its cost anything else
	// unmarshals to the zero value silently, every task lands at 0, and the arm then renders
	// $0 / $0 / $0 and sorts as the CHEAPEST — a flattering lie with no way for a reader to see
	// it. A float64 cannot express absence, and making the column nullable would spread that
	// through five call sites; counting the rows that priced is the same information one join
	// cheaper. An arm that ran tasks and cost exactly nothing on every one of them is not a
	// measurement either, so `n/a` is the honest render in both cases.
	CostRows int64 `json:"cost_rows"`
}

// BenchRuns returns every ingested run with its per-arm aggregates.
func (d *DB) BenchRuns() ([]*BenchRun, error) {
	rows, err := d.sql.Query(`SELECT id, name, ts, dataset, model, summary FROM bench_runs ORDER BY ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*BenchRun{}
	for rows.Next() {
		var r BenchRun
		var summary string
		if err := rows.Scan(&r.ID, &r.Name, &r.TS, &r.Dataset, &r.Model, &summary); err != nil {
			return nil, err
		}
		r.Summary = json.RawMessage(summary)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		arms, err := d.benchArms(r.ID)
		if err != nil {
			return nil, err
		}
		r.Arms = arms
	}
	return out, nil
}

func (d *DB) benchArms(runID int64) ([]BenchArm, error) {
	rows, err := d.sql.Query(`SELECT arm, COUNT(*),
		SUM(CASE WHEN exception = 0 THEN 1 ELSE 0 END),
		SUM(CASE WHEN reward >= 1 THEN 1 ELSE 0 END),
		AVG(reward), AVG(steps), SUM(cost_usd), AVG(cost_usd), SUM(norm_cost_usd),
		SUM(cache_read), SUM(cache_write), SUM(fresh_input), SUM(completion_tokens),
		AVG(wall_s), SUM(exception),
		SUM(CASE WHEN cost_usd > 0 THEN 1 ELSE 0 END)
		FROM bench_tasks WHERE run_id = ? GROUP BY arm ORDER BY arm`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BenchArm
	for rows.Next() {
		var a BenchArm
		if err := rows.Scan(&a.Arm, &a.Tasks, &a.Scored, &a.Solved, &a.MeanReward, &a.MeanSteps,
			&a.TotalCostUSD, &a.MeanCostUSD, &a.TotalNormCostUSD,
			&a.CacheRead, &a.CacheWrite, &a.FreshInput, &a.Completion,
			&a.MeanWallS, &a.Exceptions, &a.CostRows); err != nil {
			return nil, err
		}
		if a.Scored > 0 {
			a.SolveRate = float64(a.Solved) / float64(a.Scored)
		}
		if total := a.CacheRead + a.CacheWrite + a.FreshInput; total > 0 {
			a.CacheHitRate = float64(a.CacheRead) / float64(total)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// BenchTask is one task row for the per-task drill-down.
type BenchTask struct {
	Arm         string  `json:"arm"`
	Task        string  `json:"task"`
	Reward      float64 `json:"reward"`
	Steps       int64   `json:"steps"`
	CacheRead   int64   `json:"cache_read"`
	CacheWrite  int64   `json:"cache_write"`
	FreshInput  int64   `json:"fresh_input"`
	Completion  int64   `json:"completion_tokens"`
	CostUSD     float64 `json:"cost_usd"`
	NormCostUSD float64 `json:"norm_cost_usd"`
	WallS       float64 `json:"wall_s"`
	Exception   bool    `json:"exception"`
}

// BenchTasks returns a run's task rows, optionally restricted to one arm.
func (d *DB) BenchTasks(runID int64, arm string) ([]*BenchTask, error) {
	q := `SELECT arm, task, reward, steps, cache_read, cache_write, fresh_input,
		completion_tokens, cost_usd, norm_cost_usd, wall_s, exception
		FROM bench_tasks WHERE run_id = ?`
	args := []any{runID}
	if arm != "" {
		q += " AND arm = ?"
		args = append(args, arm)
	}
	q += " ORDER BY task, arm"
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*BenchTask{}
	for rows.Next() {
		var t BenchTask
		var exc int
		if err := rows.Scan(&t.Arm, &t.Task, &t.Reward, &t.Steps, &t.CacheRead, &t.CacheWrite,
			&t.FreshInput, &t.Completion, &t.CostUSD, &t.NormCostUSD, &t.WallS, &exc); err != nil {
			return nil, err
		}
		t.Exception = exc != 0
		out = append(out, &t)
	}
	return out, rows.Err()
}
