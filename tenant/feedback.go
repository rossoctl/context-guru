package tenant

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// User feedback: stars plus mandatory prose, stored in the CONTROL database.
//
// It lives here rather than in dash for the same reason accounts do: dash's request
// store is a derived view that renames itself aside and starts fresh on a schema
// change. Somebody's written answer to "would you recommend this" is not derivable
// from anything, so losing it to a schemaVersion bump would be losing the only copy.
//
// The email to the manager is NOT part of storing. A submission is committed here
// first and mailed afterwards, off the request path (see proxy.deliverFeedback), and
// MailedAt records whether that ever succeeded — so a dead relay costs a
// notification, never the answer.

// Text limits. 50 is the owner's rule, and it is enforced on the SERVER: the form
// counts characters too, but a form is a courtesy and a POST is the boundary.
const (
	MinFeedbackChars = 50
	// MaxFeedbackChars bounds what goes into a row and into an email body. Generous
	// for a paragraph, small enough that a submission cannot be a payload.
	MaxFeedbackChars = 4000
)

var (
	ErrFeedbackText  = fmt.Errorf("tenant: the comment must be at least %d characters of real text", MinFeedbackChars)
	ErrFeedbackLong  = fmt.Errorf("tenant: that text is longer than %d characters", MaxFeedbackChars)
	ErrFeedbackScore = errors.New("tenant: every rating must be a whole number of stars from 1 to 5")
	ErrFeedbackDim   = errors.New("tenant: unknown rating")
)

// FeedbackDimensions is the fixed set of star questions, in the order the form and
// the manager's view both present them. It is the ONE list: the handler validates
// against it, Summarize orders by it, and a key that is not in it (and is not an
// agent, below) is refused rather than silently stored and never read.
//
// Why these eight and not more: each one is a decision someone could act on. The
// owner asked for "a general feel" (overall), whether the agent still behaves
// (as_good_as_before), the components, latency, observability, ease of use, and a
// recommendation — and the per-agent question is asked once per agent the account has
// actually sent traffic for, because asking a Claude Code user to rate Bob produces a
// number that means nothing.
var FeedbackDimensions = []string{
	"overall",           // general satisfaction, the headline
	"as_good_as_before", // does the agent still do its job as well as it did — or better
	"agent",             // per-agent behaviour; stored as "agent:<name>", see AgentDimension
	"components",        // the compaction components: do they remove the right things
	"latency",           // added latency on the hot path
	"observability",     // is the dashboard actually useful
	"ease",              // how easy it was to set up and to use
	"recommend",         // likelihood to recommend, the NPS question
}

// requiredDimensions is every fixed question. "agent" is not one of them: it is a
// PREFIX, and which agents to ask about depends on the account's own traffic.
func requiredDimensions() []string {
	out := make([]string, 0, len(FeedbackDimensions)-1)
	for _, d := range FeedbackDimensions {
		if d != "agent" {
			out = append(out, d)
		}
	}
	return out
}

// AgentDimension is how a per-agent rating is keyed: "agent:claude-code".
const AgentDimension = "agent:"

// validDimension reports whether a submitted key is one we store. Agent names come
// from the caller's own request rows, so they are constrained to the shape dash
// records rather than accepted as free text — a dimension key is a column value in
// every aggregate and every email.
func validDimension(key string) bool {
	for _, d := range requiredDimensions() {
		if key == d {
			return true
		}
	}
	name, ok := strings.CutPrefix(key, AgentDimension)
	if !ok || name == "" || len(name) > 32 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '.' && r != '_' {
			return false
		}
	}
	return true
}

// Feedback is one submission.
type Feedback struct {
	ID       int64
	TenantID string
	// Email and Label are copied in at write time rather than joined at read time, so
	// the manager's list still says who wrote something after an account is renamed.
	Email     string
	Label     string
	CreatedAt time.Time
	// Scores maps a dimension to 1..5 stars.
	Scores map[string]int
	// Wanted is "what would you like added" — optional, because a user with nothing to
	// ask for should not have to invent something to get past the form.
	Wanted string
	// Comment is the mandatory prose.
	Comment string
	// MailedAt is when the manager's copy was accepted by the relay; zero means it was
	// never delivered. The row is the source of truth either way.
	MailedAt time.Time
}

// meaningfulLen counts the characters a reader would see.
//
// strings.Fields collapses every run of unicode whitespace, so neither 50 spaces nor
// "a" + 48 spaces + "b" reaches the minimum — which a plain TrimSpace + len would both
// have let through.
func meaningfulLen(s string) int {
	return len([]rune(strings.Join(strings.Fields(s), " ")))
}

// AddFeedback validates and stores one submission.
//
// The tenant comes from the authenticated principal, never from the request body:
// this is the only writer, so there is no path by which a submission can be attributed
// to somebody else.
func (r *Registry) AddFeedback(t *Tenant, scores map[string]int, wanted, comment string) (*Feedback, error) {
	if t == nil {
		return nil, ErrForbidden
	}
	if meaningfulLen(comment) < MinFeedbackChars {
		return nil, ErrFeedbackText
	}
	for _, s := range []string{comment, wanted} {
		if len([]rune(s)) > MaxFeedbackChars {
			return nil, ErrFeedbackLong
		}
	}
	for key, v := range scores {
		if !validDimension(key) {
			return nil, fmt.Errorf("%w %q", ErrFeedbackDim, key)
		}
		if v < 1 || v > 5 {
			return nil, fmt.Errorf("%w (%s = %d)", ErrFeedbackScore, key, v)
		}
	}
	for _, d := range requiredDimensions() {
		if _, ok := scores[d]; !ok {
			return nil, fmt.Errorf("%w: %s has no rating", ErrFeedbackScore, d)
		}
	}

	fb := &Feedback{
		TenantID: t.ID, Email: t.Email, Label: t.Label,
		CreatedAt: time.Now(), Scores: scores,
		Wanted: strings.TrimSpace(wanted), Comment: strings.TrimSpace(comment),
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO feedback
	  (tenant_id,email,label,created_at,wanted,comment,mailed_at)
	  VALUES (?,?,?,?,?,?,0)`,
		fb.TenantID, fb.Email, fb.Label, fb.CreatedAt.UnixMilli(), fb.Wanted, fb.Comment)
	if err != nil {
		return nil, err
	}
	if fb.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	for key, v := range scores {
		if _, err := tx.Exec(`INSERT INTO feedback_scores (feedback_id,dimension,score)
		  VALUES (?,?,?)`, fb.ID, key, v); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return fb, nil
}

// FeedbackList returns submissions newest first. An empty tenantID means every
// account's — which is a MANAGER view, gated at the handler, never here.
func (r *Registry) FeedbackList(tenantID string, limit int) ([]*Feedback, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	q := `SELECT id,tenant_id,email,label,created_at,wanted,comment,mailed_at FROM feedback`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]*Feedback{}
	var out []*Feedback
	for rows.Next() {
		fb := &Feedback{Scores: map[string]int{}}
		var created, mailed int64
		if err := rows.Scan(&fb.ID, &fb.TenantID, &fb.Email, &fb.Label, &created,
			&fb.Wanted, &fb.Comment, &mailed); err != nil {
			return nil, err
		}
		fb.CreatedAt = msTime(created)
		fb.MailedAt = msTime(mailed)
		byID[fb.ID] = fb
		out = append(out, fb)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	// One extra query for every score, joined in memory. A correlated subquery per row
	// would be N+1 round trips for the same answer.
	sc, err := r.db.Query(`SELECT s.feedback_id,s.dimension,s.score
	  FROM feedback_scores s JOIN feedback f ON f.id = s.feedback_id
	  WHERE (? = '' OR f.tenant_id = ?)`, tenantID, tenantID)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	for sc.Next() {
		var id int64
		var dim string
		var v int
		if err := sc.Scan(&id, &dim, &v); err != nil {
			return nil, err
		}
		if fb := byID[id]; fb != nil {
			fb.Scores[dim] = v
		}
	}
	return out, sc.Err()
}

// MarkFeedbackMailed records that the manager's copy was delivered.
func (r *Registry) MarkFeedbackMailed(id int64, at time.Time) error {
	_, err := r.db.Exec(`UPDATE feedback SET mailed_at = ? WHERE id = ?`, at.UnixMilli(), id)
	return err
}

// ManagerEmail is the address configured with --manager-email, for the notification
// path. Exported as an accessor rather than a field so nothing can rewrite it after
// the registry is open.
func (r *Registry) ManagerEmail() string { return strings.TrimSpace(r.opts.ManagerEmail) }

// FirstManagerEmail returns a registered manager's address, for when --manager-email
// was not set but somebody holds the role anyway.
func (r *Registry) FirstManagerEmail() (string, error) {
	var email string
	err := r.db.QueryRow(`SELECT email FROM tenants WHERE role = ? AND disabled = 0
	  ORDER BY created_at LIMIT 1`, string(RoleManager)).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return email, err
}

// --- aggregation ------------------------------------------------------------

// DimStat is one question's arithmetic: how many people answered it, their mean, and
// the shape of the answers. The distribution is reported alongside the mean because a
// 3.0 made of 3s and a 3.0 made of 1s and 5s are different situations.
type DimStat struct {
	Dimension string  `json:"dimension"`
	N         int     `json:"n"`
	Mean      float64 `json:"mean"`
	// Dist counts stars: Dist[0] is how many 1-star answers, Dist[4] how many 5-star.
	Dist [5]int `json:"dist"`
}

// NPSSplit is the recommend question read the way an NPS is read. The 1-5 mapping is
// stated here because there is no standard one: 5 promotes, 4 is passive, 1-3 detract.
// Score is promoters% − detractors%, so it runs -100..100.
type NPSSplit struct {
	Promoters  int     `json:"promoters"`
	Passives   int     `json:"passives"`
	Detractors int     `json:"detractors"`
	N          int     `json:"n"`
	Score      float64 `json:"score"`
}

// DayPoint is one UTC day of the trend. UTC so two people reading the same chart see
// the same buckets.
type DayPoint struct {
	Day  int64   `json:"day"` // epoch ms, midnight UTC
	N    int     `json:"n"`
	Mean float64 `json:"mean"` // mean of "overall" that day
}

// FeedbackSummary is everything the manager's view needs that is not a raw answer.
type FeedbackSummary struct {
	N          int        `json:"n"`
	Dimensions []DimStat  `json:"dimensions"`
	NPS        NPSSplit   `json:"nps"`
	Trend      []DayPoint `json:"trend"`
	// Unmailed counts submissions whose notification never got out, so a broken relay
	// is visible in the UI instead of only in the log.
	Unmailed int `json:"unmailed"`
}

// Summarize computes the aggregate from the submissions themselves.
//
// In Go rather than in SQL or in the browser: it is arithmetic somebody will act on, so
// it belongs somewhere a test can seed five rows and assert the mean.
func Summarize(fs []*Feedback) FeedbackSummary {
	sum := FeedbackSummary{N: len(fs), Dimensions: []DimStat{}, Trend: []DayPoint{}}
	stats := map[string]*DimStat{}
	days := map[int64]*DayPoint{}
	for _, fb := range fs {
		if fb.MailedAt.IsZero() {
			sum.Unmailed++
		}
		for dim, v := range fb.Scores {
			if v < 1 || v > 5 {
				continue // a row written by an older, laxer build is not arithmetic
			}
			s := stats[dim]
			if s == nil {
				s = &DimStat{Dimension: dim}
				stats[dim] = s
			}
			s.N++
			s.Dist[v-1]++
			s.Mean += float64(v) // running total; divided below
			if dim == "recommend" {
				sum.NPS.N++
				switch {
				case v == 5:
					sum.NPS.Promoters++
				case v == 4:
					sum.NPS.Passives++
				default:
					sum.NPS.Detractors++
				}
			}
		}
		if v, ok := fb.Scores["overall"]; ok && v >= 1 && v <= 5 {
			d := fb.CreatedAt.UTC().Truncate(24 * time.Hour).UnixMilli()
			p := days[d]
			if p == nil {
				p = &DayPoint{Day: d}
				days[d] = p
			}
			p.N++
			p.Mean += float64(v)
		}
	}
	for _, s := range stats {
		s.Mean /= float64(s.N)
	}
	if sum.NPS.N > 0 {
		sum.NPS.Score = (float64(sum.NPS.Promoters) - float64(sum.NPS.Detractors)) / float64(sum.NPS.N) * 100
	}
	// Fixed questions in their declared order, then the agents alphabetically: a list
	// that reorders itself as the numbers move cannot be scanned twice.
	for _, d := range requiredDimensions() {
		if s := stats[d]; s != nil {
			sum.Dimensions = append(sum.Dimensions, *s)
			delete(stats, d)
		}
	}
	rest := make([]string, 0, len(stats))
	for d := range stats {
		rest = append(rest, d)
	}
	sort.Strings(rest)
	for _, d := range rest {
		sum.Dimensions = append(sum.Dimensions, *stats[d])
	}
	for _, p := range days {
		p.Mean /= float64(p.N)
		sum.Trend = append(sum.Trend, *p)
	}
	sort.Slice(sum.Trend, func(i, j int) bool { return sum.Trend[i].Day < sum.Trend[j].Day })
	return sum
}
