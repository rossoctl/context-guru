package bob

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Bob's client rebuilds its base URL from the profile's region_domain, keeping our
// PORT and swapping our host — so a proxied session dies with "fetch failed" and no
// request in the log. Dropping the region fields is what keeps it pointed at us; every
// other field has to survive, because instance_id is what its next call is keyed on.
func TestStripProfileRegion(t *testing.T) {
	in := []byte(`{"user_id":"u@ibm.com","instances":[` +
		`{"instance_id":"i1","region":"us-east","region_domain":"us-east.bob.ibm.com","plan_id":"p"},` +
		`{"instance_id":"i2"}]}`)
	out, ok := StripProfileRegion(in)
	if !ok {
		t.Fatal("StripProfileRegion refused a profile document")
	}
	if s := string(out); strings.Contains(s, "region") {
		t.Errorf("region survived: %s", s)
	}
	var doc struct {
		UserID    string `json:"user_id"`
		Instances []struct {
			ID     string `json:"instance_id"`
			PlanID string `json:"plan_id"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten profile does not parse: %v", err)
	}
	if doc.UserID != "u@ibm.com" || len(doc.Instances) != 2 ||
		doc.Instances[0].ID != "i1" || doc.Instances[0].PlanID != "p" || doc.Instances[1].ID != "i2" {
		t.Errorf("rewrite lost a field: %s", out)
	}
}

// Anything we cannot recognise is forwarded untouched rather than mangled.
func TestStripProfileRegionRefusesWhatIsNotAProfile(t *testing.T) {
	for _, bad := range []string{"not json", `{"instances":"nope"}`, `[]`} {
		if out, ok := StripProfileRegion([]byte(bad)); ok || out != nil {
			t.Errorf("StripProfileRegion(%q) = %q, %v; want nil, false", bad, out, ok)
		}
	}
}

// Only the profile GET is rewritten; the model route and everything else is verbatim.
func TestIsProfileMatchesOnlyTheProfileGet(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/admin/v1/profile", true},
		{http.MethodGet, "/ADMIN/v1/Profile", true},
		{http.MethodPost, "/admin/v1/profile", false},
		{http.MethodGet, "/inference/v1/model/info", false},
	} {
		if got := IsProfile(c.method, c.path); got != c.want {
			t.Errorf("IsProfile(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestProfilePathIsTheAdminRoute(t *testing.T) {
	if ProfilePath != "/admin/v1/profile" {
		t.Fatalf("ProfilePath = %q", ProfilePath)
	}
}

func TestIsModelInfoMatchesBothModelInfoGets(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/inference/v1/model/info", true},
		{http.MethodGet, "/v1/model/info", true},
		{http.MethodPost, "/inference/v1/model/info", false},
		{http.MethodGet, "/INFERENCE/v1/model/info", false},
		{http.MethodGet, "/admin/v1/profile", false},
	} {
		if got := IsModelInfo(c.method, c.path); got != c.want {
			t.Errorf("IsModelInfo(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// The LiteLLM shape Bob reads: data[].model_name -> data[].model_info.max_input_tokens.
func TestModelWindowsReadsTheLiteLLMShape(t *testing.T) {
	got := ModelWindows([]byte(`{"data":[{"model_name":"claude-opus-5","model_info":{"max_input_tokens":400000}},{"model_name":"x","model_info":{}}]}`))
	if len(got) != 1 || got["claude-opus-5"] != 400000 {
		t.Fatalf("ModelWindows = %v, want map[claude-opus-5:400000]", got)
	}
}

func TestModelWindowsIsBoundedAndSafe(t *testing.T) {
	for _, bad := range []string{
		`not json`,
		`{"data":"x"}`,
		`{"data":[{"model_name":"a","model_info":{"max_input_tokens":-5}}]}`,
		`{"data":[{"model_name":"a","model_info":{"max_input_tokens":0}}]}`,
		`{"data":[{"model_name":"a","model_info":{"max_input_tokens":1099511627776}}]}`,
		`{"data":[{"model_name":"a","model_info":{"max_input_tokens":"1000"}}]}`,
		`{"data":[{"model_name":"","model_info":{"max_input_tokens":1000}}]}`,
		`{"data":[{"model_name":"` + strings.Repeat("n", 129) + `","model_info":{"max_input_tokens":1000}}]}`,
	} {
		if got := ModelWindows([]byte(bad)); len(got) != 0 {
			t.Errorf("ModelWindows(%.60q) = %v, want empty", bad, got)
		}
	}
	edge := ModelWindows([]byte(`{"data":[{"model_name":"` + strings.Repeat("n", 128) +
		`","model_info":{"max_input_tokens":1099511627775}},{"model_name":"one","model_info":{"max_input_tokens":1}}]}`))
	if len(edge) != 2 || edge[strings.Repeat("n", 128)] != 1<<40-1 || edge["one"] != 1 {
		t.Errorf("edge values = %v", edge)
	}
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"model_name":"m%d","model_info":{"max_input_tokens":1000}}`, i)
	}
	sb.WriteString(`]}`)
	if got := ModelWindows([]byte(sb.String())); len(got) != MaxModelWindows {
		t.Errorf("map holds %d entries, want %d", len(got), MaxModelWindows)
	}
	if MaxModelWindows != 512 {
		t.Errorf("MaxModelWindows = %d, want 512", MaxModelWindows)
	}
}
