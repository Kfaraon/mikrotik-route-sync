package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/aggregator"
	"github.com/Kfaraon/mikrotik-route-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/history"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	"github.com/Kfaraon/mikrotik-route-sync/internal/validator"
)

type Syncer struct {
	cfg    *config.Config
	log    *slog.Logger
	cache  *storage.Cache
	notify notifier.Notifier
	mt     *mikrotik.Client
	http   *collectors.HTTP
	mu     sync.Mutex
	locks  map[string]*sync.Mutex
}
type Result struct {
	Service, Method, Status                                    string
	Collected, Aggregated, Existing, Added, Removed, Unchanged int
	Duration                                                   time.Duration
	Error                                                      string
}

func NewSyncer(cfg *config.Config, log *slog.Logger, cache *storage.Cache, n notifier.Notifier) *Syncer {
	to, _ := time.ParseDuration(cfg.External.HTTPTimeout)
	if to == 0 {
		to = 15 * time.Second
	}
	return &Syncer{cfg: cfg, log: log, cache: cache, notify: n, mt: mikrotik.New(cfg.MikroTik), http: collectors.NewHTTP(to, cfg.External.MaxResponseMB), locks: map[string]*sync.Mutex{}}
}
func (s *Syncer) lockFor(name string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.locks[name]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[name] = m
	}
	return m
}
func (s *Syncer) SyncMany(ctx context.Context, services []string, dry bool) error {
	sem := make(chan struct{}, s.cfg.Scheduler.MaxConcurrent)
	var wg sync.WaitGroup
	var first error
	var em sync.Mutex
	for _, name := range services {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			r, e := s.SyncService(ctx, name, dry, false)
			s.log.Info("sync result", "service", name, "status", r.Status, "added", r.Added, "removed", r.Removed, "unchanged", r.Unchanged, "duration_ms", r.Duration.Milliseconds())
			if e != nil {
				em.Lock()
				if first == nil {
					first = e
				}
				em.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
}
func (s *Syncer) collect(ctx context.Context, name string) ([]string, string, error) {
	ov := s.cfg.Overrides[name]
	cl := classifier.Classify(name, ov.Method)
	domains := ov.Domains
	if len(domains) == 0 {
		domains = cl.Domains
	}
	var all []string
	methods := []string{}
	for _, m := range cl.Methods {
		methods = append(methods, m)
		switch m {
		case "cdn", "static_url":
			urls := cl.StaticURLs
			if ov.StaticURL != "" {
				urls = []string{ov.StaticURL}
			}
			for _, u := range urls {
				x, e := s.http.FetchLines(ctx, u)
				if e != nil && strings.Contains(u, "fastly") {
					x, e = s.http.FetchFastly(ctx)
				}
				if e != nil {
					return nil, strings.Join(methods, "+"), e
				}
				all = append(all, x...)
			}
		case "dynamic":
			x, e := collectors.DNS(ctx, domains, s.cfg.External.Resolver)
			if e != nil {
				return nil, strings.Join(methods, "+"), e
			}
			all = append(all, x...)
		case "asn", "whois":
			asn := cl.ASN
			if asn == "" && strings.HasPrefix(strings.ToUpper(name), "AS") {
				asn = strings.ToUpper(name)
			}
			if asn == "" {
				return nil, strings.Join(methods, "+"), fmt.Errorf("ASN resolution for %s requires explicit ASN/known service in this build", name)
			}
			x, e := collectors.BGPViewASN(ctx, s.http, asn)
			if e != nil {
				return nil, strings.Join(methods, "+"), e
			}
			limit := ov.MaxASNPrefixes
			if limit == 0 {
				limit = s.cfg.Safety.MaxASNPrefixes
			}
			if limit > 0 && len(x) > limit {
				return nil, strings.Join(methods, "+"), fmt.Errorf("ASN prefix count %d exceeds safety limit %d", len(x), limit)
			}
			all = append(all, x...)
		default:
			return nil, strings.Join(methods, "+"), fmt.Errorf("unsupported collector %s", m)
		}
	}
	return all, strings.Join(methods, "+"), nil
}
func (s *Syncer) SyncService(ctx context.Context, name string, dry, force bool) (res Result, err error) {
	start := time.Now()
	res.Service = name
	defer func() {
		res.Duration = time.Since(start)
		if err != nil {
			res.Status = "error"
			res.Error = err.Error()
		} else if res.Status == "" {
			res.Status = "ok"
		}
		rec := history.Record{Time: time.Now(), Service: name, Method: res.Method, Status: res.Status, Error: res.Error, Collected: res.Collected, Aggregated: res.Aggregated, Added: res.Added, Removed: res.Removed, Unchanged: res.Unchanged, DurationMS: res.Duration.Milliseconds()}
		_ = s.cache.Put("history", fmt.Sprintf("%020d:%s", time.Now().UnixNano(), name), rec)
	}()
	if !config.ValidateServiceName(name) {
		return res, fmt.Errorf("invalid service name")
	}
	m := s.lockFor(name)
	m.Lock()
	defer m.Unlock()
	if err = validator.SanitizeComment(name); err != nil {
		return res, err
	}
	raw, method, e := s.collect(ctx, name)
	if e != nil {
		return res, e
	}
	res.Method = method
	res.Collected = len(raw)
	v := validator.Validator{Safety: s.cfg.Safety}
	valid, e := v.Validate(raw, s.cfg.Overrides[name])
	if e != nil {
		return res, e
	}
	agg, e := aggregator.Aggregate(valid)
	if e != nil {
		s.log.Error("aggregation failed; using validated list", "service", name, "error", e)
		agg = valid
	}
	res.Aggregated = len(agg)
	if len(agg) == 0 {
		return res, fmt.Errorf("fail-closed: desired route set is empty")
	}
	existing, e := s.mt.ListServiceRoutes(ctx, name)
	if e != nil {
		return res, e
	}
	res.Existing = len(existing)
	desired := make([]RouteKey, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, RouteKey{CIDR: p.String(), Gateway: s.cfg.MikroTik.Gateway, Table: s.cfg.MikroTik.RoutingTable, Distance: s.cfg.MikroTik.Distance})
	}
	d, e := ComputeDiff(desired, existing)
	if e != nil {
		return res, e
	}
	res.Added, res.Removed, res.Unchanged = len(d.Add), len(d.Remove), d.Unchanged
	if len(existing) > 0 && len(desired) == 0 {
		return res, fmt.Errorf("fail-closed: existing routes present but desired is empty")
	}
	ratio := 0.0
	if len(existing) > 0 {
		ratio = float64(len(d.Remove)) / float64(len(existing))
	}
	if !force && ratio > s.cfg.Safety.MaxDeleteRatio {
		return res, fmt.Errorf("safe-diff blocked removal of %d/%d routes (%.1f%% > %.1f%%); use --force after dry-run", len(d.Remove), len(existing), 100*ratio, 100*s.cfg.Safety.MaxDeleteRatio)
	}
	if dry {
		res.Status = "dry-run"
		return res, nil
	}
	snapshotKey := fmt.Sprintf("%s:%020d", name, time.Now().UnixNano())
	_ = s.cache.Put("snapshots", snapshotKey, existing)
	comment := s.cfg.MikroTik.CommentPrefix + ":" + name
	added := []mikrotik.Route{}
	removed := []mikrotik.Route{}
	rollback := func(cause error) error {
		var rerr error
		for _, r := range added {
			if r.ID != "" {
				if e := s.mt.DeleteRoute(ctx, r.ID); e != nil {
					rerr = e
				}
			}
		}
		for _, r := range removed {
			r.ID = ""
			if _, e := s.mt.AddRoute(ctx, r); e != nil {
				rerr = e
			}
		}
		if rerr != nil {
			return fmt.Errorf("apply failed: %v; rollback degraded: %v", cause, rerr)
		}
		return fmt.Errorf("apply failed and rolled back: %w", cause)
	}
	for _, k := range d.Add {
		r, e := s.mt.AddRoute(ctx, mikrotik.Route{DstAddress: k.CIDR, Gateway: k.Gateway, RoutingTable: k.Table, Distance: strconv.Itoa(k.Distance), Comment: comment})
		if e != nil {
			return res, rollback(e)
		}
		added = append(added, r)
	}
	for _, r := range d.Remove {
		if e := s.mt.DeleteRoute(ctx, r.ID); e != nil {
			return res, rollback(e)
		}
		removed = append(removed, r)
	}
	_ = s.notify.Send(ctx, fmt.Sprintf("✅ %s: +%d -%d =%d", name, res.Added, res.Removed, res.Unchanged))
	return res, nil
}
func (s *Syncer) AddService(ctx context.Context, name string) error {
	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service")
	}
	for _, x := range s.cfg.Services {
		if x == name {
			return fmt.Errorf("service already exists")
		}
	}
	if _, e := s.SyncService(ctx, name, true, false); e != nil {
		return fmt.Errorf("preflight: %w", e)
	}
	s.cfg.Services = append(s.cfg.Services, name)
	return nil
}
func PrefixesToStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

func (s *Syncer) RemoveService(ctx context.Context, name string, force bool) error {
	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service")
	}
	routes, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		return err
	}
	if len(routes) > s.cfg.Safety.RequireConfirmationOver && !force {
		return fmt.Errorf("removal of %d routes requires --force", len(routes))
	}
	snap := fmt.Sprintf("%s:%020d", name, time.Now().UnixNano())
	_ = s.cache.Put("snapshots", snap, routes)
	deleted := []mikrotik.Route{}
	for _, r := range routes {
		if err := s.mt.DeleteRoute(ctx, r.ID); err != nil {
			for _, old := range deleted {
				old.ID = ""
				_, _ = s.mt.AddRoute(ctx, old)
			}
			return fmt.Errorf("remove service failed and rollback attempted: %w", err)
		}
		deleted = append(deleted, r)
	}
	return nil
}

func (s *Syncer) Backup(ctx context.Context, name string) ([]mikrotik.Route, error) {
	return s.mt.ListServiceRoutes(ctx, name)
}
func (s *Syncer) Restore(ctx context.Context, name string, routes []mikrotik.Route) error {
	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service")
	}
	current, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		return err
	}
	for _, r := range current {
		if err = s.mt.DeleteRoute(ctx, r.ID); err != nil {
			return err
		}
	}
	for _, r := range routes {
		r.ID = ""
		r.Comment = s.cfg.MikroTik.CommentPrefix + ":" + name
		if _, err = s.mt.AddRoute(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// ListServices возвращает список всех сервисов из конфигурации
func (s *Syncer) ListServices() []string {
	return s.cfg.Services
}

// SyncOne синхронизирует один сервис (обёртка для SyncService)
func (s *Syncer) SyncOne(ctx context.Context, name string) error {
	_, err := s.SyncService(ctx, name, false, false)
	return err
}

// SetServiceSchedule устанавливает расписание для сервиса
// TODO: Реализовать сохранение в конфиг и hot-reload
func (s *Syncer) SetServiceSchedule(name string, schedule string) error {
	// Пока возвращаем ошибку "не реализовано"
	return fmt.Errorf("SetServiceSchedule not implemented yet")
}

// === Методы для работы со snapshots ===

// CreateSnapshot создаёт snapshot текущих маршрутов сервиса
func (s *Syncer) CreateSnapshot(ctx context.Context, service string) (string, error) {
	routes, err := s.mt.ListServiceRoutes(ctx, service)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}

	snapshotID, err := s.cache.CreateSnapshot(service, routes)
	if err != nil {
		return "", fmt.Errorf("create snapshot: %w", err)
	}

	s.log.Info("snapshot created", "service", service, "snapshot_id", snapshotID, "routes", len(routes))
	return snapshotID, nil
}

// ListSnapshots возвращает список snapshots для сервиса
func (s *Syncer) ListSnapshots(ctx context.Context, service string) ([]storage.SnapshotInfo, error) {
	return s.cache.ListSnapshots(service)
}

// GetSnapshot получает snapshot по ID
func (s *Syncer) GetSnapshot(ctx context.Context, service, snapshotID string, v any) error {
	return s.cache.GetSnapshot(service, snapshotID, v)
}

// DeleteSnapshot удаляет snapshot
func (s *Syncer) DeleteSnapshot(ctx context.Context, service, snapshotID string) error {
	return s.cache.DeleteSnapshot(service, snapshotID)
}

// CleanupSnapshots удаляет snapshots старше TTL
func (s *Syncer) CleanupSnapshots(ctx context.Context, service string, ttl time.Duration) (int, error) {
	return s.cache.CleanupExpiredSnapshots(ttl)
}

// ===== В начало файла добавить в блок imports: =====
// 	"net/http"

// ===== Заменить функцию NewSyncer: =====
/*
func NewSyncer(cfg *config.Config, log *slog.Logger, cache *storage.Cache, n notifier.Notifier) *Syncer {
	to := cfg.External.HTTPTimeout
	if to == 0 {
		to = 15 * time.Second
	}
	httpClient := &http.Client{Timeout: to}
	return &Syncer{
		cfg:    cfg,
		log:    log,
		cache:  cache,
		notify: n,
		mt:     mikrotik.New(cfg.MikroTik),
		http:   collectors.NewHTTP(httpClient, cfg.External.MaxResponseMB),
		locks:  map[string]*sync.Mutex{},
	}
}
*/

// ===== Добавить в конец файла: =====

// Snapshot возвращает карту количества маршрутов по каждому сервису.
func (s *Syncer) Snapshot(ctx context.Context) (map[string]int, error) {
	result := make(map[string]int, len(s.cfg.Services))
	for _, name := range s.cfg.Services {
		routes, err := s.mt.ListServiceRoutes(ctx, name)
		if err != nil {
			s.log.Warn("snapshot: failed to list routes", "service", name, "err", err)
			result[name] = 0
			continue
		}
		result[name] = len(routes)
	}
	return result, nil
}

// PingMikroTik проверяет доступность MikroTik RouterOS.
func (s *Syncer) PingMikroTik(ctx context.Context) error {
	return s.mt.Ping(ctx)
}

// SyncOneResult синхронизирует один сервис и возвращает Result.
func (s *Syncer) SyncOneResult(ctx context.Context, name string, dry bool) (Result, error) {
	return s.SyncService(ctx, name, dry, false)
}

// RouteCount возвращает общее количество синхронизированных маршрутов.
func (s *Syncer) RouteCount(ctx context.Context) (int, error) {
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, count := range snap {
		total += count
	}
	return total, nil
}
