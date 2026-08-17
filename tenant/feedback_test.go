package tenant

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// fullScores is a complete, valid set of star ratings. Every question answered, because
// the server requires that — a partially answered form is a rejected one.
func fullScores(v int) map[string]int {
	m := map[string]int{}
	for _, q := range FeedbackQuestions {
		m[q.Key] = v
	}
	return m
}

// The 50-character rule, and the three ways somebody gets past a naive check.
func TestFeedbackTextMinimumIsEnforced(t *testing.T) {
	r := open(t, Options{})
	tn, _ := register(t, r, "a@ibm.com")

	for name, comment := range map[string]string{
		"empty":             "",
		"short":             "too short to be useful",
		"whitespace only":   strings.Repeat(" ", 80),
		"tabs and newlines": strings.Repeat("\t\n ", 40),
		// 50 characters of which 48 are spaces. TrimSpace leaves 50, so a length check on
		// the trimmed string passes and the manager receives "a" and "b".
		"padded to length": "a" + strings.Repeat(" ", 48) + "b",
		"49 real chars":    strings.Repeat("x", 49),
	} {
		if _, err := r.AddFeedback(tn, "bob", fullScores(4), comment); !errors.Is(err, ErrFeedbackText) {
			t.Errorf("%s: err = %v, want ErrFeedbackText", name, err)
		}
	}

	// Exactly 50 real characters passes, and interior spacing counts as one character
	// each rather than being stripped: this is prose, not an identifier.
	ok := strings.Repeat("x", 50)
	if _, err := r.AddFeedback(tn, "bob", fullScores(4), ok); err != nil {
		t.Errorf("50 real characters was refused: %v", err)
	}
	spaced := strings.Repeat("ab ", 20) // 59 collapsed characters
	if _, err := r.AddFeedback(tn, "bob", fullScores(4), spaced); err != nil {
		t.Errorf("real words with spaces were refused: %v", err)
	}
	if _, err := r.AddFeedback(tn, "bob", fullScores(4), strings.Repeat("y", MaxFeedbackChars+1)); !errors.Is(err, ErrFeedbackLong) {
		t.Error("an oversized comment was accepted")
	}
}

func TestFeedbackScoresAreValidated(t *testing.T) {
	r := open(t, Options{})
	tn, _ := register(t, r, "a@ibm.com")
	good := strings.Repeat("This is a real sentence of feedback about the proxy. ", 2)

	for name, scores := range map[string]map[string]int{
		"zero stars":  {"overall": 0},
		"six stars":   {"overall": 6},
		"negative":    {"overall": -3},
		"unknown key": {"made_up_question": 4},
		// A dimension key is rendered in the manager's browser and in an email, so it is
		// an allow-list rather than free text.
		"markup as a key": {"<script>": 4},
		"a retired key":   {"as_good_as_before": 4},
	} {
		if _, err := r.AddFeedback(tn, "claude-code", scores, good); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	fb, err := r.AddFeedback(tn, "claude-code", fullScores(5), good)
	if err != nil {
		t.Fatalf("valid submission refused: %v", err)
	}
	if fb.Agent != "claude-code" || fb.TenantID != tn.ID || fb.Email != tn.Email {
		t.Errorf("stored row is wrong: %+v", fb)
	}
	if !fb.MailedAt.IsZero() {
		t.Error("a freshly stored row claims to have been mailed")
	}
}

// All seven questions are mandatory: leaving any one of them unrated is a rejection, not
// a stored partial answer that quietly skews the aggregate for that question.
func TestFeedbackRequiresEveryStarQuestion(t *testing.T) {
	r := open(t, Options{})
	tn, _ := register(t, r, "a@ibm.com")
	good := strings.Repeat("This is a real sentence of feedback about the proxy. ", 2)

	if len(FeedbackQuestions) != 7 {
		t.Fatalf("the owner asked for seven questions, this build asks %d", len(FeedbackQuestions))
	}
	for _, q := range FeedbackQuestions {
		partial := fullScores(4)
		delete(partial, q.Key)
		if _, err := r.AddFeedback(tn, "bob", partial, good); !errors.Is(err, ErrFeedbackScore) {
			t.Errorf("a form with no %s rating was accepted: %v", q.Key, err)
		}
	}
	if all, err := r.FeedbackList("", 0); err != nil || len(all) != 0 {
		t.Fatalf("a partly answered form was stored: %+v %v", all, err)
	}
}

// The selector is mandatory and closed: the answer groups the aggregate and names an
// agent in an email, so a third value is refused rather than stored as a group nobody
// asked for.
func TestFeedbackAgentIsRequiredAndClosed(t *testing.T) {
	r := open(t, Options{})
	tn, _ := register(t, r, "a@ibm.com")
	good := strings.Repeat("This is a real sentence of feedback about the proxy. ", 2)

	for _, agent := range []string{"", " ", "Claude Code", "claude_code", "cursor",
		"claude-code<script>"} {
		if _, err := r.AddFeedback(tn, agent, fullScores(4), good); !errors.Is(err, ErrFeedbackAgent) {
			t.Errorf("agent %q: err = %v, want ErrFeedbackAgent", agent, err)
		}
	}
	if all, err := r.FeedbackList("", 0); err != nil || len(all) != 0 {
		t.Fatalf("a submission with no agent was stored: %+v %v", all, err)
	}
	for _, agent := range []string{"claude-code", "bob"} {
		fb, err := r.AddFeedback(tn, agent, fullScores(4), good)
		if err != nil {
			t.Fatalf("agent %q was refused: %v", agent, err)
		}
		back, err := r.FeedbackList(tn.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if back[0].ID != fb.ID || back[0].Agent != agent {
			t.Errorf("agent did not survive the round trip: %+v", back[0])
		}
	}
}

// Storing must not depend on anything but the database: the mail path is not consulted
// here at all, which is what makes "the relay is down" a notification problem.
func TestFeedbackRoundTripsWithScores(t *testing.T) {
	r := open(t, Options{})
	a, _ := register(t, r, "a@ibm.com")
	b, _ := register(t, r, "b@ibm.com")
	long := strings.Repeat("Detailed feedback about compaction quality and latency. ", 2)

	if _, err := r.AddFeedback(a, "claude-code", fullScores(5), long); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddFeedback(b, "bob", fullScores(2), long); err != nil {
		t.Fatal(err)
	}

	all, err := r.FeedbackList("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("service-wide list has %d rows, want 2", len(all))
	}
	for _, fb := range all {
		if len(fb.Scores) != len(FeedbackQuestions) {
			t.Errorf("row %d carries %d scores, want %d: %v",
				fb.ID, len(fb.Scores), len(FeedbackQuestions), fb.Scores)
		}
	}
	mine, err := r.FeedbackList(a.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].TenantID != a.ID {
		t.Fatalf("per-tenant list = %+v", mine)
	}
	if mine[0].Scores["overall"] != 5 {
		t.Errorf("scores did not survive the round trip: %v", mine[0].Scores)
	}

	// Delivery is recorded separately from storage.
	if err := r.MarkFeedbackMailed(mine[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	again, _ := r.FeedbackList(a.ID, 0)
	if again[0].MailedAt.IsZero() {
		t.Error("MarkFeedbackMailed did not stick")
	}
}

// The arithmetic, on a seeded set where every number can be worked out by hand.
func TestSummarizeArithmetic(t *testing.T) {
	day := int64(24 * 60 * 60 * 1000)
	base := time.UnixMilli(2 * day).UTC() // midnight UTC, day 2
	mk := func(dayOffset int, overall, recommend int, mailed bool) *Feedback {
		fb := &Feedback{
			CreatedAt: base.Add(time.Duration(dayOffset)*24*time.Hour + 5*time.Hour),
			Scores:    map[string]int{"overall": overall, "recommend": recommend},
		}
		if mailed {
			fb.MailedAt = fb.CreatedAt
		}
		return fb
	}
	// overall: 5,4,3,1,5 → sum 18, mean 3.6, distribution [1,0,1,1,2]
	// recommend: 5,5,4,1,3 → promoters 2, passive 1, detractors 2 → NPS (2-2)/5 = 0
	set := []*Feedback{
		mk(0, 5, 5, true),
		mk(0, 4, 5, true),
		mk(1, 3, 4, false),
		mk(1, 1, 1, false),
		mk(2, 5, 3, true),
	}
	s := Summarize(set)

	if s.N != 5 {
		t.Errorf("N = %d, want 5", s.N)
	}
	if s.Unmailed != 2 {
		t.Errorf("Unmailed = %d, want 2", s.Unmailed)
	}
	var overall *DimStat
	for i := range s.Dimensions {
		if s.Dimensions[i].Dimension == "overall" {
			overall = &s.Dimensions[i]
		}
	}
	if overall == nil {
		t.Fatalf("no overall dimension in %+v", s.Dimensions)
	}
	if overall.N != 5 || math.Abs(overall.Mean-3.6) > 1e-9 {
		t.Errorf("overall: N=%d mean=%v, want 5 and 3.6", overall.N, overall.Mean)
	}
	if overall.Dist != [5]int{1, 0, 1, 1, 2} {
		t.Errorf("overall distribution = %v, want [1 0 1 1 2]", overall.Dist)
	}
	// The fixed questions keep their declared order regardless of map iteration.
	if s.Dimensions[0].Dimension != "overall" {
		t.Errorf("dimensions are not in declared order: %v", s.Dimensions[0].Dimension)
	}
	if s.NPS.Promoters != 2 || s.NPS.Passives != 1 || s.NPS.Detractors != 2 || s.NPS.N != 5 {
		t.Errorf("NPS split = %+v", s.NPS)
	}
	if math.Abs(s.NPS.Score) > 1e-9 {
		t.Errorf("NPS score = %v, want 0", s.NPS.Score)
	}
	// Trend: three UTC days, ascending, means 4.5 / 2.0 / 5.0.
	if len(s.Trend) != 3 {
		t.Fatalf("trend has %d points, want 3: %+v", len(s.Trend), s.Trend)
	}
	wantMeans := []float64{4.5, 2, 5}
	for i, p := range s.Trend {
		if math.Abs(p.Mean-wantMeans[i]) > 1e-9 {
			t.Errorf("trend[%d].Mean = %v, want %v", i, p.Mean, wantMeans[i])
		}
		if i > 0 && p.Day <= s.Trend[i-1].Day {
			t.Errorf("trend is not ascending: %+v", s.Trend)
		}
		if p.Day%day != 0 {
			t.Errorf("trend[%d].Day = %d, not a UTC midnight", i, p.Day)
		}
	}
	// An empty set is an empty summary, not a division by zero.
	if e := Summarize(nil); e.N != 0 || len(e.Dimensions) != 0 || e.NPS.Score != 0 {
		t.Errorf("empty summary = %+v", e)
	}
}

// The per-agent breakdown, on seeded rows where each side's numbers can be worked out by
// hand. This is what the selector is for: two agents whose answers disagree must not
// average each other away.
func TestSummarizePerAgentBreakdown(t *testing.T) {
	mk := func(agent string, overall, recommend int) *Feedback {
		return &Feedback{
			CreatedAt: time.UnixMilli(0).UTC(), Agent: agent,
			Scores: map[string]int{"overall": overall, "recommend": recommend},
		}
	}
	// Claude Code: overall 5,4,3 → mean 4; recommend 5,5,4 → 2 promoters, 1 passive, NPS +66.7
	// Bob:         overall 2,2   → mean 2; recommend 1,3   → 2 detractors,        NPS -100
	s := Summarize([]*Feedback{
		mk("claude-code", 5, 5), mk("claude-code", 4, 5), mk("claude-code", 3, 4),
		mk("bob", 2, 1), mk("bob", 2, 3),
	})
	if s.N != 5 {
		t.Errorf("N = %d, want 5", s.N)
	}
	if len(s.ByAgent) != 2 {
		t.Fatalf("by-agent breakdown has %d groups, want 2: %+v", len(s.ByAgent), s.ByAgent)
	}
	for _, want := range []struct {
		agent string
		n     int
		mean  float64
		nps   float64
	}{
		{"claude-code", 3, 4, 200.0 / 3},
		{"bob", 2, 2, -100},
	} {
		got := s.ByAgent[want.agent]
		if got == nil {
			t.Fatalf("no breakdown for %s", want.agent)
		}
		if got.N != want.n {
			t.Errorf("%s: N = %d, want %d", want.agent, got.N, want.n)
		}
		var overall *DimStat
		for i := range got.Dimensions {
			if got.Dimensions[i].Dimension == "overall" {
				overall = &got.Dimensions[i]
			}
		}
		if overall == nil {
			t.Fatalf("%s: no overall dimension in %+v", want.agent, got.Dimensions)
		}
		if overall.N != want.n || math.Abs(overall.Mean-want.mean) > 1e-9 {
			t.Errorf("%s: overall N=%d mean=%v, want %d and %v",
				want.agent, overall.N, overall.Mean, want.n, want.mean)
		}
		if math.Abs(got.NPS.Score-want.nps) > 1e-9 {
			t.Errorf("%s: NPS = %v, want %v", want.agent, got.NPS.Score, want.nps)
		}
		if got.ByAgent != nil {
			t.Errorf("%s: the breakdown carries its own breakdown", want.agent)
		}
	}
	// The whole-set numbers are still the whole set: overall 5,4,3,2,2 → mean 3.2.
	if m := s.Dimensions[0]; m.Dimension != "overall" || math.Abs(m.Mean-3.2) > 1e-9 {
		t.Errorf("service-wide overall = %+v, want mean 3.2", m)
	}
	if s := Summarize(nil); s.ByAgent != nil {
		t.Errorf("an empty set has a breakdown: %+v", s.ByAgent)
	}
}

// Feedback outlives the account that wrote it: the manager's record of what somebody
// said must not vanish because the account was cleaned up.
func TestFeedbackSurvivesTenantDeletion(t *testing.T) {
	r := open(t, Options{})
	tn, _ := register(t, r, "a@ibm.com")
	long := strings.Repeat("A comment long enough to satisfy the fifty-character rule. ", 2)
	if _, err := r.AddFeedback(tn, "bob", fullScores(3), long); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`DELETE FROM tenants WHERE id = ?`, tn.ID); err != nil {
		t.Fatal(err)
	}
	all, err := r.FeedbackList("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Email != "a@ibm.com" {
		t.Fatalf("feedback did not survive the account: %+v", all)
	}
}

func TestManagerAddressLookup(t *testing.T) {
	r := open(t, Options{ManagerEmail: "boss@ibm.com"})
	if got := r.ManagerEmail(); got != "boss@ibm.com" {
		t.Errorf("ManagerEmail() = %q", got)
	}
	if _, err := r.FirstManagerEmail(); !errors.Is(err, ErrNotFound) {
		t.Errorf("no manager registered yet, but FirstManagerEmail returned %v", err)
	}
	register(t, r, "a@ibm.com")
	register(t, r, "boss@ibm.com") // matches ManagerEmail, so registers as a manager
	got, err := r.FirstManagerEmail()
	if err != nil || got != "boss@ibm.com" {
		t.Errorf("FirstManagerEmail() = %q, %v", got, err)
	}
}
