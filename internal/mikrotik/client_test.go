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

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
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
		Host:      host,
		Port:      port,
		Username:  "api",
		Password:  "pass",
		Timeout:   config.Duration(5e9),
		RateLimit: 100,
	}
	c := New(cfg, config.RetryConfig{MaxAttempts: 1}, testLogger())
	return c, srv
}

func sampleEntries() []addresslist.Entry {
	return []addresslist.Entry{
		{ID: "*1", Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:youtube"},
		{ID: "*2", Address: "1.1.1.0/24", List: "TO-VPN", Comment: "AUTO:youtube"},
		{ID: "*3", Address: "9.9.9.9/32", List: "TO-VPN", Comment: "AUTO:instagram"},
		{ID: "*4", Address: "5.5.5.0/24", List: "TO-VPN"},
		{ID: "*5", Address: "7.7.7.0/24", List: "OTHER-LIST", Comment: "AUTO:youtube"},
		{ID: "*6", Address: "6.6.6.6", List: "TO-VPN", Comment: "AUTO:youtube", Dynamic: "true"},
	}
}

func TestListServiceEntriesIsolation(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(sampleEntries())
	})
	defer srv.Close()

	got, err := c.ListServiceEntries(context.Background(), "TO-VPN", "AUTO", "youtube")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Только TO-VPN + AUTO:youtube + не-dynamic: *1, *2
	if len(got) != 2 || got[0].ID != "*1" || got[1].ID != "*2" {
		t.Fatalf("isolation violation, got %+v", got)
	}
}

func TestAddEntryObjectReply(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("add must use PUT, got %s", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/rest/ip/firewall/address-list") {
			t.Errorf("wrong endpoint: %s", r.URL.Path)
		}
		// Формат реального RouterOS v7: объект {"id":"*1A"}
		_, _ = w.Write([]byte(`{"id":"*1A"}`))
	})
	defer srv.Close()

	id, err := c.AddEntry(context.Background(), addresslist.Entry{Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:x"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if id != "*1A" {
		t.Fatalf("expected *1A, got %q", id)
	}
}

func TestAddEntryArrayAndDotID(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{".id":"*2B"}]`))
	})
	defer srv.Close()

	id, err := c.AddEntry(context.Background(), addresslist.Entry{Address: "8.8.8.0/24"})
	if err != nil || id != "*2B" {
		t.Fatalf("expected *2B/nil, got %q/%v", id, err)
	}
}

func TestListEntriesServerFilterVerifiedClientSide(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Сервер "фильтрует" по list=, но возвращает мусор другой списки —
		// клиентская проверка обязательна.
		_ = json.NewEncoder(w).Encode([]addresslist.Entry{
			{ID: "*1", Address: "8.8.8.0/24", List: "TO-VPN"},
			{ID: "*9", Address: "4.4.4.0/24", List: "WRONG"},
		})
	})
	defer srv.Close()

	got, err := c.ListEntries(context.Background(), "TO-VPN")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "*1" {
		t.Fatalf("client-side list filter failed: %+v", got)
	}
}

func TestListEntriesNoBrokenPaginationParams(t *testing.T) {
	var seenQueries []string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenQueries = append(seenQueries, r.URL.RawQuery)
		_ = json.NewEncoder(w).Encode([]addresslist.Entry{})
	})
	defer srv.Close()

	if _, err := c.ListEntries(context.Background(), "TO-VPN"); err != nil {
		t.Fatal(err)
	}
	if len(seenQueries) == 0 {
		t.Fatal("no requests")
	}
	for _, q := range seenQueries {
		// RouterOS v7 возвращает [] на address-list при наличии limit/skip
		lq := strings.ToLower(q)
		if strings.Contains(lq, "limit=") || strings.Contains(lq, "skip=") {
			t.Fatalf("pagination params break address-list reads: %s", q)
		}
		if !strings.Contains(q, ".proplist=") {
			t.Fatalf("must request only needed fields: %s", q)
		}
	}
	// first request must filter by list server-side
	if !strings.Contains(seenQueries[0], "list=TO-VPN") {
		t.Fatalf("first query must use ?list= filter: %s", seenQueries[0])
	}
}

func TestDeleteEntryRejectsBadID(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	defer srv.Close()

	for _, bad := range []string{"", "*1/../2", "*1?x", "*1#y"} {
		if err := c.DeleteEntry(context.Background(), bad); err == nil {
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
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			_, _ = w.Write([]byte(`{"id":"*new1"}`))
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
	cfg.Firewall.AddressList = "TO-VPN"
	cfg.Firewall.CommentPrefix = "AUTO"
	tx := NewTransaction(c, cfg, nil, "instagram", testLogger())

	err := tx.Apply(context.Background(),
		[]addresslist.Entry{{Address: "8.8.8.0/24"}},
		[]addresslist.Entry{{ID: "*old1", Address: "1.1.1.0/24"}},
	)
	if err == nil {
		t.Fatal("expected apply error")
	}
	if tx.State() != StateRolledBack {
		t.Fatalf("expected rolled_back, got %s", tx.State())
	}
	// rollback должен удалить добавленную запись
	if len(deleted) != 1 || deleted[0] != "*new1" {
		t.Fatalf("rollback did not delete added entry: %v", deleted)
	}
}
