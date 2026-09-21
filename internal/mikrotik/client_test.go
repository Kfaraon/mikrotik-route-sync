package mikrotik

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	cfg := config.MikroTikConfig{
		Host:          host,
		Port:          port,
		Username:      "api",
		Password:      "pass",
		Timeout:       config.Duration(5e9),
		CommentPrefix: "AUTO",
		RateLimit:     100,
	}
	c := New(cfg, config.RetryConfig{MaxAttempts: 1}, testLogger())
	return c, srv
}

func TestListServiceRoutesFiltersByComment(t *testing.T) {
	routes := []Route{
		{ID: "*1", DstAddress: "8.8.8.0/24", Comment: "AUTO:instagram"},
		{ID: "*2", DstAddress: "1.1.1.0/24", Comment: "AUTO:youtube"},
		{ID: "*3", DstAddress: "9.9.9.0/24", Comment: "manual route"},
	}
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(routes)
	})
	defer srv.Close()

	got, err := c.ListServiceRoutes(context.Background(), "instagram")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "*1" {
		t.Fatalf("isolation violation, got %+v", got)
	}
}

func TestAddRouteReturnsID(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("add must use PUT, got %s", r.Method)
		}
		// Формат реального RouterOS v7: объект {"id":"*1A"}
		_, _ = w.Write([]byte(`{"id":"*1A"}`))
	})
	defer srv.Close()

	id, err := c.AddRoute(context.Background(), Route{DstAddress: "8.8.8.0/24", Comment: "AUTO:x"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if id != "*1A" {
		t.Fatalf("expected *1A, got %q", id)
	}
}

func TestAddRouteArrayAndDotID(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{".id":"*2B"}]`))
	})
	defer srv.Close()

	id, err := c.AddRoute(context.Background(), Route{DstAddress: "8.8.8.0/24"})
	if err != nil || id != "*2B" {
		t.Fatalf("expected *2B/nil, got %q/%v", id, err)
	}
}

func TestListServiceRoutesNoServerCommentFilter(t *testing.T) {
	var gotPath string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		_ = json.NewEncoder(w).Encode([]Route{
			{ID: "*1", DstAddress: "8.8.8.0/24", Comment: "AUTO:youtube"},
			{ID: "*2", DstAddress: "1.1.1.0/24", Comment: "AUTO:youtube"},
			{ID: "*3", DstAddress: "9.9.9.9/32", Comment: "AUTO:instagram"},
			{ID: "*4", DstAddress: "5.5.5.0/24"},
		})
	})
	defer srv.Close()

	got, err := c.ListServiceRoutes(context.Background(), "youtube")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotPath, "comment=") {
		t.Fatalf("must not rely on server-side comment filter: %s", gotPath)
	}
	if len(got) != 2 || got[0].ID != "*1" || got[1].ID != "*2" {
		t.Fatalf("bad filter result: %+v", got)
	}
}

func TestDeleteRouteRejectsBadID(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	defer srv.Close()

	for _, bad := range []string{"", "*1/../2", "*1?x", "*1#y"} {
		if err := c.DeleteRoute(context.Background(), bad); err == nil {
			t.Fatalf("expected rejection of id %q", bad)
		}
	}
}

func TestRedactHidesSecrets(t *testing.T) {
	out := redact(`{"password":"hunter2","token":"abcdef","note":"keep"}`)
	if strings.Contains(out, "hunter2") || strings.Contains(out, "abcdef") {
		t.Fatalf("redaction failed: %s", out)
	}
	if !strings.Contains(out, "keep") {
		t.Fatalf("redaction destroyed unrelated data: %s", out)
	}
}

func TestTransactionRollbackOnFailure(t *testing.T) {
	var deleted []string
	var added []string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			added = append(added, r.URL.Path)
			_, _ = w.Write([]byte(`[{".id":"*new1"}]`))
		case http.MethodDelete:
			id := r.URL.Query().Get(".id")
			if id == "*old1" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			deleted = append(deleted, id)
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}
	})
	defer srv.Close()

	cfg := &config.Config{}
	cfg.MikroTik.CommentPrefix = "AUTO"
	tx := NewTransaction(c, cfg, nil, "instagram", testLogger())

	err := tx.Apply(context.Background(),
		[]Route{{DstAddress: "8.8.8.0/24"}},
		[]Route{{ID: "*old1", DstAddress: "1.1.1.0/24"}},
	)
	if err == nil {
		t.Fatal("expected apply error")
	}
	if tx.State() != StateRolledBack {
		t.Fatalf("expected rolled_back, got %s", tx.State())
	}
	// rollback должен удалить добавленный маршрут
	if len(deleted) != 1 || deleted[0] != "*new1" {
		t.Fatalf("rollback did not delete added route: %v", deleted)
	}
}
