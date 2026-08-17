package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/tenant"
)

// validFeedback is a body the server accepts: every fixed question rated, and a comment
// past the fifty-character minimum.
func validFeedback(t *testing.T, overall int, comment string) string {
	t.Helper()
	scores := scoreMap()
	scores["overall"] = overall
	b, err := json.Marshal(map[string]any{
		"agent": "claude-code", "scores": scores, "comment": comment,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const longComment = "Compaction has been invisible in day-to-day use, which is the " +
	"highest compliment I can pay it. The diff view is what sold me."

// waitForSink polls the mail sink until it contains `want`, and returns the whole file.
// Mailing is deliberately off the request path, so the POST returns before the message
// exists — this waits for the background send rather than assuming it already happened.
func waitForSink(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := os.ReadFile(os.Getenv(envMailDevSink))
		if err == nil && strings.Contains(string(b), want) {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("mail sink never contained %q; it holds:\n%s", want, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFeedbackIsStoredAndMailedToTheManager(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "laptop")
	jar := w.Result().Cookies()

	w, out := f.do(t, "POST", "/api/feedback", validFeedback(t, 5, longComment), jar)
	if w.Code != http.StatusCreated {
		t.Fatalf("submit = %d %s", w.Code, w.Body)
	}
	if out["id"] == nil {
		t.Errorf("the reply does not identify the stored row: %v", out)
	}

	// The manager's copy goes out AFTER the response, so it is waited for here.
	sink := waitForSink(t, "New context-guru feedback")
	if !strings.Contains(sink, "To: boss@ibm.com") {
		t.Errorf("feedback was not addressed to the manager:\n%s", sink)
	}
	if !strings.Contains(sink, "Subject: context-guru feedback: 5/5 overall on Claude Code from a@ibm.com") {
		t.Errorf("subject line is wrong:\n%s", sink)
	}
	// The mail names the agent and asks the questions in words, not in dimension keys.
	for _, want := range []string{
		"Agent: Claude Code",
		fmt.Sprintf("%d/5  %s", 5, tenant.QuestionLabel("overall")),
		fmt.Sprintf("%d/5  %s", 4, tenant.QuestionLabel("compaction")),
		"the highest compliment",
	} {
		if !strings.Contains(sink, want) {
			t.Errorf("the mail is missing %q:\n%s", want, sink)
		}
	}

	// And the delivery was recorded against the row, so the manager's view can tell a
	// delivered notification from a lost one.
	deadline := time.Now().Add(3 * time.Second)
	for {
		all, err := f.reg.FeedbackList("", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) == 1 && !all[0].MailedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mailed_at was never recorded: %+v", all)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The 50-character minimum is the SERVER's rule. The form counts too, but a hand-rolled
// POST is the case that matters.
func TestFeedbackRejectsShortAndBlankText(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "laptop")
	jar := w.Result().Cookies()

	for name, comment := range map[string]string{
		"empty":           "",
		"short":           "works fine",
		"fifty spaces":    strings.Repeat(" ", 50),
		"eighty spaces":   strings.Repeat(" ", 80),
		"newlines only":   strings.Repeat("\n", 60),
		"padded to fifty": "a" + strings.Repeat(" ", 48) + "b",
	} {
		w, out := f.do(t, "POST", "/api/feedback", validFeedback(t, 4, comment), jar)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: = %d, want 422 (%s)", name, w.Code, w.Body)
			continue
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, "50") {
			t.Errorf("%s: the error does not state the rule: %q", name, msg)
		}
	}
	// A rating outside 1..5, and a question nobody asked, are refused the same way.
	for _, body := range []string{
		`{"agent":"bob","scores":{"overall":9},"comment":"` + longComment + `"}`,
		`{"agent":"bob","scores":{"whatever":3},"comment":"` + longComment + `"}`,
	} {
		if w, _ := f.do(t, "POST", "/api/feedback", body, jar); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", body, w.Code)
		}
	}
	// Nothing was stored by any of the above.
	if all, err := f.reg.FeedbackList("", 0); err != nil || len(all) != 0 {
		t.Fatalf("rejected submissions were stored: %+v %v", all, err)
	}
}

// The 49/50 boundary itself, over HTTP, because that is where the rule is quoted back.
func TestFeedbackCommentBoundaryOverHTTP(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "laptop")
	jar := w.Result().Cookies()

	if w, _ := f.do(t, "POST", "/api/feedback",
		validFeedback(t, 4, strings.Repeat("x", 49)), jar); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("49 characters = %d, want 422", w.Code)
	}
	// 50 collapsed characters carried by 17 words plus their spaces: whitespace between
	// words counts, runs of it collapse, so both sides of the wire count the same 50.
	if w, _ := f.do(t, "POST", "/api/feedback",
		validFeedback(t, 4, strings.Repeat("ab ", 17)), jar); w.Code != http.StatusCreated {
		t.Errorf("50 characters = %d, want 201 (%s)", w.Code, w.Body)
	}
	if all, err := f.reg.FeedbackList("", 0); err != nil || len(all) != 1 {
		t.Fatalf("stored %d rows, want only the accepted one: %v", len(all), err)
	}
}

// The agent selector is the server's rule too: mandatory, and one of exactly two values.
// A third is refused rather than stored as a group the aggregate then reports.
func TestFeedbackAgentSelectorIsEnforced(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "laptop")
	jar := w.Result().Cookies()

	for _, agent := range []string{"", "cursor", "Bob", "claude-code<script>"} {
		body, err := json.Marshal(map[string]any{
			"agent": agent, "scores": scoreMap(), "comment": longComment,
		})
		if err != nil {
			t.Fatal(err)
		}
		w, out := f.do(t, "POST", "/api/feedback", string(body), jar)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("agent %q = %d, want 422 (%s)", agent, w.Code, w.Body)
			continue
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, "agent") {
			t.Errorf("agent %q: the error does not name the rule: %q", agent, msg)
		}
	}
	if all, err := f.reg.FeedbackList("", 0); err != nil || len(all) != 0 {
		t.Fatalf("a submission with an unknown agent was stored: %+v %v", all, err)
	}
	// A star question the form no longer asks is refused the same way.
	body := `{"agent":"bob","scores":{"as_good_as_before":4},"comment":"` + longComment + `"}`
	if w, _ := f.do(t, "POST", "/api/feedback", body, jar); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a retired question = %d, want 422", w.Code)
	}
}

// The breakdown the selector exists for, end to end: two agents, read apart.
func TestManagerSeesTheAgentBreakdown(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "laptop")
	mgrJar := w.Result().Cookies()
	w, _ = f.signUp(t, "a@ibm.com", "laptop")
	userJar := w.Result().Cookies()

	// Claude Code: overall 5 and 3, so mean 4. Bob: overall 1.
	for _, post := range []struct {
		agent   string
		overall int
	}{{"claude-code", 5}, {"claude-code", 3}, {"bob", 1}} {
		scores := scoreMap()
		scores["overall"] = post.overall
		body, err := json.Marshal(map[string]any{
			"agent": post.agent, "scores": scores, "comment": longComment,
		})
		if err != nil {
			t.Fatal(err)
		}
		if w, _ := f.do(t, "POST", "/api/feedback", string(body), userJar); w.Code != http.StatusCreated {
			t.Fatalf("submit for %s = %d %s", post.agent, w.Code, w.Body)
		}
	}

	w, out := f.do(t, "GET", "/api/feedback", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("manager read = %d %s", w.Code, w.Body)
	}
	sum, _ := out["summary"].(map[string]any)
	by, _ := sum["by_agent"].(map[string]any)
	if len(by) != 2 {
		t.Fatalf("by_agent = %v, want the two agents", by)
	}
	for agent, wantMean := range map[string]float64{"claude-code": 4, "bob": 1} {
		g, _ := by[agent].(map[string]any)
		if g == nil {
			t.Fatalf("no breakdown for %s in %v", agent, by)
		}
		dims, _ := g["dimensions"].([]any)
		first, _ := dims[0].(map[string]any)
		if first["dimension"] != "overall" {
			t.Fatalf("%s: dimensions do not lead with overall: %v", agent, dims)
		}
		if m, _ := first["mean"].(float64); m != wantMean {
			t.Errorf("%s: mean overall = %v, want %v", agent, first["mean"], wantMean)
		}
	}
	// And every answer says which agent it was about.
	subs, _ := out["submissions"].([]any)
	agents := map[string]int{}
	for _, s := range subs {
		row, _ := s.(map[string]any)
		a, _ := row["agent"].(string)
		agents[a]++
	}
	if agents["claude-code"] != 2 || agents["bob"] != 1 {
		t.Errorf("the answers do not carry their agent: %v", agents)
	}
	// The wording travels with the data, so the UI keeps no second copy of it.
	qs, _ := out["questions"].([]any)
	if len(qs) != len(tenant.FeedbackQuestions) {
		t.Errorf("questions = %v, want %d", qs, len(tenant.FeedbackQuestions))
	}
	if first, _ := qs[0].(map[string]any); first["key"] != "overall" ||
		first["label"] != tenant.QuestionLabel("overall") {
		t.Errorf("the served questions carry no wording: %v", qs[0])
	}
}

// A plain user may write feedback and read NONE of it — not another account's, not the
// aggregate, not even their own. The aggregate is the subtle one: "you said 2, everyone
// else says 4.4" is a disclosure about other people's answers.
func TestFeedbackReadIsManagerOnly(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "laptop")
	userJar := w.Result().Cookies()
	victim, _ := f.signUp(t, "victim@ibm.com", "desktop")
	victimJar := victim.Result().Cookies()

	if w, _ := f.do(t, "POST", "/api/feedback",
		validFeedback(t, 1, "The latency on long sessions is what I would fix first, "+
			"everything else has been solid."), victimJar); w.Code != http.StatusCreated {
		t.Fatalf("the victim could not submit: %d", w.Code)
	}
	if w, _ := f.do(t, "POST", "/api/feedback", validFeedback(t, 5, longComment), userJar); w.Code != http.StatusCreated {
		t.Fatalf("the user could not submit: %d", w.Code)
	}

	// Every shape of the read, including a crafted tenant selector. A crafted parameter
	// must not be the thing that decides: the principal is.
	for _, path := range []string{
		"/api/feedback",
		"/api/feedback?tenant=*",
		"/api/feedback?tenant=" + f.tenantID(t, "victim@ibm.com"),
		"/api/feedback?tenant=" + f.tenantID(t, "a@ibm.com"), // their OWN rows: still no
	} {
		w, out := f.do(t, "GET", path, "", userJar)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s for a plain user = %d, want 403: %s", path, w.Code, w.Body)
		}
		if out["submissions"] != nil || out["summary"] != nil {
			t.Errorf("%s served data to a plain user: %v", path, out)
		}
		if strings.Contains(w.Body.String(), "latency on long sessions") {
			t.Errorf("%s leaked another account's comment:\n%s", path, w.Body)
		}
	}
	// No cookie at all is a 401, not a default-open read.
	if w, _ := f.do(t, "GET", "/api/feedback", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("/api/feedback with no principal = %d, want 401", w.Code)
	}
	if w, _ := f.do(t, "POST", "/api/feedback", validFeedback(t, 4, longComment), nil); w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/feedback with no principal = %d, want 401", w.Code)
	}
}

func TestManagerReadsEveryAnswerAndTheAggregate(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "laptop") // matches the fixture's ManagerEmail
	mgrJar := w.Result().Cookies()
	w, _ = f.signUp(t, "a@ibm.com", "laptop")
	userJar := w.Result().Cookies()

	if w, _ := f.do(t, "POST", "/api/feedback", validFeedback(t, 5, longComment), userJar); w.Code != http.StatusCreated {
		t.Fatalf("submit = %d", w.Code)
	}
	if w, _ := f.do(t, "POST", "/api/feedback",
		validFeedback(t, 3, "Setup took me two attempts because I missed the custom "+
			"header, but it has been steady since."), mgrJar); w.Code != http.StatusCreated {
		t.Fatalf("the manager could not submit their own: %d", w.Code)
	}

	w, out := f.do(t, "GET", "/api/feedback", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("manager read = %d %s", w.Code, w.Body)
	}
	subs, _ := out["submissions"].([]any)
	if len(subs) != 2 {
		t.Fatalf("manager sees %d submissions, want 2: %v", len(subs), out)
	}
	if !strings.Contains(w.Body.String(), "a@ibm.com") {
		t.Error("the manager's list does not say who wrote what")
	}
	sum, _ := out["summary"].(map[string]any)
	if sum == nil {
		t.Fatalf("no summary in %v", out)
	}
	if n, _ := sum["n"].(float64); n != 2 {
		t.Errorf("summary n = %v, want 2", sum["n"])
	}
	dims, _ := sum["dimensions"].([]any)
	if len(dims) == 0 {
		t.Fatal("the summary carries no per-dimension statistics")
	}
	first, _ := dims[0].(map[string]any)
	if first["dimension"] != "overall" {
		t.Errorf("dimensions do not lead with overall: %v", first)
	}
	// overall was 5 and 3, so the mean is 4.
	if m, _ := first["mean"].(float64); m != 4 {
		t.Errorf("overall mean = %v, want 4", first["mean"])
	}
	if _, ok := sum["nps"]; !ok {
		t.Error("the summary has no NPS split")
	}
	if _, ok := sum["trend"]; !ok {
		t.Error("the summary has no trend")
	}

	// Narrowing to one account is a manager's filter, and it narrows rather than widens.
	w, out = f.do(t, "GET", "/api/feedback?tenant="+f.tenantID(t, "a@ibm.com"), "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("filtered read = %d", w.Code)
	}
	subs, _ = out["submissions"].([]any)
	if len(subs) != 1 {
		t.Errorf("filtered read returned %d rows, want 1", len(subs))
	}
	if strings.Contains(w.Body.String(), "boss@ibm.com") {
		t.Error("the filter did not narrow to the requested account")
	}
}

// A deployment with no relay must still accept feedback. Storing comes first and
// mailing second precisely so that an unfinished mail configuration costs a
// notification rather than the answer.
func TestFeedbackStoresWithNoMailPath(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "laptop") // registration still needs the sink
	jar := w.Result().Cookies()

	// Now take the mail path away entirely.
	t.Setenv(envMailDevSink, "")
	t.Setenv(envSMTPHost, "")

	w, _ = f.do(t, "POST", "/api/feedback", validFeedback(t, 4, longComment), jar)
	if w.Code != http.StatusCreated {
		t.Fatalf("submit with no mail path = %d %s", w.Code, w.Body)
	}
	all, err := f.reg.FeedbackList("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the submission was not stored: %+v", all)
	}
	if !all[0].MailedAt.IsZero() {
		t.Error("mailed_at is set although there was no relay to accept it")
	}
}

// Free text is attacker-controlled and ends up in an email. A CR/LF in it must not be
// able to add a header — the classic way a feedback form becomes an open relay to
// somebody's Bcc.
func TestFeedbackMailCannotInjectHeaders(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "laptop")
	jar := w.Result().Cookies()

	nasty := "Everything works well enough for daily use, thanks for building it.\r\n" +
		"Bcc: attacker@evil.test\r\nSubject: you have won a prize\r\n\r\nregards"
	body, err := json.Marshal(map[string]any{
		"agent":   "bob",
		"scores":  scoreMap(),
		"comment": nasty,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := f.do(t, "POST", "/api/feedback", string(body), jar); w.Code != http.StatusCreated {
		t.Fatalf("submit = %d", w.Code)
	}
	sink := waitForSink(t, "New context-guru feedback")

	// The text itself is delivered — it is the feedback — but never as a header. Nothing
	// in the message starts a line with one of the injected field names.
	for _, forged := range []string{"\nBcc:", "\rBcc:", "\nSubject: you have won"} {
		if strings.Contains(sink, forged) {
			t.Errorf("free text injected a header (%q):\n%s", forged, sink)
		}
	}
	if n := strings.Count(sink, "\nSubject: context-guru feedback"); n != 1 {
		t.Errorf("expected exactly one real subject line, found %d:\n%s", n, sink)
	}
	if !strings.Contains(sink, "Everything works well enough") {
		t.Errorf("the comment itself did not survive:\n%s", sink)
	}

	// The shared sender is the guard, so a hostile subject or recipient from anywhere is
	// neutralised too — not just this one caller. The injected text survives as literal
	// characters ON the header it was smuggled into, which is harmless; what must never
	// happen is a new line beginning with a field name.
	if err := sendMail("ops@ibm.com\r\nBcc: attacker@evil.test",
		"hello\r\nBcc: attacker@evil.test", "body"); err != nil {
		t.Fatalf("sendMail: %v", err)
	}
	sink = waitForSink(t, "hello")
	for _, line := range strings.Split(sink, "\n") {
		if strings.HasPrefix(strings.TrimRight(line, "\r"), "Bcc:") {
			t.Errorf("sendMail produced a forged header line %q:\n%s", line, sink)
		}
	}
}

// scoreMap is a complete valid rating set, for tests that build their own body.
func scoreMap() map[string]int {
	m := map[string]int{}
	for _, q := range tenant.FeedbackQuestions {
		m[q.Key] = 4
	}
	return m
}

// tenantID looks up an account's id by email, for building manager filter URLs.
func (f *hostedFixture) tenantID(t *testing.T, email string) string {
	t.Helper()
	tn, err := f.reg.ByEmail(email)
	if err != nil {
		t.Fatalf("ByEmail(%q): %v", email, err)
	}
	return tn.ID
}
