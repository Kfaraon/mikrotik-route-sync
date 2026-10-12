package web

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/history"
)

// TestServiceRowsCarryGUIDAndFilterHistory — /api/v1/services отдаёт GUID
// текущего воплощения, а «Последний запуск» берётся только из записей этого
// воплощения (записи прежнего сервиса с тем же именем отсекаются).
func TestServiceRowsCarryGUIDAndFilterHistory(t *testing.T) {
	srv, cfg := newTestServer(t)

	const id = "11111111-1111-4111-8111-111111111111"
	srv.cfg.Lock()
	if srv.cfg.ServiceIDs == nil {
		srv.cfg.ServiceIDs = map[string]string{}
	}
	srv.cfg.ServiceIDs["testsvc"] = id
	srv.cfg.Unlock()

	now := time.Now()
	srv.syncer.History().Add(history.Record{
		Time: now.Add(-time.Hour), Service: "testsvc", ServiceID: "old-incarnation", Added: 1,
	})
	srv.syncer.History().Add(history.Record{
		Time: now, Service: "testsvc", ServiceID: id, Added: 5,
	})

	rows := srv.serviceRows(httptest.NewRequest("GET", "/api/v1/services", nil))
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].GUID != id {
		t.Fatalf("row GUID mismatch: %q", rows[0].GUID)
	}
	if cfg.ServiceIDs["testsvc"] != id {
		t.Fatalf("config id mismatch: %q", cfg.ServiceIDs["testsvc"])
	}
	// LastSync должен отражать запись текущего воплощения (Added=5), а не old.
	if !contains(rows[0].LastSync, "успех") {
		t.Fatalf("unexpected LastSync: %q", rows[0].LastSync)
	}
}

// TestAPIHistoryFiltersByIncarnation — при запросе конкретного сервиса API
// отдаёт только записи его текущего воплощения.
func TestAPIHistoryFiltersByIncarnation(t *testing.T) {
	srv, _ := newTestServer(t)

	const id = "22222222-2222-4222-8222-222222222222"
	srv.cfg.Lock()
	if srv.cfg.ServiceIDs == nil {
		srv.cfg.ServiceIDs = map[string]string{}
	}
	srv.cfg.ServiceIDs["testsvc"] = id
	srv.cfg.Unlock()

	srv.syncer.History().Add(history.Record{Service: "testsvc", ServiceID: "old", Added: 1})
	srv.syncer.History().Add(history.Record{Service: "testsvc", ServiceID: id, Added: 2})

	rec := httptest.NewRequest("GET", "/api/v1/history?service=testsvc", nil)
	w := httptest.NewRecorder()
	srv.apiHistory(w, rec)

	body := w.Body.String()
	if contains(body, `"added":1`) {
		t.Fatalf("old incarnation leaked into history: %s", body)
	}
	if !contains(body, `"added":2`) {
		t.Fatalf("current incarnation missing: %s", body)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
