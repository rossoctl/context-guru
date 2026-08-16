package proxy

import (
	"os"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/tenant"
)

// A LIVE send through a real relay. Skipped unless CG_SMTP_LIVE_TO names a mailbox to
// send to, because a unit test that mails a stranger is not a unit test.
//
// This is the only way to know the mail path works: everything else in the suite goes
// through the file sink, which proves the code is generated and formatted but not that
// any relay will accept it. Run it after changing smtpSend, or when standing up a new
// deployment:
//
//	CG_SMTP_HOST=na.relay.ibm.com \
//	CG_SMTP_FROM=context-guru@<this-host> \
//	CG_SMTP_LIVE_TO=you@example.com \
//	  go test ./proxy/ -run TestSendCodeLive -v
//
// It asserts only that the relay ACCEPTED the message. Whether it landed in the inbox is
// not something this process can see, so the test does not claim it — go and look.
func TestSendCodeLive(t *testing.T) {
	to := os.Getenv("CG_SMTP_LIVE_TO")
	if to == "" || os.Getenv(envSMTPHost) == "" {
		t.Skip("set CG_SMTP_LIVE_TO and " + envSMTPHost + " to run a live send")
	}
	// A throwaway code that is not a pending challenge for any account: nothing can be
	// signed in to with it, so it is safe to put in a real mailbox.
	if err := sendCode(to, tenant.PurposeLogin, tenant.Code{
		Plain: "000000", ExpiresAt: time.Now().Add(tenant.CodeTTL),
	}); err != nil {
		t.Fatalf("live send to %s: %v", to, err)
	}
	t.Logf("the relay accepted a message for %s; check the mailbox to confirm delivery", to)
}
