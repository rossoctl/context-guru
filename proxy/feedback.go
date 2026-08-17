package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/tenant"
)

// The feedback surface: one write for everybody, one read for the manager.
//
// Two rules shape it.
//
// First, storing and notifying are separate. The POST commits the row and returns; the
// email is handed to a goroutine afterwards. A relay that takes ten seconds — or is not
// configured at all — therefore costs a notification and never an answer, and
// feedback.mailed_at says which submissions got out.
//
// Second, there is no read route for a plain user, not even for their own submissions.
// The owner asked for the aggregate to be theirs alone, and "you rated latency 2, the
// average is 4.4" is a disclosure about other people's answers however it is phrased.
// So: POST is scoped to the caller, GET is manager-only, and a user has nothing to read.

// ctlSubmitFeedback stores one submission for the signed-in account.
func (h *Handler) ctlSubmitFeedback(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	var in struct {
		Agent   string         `json:"agent"`
		Scores  map[string]int `json:"scores"`
		Comment string         `json:"comment"`
	}
	if err := readJSON(w, r, &in); err != nil {
		ctlErr(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return
	}
	// t, from the cookie — the body carries no tenant field and could not be believed if
	// it did.
	fb, err := h.registry().AddFeedback(t, in.Agent, in.Scores, in.Comment)
	if err != nil {
		switch {
		case isFeedbackRejection(err):
			// The message names the rule that was broken; the form shows it beside the
			// field. 422 rather than 400: the JSON parsed fine, the content did not qualify.
			ctlErr(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, tenant.ErrForbidden):
			ctlErr(w, http.StatusForbidden, "not permitted")
		default:
			ctlErr(w, http.StatusInternalServerError, "could not store that feedback")
		}
		return
	}
	// Stored. Everything below this line is best-effort notification, off the request
	// path: the address is resolved here (one indexed read) and the send happens in the
	// background, so a slow relay cannot hold this response open.
	go h.deliverFeedback(fb, h.managerAddress())
	writeJSON(w, http.StatusCreated, map[string]any{
		"status": "stored",
		"id":     fb.ID,
		"thanks": "Thank you — this went to the manager and is stored here.",
	})
}

// isFeedbackRejection reports whether the registry refused the CONTENT of a
// submission, as opposed to failing to store a valid one — a 422 the user can fix
// against a 500 they cannot.
func isFeedbackRejection(err error) bool {
	for _, t := range []error{tenant.ErrFeedbackText, tenant.ErrFeedbackLong,
		tenant.ErrFeedbackScore, tenant.ErrFeedbackDim, tenant.ErrFeedbackAgent} {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}

// ctlFeedback is the manager's view: every answer plus the arithmetic over them.
func (h *Handler) ctlFeedback(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if !t.IsManager() {
		ctlErr(w, http.StatusForbidden, "manager only")
		return
	}
	// A manager may narrow to one account. Only a manager reaches this line at all, so
	// the parameter widens nothing — the 403 above is the boundary, not this filter.
	target := strings.TrimSpace(r.URL.Query().Get("tenant"))
	if target == "*" {
		target = ""
	}
	all, err := h.registry().FeedbackList(target, 1000)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not read the feedback")
		return
	}
	out := make([]map[string]any, 0, len(all))
	for _, fb := range all {
		out = append(out, map[string]any{
			"id": fb.ID, "tenant": fb.TenantID, "email": fb.Email, "label": fb.Label,
			"created_at": msOrZero(fb.CreatedAt), "scores": fb.Scores,
			"agent": fb.Agent, "comment": fb.Comment,
			"mailed_at": msOrZero(fb.MailedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary":     tenant.Summarize(all),
		"submissions": out,
		// The star questions and the agents, keys and wording both, in the order the form
		// and this view present them — so the UI has one ordering and one set of words
		// rather than a hand-kept copy of either.
		"questions": tenant.FeedbackQuestions,
		"agents":    tenant.FeedbackAgents,
	})
}

// managerAddress is where a submission is mailed.
//
// --manager-email wins: it is the operator's stated destination, and it works before
// that person has registered. Otherwise the oldest enabled manager account, which is
// what a deployment bootstrapped without the flag has.
func (h *Handler) managerAddress() string {
	reg := h.registry()
	if reg == nil {
		return ""
	}
	if a := reg.ManagerEmail(); a != "" {
		return a
	}
	if a, err := reg.FirstManagerEmail(); err == nil {
		return a
	}
	return ""
}

// deliverFeedback mails the manager their copy and records that it got out.
//
// Runs in its own goroutine, so every failure here is logged and dropped rather than
// returned: the row is already committed and is the source of truth. A submission with
// mailed_at = 0 is a notification that never arrived, and the manager's view counts
// them so a dead relay is visible in the product rather than only in the log.
func (h *Handler) deliverFeedback(fb *tenant.Feedback, to string) {
	if to == "" {
		slog.Warn("context-guru: feedback stored but not mailed: no manager address " +
			"(set --manager-email or give an account the manager role)")
		return
	}
	if err := sendMail(to, feedbackSubject(fb), feedbackBody(fb)); err != nil {
		// The comment itself is not logged: it is the user's words, and a log is a wider
		// audience than the manager's mailbox.
		slog.Warn("context-guru: could not mail feedback to the manager",
			"feedback_id", fb.ID, "err", err.Error())
		return
	}
	if err := h.registry().MarkFeedbackMailed(fb.ID, time.Now()); err != nil {
		slog.Warn("context-guru: mailed feedback but could not record it",
			"feedback_id", fb.ID, "err", err.Error())
	}
}

// feedbackSubject names the submitter and the headline score.
//
// Every interpolated value is either a validated email (no whitespace — see
// checkEmail), an integer, or a label from tenant.FeedbackAgents — a closed list of
// literals, not the submitted string — and sendMail strips control characters from the
// whole line regardless. The free text is deliberately NOT here: a subject built from
// attacker-supplied prose is the header-injection primitive, and it is also just a bad
// subject line.
func feedbackSubject(fb *tenant.Feedback) string {
	return fmt.Sprintf("context-guru feedback: %d/5 overall on %s from %s",
		fb.Scores["overall"], tenant.AgentLabel(fb.Agent), fb.Email)
}

// feedbackBody renders the mail. Plain text, one score per line, prose last.
//
// The user's text goes in the BODY only. net/smtp's DATA writer is a textproto
// dot-writer, so it escapes a leading "." and normalises line endings itself — there is
// no way for a body to break out into the command stream.
func feedbackBody(fb *tenant.Feedback) string {
	var b strings.Builder
	fmt.Fprintf(&b, "New context-guru feedback\n\nFrom:  %s (%s)\nAgent: %s\nWhen:  %s\n\nRatings, 1-5 stars:\n",
		fb.Email, fb.Label, tenant.AgentLabel(fb.Agent), fb.CreatedAt.Format(time.RFC1123))
	// The questions in the order they were asked, worded as they were asked: a mail that
	// says "compaction 2" makes the manager go and read the form to find out what was rated.
	for _, q := range tenant.FeedbackQuestions {
		if v, ok := fb.Scores[q.Key]; ok {
			fmt.Fprintf(&b, "  %d/5  %s\n", v, q.Label)
		}
	}
	fmt.Fprintf(&b, "\nComment:\n%s\n", indent(fb.Comment))
	fmt.Fprintf(&b, "\n--\nStored as feedback #%d; the dashboard's Feedback tab has the "+
		"aggregate and every answer.\n", fb.ID)
	return b.String()
}

// indent prefixes each line, so a multi-paragraph comment is visibly one block and
// cannot be mistaken for the message's own structure.
func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\n  ")
}
