package dash

// A project whose whole history was imported has nothing for Cleanup to price — and the page has
// to say so rather than break.
//
// THE BUG, reported as "if I select /Users/vitalyz/Projects/ai-native-company I get a bug". That
// project's 115 requests were all recovered from Claude Code's transcripts, before the proxy was
// installed; none was measured. Cleanup prices what each request DECLARED, which a transcript does
// not record, so the scoped document is legitimately empty. But the timeline came back with
// `"buckets": null` — Go marshals a nil slice as null — and the chart read `.buckets.length` off
// it and threw, so the reader got a broken page instead of an explanation.

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
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Source: SourceImported,
			Project: "/Users/v/imported-only", CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	tl, err := db.ToolTimeline(Filter{Project: "/Users/v/imported-only"}, 30)
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
func TestTheInsightsEndpointForAnImportedOnlyProjectHasNoNullCollections(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Source: SourceImported,
			Project: "/Users/v/imported-only", CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPI(t, db)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/api/insights?timeline=1&project="+url.QueryEscape("/Users/v/imported-only"), nil))
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
	// AND THE DOCUMENT SAYS WHY IT IS EMPTY. Coverage already carries the split; the page needs it
	// to explain that nothing here was measured, rather than implying the project carried nothing.
	var doc InsightsDoc
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Coverage == nil || doc.Coverage.MeasuredRequests != 0 || doc.Coverage.ImportedRequests != 1 {
		t.Errorf("coverage = %+v; want 0 measured and 1 imported, which is what the view needs "+
			"to say 'nothing here was measured' rather than 'nothing here was carried'",
			doc.Coverage)
	}
}
