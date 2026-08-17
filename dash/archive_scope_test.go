package dash

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// archiveScopeFixture: two tenants each with one archived session.
func archiveScopeFixture(t *testing.T, principal func(*http.Request) (Principal, bool)) (*scopeFixture, *memRemote) {
	t.Helper()
	m := newMemRemote()
	rec, err := NewRecorder(Options{DBPath: ":memory:", CaptureContent: true,
		ContentCap: 4096, Remote: m})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })

	for _, tid := range []string{"tenant-a", "tenant-b"} {
		seedSessionWithContent(t, rec.db, tid, tid+":old", 2, 90*24*time.Hour)
		cands, err := rec.db.coldSessions(time.Now().UnixMilli(), ArchiveFull, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cands {
			if c.TenantID != tid {
				continue
			}
			if _, err := rec.ArchiveSessionFull(context.Background(), c); err != nil {
				t.Fatal(err)
			}
		}
	}

	f := &scopeFixture{rec: rec, api: NewAPI(rec), ids: map[string]int64{}}
	if principal != nil {
		f.api.SetAuth(principal)
	}
	f.mux = http.NewServeMux()
	f.api.Mount(f.mux)
	return f, m
}

// The archive must not become a way around tenant scoping.
func TestArchiveAPIIsTenantScoped(t *testing.T) {
	f, _ := archiveScopeFixture(t, asTenant("tenant-a", false))

	code, body := f.get(t, "/api/archive")
	if code != http.StatusOK {
		t.Fatalf("/api/archive = %d", code)
	}
	if strings.Contains(body, "tenant-b") {
		t.Errorf("the archive index leaked tenant-b:\n%s", body)
	}
	if !strings.Contains(body, "tenant-a") {
		t.Errorf("the caller cannot see their own archive:\n%s", body)
	}
	// Fetching another tenant's archived session by name is a 404.
	code, _ = f.get(t, "/api/archive/tenant-b:old")
	if code != http.StatusNotFound {
		t.Errorf("fetching another tenant's archive = %d, want 404", code)
	}
	// Their own comes back with content.
	code, body = f.get(t, "/api/archive/tenant-a:old")
	if code != http.StatusOK {
		t.Fatalf("own archived session = %d %s", code, body)
	}
	if !strings.Contains(body, "TRANSCRIPT-tenant-a:old") {
		t.Errorf("own archived transcripts missing:\n%s", body)
	}
}

// NEW RULE, replacing "archived transcripts for nobody": a manager reads archived metrics
// AND archived transcripts for everyone — the same rule as the live path, so a session
// does not become unreadable to a manager the moment it goes cold.
func TestManagerReadsAnyTenantsArchivedTranscripts(t *testing.T) {
	f, _ := archiveScopeFixture(t, asTenant("tenant-a", true))
	code, body := f.get(t, "/api/archive/tenant-b:old")
	if code != http.StatusOK {
		t.Fatalf("manager reading another tenant's archive = %d", code)
	}
	if !strings.Contains(body, "TRANSCRIPT-tenant-b") {
		t.Errorf("a manager could not read another tenant's archived transcripts:\n%s", body)
	}
	if !strings.Contains(body, "tenant-b:old") {
		t.Errorf("a manager could not see the archived session's metadata:\n%s", body)
	}
}

// An unreachable remote must be reported as unavailable, not as "no such session" —
// otherwise a Box outage looks like data loss.
func TestArchiveFetchReportsRemoteOutageDistinctly(t *testing.T) {
	f, m := archiveScopeFixture(t, asTenant("tenant-a", false))
	m.failGet = context.DeadlineExceeded
	code, body := f.get(t, "/api/archive/tenant-a:old")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("with cold storage down = %d, want 503\n%s", code, body)
	}
	if !strings.Contains(body, "unreachable") {
		t.Errorf("the error does not say the remote is unreachable: %s", body)
	}
}

// An unauthenticated caller gets nothing from the archive routes either.
func TestArchiveRoutesFailClosed(t *testing.T) {
	f, _ := archiveScopeFixture(t, func(*http.Request) (Principal, bool) { return Principal{}, false })
	for _, p := range []string{"/api/archive", "/api/archive/tenant-a:old"} {
		if code, _ := f.get(t, p); code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", p, code)
		}
	}
}
