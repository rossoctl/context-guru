package proxy

import "testing"

// #1243. A 1-hour-on-wait ping moves the entry of a 5-minute main thread to 1 hour, and a read 5 to 60
// minutes after the ping is a rescue: a 1.25x rebuild became a 0.1x read. The accountant library cannot
// book it yet: it judges each part of an entry by the TTL of the breakpoint that WROTE it, and a read
// never changes that (rossoctl/accountant#106 adds the opt-in Provider.ReadSetsTTL and Ping.TTL, not
// released yet). The proxy does not work around it by re-deriving the TTL from timing.
//
// When the library version with Ping.TTL is wired in:
//   - PingSavings sets Ping.TTL = time.Hour for a report with OneHour, and the provider profile sets
//     ReadSetsTTL for the Bedrock route;
//   - this test is enabled and checks: a return more than 5 minutes after the ping books an EffectRescue
//     for the tokens both read, and a return inside 5 minutes books none.
func TestOneHourPingReturnAfterFiveMinutesBooksTheRescue(t *testing.T) {
	t.Skip("waiting for the accountant release with Provider.ReadSetsTTL and Ping.TTL (rossoctl/accountant#106)")
}
