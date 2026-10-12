package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestLogSyncResultCarriesServiceID — запись результата синка содержит
// service_id: по нему UI отличает логи текущего воплощения сервиса.
func TestLogSyncResultCarriesServiceID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	const id = "11111111-1111-4111-8111-111111111111"
	LogSyncResult(logger, SyncResult{
		Service:   "youtube.com",
		ServiceID: id,
		Success:   true,
	}, time.Second)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("decode log: %v (%s)", err, buf.String())
	}
	if got := entry["service_id"]; got != id {
		t.Fatalf("service_id missing/wrong: %v", got)
	}
	if !strings.Contains(buf.String(), id) {
		t.Fatalf("raw log does not contain id: %s", buf.String())
	}
}
