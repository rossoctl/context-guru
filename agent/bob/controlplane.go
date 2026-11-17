package bob

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// ProfilePath is the profile route Bob's CLI calls before anything else, and the
// one response a host does not forward verbatim. See StripProfileRegion.
const ProfilePath = "/admin/v1/profile"

// MaxModelWindows is the most models ModelWindows returns.
const MaxModelWindows = 512

// IsProfile reports whether a request is the profile GET.
func IsProfile(method, path string) bool {
	return method == http.MethodGet && strings.EqualFold(path, ProfilePath)
}

// StripProfileRegion removes region/region_domain from every instance in a Bob profile
// document, reporting false when the body is not one (then it must be forwarded as-is).
func StripProfileRegion(b []byte) ([]byte, bool) {
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, false
	}
	insts, ok := doc["instances"].([]any)
	if !ok {
		return nil, false
	}
	for _, it := range insts {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		delete(m, "region")
		delete(m, "region_domain")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// IsModelInfo reports whether a request is the model/info GET.
func IsModelInfo(method, path string) bool {
	return method == http.MethodGet && (path == "/inference/v1/model/info" || path == "/v1/model/info")
}

// ModelWindows reads data[].model_name -> data[].model_info.max_input_tokens (the LiteLLM
// shape Bob reads in getLiteLLMModels). A body that is not that shape gives an empty map.
// Metadata only: a number for each model id, at most MaxModelWindows entries.
func ModelWindows(body []byte) map[string]int {
	got := map[string]int{}
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return got
	}
	data.ForEach(func(_, v gjson.Result) bool {
		name := v.Get("model_name").String()
		w := v.Get("model_info.max_input_tokens")
		if name != "" && len(name) <= 128 && w.Type == gjson.Number && w.Int() > 0 && w.Int() < 1<<40 && len(got) < MaxModelWindows {
			got[name] = int(w.Int())
		}
		return true
	})
	return got
}
