package proxy

// The inventory must be scanned on the request's WIRE, not on its provider (#817). A Responses
// request has provider "openai" and reads as OpenAI Chat if the provider is passed. The call is
// one line in the handler, so this is a source-level check, like the project hand-off test in
// inventoryproject_test.go; the dialect rules are tested in dash and schema.

import (
	"os"
	"regexp"
	"testing"

	"github.com/rossoctl/context-guru/schema"
)

func TestTheInventoryIsScannedOnTheWire(t *testing.T) {
	b, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !regexp.MustCompile(`wire := schema\.WireFor\(string\(provider\), api\)\s+cp\.noteInventory\(wire, body\)`).MatchString(src) {
		t.Error("chat() must compute the wire with schema.WireFor and pass it to noteInventory")
	}
	if regexp.MustCompile(`noteInventory\(string\(provider\)`).MatchString(src) {
		t.Error("noteInventory is given the provider; a Responses body would read as OpenAI Chat")
	}
}

// The routes pass api "" or "responses"; the wire for the Responses route is Responses.
func TestTheResponsesRouteIsTheResponsesWire(t *testing.T) {
	if got := schema.WireFor("openai", "responses"); got != schema.WireResponses {
		t.Errorf("wire = %q", got)
	}
	if got := schema.WireFor("openai", ""); got != schema.WireOpenAI {
		t.Errorf("wire = %q", got)
	}
}
