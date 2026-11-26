package dash

// A project with no measured tool declarations has nothing for Cleanup to price — and the page has
// to render that rather than break.
//
// THE BUG, first reported for a project whose requests were all imported from transcripts (the
// import is gone, but a project with no measured declarations still produces the same empty
// document). The timeline came back with `"buckets": null` — Go marshals a nil slice as null — and
// the chart read `.buckets.length` off it and threw, so the reader got a broken page.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The timeline's buckets are an EMPTY ARRAY, never null, when nothing in scope was measured.
func TestAnEmptyTimelineMarshalsBucketsAsAnEmptyArray(t *testing.T) {
	db := openTestDB(t)
	// Imported only: carries usage and no declarations.
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Project: "/Users/v/never-measured", CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	tl, err := db.ToolTimeline(Filter{Project: "/Users/v/never-measured"}, 30)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"buckets":null`) {
		t.Errorf("timeline = %s; buckets marshals as null. The chart reads .buckets.length and "+
			"throws on null, so a project with no measured traffic renders a broken page", b)
	}
	if !strings.Contains(string(b), `"buckets":[]`) {
		t.Errorf("timeline = %s; want an empty buckets array", b)
	}
}

// Through the endpoint the page actually calls, with the query it actually sends.
func TestTheInsightsEndpointForAProjectWithNoMeasuredToolsHasNoNullCollections(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Project: "/Users/v/never-measured", CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPI(t, db)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/insights?timeline=1&project="+url.QueryEscape("/Users/v/never-measured"), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/insights = %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Every collection the page indexes into. A null in any of them is a TypeError in the view.
	for _, field := range []string{`"buckets":null`, `"items":null`, `"owners":null`} {
		if strings.Contains(body, field) {
			t.Errorf("response contains %s; the view indexes into this collection, so null "+
				"throws and the reader gets a broken page instead of the reason it is empty",
				field)
		}
	}
}
