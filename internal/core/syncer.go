package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/aggregator"
	"github.com/Kfaraon/mikrotik-route-sync/internal/audit"
	"github.com/Kfaraon/mikrotik-route-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/history"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	"github.com/Kfaraon/mikrotik-route-sync/internal/validator"
	"github.com/Kfaraon/mikrotik-route-sync/internal/version"
)

type Result struct {
	Service   string    `json:"service"`
	Changed   bool      `json:"changed"`
	DryRun    bool      `json:"dry_run"`
	Added     int       `json:"added"`
	Removed   int       `json:"removed"`
	Total     int       `json:"total"`
	Desired   int       `json:"desired"`
	Error     string    `json:"error,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type Syncer struct {
	cfg    *config.Config
	log    *slog.Logger
	cache  *storage.Cache
	notify notifier.Notifier
	mt     *mikrotik.Client
	http   *collectors.HTTP
	locks  map[string]*sync.Mutex
	mu     sync.Mutex
}

// NewSyncer создаёт новый синхронизатор.
// ИСПРАВЛЕНО: создаётся http.Client с таймаутом вместо передачи
// time.Duration напрямую в NewHTTP (который ожидает *http.Client).
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

func (s *Syncer) SyncService(ctx context.Context, name string, dry, confirm bool) (Result, error) {
	log := s.log.With("service", name, "dry", dry)
	start := time.Now()
	m := s.lockFor(name)
	m.Lock()
	defer m.Unlock()
	res := Result{Service: name, DryRun: dry, Timestamp: start}
	if dry {
		log.Info("starting dry-run sync")
	} else {
		log.Info("starting sync")
	}
	if !config.ValidateServiceName(name) {
		res.Error = "invalid service name"
		log.Error("invalid service name", "name", name)
		return res, fmt.Errorf("invalid service name")
	}
	desired, err := s.collect(ctx, name)
	if err != nil {
		res.Error = err.Error()
		log.Error("collection failed", "err", err)
		return res, fmt.Errorf("collect: %w", err)
	}
	res.Desired = len(desired)
	existing, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		res.Error = err.Error()
		log.Error("failed to list routes", "err", err)
		return res, fmt.Errorf("list routes: %w", err)
	}
	diff, err := ComputeDiff(desired, existing)
	if err != nil {
		res.Error = err.Error()
		log.Error("diff computation failed", "err", err)
		return res, fmt.Errorf("diff: %w", err)
	}
	res.Added = len(diff.Add)
	res.Removed = len(diff.Delete)
	res.Total = len(existing) + len(diff.Add) - len(diff.Delete)
	res.Changed = len(diff.Add) > 0 || len(diff.Delete) > 0
	if dry {
		log.Info("dry-run completed", "added", res.Added, "removed", res.Removed, "total", res.Total, "changed", res.Changed)
		return res, nil
	}
	limit := s.cfg.Safety.RequireConfirmationOver
	if limit > 0 && res.Added > limit {
		if !confirm {
			res.Error = fmt.Sprintf("change affects %d routes, exceeds confirmation threshold %d", res.Added, limit)
			log.Warn("confirmation required", "added", res.Added, "threshold", limit)
			return res, fmt.Errorf("%s", res.Error)
		}
	}
	if len(existing) > 0 {
		ratio := float64(len(diff.Delete)) / float64(len(existing))
		if ratio > s.cfg.Safety.MaxDeleteRatio {
			res.Error = fmt.Sprintf("delete ratio %.2f exceeds maximum %.2f", ratio, s.cfg.Safety.MaxDeleteRatio)
			log.Error("delete ratio exceeded", "ratio", ratio, "max", s.cfg.Safety.MaxDeleteRatio)
			return res, fmt.Errorf("%s", res.Error)
		}
	}
	tx := mikrotik.NewTransaction(s.mt, name, s.cfg.MikroTik.CommentPrefix)
	tx.Begin(ctx)
	for _, k := range diff.Add {
		r := mikrotik.Route{DstAddress: k.CIDR, Gateway: k.Gateway, RoutingTable: k.Table, Distance: strconv.Itoa(k.Distance)}
		if err := s.mt.AddRoute(ctx, r); err != nil {
			tx.Rollback(ctx)
			res.Error = err.Error()
			log.Error("add route failed, rolled back", "route", k.CIDR, "err", err)
			return res, fmt.Errorf("add route: %w", err)
		}
	}
	for _, k := range diff.Delete {
		r := mikrotik.Route{DstAddress: k.CIDR, Gateway: k.Gateway, RoutingTable: k.Table, Distance: strconv.Itoa(k.Distance)}
		if err := s.mt.DeleteRoute(ctx, r); err != nil {
			tx.Rollback(ctx)
			res.Error = err.Error()
			log.Error("delete route failed, rolled back", "route", k.CIDR, "err", err)
			return res, fmt.Errorf("delete route: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		res.Error = err.Error()
		log.Error("commit failed", "err", err)
		return res, fmt.Errorf("commit: %w", err)
	}
	elapsed := time.Since(start)
	logging.LogSyncResult(log, res, elapsed)
	s.notify.Notify(ctx, res)
	s.createSnapshot(ctx, name, desired)
	return res, nil
}

func (s *Syncer) SyncMany(ctx context.Context, services []string, dry bool) error {
	log := s.log.With("services", len(services), "dry", dry)
	log.Info("starting multi-service sync")
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.cfg.Scheduler.MaxConcurrent)
	results := make(chan Result, len(services))
	var first error
	var once sync.Once
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
			if e != nil {
				once.Do(func() { first = e })
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	var summary []Result
	for r := range results {
		summary = append(summary, r)
	}
	logging.LogSyncSummary(log, summary)
	return first
}

func (s *Syncer) collect(ctx context.Context, name string) ([]RouteKey, error) {
	ov := s.cfg.Overrides[name]
	class := classifier.Classify(name, ov)
	if class == "" {
		return nil, fmt.Errorf("unable to classify service %s", name)
	}
	var raw []string
	var err error
	switch class {
	case classifier.CDN:
		raw, err = collectors.NewCDN(s.http).Collect(ctx, name, ov)
	case classifier.ASN:
		raw, err = collectors.NewASN(s.http).Collect(ctx, name, ov)
	case classifier.Whois:
		raw, err = collectors.NewWhois(s.http).Collect(ctx, name, ov)
	case classifier.Static:
		raw, err = collectors.NewStatic(s.http).Collect(ctx, name, ov)
	case classifier.Dynamic:
		raw, err = collectors.NewDynamic(s.http, resolver.New(s.cfg.External)).Collect(ctx, name, ov)
	default:
		return nil, fmt.Errorf("unknown classifier %s", class)
	}
	if err != nil {
		return nil, fmt.Errorf("collector %s: %w", class, err)
	}
	v := validator.Validator{Safety: s.cfg.Safety}
	prefixes, err := v.Validate(raw, ov)
	if err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}
	agg, err := aggregator.Aggregate(prefixes)
	if err != nil {
		return nil, fmt.Errorf("aggregation: %w", err)
	}
	desired := make([]RouteKey, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, RouteKey{CIDR: p.String(), Gateway: s.cfg.MikroTik.Gateway, Table: s.cfg.MikroTik.RoutingTable, Distance: s.cfg.MikroTik.Distance})
	}
	sort.Slice(desired, func(i, j int) bool {
		if desired[i].CIDR == desired[j].CIDR {
			return desired[i].Distance < desired[j].Distance
		}
		return desired[i].CIDR < desired[j].CIDR
	})
	return desired, nil
}

func (s *Syncer) AddService(ctx context.Context, name string, prefixes []string, override config.ServiceOverride) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service name: %s", name)
	}
	if err := validator.SanitizeComment(name); err != nil {
		return err
	}
	for _, p := range s.cfg.Services {
		if p == name {
			return fmt.Errorf("service %s already exists", name)
		}
	}
	if len(prefixes) > 0 {
		override.StaticURL = ""
	}
	s.cfg.Services = append(s.cfg.Services, name)
	if s.cfg.Overrides == nil {
		s.cfg.Overrides = make(map[string]config.ServiceOverride)
	}
	s.cfg.Overrides[name] = override
	return s.cfg.Save()
}

func (s *Syncer) RemoveService(ctx context.Context, name string, purgeRoutes bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service name: %s", name)
	}
	idx := -1
	for i, n := range s.cfg.Services {
		if n == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("service %s not found", name)
	}
	if purgeRoutes {
		routes, err := s.mt.ListServiceRoutes(ctx, name)
		if err != nil {
			return fmt.Errorf("list routes: %w", err)
		}
		for _, r := range routes {
			if err := s.mt.DeleteRoute(ctx, r); err != nil {
				s.log.Warn("failed to delete route on removal", "service", name, "route", r.DstAddress, "err", err)
			}
		}
	}
	s.cfg.Services = append(s.cfg.Services[:idx], s.cfg.Services[idx+1:]...)
	delete(s.cfg.Overrides, name)
	return s.cfg.Save()
}

func (s *Syncer) Backup(ctx context.Context, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	routes, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}
	key := "backup:" + name
	return s.cache.SaveBackup(key, routes)
}

func (s *Syncer) Restore(ctx context.Context, name string, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	routes, err := s.cache.LoadBackup("backup:" + name)
	if err != nil {
		return fmt.Errorf("load backup: %w", err)
	}
	for _, r := range routes {
		if err := s.mt.AddRoute(ctx, r); err != nil {
			return fmt.Errorf("restore route %s: %w", r.DstAddress, err)
		}
	}
	return nil
}

func (s *Syncer) ListServices(ctx context.Context) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.cfg.Services))
	copy(out, s.cfg.Services)
	sort.Strings(out)
	return out
}

func (s *Syncer) SyncOne(ctx context.Context, name string) (Result, error) {
	return s.SyncService(ctx, name, false, false)
}

func (s *Syncer) SetServiceSchedule(name string, schedule string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service name: %s", name)
	}
	if s.cfg.Schedules.Services == nil {
		s.cfg.Schedules.Services = make(map[string]config.ServiceSchedule)
	}
	s.cfg.Schedules.Services[name] = config.ServiceSchedule{Schedule: schedule}
	return s.cfg.Save()
}

func (s *Syncer) CreateSnapshot(ctx context.Context, service string, prefixes []netip.Prefix) (string, error) {
	if !s.cfg.Snapshots.Enabled {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	strs := make([]string, len(prefixes))
	for i, p := range prefixes {
		strs[i] = p.String()
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	if err := s.cache.SaveSnapshot(service, id, strs); err != nil {
		return "", fmt.Errorf("save snapshot: %w", err)
	}
	s.CleanupSnapshots(service)
	return id, nil
}

func (s *Syncer) ListSnapshots(service string) []string {
	return s.cache.ListSnapshots(service)
}

func (s *Syncer) GetSnapshot(service, id string) ([]string, error) {
	return s.cache.LoadSnapshot(service, id)
}

func (s *Syncer) DeleteSnapshot(service, id string) error {
	return s.cache.DeleteSnapshot(service, id)
}

func (s *Syncer) CleanupSnapshots(service string) {
	ids := s.cache.ListSnapshots(service)
	maxCount := s.cfg.Snapshots.MaxCount
	if len(ids) > maxCount {
		sort.Strings(ids)
		for _, id := range ids[:len(ids)-maxCount] {
			_ = s.cache.DeleteSnapshot(service, id)
		}
	}
}

func (s *Syncer) createSnapshot(ctx context.Context, name string, desired []RouteKey) {
	if !s.cfg.Snapshots.Enabled {
		return
	}
	prefixes := make([]netip.Prefix, 0, len(desired))
	for _, k := range desired {
		p, err := netip.ParsePrefix(k.CIDR)
		if err != nil {
			s.log.Warn("skipping invalid prefix for snapshot", "prefix", k.CIDR)
			continue
		}
		prefixes = append(prefixes, p)
	}
	if _, err := s.CreateSnapshot(ctx, name, prefixes); err != nil {
		s.log.Warn("failed to create snapshot", "service", name, "err", err)
	}
}

// ======================== ДОБАВЛЕННЫЕ МЕТОДЫ (Этап 1) ========================

// Snapshot возвращает карту количества маршрутов по каждому сервису.
// Используется в Web UI для отображения общей статистики.
// ДОБАВЛЕНО: метод вызывался в server.go, но отсутствовал.
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
// Возвращает nil, если соединение установлено успешно.
// ДОБАВЛЕНО: метод вызывался в server.go (с опечаткой), но отсутствовал.
func (s *Syncer) PingMikroTik(ctx context.Context) error {
	return s.mt.Ping(ctx)
}

// SyncOneResult синхронизирует один сервис и возвращает Result.
// Используется в REST API для dry-run операций.
// ДОБАВЛЕНО: метод вызывался в server.go, но отсутствовал.
func (s *Syncer) SyncOneResult(ctx context.Context, name string, dry bool) (Result, error) {
	return s.SyncService(ctx, name, dry, false)
}

// RouteCount возвращает общее количество синхронизированных маршрутов.
// ДОБАВЛЕНО: вспомогательный метод для Web UI.
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
