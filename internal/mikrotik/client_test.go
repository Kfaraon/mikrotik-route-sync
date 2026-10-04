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

	entry, err := c.AddEntry(context.Background(), addresslist.Entry{Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:x"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if entry.ID != "*1A" {
		t.Fatalf("expected *1A, got %q", entry.ID)
	}
}

func TestAddEntryArrayAndDotID(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{".id":"*2B"}]`))
	})
	defer srv.Close()

	entry, err := c.AddEntry(context.Background(), addresslist.Entry{Address: "8.8.8.0/24"})
	if err != nil || entry.ID != "*2B" {
		t.Fatalf("expected *2B/nil, got %q/%v", entry.ID, err)
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

func TestDeleteEntryUsesRemoveEndpoint(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`[]`))
	})
	defer srv.Close()

	if err := c.DeleteEntry(context.Background(), "*A82"); err != nil {
		t.Fatal(err)
	}
	// RouterOS v7 НЕ принимает DELETE ?.id=*XX (400) — нужен POST /remove.
	if gotMethod != http.MethodPost || !strings.HasSuffix(gotPath, "/ip/firewall/address-list/remove") {
		t.Fatalf("expected POST .../address-list/remove, got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"numbers":"*A82"`) {
		t.Fatalf("expected {\"numbers\":\"*A82\"} body, got %s", gotBody)
	}
}

func TestDeleteEntryFallsBackToLegacyQuery(t *testing.T) {
	var legacyCalls int
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/remove"):
			http.Error(w, `{"detail":"not found","error":404,"message":"Not Found"}`, http.StatusNotFound)
		case r.Method == http.MethodDelete && r.URL.Query().Get(".id") == "*1":
			legacyCalls++
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	})
	defer srv.Close()

	if err := c.DeleteEntry(context.Background(), "*1"); err != nil {
		t.Fatal(err)
	}
	if legacyCalls != 1 {
		t.Fatalf("expected 1 legacy delete call, got %d", legacyCalls)
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
		case http.MethodPost:
			// основной синтаксис удаления: POST /remove {"numbers":...}
			if !strings.HasSuffix(r.URL.Path, "/remove") {
				http.Error(w, "bad path", http.StatusNotFound)
				return
			}
			var body struct {
				Numbers string `json:"numbers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Numbers == "*old1" {
				http.Error(w, `{"detail":"no such item","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			deleted = append(deleted, body.Numbers)
			_, _ = w.Write([]byte(`[]`))
		case http.MethodDelete:
			// legacy fallback
			id := r.URL.Query().Get(".id")
			if id == "*old1" {
				http.Error(w, `{"detail":"no such item","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
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
		nil,
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

// PROMPT II.4.4: порядок применения — сначала POST новых записей, затем
// re-enable (update), затем DELETE устаревших.
func TestTransactionApplyOrderAddUpdateRemove(t *testing.T) {
	var ops []string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			ops = append(ops, "add")
			_, _ = w.Write([]byte(`{"id":"*n1"}`))
		case http.MethodPatch:
			ops = append(ops, "update")
			b, _ := io.ReadAll(r.Body)
			// цель обновления — всегда включение записи
			if !strings.Contains(string(b), `"disabled":"false"`) {
				t.Errorf("PATCH must re-enable (disabled=false), got %s", b)
			}
			_, _ = w.Write([]byte(`[]`))
		case http.MethodPost:
			if !strings.HasSuffix(r.URL.Path, "/remove") {
				http.Error(w, "bad path", http.StatusNotFound)
				return
			}
			ops = append(ops, "remove")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
		}
	})
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Firewall.AddressList = "TO-VPN"
	cfg.Firewall.CommentPrefix = "AUTO"
	tx := NewTransaction(c, cfg, nil, "demo", testLogger())

	err := tx.Apply(context.Background(),
		[]addresslist.Entry{{Address: "8.8.8.0/24"}},
		[]addresslist.Entry{{ID: "*u1", Address: "1.1.1.0/24", Disabled: "true"}},
		[]addresslist.Entry{{ID: "*o1", Address: "9.9.9.0/24"}},
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if tx.State() != StateApplied {
		t.Fatalf("expected applied, got %s", tx.State())
	}
	want := []string{"add", "update", "remove"}
	if len(ops) != len(want) || ops[0] != want[0] || ops[1] != want[1] || ops[2] != want[2] {
		t.Fatalf("wrong operation order: %v (want %v)", ops, want)
	}
}

// Сбой обновления (PATCH) откатывает уже созданные записи и не доходит до удаления.
func TestTransactionUpdateFailureRollsBack(t *testing.T) {
	var removed []string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			_, _ = w.Write([]byte(`{"id":"*n1"}`))
		case http.MethodPatch:
			http.Error(w, `{"detail":"failure","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
		case http.MethodPost:
			var body struct {
				Numbers string `json:"numbers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			removed = append(removed, body.Numbers)
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	})
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Firewall.AddressList = "TO-VPN"
	cfg.Firewall.CommentPrefix = "AUTO"
	tx := NewTransaction(c, cfg, nil, "demo", testLogger())

	err := tx.Apply(context.Background(),
		[]addresslist.Entry{{Address: "8.8.8.0/24"}},
		[]addresslist.Entry{{ID: "*u1", Address: "1.1.1.0/24"}},
		[]addresslist.Entry{{ID: "*o1", Address: "9.9.9.0/24"}},
	)
	if err == nil {
		t.Fatal("expected update failure")
	}
	if tx.State() != StateRolledBack {
		t.Fatalf("expected rolled_back, got %s", tx.State())
	}
	// rollback удаляет созданную запись, запланированное удаление не выполнялось
	if len(removed) != 1 || removed[0] != "*n1" {
		t.Fatalf("rollback must delete only created entry: %v", removed)
	}
}

// Rollback после успешного re-enable: запись возвращается в disabled=true.
func TestTransactionRollbackRestoresDisabled(t *testing.T) {
	var removed []string
	var patches []string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			_, _ = w.Write([]byte(`{"id":"*n1"}`))
		case http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			patches = append(patches, string(b))
			_, _ = w.Write([]byte(`[]`))
		case http.MethodPost:
			var body struct {
				Numbers string `json:"numbers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Numbers == "*o1" {
				// удаление не удаётся → откат
				http.Error(w, `{"detail":"failure","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			removed = append(removed, body.Numbers)
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	})
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Firewall.AddressList = "TO-VPN"
	cfg.Firewall.CommentPrefix = "AUTO"
	tx := NewTransaction(c, cfg, nil, "demo", testLogger())

	err := tx.Apply(context.Background(),
		[]addresslist.Entry{{Address: "8.8.8.0/24"}},
		[]addresslist.Entry{{ID: "*u1", Address: "1.1.1.0/24", Disabled: "true"}},
		[]addresslist.Entry{{ID: "*o1", Address: "9.9.9.0/24"}},
	)
	if err == nil {
		t.Fatal("expected delete failure")
	}
	if tx.State() != StateRolledBack {
		t.Fatalf("expected rolled_back, got %s", tx.State())
	}
	if len(removed) != 1 || removed[0] != "*n1" {
		t.Fatalf("rollback must delete created entry: %v", removed)
	}
	// сначала re-enable (disabled=false), затем откат (disabled=true)
	if len(patches) != 2 {
		t.Fatalf("expected update + rollback re-disable patches, got %v", patches)
	}
	if !strings.Contains(patches[0], `"disabled":"false"`) {
		t.Fatalf("first patch must re-enable: %s", patches[0])
	}
	if !strings.Contains(patches[1], `"disabled":"true"`) {
		t.Fatalf("rollback must re-disable: %s", patches[1])
	}
}

// PATCH {path}/{.id} — рабочая схема обновления на живом RouterOS v7.
func TestUpdateEntryUsesPatchResourcePath(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{".id":"*42","disabled":"false"}`))
	})
	defer srv.Close()

	err := c.UpdateEntry(context.Background(), addresslist.Entry{
		ID: "*42", Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:x", Disabled: "false",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("expected PATCH, got %s", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/rest/ip/firewall/address-list/*42") {
		t.Fatalf("expected resource path with .id, got %s", gotPath)
	}
	if gotBody != `{"disabled":"false"}` {
		t.Fatalf("expected disabled-only body, got %s", gotBody)
	}
}

func TestUpdateEntryRejectsBadInput(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected, got %s %s", r.Method, r.URL.Path)
	})
	defer srv.Close()

	for _, bad := range []string{"", "*1/../2", "*1?x", "*1#y"} {
		err := c.UpdateEntry(context.Background(), addresslist.Entry{ID: bad, Disabled: "false"})
		if err == nil {
			t.Fatalf("expected rejection of id %q", bad)
		}
	}
	// Без цели обновления — ошибка, без запроса.
	if err := c.UpdateEntry(context.Background(), addresslist.Entry{ID: "*1"}); err == nil {
		t.Fatal("expected error when disabled is empty")
	}
}

func TestUpdateEntryErrorStatus(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"no such item","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
	})
	defer srv.Close()

	err := c.UpdateEntry(context.Background(), addresslist.Entry{ID: "*404", Disabled: "false"})
	if err == nil {
		t.Fatal("expected error on 400")
	}
}
