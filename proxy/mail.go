package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/tenant"
)

// Sending the verification code.
//
// Stdlib net/smtp, no dependency. The message is a handful of headers and one line of
// text; a mail library would be a supply-chain risk taken on to save a Sprintf.
//
// Everything is read from the ENVIRONMENT on each send, like the upstream credentials
// and the registration mode — so an operator can point at a different relay, or turn
// the dev sink off, without a restart. No default is a real host: if nothing is
// configured, sends FAIL and the flow tells the user so, because a registration that
// silently discards its code is worse than one that refuses.
//
//	CG_SMTP_HOST      relay hostname. Empty = no mail path (see devSink below).
//	CG_SMTP_PORT      default 25.
//	CG_SMTP_FROM      envelope + From: address. Default "context-guru@<hostname>".
//	CG_SMTP_HELO      EHLO name. Default the local hostname.
//	CG_SMTP_USER      SMTP AUTH user, if the relay wants one. Usually empty inside a
//	CG_SMTP_PASSWORD  corporate network, where the relay authenticates by source IP.
//	CG_SMTP_INSECURE  "1" skips STARTTLS certificate verification. For a relay with an
//	                  internal-CA or mismatched certificate ONLY, and it downgrades to
//	                  unauthenticated encryption — the code is still in the clear to
//	                  anyone who can MITM the relay path.
//	CG_MAIL_DEV_SINK  development escape hatch, see devSink.

const (
	envSMTPHost     = "CG_SMTP_HOST"
	envSMTPPort     = "CG_SMTP_PORT"
	envSMTPFrom     = "CG_SMTP_FROM"
	envSMTPHelo     = "CG_SMTP_HELO"
	envSMTPUser     = "CG_SMTP_USER"
	envSMTPPassword = "CG_SMTP_PASSWORD"
	envSMTPInsecure = "CG_SMTP_INSECURE"
	envMailDevSink  = "CG_MAIL_DEV_SINK"

	// mailTimeout bounds a whole send. A registration request waits on this, so it is
	// short: a relay that has not answered in 10 seconds is not about to.
	mailTimeout = 10 * time.Second
)

// errNoMailPath is returned when neither a relay nor a dev sink is configured. It is a
// 503 rather than a 500: the service is fine, the operator has not finished setting it
// up, and the message says which knob is missing.
var errNoMailPath = statusError{503,
	"this deployment cannot send email yet, so accounts cannot be verified; " +
		"the operator must set CG_SMTP_HOST (see docs/hosted-service.md)"}

// devSink resolves CG_MAIL_DEV_SINK: "" (off), "log", or a file path.
//
// This exists because a deployment with no relay would otherwise be unable to create
// its first account at all. It is OFF unless explicitly set, and it must never be set
// on a real deployment: "log" writes the six digits to the server log, which means
// anyone who can read the log can complete anyone's sign-in. There is no automatic
// "am I production" check to lean on here — no such flag exists in this build — so the
// safeguard is that the operator has to type the variable, and every send through this
// path logs a WARNING saying what it just did.
func devSink() string { return strings.TrimSpace(os.Getenv(envMailDevSink)) }

// sendCode mails a verification code, or writes it to the configured dev sink.
//
// The code is passed straight through to the message body and NOWHERE else: not into
// an slog attribute, not into an error string, not into the HTTP response. The only
// exception is the dev sink, which exists precisely to print it.
func sendCode(to string, purpose tenant.CodePurpose, c tenant.Code) error {
	subject := "Your context-guru verification code"
	what := "finish signing in"
	if purpose == tenant.PurposeRegister {
		what = "confirm this address and finish creating your context-guru account"
	}
	body := fmt.Sprintf(`Your context-guru verification code is:

    %s

Enter it to %s. It expires at %s — about %d minutes from now — and can be used once.

If you did not ask for this, you can ignore it: nothing happens until the code is
entered, and it stops working shortly.
`, c.Plain, what, c.ExpiresAt.Format(time.RFC1123), int(tenant.CodeTTL/time.Minute))

	if sink := devSink(); sink != "" && os.Getenv(envSMTPHost) == "" {
		return writeDevSink(sink, to, subject, body)
	}
	host := strings.TrimSpace(os.Getenv(envSMTPHost))
	if host == "" {
		return errNoMailPath
	}
	return smtpSend(host, to, subject, body)
}

// writeDevSink puts the mail where a developer can read it. "log" goes to slog at
// INFO (with a WARNING first, so nobody discovers this by accident in a log they
// forgot was public); anything else is treated as a file path and appended to with
// 0600, owner-only.
func writeDevSink(sink, to, subject, body string) error {
	if strings.EqualFold(sink, "log") {
		slog.Warn("context-guru: " + envMailDevSink + "=log is set: verification codes are " +
			"being written to this log in plaintext. NEVER set this on a real deployment.")
		slog.Info("context-guru: verification mail (dev sink)", "to", to, "body", body)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(sink), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(sink, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "--- %s\nTo: %s\nSubject: %s\n\n%s\n",
		time.Now().Format(time.RFC3339), to, subject, body)
	return err
}

// smtpSend delivers one message over SMTP, upgrading to TLS when the relay offers it.
//
// STARTTLS is attempted whenever advertised and, if it is advertised, a FAILURE to
// negotiate aborts the send rather than continuing in the clear. A code that travels
// plaintext because a certificate expired is exactly the silent downgrade that makes
// "we use TLS" untrue. A relay that does not advertise STARTTLS at all sends
// unencrypted — unavoidable, and the reason the port and host are the operator's
// explicit choice.
func smtpSend(host, to, subject, body string) error {
	port := strings.TrimSpace(os.Getenv(envSMTPPort))
	if port == "" {
		port = "25"
	}
	from := strings.TrimSpace(os.Getenv(envSMTPFrom))
	if from == "" {
		from = "context-guru@" + localHostname()
	}
	helo := strings.TrimSpace(os.Getenv(envSMTPHelo))
	if helo == "" {
		helo = localHostname()
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), mailTimeout)
	if err != nil {
		return fmt.Errorf("smtp dial %s:%s: %w", host, port, err)
	}
	// One deadline for the whole conversation, set on the connection rather than
	// per-command: a relay that accepts the TCP handshake and then stalls would
	// otherwise hold a registration request open indefinitely.
	_ = conn.SetDeadline(time.Now().Add(mailTimeout))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello(helo); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{
			ServerName:         host,
			InsecureSkipVerify: os.Getenv(envSMTPInsecure) == "1", //nolint:gosec // operator opt-in, documented above
			MinVersion:         tls.VersionTLS12,
		}); err != nil {
			return fmt.Errorf("smtp starttls: %w (set %s=1 only if this relay uses an internal CA)",
				err, envSMTPInsecure)
		}
	}
	// AUTH only when a user is configured AND the relay offers it. The password is read
	// here, at send time, and never stored, logged, or echoed.
	if user := strings.TrimSpace(os.Getenv(envSMTPUser)); user != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: " + envSMTPUser + " is set but this relay offers no AUTH")
		}
		if err := c.Auth(smtp.PlainAuth("", user, os.Getenv(envSMTPPassword), host)); err != nil {
			// Deliberately not wrapped with anything from the credential.
			return errors.New("smtp: authentication rejected by the relay")
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	msg := strings.Join([]string{
		"From: context-guru <" + from + ">",
		"To: " + to,
		"Subject: " + subject,
		"Date: " + time.Now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Auto-Submitted: auto-generated",
		"", body, "",
	}, "\r\n")
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	return c.Quit()
}

func localHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	return h
}

// MailConfigured reports whether this deployment has any way to send a code, for the
// startup banner — so an operator learns at boot that registration cannot complete,
// rather than from a user's bug report.
func MailConfigured() (bool, string) {
	if h := strings.TrimSpace(os.Getenv(envSMTPHost)); h != "" {
		// A From: without a dot in its domain is what an unset CG_SMTP_FROM produces on a
		// host with a short name. A relay may well accept it and then have nowhere to send
		// the bounce, which looks exactly like "verification email is broken" with nothing
		// in the log — so name it here rather than letting the operator find out from a
		// user who never received a code.
		if from := strings.TrimSpace(os.Getenv(envSMTPFrom)); from == "" ||
			!strings.Contains(from[strings.IndexByte(from, '@')+1:], ".") {
			return true, "smtp " + h + " (WARNING: set " + envSMTPFrom +
				" to an address in a real domain; the default is not routable)"
		}
		return true, "smtp " + h
	}
	if s := devSink(); s != "" {
		return true, "DEV SINK (" + s + ") — codes are not emailed"
	}
	return false, "none: set " + envSMTPHost
}
