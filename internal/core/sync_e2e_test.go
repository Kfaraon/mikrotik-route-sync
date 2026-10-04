package core

// E2E-тесты полного цикла синхронизации: формирование IPv4-набора
// (collect → validate → aggregate) → diff → применение на «RouterOS»
// (PUT/PATCH/POST remove) через фейковый REST-сервер и фейковый коллектор.
//
// Проверяется соответствие PROMPT.md:
//   - IV.1/IV.4: добавление записей list=TO-VPN, comment=AUTO:<service>,
//     disabled=false; порядок add → update → remove;
//   - II.4: fail-closed — при ошибке сбора записи не трогаются;
//   - II.6: safety-блок массового удаления и обход через --force;
//   - II.7: идемпотентность + учёт disabled (re-enable при
//     firewall.manage_disabled=true);
//   - II.1: изоляция записей других сервисов.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

// ============================================================================
// Фейковый RouterOS: stateful /ip/firewall/address-list
// ============================================================================

type fakeRouter struct {
	mu      sync.Mutex
	entries map[string]addresslist.Entry
	seq     int
	ops     []string // только мутации: PUT / PATCH / DELETE
}

func newFakeRouter(seed ...addresslist.Entry) *fakeRouter {
	fr := &fakeRouter{entries: map[string]addresslist.Entry{}}
	for _, e := range seed {
		fr.entries[e.ID] = e
	}
	return fr
}

func (fr *fakeRouter) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		defer fr.mu.Unlock()

		path := r.URL.Path
		switch {
		case r.Method == http.MethodPut:
			var e addresslist.Entry
			if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
				http.Error(w, `{"detail":"Failed to parse json","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			fr.seq++
			id := fmt.Sprintf("*%d", fr.seq)
			e.ID = id
			if e.Disabled == "" {
				e.Disabled = "false"
			}
			e.Dynamic = "false"
			fr.entries[id] = e
			fr.ops = append(fr.ops, "PUT")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

		case r.Method == http.MethodPatch:
			id := path[strings.LastIndex(path, "/")+1:]
			e, ok := fr.entries[id]
			if !ok {
				http.Error(w, `{"detail":"no such item","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			var patch map[string]string
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				http.Error(w, `{"detail":"Failed to parse json","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			if v, ok := patch["disabled"]; ok {
				e.Disabled = v
			}
			fr.entries[id] = e
			fr.ops = append(fr.ops, "PATCH")
			_ = json.NewEncoder(w).Encode(e)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/remove"):
			var body struct {
				Numbers string `json:"numbers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := fr.entries[body.Numbers]; !ok {
				http.Error(w, `{"detail":"no such item","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			delete(fr.entries, body.Numbers)
			fr.ops = append(fr.ops, "DELETE")
			_, _ = w.Write([]byte(`[]`))

		case r.Method == http.MethodGet:
			list := r.URL.Query().Get("list")
			out := make([]addresslist.Entry, 0, len(fr.entries))
			for _, e := range fr.entries {
				if list == "" || e.List == list {
					out = append(out, e)
				}
			}
			_ = json.NewEncoder(w).Encode(out)

		default:
			// Legacy DELETE и прочие методы не должны использоваться.
			http.Error(w, fmt.Sprintf("unexpected %s %s", r.Method, path), http.StatusMethodNotAllowed)
		}
	}
}

// takeOps возвращает записанные мутации и очищает журнал.
func (fr *fakeRouter) takeOps() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	ops := fr.ops
	fr.ops = nil
	return ops
}

// setDisabled меняет disabled напрямую (имитация ручного включения/выключения).
func (fr *fakeRouter) setDisabled(id, val string) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	e := fr.entries[id]
	e.Disabled = val
	fr.entries[id] = e
}

// addresses возвращает отсортированные адреса всех записей.
func (fr *fakeRouter) addresses() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	out := make([]string, 0, len(fr.entries))
	for _, e := range fr.entries {
		out = append(out, e.Address)
	}
	sort.Strings(out)
	return out
}

// find возвращает запись по адресу (или false).
func (fr *fakeRouter) find(address string) (addresslist.Entry, bool) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, e := range fr.entries {
		if e.Address == address {
			return e, true
		}
	}
	return addresslist.Entry{}, false
}

// all возвращает все записи, отсортированные по адресу.
func (fr *fakeRouter) all() []addresslist.Entry {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	out := make([]addresslist.Entry, 0, len(fr.entries))
	for _, e := range fr.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

func (fr *fakeRouter) count() int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return len(fr.entries)
}

// ============================================================================
// Фейковый коллектор (замена static_url без сети)
// ============================================================================

type fakeSource struct {
	mu       sync.Mutex
	prefixes []string
	err      error
}

func (fs *fakeSource) set(prefixes []string, err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.prefixes = prefixes
	fs.err = err
}

func (fs *fakeSource) collect() ([]netip.Prefix, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.err != nil {
		return nil, fs.err
	}
	if len(fs.prefixes) == 0 {
		return nil, fmt.Errorf("source returned zero prefixes")
	}
	out := make([]netip.Prefix, 0, len(fs.prefixes))
	for _, s := range fs.prefixes {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("bad prefix %q: %w", s, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

type fakeCollector struct{ src *fakeSource }

func (c *fakeCollector) Name() string { return "static_url" }

func (c *fakeCollector) Collect(ctx context.Context, service string, opts collectors.Options) (*collectors.Result, error) {
	ps, err := c.src.collect()
	if err != nil {
		return nil, err
	}
	return &collectors.Result{Prefixes: ps, Source: "fake", Method: "static_url"}, nil
}

// ============================================================================
// Сборка syncer'а для e2e
// ============================================================================

func newE2ESyncer(t *testing.T, fr *fakeRouter, src *fakeSource, manageDisabled bool) *Syncer {
	t.Helper()

	srv := httptest.NewServer(fr.handler())
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	dir := t.TempDir()
	md := "false"
	if manageDisabled {
		md = "true"
	}
	body := "timezone: UTC\n" +
		"mikrotik: {host: " + host + ", port: " + portStr + ", username: api, password: p}\n" +
		"firewall: {address_list: TO-VPN, comment_prefix: AUTO, manage_disabled: " + md + "}\n" +
		"safety: {max_delete_ratio: 0.5}\n" +
		"snapshots: {enabled: false}\n" +
		"schedules: {global: 'every 6h'}\n" +
		"services: [demo]\n" +
		"overrides: {demo: {method: static_url, static_url: 'http://source.invalid/list.txt'}}\n"
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.MikroTik.Port = port

	cache, err := storage.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewSyncer(cfg, log, cache, &notifier.NoopNotifier{})
	if err != nil {
		t.Fatal(err)
	}

	// Подменяем реестр коллекторов: метод static_url читает фейковый источник
	// вместо HTTP (SSRF-guard блокирует loopback, сеть в тестах не нужна).
	reg := collectors.NewRegistry(s.deps)
	reg.Register("static_url", func(p *collectors.Params) (collectors.Collector, error) {
		return &fakeCollector{src: src}, nil
	})
	s.regPtr.Store(reg)

	return s
}

// ============================================================================
// Тесты
// ============================================================================

const (
	addrA = "1.1.1.0/24"
	addrB = "8.8.8.0/24"
	addrC = "9.9.9.0/24"
	addrD = "1.0.0.0/24"
)

// IV.1: первый sync создаёт записи с list=TO-VPN, comment=AUTO:demo,
// disabled=false, address из агрегированного набора.
func TestE2ESyncCreatesManagedEntries(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrB, addrA}, nil)
	s := newE2ESyncer(t, fr, src, true)

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !res.Success || res.Added != 2 || res.Removed != 0 || res.Unchanged != 0 || res.Updated != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := fr.addresses(); len(got) != 2 || got[0] != addrA || got[1] != addrB {
		t.Fatalf("router addresses: %v", got)
	}
	for _, want := range []string{addrA, addrB} {
		e, ok := fr.find(want)
		if !ok {
			t.Fatalf("entry %s missing", want)
		}
		if e.List != "TO-VPN" || e.Comment != "AUTO:demo" || e.Disabled != "false" || e.Dynamic == "true" {
			t.Fatalf("managed entry fields wrong: %+v", e)
		}
	}
	if ops := fr.takeOps(); len(ops) != 2 || ops[0] != "PUT" || ops[1] != "PUT" {
		t.Fatalf("expected two PUT creates, got %v", ops)
	}
}

// II.7: повторный sync с теми же данными не выполняет ни одной мутации.
func TestE2ESyncIdempotent(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA, addrB}, nil)
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	fr.takeOps()

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Added != 0 || res.Removed != 0 || res.Updated != 0 || res.Unchanged != 2 {
		t.Fatalf("second sync must be no-op: %+v", res)
	}
	if ops := fr.takeOps(); len(ops) != 0 {
		t.Fatalf("idempotent sync must not mutate router: %v", ops)
	}
}

// IV.4: при изменении источника add выполняется ДО remove (PROMPT II.4.4).
func TestE2ESyncAppliesChangesInOrder(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA, addrB}, nil)
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	fr.takeOps()

	// Источник: addrB заменён на addrD → одновременно add и remove.
	src.set([]string{addrA, addrD}, nil)
	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Added != 1 || res.Removed != 1 || res.Unchanged != 1 {
		t.Fatalf("unexpected diff: %+v", res)
	}
	ops := fr.takeOps()
	if len(ops) != 2 || ops[0] != "PUT" || ops[1] != "DELETE" {
		t.Fatalf("must be PUT before DELETE (PROMPT II.4.4), got %v", ops)
	}
}

// II.1 + новое требование: адрес, занятый записью другого сервиса, НЕ
// перехватывается — без дубликатов и пересечений.
func TestE2EKeepsOtherServiceEntriesNoOverlap(t *testing.T) {
	fr := newFakeRouter(addresslist.Entry{
		ID: "*77", Address: addrC, List: "TO-VPN", Comment: "AUTO:other", Disabled: "false",
	})
	src := &fakeSource{}
	src.set([]string{addrA, addrB}, nil)
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	fr.takeOps()

	// Источник: addrB заменён на addrC — но addrC уже занят AUTO:other.
	src.set([]string{addrA, addrC}, nil)
	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	// addrC исключён пересечением → добавлять нечего; addrB уходит в remove.
	if res.Added != 0 || res.Removed != 1 || res.Unchanged != 1 {
		t.Fatalf("unexpected diff: %+v", res)
	}

	ops := fr.takeOps()
	if len(ops) != 1 || ops[0] != "DELETE" {
		t.Fatalf("expected single DELETE of stale addrB, got %v", ops)
	}

	// Изоляция: запись AUTO:other пережила синхронизацию demo, дубль не создан.
	if e, ok := fr.find(addrC); !ok || e.Comment != "AUTO:other" || e.ID != "*77" {
		t.Fatalf("entry of another service was touched: ok=%v e=%+v", ok, e)
	}
	if _, ok := fr.find(addrB); ok {
		t.Fatalf("stale addrB must be removed")
	}
	got := fr.addresses()
	if len(got) != 2 || got[0] != addrA || got[1] != addrC {
		t.Fatalf("addresses: %v want [%s %s]", got, addrA, addrC)
	}
}

// II.1 + новое требование (без пересечений): manual-записи MikroTik
// вычитываются из желаемого набора — точный дубль не создаётся, чужая подсеть
// вырезается из нашего префикса, полное перекрытие → успех без изменений
// («уже покрыто» чужими записями, изоляция не нарушается).
func TestE2ESkipsOverlappingManualEntries(t *testing.T) {
	fr := newFakeRouter(
		addresslist.Entry{ID: "*81", Address: "5.5.5.0/24", List: "TO-VPN", Comment: "Manual", Disabled: "false"},
		addresslist.Entry{ID: "*82", Address: "9.9.9.0/25", List: "TO-VPN", Comment: "Manual", Disabled: "false"},
	)
	src := &fakeSource{}
	// 5.5.5.0/24 — точное пересечение (исчезает); 9.9.9.0/24 — чужой /25
	// внутри (сплит: остаётся 9.9.9.128/25); 8.8.8.0/24 — без пересечений.
	src.set([]string{"5.5.5.0/24", addrB, "9.9.9.0/24"}, nil)
	s := newE2ESyncer(t, fr, src, true)

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Added != 2 || res.Removed != 0 {
		t.Fatalf("expected 2 adds (8.8.8.0/24 + 9.9.9.128/25): %+v", res)
	}

	byAddr := map[string][]string{}
	for _, e := range fr.all() {
		byAddr[e.Address] = append(byAddr[e.Address], e.Comment)
	}
	if len(byAddr) != 4 {
		t.Fatalf("expected 4 distinct addresses, got %v", byAddr)
	}
	assertComments := func(addr string, want ...string) {
		t.Helper()
		got := byAddr[addr]
		if len(got) != len(want) {
			t.Fatalf("%s comments: %v want %v", addr, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s comments: %v want %v", addr, got, want)
			}
		}
	}
	assertComments("5.5.5.0/24", "Manual")      // дубль не создан
	assertComments("9.9.9.0/25", "Manual")      // чужая подсеть цела
	assertComments("9.9.9.128/25", "AUTO:demo") // вырезанный остаток наш
	assertComments("8.8.8.0/24", "AUTO:demo")   // без пересечений

	// Весь источник перекрыт чужой записью: «уже покрыто» — успех с нулём
	// изменений, чужие и управляемые записи на роутере без изменений.
	fr.takeOps()
	src.set([]string{"5.5.5.0/24"}, nil)
	res2, err2 := s.SyncService(context.Background(), "demo", false, false)
	if err2 != nil || !res2.Success {
		t.Fatalf("full overlap must be a no-op success: err=%v res=%+v", err2, res2)
	}
	if res2.Added != 0 || res2.Removed != 0 {
		t.Fatalf("full overlap must apply no changes: %+v", res2)
	}
	if ops := fr.takeOps(); len(ops) != 0 {
		t.Fatalf("no-op sync must not mutate router: %v", ops)
	}
	if fr.count() != 4 {
		t.Fatalf("existing entries must survive, got %d", fr.count())
	}
}

// II.7 + I: выключенная управляемая запись re-enable-ится при
// firewall.manage_disabled=true (PATCH disabled=false).
func TestE2EReenabledDisabledEntryWhenManaging(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA, addrB}, nil)
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	e, _ := fr.find(addrA)
	fr.setDisabled(e.ID, "true")
	fr.takeOps()

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Updated != 1 || res.Added != 0 || res.Removed != 0 || res.Unchanged != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if ops := fr.takeOps(); len(ops) != 1 || ops[0] != "PATCH" {
		t.Fatalf("expected single PATCH re-enable, got %v", ops)
	}
	got, _ := fr.find(addrA)
	if got.Disabled != "false" {
		t.Fatalf("entry must be re-enabled, got %+v", got)
	}
}

// firewall.manage_disabled=false: выключенные записи не трогаются и не
// считаются unchanged (отчёт honest, роутер без изменений).
func TestE2EKeepsDisabledEntryWhenNotManaging(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA, addrB}, nil)
	s := newE2ESyncer(t, fr, src, false)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	e, _ := fr.find(addrA)
	fr.setDisabled(e.ID, "true")
	fr.takeOps()

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Updated != 0 || res.Added != 0 || res.Removed != 0 || res.Unchanged != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if ops := fr.takeOps(); len(ops) != 0 {
		t.Fatalf("manage_disabled=false must not mutate router: %v", ops)
	}
	if got, _ := fr.find(addrA); got.Disabled != "true" {
		t.Fatalf("manual disable must be preserved: %+v", got)
	}
}

// II.4: ошибка источника — fail-closed, роутер без изменений.
func TestE2EFailClosedOnCollectError(t *testing.T) {
	fr := newFakeRouter(addresslist.Entry{
		ID: "*77", Address: addrB, List: "TO-VPN", Comment: "AUTO:demo", Disabled: "false",
	})
	src := &fakeSource{}
	src.set(nil, fmt.Errorf("source down"))

	s := newE2ESyncer(t, fr, src, true)
	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err == nil || res.Success {
		t.Fatalf("collect error must fail sync: err=%v res=%+v", err, res)
	}
	if ops := fr.takeOps(); len(ops) != 0 {
		t.Fatalf("fail-closed: router must not be touched: %v", ops)
	}
	if fr.count() != 1 {
		t.Fatalf("existing entries must survive, got %d", fr.count())
	}
}

// II.6: массовое удаление блокируется, --force обходит.
func TestE2ESafetyBlocksMassDeleteAndForceOverrides(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA, addrB, addrC}, nil)
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if fr.count() != 3 {
		t.Fatalf("expected 3 entries, got %d", fr.count())
	}
	fr.takeOps()

	// 2 удаления из 3 = 0.67 > 0.5 → safety-блок.
	src.set([]string{addrA}, nil)
	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err == nil || res.Success {
		t.Fatalf("mass delete must be blocked: err=%v res=%+v", err, res)
	}
	if ops := fr.takeOps(); len(ops) != 0 {
		t.Fatalf("blocked sync must not mutate router: %v", ops)
	}
	if fr.count() != 3 {
		t.Fatalf("entries must survive safety block, got %d", fr.count())
	}

	// --force применяет изменения.
	res, err = s.SyncService(context.Background(), "demo", false, true)
	if err != nil {
		t.Fatalf("forced sync: %v", err)
	}
	if !res.Success || res.Removed != 2 || res.Added != 0 || fr.count() != 1 {
		t.Fatalf("forced sync wrong: res=%+v count=%d", res, fr.count())
	}
}

// Dry-run не выполняет ни одной мутации RouterOS (PROMPT IV.3).
func TestE2EDryRunPerformsNoMutations(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA, addrB}, nil)
	s := newE2ESyncer(t, fr, src, true)

	res, err := s.SyncService(context.Background(), "demo", true, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !res.Success || !res.DryRun || res.Added != 2 {
		t.Fatalf("unexpected dry-run result: %+v", res)
	}
	if ops := fr.takeOps(); len(ops) != 0 {
		t.Fatalf("dry-run must not mutate router: %v", ops)
	}
	if fr.count() != 0 {
		t.Fatalf("dry-run must not create entries, got %d", fr.count())
	}
}

// DiffService (используется CLI/web dry-run) включает update-кандидатов.
func TestE2EDiffServiceReportsUpdate(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA}, nil)
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	e, _ := fr.find(addrA)
	fr.setDisabled(e.ID, "true")

	d, err := s.DiffService(context.Background(), "demo")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Update) != 1 || d.Update[0] != addrA {
		t.Fatalf("diff must report update candidate: %+v", d)
	}
	if len(d.Unchanged) != 0 {
		t.Fatalf("disabled entry must not be unchanged: %+v", d)
	}
	if d.List != "TO-VPN" || d.Comment != "AUTO:demo" {
		t.Fatalf("diff context: %+v", d)
	}
}

// Прогресс синхронизации: карта заполняется, очищается через defer
// при любом исходе (успех и ошибка) и отдаётся копией.
func TestProgressClearedAfterSuccessAndError(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA}, nil)
	s := newE2ESyncer(t, fr, src, true)

	s.setProgress("demo", "проверка", 50, false)
	got := s.ProgressSnapshot()
	if len(got) != 1 || got["demo"].Percent != 50 || got["demo"].Phase != "проверка" {
		t.Fatalf("progress snapshot: %+v", got)
	}
	s.clearProgress("demo")
	if got := s.ProgressSnapshot(); len(got) != 0 {
		t.Fatalf("progress not cleared: %+v", got)
	}

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := s.ProgressSnapshot(); len(got) != 0 {
		t.Fatalf("progress left after success: %+v", got)
	}

	// Ошибка сбора (fail-closed): defer обязан убрать прогресс.
	src.set(nil, fmt.Errorf("source down"))
	if _, err := s.SyncService(context.Background(), "demo", false, false); err == nil {
		t.Fatal("expected collect error")
	}
	if got := s.ProgressSnapshot(); len(got) != 0 {
		t.Fatalf("progress left after error: %+v", got)
	}
}
