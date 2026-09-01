package dash

import (
	"fmt"
	"sort"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The per-user, per-hour suggester's train/test split.
//
// # Why this exists at all: every prediction on the KV-cache and Campaigns pages is in-sample
//
// KVCacheSuggest picks each (user, hour) cell's winning arm by replaying every candidate
// over that cell's own rows and taking the argmax — then reports THAT REPLAY'S saving as
// the cell's predicted saving. The arm is chosen to maximise the very number that is
// then presented as its prediction, over exactly the rows it was chosen on. That is a
// measure of fit, not of forecast: it is biased upward by construction, and the bias
// grows as a cell gets thinner, because the more arms compete over the fewer requests
// the more likely one of them wins on noise alone.
//
// A campaign then freezes that figure as PredictedUSD (tenant/campaign.go) and the
// Campaigns tab shows it beside a real, live measurement — inviting exactly the
// comparison the frozen number is least able to support. Nothing was WRONG in that
// chain; every function did what its doc comment said. What was missing is any way to
// ask how much of a cell's predicted saving survives contact with rows it was not
// chosen on.
//
// This answers that, and only that: choose on a TRAIN window, score on a disjoint TEST
// window, report both. The gap between them is this deployment's own overfitting
// estimate — not a correction to apply to the prediction, which would be inventing a
// number, but the measurement a reader needs before trusting one.
//
// # Why it is built from two KVCacheSuggest calls and no new simulation code
//
// A suggest cell already carries every candidate's saving over its own rows
// (KVCacheSuggestion.Candidates), not just the winner's. So "the arm train picked,
// scored on test rows" is a LOOKUP in the test window's own candidate list, not a new
// replay: run the existing suggester over each window and join on (user, hour, arm).
// Both halves are then produced by the same code path that produces the live page, which
// is the property that matters — a second, holdout-specific simulator could drift from
// the one whose numbers a campaign actually freezes, and the comparison would quietly
// stop meaning anything.
//
// # Why all four bounds are required, unlike every other filter in this package
//
// dash.Filter treats 0 as unbounded and the dashboard's own range control documents that
// "either bound alone is a legitimate window". Here an unbounded train window would
// include the test period, so the arm would be chosen partly on the rows it is then
// scored on — a split that silently isn't one, reported as though it were. There is no
// safe default for a bound whose absence destroys the only thing the view measures, so
// an absent one is an error rather than a fallback.

// KVCacheHoldoutArm is one arm's saving on the train window beside its saving on the test
// window, for one (user, hour) cell.
type KVCacheHoldoutArm struct {
	Strategy string `json:"strategy"`
	// TrainSavingUSD/TestSavingUSD are this arm's saving over the SAME BASELINE in each
	// window (kvcache.Compare's AbsoluteUSD). Negative where the arm costs more than the
	// baseline, and reported that way — see kvcache.Savings.AbsoluteUSD on why a
	// comparison that clamps stops being one.
	TrainSavingUSD float64 `json:"train_saving_usd"`
	TrainKnown     bool    `json:"train_known"`
	TestSavingUSD  float64 `json:"test_saving_usd"`
	// TestKnown is false where this arm has no priced result in the test window at all —
	// the cell had no test traffic, or nothing in it could be priced. The field is then
	// meaningless and the UI must render "not enough data", never 0.00.
	TestKnown bool `json:"test_known"`
	// Chosen marks the arm the TRAIN window's own suggester picked as this cell's winner:
	// the one a campaign built from the train window would actually have enforced.
	Chosen bool `json:"chosen"`
}

// KVCacheHoldoutCell is one (user, hour-of-day) cell's train-chosen arm, scored on
// held-out rows.
type KVCacheHoldoutCell struct {
	User    string `json:"user"`
	HourUTC int    `json:"hour_utc"`

	// TrainRequests/TestRequests are the two populations this cell was decided from and
	// scored on. Both are Sunday-Thursday only, the same filter KVCacheSuggest applies —
	// see its own doc comment on why the weekend is not read at all.
	TrainRequests int64 `json:"train_requests"`
	TestRequests  int64 `json:"test_requests"`
	// TrainInsufficientData/TestInsufficientData mirror KVCacheSuggestion.InsufficientData
	// on each side: below kvSuggestMinRequests the cell still reports every number, but
	// acting on it as a pattern is what the flag warns against.
	TrainInsufficientData bool `json:"train_insufficient_data"`
	TestInsufficientData  bool `json:"test_insufficient_data"`

	// Arm is the arm the train window chose — what a campaign created from that window
	// would have enforced for this cell.
	Arm string `json:"arm"`
	// TrainSavingUSD is that arm's IN-SAMPLE saving: chosen on these rows, scored on these
	// rows. This is the figure the live KV-cache page and a campaign's PredictedUSD both
	// show today.
	TrainSavingUSD float64 `json:"train_saving_usd"`
	TrainKnown     bool    `json:"train_known"`
	// TestSavingUSD is the same arm's saving on the test window's rows — out of sample,
	// the honest forecast. TestKnown is false where the test window has nothing to score
	// it on; the number is then absent, never a zero standing in for one.
	TestSavingUSD float64 `json:"test_saving_usd"`
	TestKnown     bool    `json:"test_known"`
	// TestNote says why TestKnown is false, so a blank cell is explained rather than just
	// blank. Empty when TestKnown is true.
	TestNote string `json:"test_note,omitempty"`

	// Arms is every candidate's train-and-test pair for this cell, the train-chosen one
	// among them and flagged — so a reader can see whether the arm that won on train was
	// anywhere near the best on test, which is the whole question this view exists for.
	Arms []KVCacheHoldoutArm `json:"arms"`
}

// KVCacheHoldout is the whole /api/kvcache/suggest/holdout payload.
type KVCacheHoldout struct {
	Baseline    string   `json:"baseline"`
	Weekdays    []string `json:"weekdays_included"`
	TimeZone    string   `json:"time_zone"`
	MinRequests int      `json:"min_requests"`

	TrainSince int64 `json:"train_since"`
	TrainUntil int64 `json:"train_until"`
	TestSince  int64 `json:"test_since"`
	TestUntil  int64 `json:"test_until"`

	Users []string             `json:"users"`
	Cells []KVCacheHoldoutCell `json:"cells"`

	// TotalTrainSavingUSD and TotalTestSavingUSD sum the CHOSEN arm over exactly the cells
	// where BOTH sides are known and neither side is below the request floor — the only
	// population where the two totals describe the same thing. ComparableCells is that
	// population's size, reported so the totals are never read as covering every cell.
	TotalTrainSavingUSD float64 `json:"total_train_saving_usd"`
	TotalTestSavingUSD  float64 `json:"total_test_saving_usd"`
	ComparableCells     int     `json:"comparable_cells"`
	// TotalCells is every cell the train window produced, comparable or not, so the gap
	// between it and ComparableCells is visible rather than inferred.
	TotalCells int `json:"total_cells"`
	// RetentionPct is TotalTestSavingUSD / TotalTrainSavingUSD × 100: how much of the
	// in-sample predicted saving survived contact with rows the arm was not chosen on.
	// Below 100 is the expected direction; it can be negative, when the chosen arms cost
	// more than the baseline out of sample, and that is reported rather than clamped for
	// the same reason kvcache.Savings.AbsoluteUSD is.
	//
	// Computed here rather than left to the caller because the two conditions that make it
	// undefined — no comparable cells at all, and a train total of exactly zero (a ratio
	// against nothing is not 0%, it is undefined, the same rule kvcache.Savings.Known
	// already encodes for percentages) — are properties of this data, not of whoever
	// renders it. A UI that divides these two totals itself has to rediscover both, and
	// dash/ui/campaigns.js deliberately does no arithmetic precisely so that it cannot get
	// this wrong.
	RetentionPct   float64 `json:"retention_pct"`
	RetentionKnown bool    `json:"retention_known"`

	Notes []string `json:"notes"`
}

// KVCacheSuggestHoldout runs the suggester over a train window and a disjoint test
// window, and joins them per (user, hour): the arm train chose, and what that same arm
// actually saved on rows it was not chosen on.
//
// f carries the scope (tenant, model, agent…) and is applied to BOTH windows — only the
// time bounds differ between them, so the two populations are the same slice of traffic
// observed over two periods rather than two different slices.
func (d *DB) KVCacheSuggestHoldout(f Filter, o KVCacheOptions, p modelinfo.Pricer,
	cfg KVCacheSimConfig, train, test Window) (*KVCacheHoldout, error) {
	if err := validHoldoutWindows(train, test); err != nil {
		return nil, err
	}

	trainF, testF := f, f
	trainF.Since, trainF.Until = train.Since, train.Until
	testF.Since, testF.Until = test.Since, test.Until

	trainOut, err := d.KVCacheSuggest(trainF, o, p, cfg)
	if err != nil {
		return nil, err
	}
	testOut, err := d.KVCacheSuggest(testF, o, p, cfg)
	if err != nil {
		return nil, err
	}

	// Indexed by (user, hour): the test window's cells, so each train cell can look up its
	// own counterpart and that counterpart's per-arm savings.
	type cellKey struct {
		user string
		hour int
	}
	testCells := make(map[cellKey]KVCacheSuggestion, len(testOut.Cells))
	for _, c := range testOut.Cells {
		testCells[cellKey{c.User, c.HourUTC}] = c
	}

	out := &KVCacheHoldout{
		Baseline: trainOut.Baseline, Weekdays: trainOut.Weekdays, TimeZone: trainOut.TimeZone,
		MinRequests: trainOut.MinRequests,
		TrainSince:  train.Since, TrainUntil: train.Until,
		TestSince: test.Since, TestUntil: test.Until,
		Users: []string{},
		Cells: []KVCacheHoldoutCell{},
	}

	for _, tc := range trainOut.Cells {
		cell := KVCacheHoldoutCell{
			User: tc.User, HourUTC: tc.HourUTC,
			TrainRequests: tc.Requests, TrainInsufficientData: tc.InsufficientData,
			Arm:            tc.BestStrategy,
			TrainSavingUSD: tc.SavingUSD,
			// Valued, not SavingKnown: SavingKnown is kvcache.Savings' PERCENT-known flag
			// (false when the baseline cost is zero, since a percentage of nothing is
			// undefined), which says nothing about whether the absolute dollar figure
			// could be priced. Valued is the flag that does.
			TrainKnown: tc.Valued,
			Arms:       []KVCacheHoldoutArm{},
		}

		testCell, haveTest := testCells[cellKey{tc.User, tc.HourUTC}]
		if haveTest {
			cell.TestRequests = testCell.Requests
			cell.TestInsufficientData = testCell.InsufficientData
		}

		// Every arm the train window scored, paired with the same arm's test-window score.
		// Driven by the TRAIN cell's candidate list, not the union of both: an arm that
		// only the test window could build is not something a campaign made from the train
		// window could ever have chosen, so it has no place in this comparison.
		testByArm := map[string]struct {
			usd    float64
			valued bool
		}{}
		if haveTest {
			for _, s := range testCell.Candidates {
				// kvcache.Savings has no Valued field of its own; a candidate reaches the
				// list whether or not it could be priced (see kvSuggestCell, which appends
				// every comparison and only SKIPS unvalued ones from the argmax). An
				// unpriceable arm's BaselineUSD and StrategyUSD are both zero, which is
				// indistinguishable from "priced, and exactly free" — so a cell whose
				// baseline could not be valued at all is treated as carrying no usable
				// test figure for ANY arm, which is the conservative reading and the only
				// one that cannot fabricate a $0.00.
				testByArm[s.Strategy] = struct {
					usd    float64
					valued bool
				}{s.AbsoluteUSD, testCell.Valued}
			}
		}
		for _, s := range tc.Candidates {
			arm := KVCacheHoldoutArm{
				Strategy:       s.Strategy,
				TrainSavingUSD: s.AbsoluteUSD,
				TrainKnown:     tc.Valued,
				Chosen:         s.Strategy == tc.BestStrategy,
			}
			if t, ok := testByArm[s.Strategy]; ok {
				arm.TestSavingUSD, arm.TestKnown = t.usd, t.valued
			}
			cell.Arms = append(cell.Arms, arm)
			if arm.Chosen {
				cell.TestSavingUSD, cell.TestKnown = arm.TestSavingUSD, arm.TestKnown
			}
		}

		// Why the chosen arm has no test figure, stated rather than left blank. Ordered
		// most-specific-first: "no traffic at all" is a different fact from "traffic, but
		// this arm could not be replayed on it", and reporting the generic reason for the
		// specific case would send a reader looking for the wrong thing.
		if !cell.TestKnown {
			switch {
			case !haveTest:
				cell.TestNote = "this account sent no Sunday-Thursday traffic in this hour " +
					"during the test window"
			case cell.TestRequests == 0:
				cell.TestNote = "no test-window requests in this cell"
			case !testCell.Valued:
				cell.TestNote = "the test window's traffic in this cell could not be priced"
			default:
				cell.TestNote = "the arm chosen on the train window could not be replayed " +
					"on the test window's rows"
			}
		}

		// The comparable population: both sides priced, and neither side thin. A cell that
		// is below the request floor on EITHER side is excluded from the totals — a
		// train-side floor breach means the arm was chosen from noise, and a test-side one
		// means it was scored against noise. Either way the pair does not support the
		// comparison, and folding it in would let a handful of three-request cells move a
		// deployment-wide retention figure.
		if cell.TrainKnown && cell.TestKnown &&
			!cell.TrainInsufficientData && !cell.TestInsufficientData {
			out.TotalTrainSavingUSD += cell.TrainSavingUSD
			out.TotalTestSavingUSD += cell.TestSavingUSD
			out.ComparableCells++
		}

		out.Cells = append(out.Cells, cell)
	}
	out.TotalCells = len(out.Cells)
	if out.ComparableCells > 0 && out.TotalTrainSavingUSD != 0 {
		out.RetentionPct = out.TotalTestSavingUSD / out.TotalTrainSavingUSD * 100
		out.RetentionKnown = true
	}

	userSet := map[string]bool{}
	for _, c := range out.Cells {
		userSet[c.User] = true
	}
	for u := range userSet {
		out.Users = append(out.Users, u)
	}
	sort.Strings(out.Users)
	sort.Slice(out.Cells, func(i, j int) bool {
		if out.Cells[i].User != out.Cells[j].User {
			return out.Cells[i].User < out.Cells[j].User
		}
		return out.Cells[i].HourUTC < out.Cells[j].HourUTC
	})

	out.Notes = []string{
		"Train saving is IN-SAMPLE: each cell's arm was chosen by replaying every candidate " +
			"over exactly these rows and taking the best, so the figure it wins with is a " +
			"measure of fit, not a forecast. It is the same number the KV-cache page's " +
			"suggestions and a campaign's frozen predicted saving already show.",
		"Test saving is the SAME arm replayed on the test window's rows, which it was not " +
			"chosen on. Train minus test is this deployment's own overfitting estimate for " +
			"that cell; it is not a correction to subtract from a prediction.",
		"Expect test below train on average, including some negative cells. An arm that wins " +
			"a thin cell on noise has nothing to reproduce, and a cell can genuinely change " +
			"behaviour between two periods — both look the same here, which is why the " +
			"request counts for both windows are shown per cell.",
		"The two totals cover only cells priced on both sides and thin on neither " +
			"(comparable_cells of total_cells). A cell missing a test figure says why, and " +
			"is never counted as a zero.",
		"Both windows read Sunday-Thursday only and are replayed per hour-of-day in UTC, " +
			"exactly as the live suggester does — see dash/kvcachesuggest.go.",
	}
	return out, nil
}

// Window is a closed-open time range in epoch milliseconds. Named separately from
// Filter's own Since/Until because a holdout takes TWO of them and passing four loose
// int64s in a row is how two of them end up swapped with no compiler complaint.
type Window struct {
	Since int64 `json:"since"`
	Until int64 `json:"until"`
}

// validHoldoutWindows refuses anything that is not a real split.
//
// Every bound must be present (see the file doc comment on why 0-as-unbounded is not
// available here), each window must be non-empty, and the two must not overlap. The
// overlap check is the load-bearing one: a train window that includes any test row means
// the arm was partly chosen on the rows it is scored on, which reports an in-sample
// number as an out-of-sample one — the single failure this whole view exists to avoid,
// and one that is invisible in the output.
//
// Order is not constrained: scoring a train window against an EARLIER test window is a
// legitimate check (does the pattern this month found also describe last month?), so a
// test window before the train window is allowed as long as the two are disjoint.
func validHoldoutWindows(train, test Window) error {
	for _, w := range []struct {
		name string
		win  Window
	}{{"train", train}, {"test", test}} {
		if w.win.Since <= 0 || w.win.Until <= 0 {
			return fmt.Errorf(
				"dash: the %s window needs both a start and an end — an open-ended window would "+
					"overlap the other one and silently stop being a holdout", w.name)
		}
		if w.win.Until <= w.win.Since {
			return fmt.Errorf("dash: the %s window ends at or before it starts", w.name)
		}
	}
	// Until is exclusive, so train.Until == test.Since is two adjacent, disjoint windows
	// sharing an instant that belongs to exactly one of them — the most common split
	// anyone will ask for, and not an overlap.
	if train.Since < test.Until && test.Since < train.Until {
		return fmt.Errorf("dash: the train and test windows overlap — an arm scored on rows it " +
			"was chosen on is an in-sample number, which is what this view exists to avoid")
	}
	return nil
}
