// Package core — оркестратор синхронизации (сервисы, история, снапшоты, кэш).
package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
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

// ============================================================================
// Типы данных
// ============================================================================

type Result struct {
	Service    string    `json:"service"`
	List       string    `json:"list"`
	Comment    string    `json:"comment"`
	Success    bool      `json:"success"`
	Added      int       `json:"added"`
	Removed    int       `json:"removed"`
	Unchanged  int       `json:"unchanged"`
	Error      string    `json:"error,omitempty"`
	DryRun     bool      `json:"dry_run"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Duration   string    `json:"duration"`
	Version    string    `json:"version,omitempty"`
}

type ServiceInfo struct {
	Name          string                 `json:"name"`
	List          string                 `json:"list"`
	Comment       string                 `json:"comment"`
	Schedule      string                 `json:"schedule"`
	Override      config.ServiceOverride `json:"override"`
	EntryCount    int                    `json:"entry_count"`
	SnapshotCount int                    `json:"snapshot_count"`
}

type SnapshotInfo struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	CreatedAt time.Time `json:"created_at"`
	Count     int       `json:"count"`
}

type BackupFile struct {
	Service   string              `json:"service"`
	List      string              `json:"list"`
	Comment   string              `json:"comment"`
	CreatedAt time.Time           `json:"created_at"`
	Entries   []addresslist.Entry `json:"entries"`
}

type OrphanEntry struct {
	Addresslist addresslist.Entry `json:"entry"`
	Service     string            `json:"comment_service"`
}

type Syncer struct {
	cfg     *config.Config
	cache   *storage.Cache
	log     *slog.Logger
	audit   *audit.Logger
	history *history.History

	mtPtr  atomic.Pointer[mikrotik.Client]
	resPtr atomic.Pointer[resolver.Resolver]
	deps   *collectors.Deps
	regPtr atomic.Pointer[collectors.Registry]

	notifyMu sync.RWMutex
	notify   notifier.Notifier

	serviceMu sync.Mutex
	startTime time.Time
	lastSync  time.Time

	busyMu sync.Mutex
	busy   map[string]bool

	degradedMu sync.Mutex
	degraded   map[string]time.Time

	onEventMu sync.Mutex
	onEvent   func(event string, data any)

	reloadMu sync.Mutex
	reload   func() error

	// telegramRestartMu защищает функцию перезапуска Telegram-бота
	telegramRestartMu   sync.Mutex
	telegramRestartFunc func() error
}

// ============================================================================
// Конструктор и горячее применение
// ============================================================================

func NewSyncer(cfg *config.Config, log *slog.Logger, cache *storage.Cache, notify notifier.Notifier) (*Syncer, error) {
	mt, err := mikrotik.NewClient(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("create mikrotik client: %w", err)
	}

	auditLog := audit.NewLogger(log)
	hist := history.NewHistory(1000)
	hist.AttachStore(cache)

	s := &Syncer{
		cfg:       cfg,
		cache:     cache,
		log:       log,
		audit:     auditLog,
		history:   hist,
		notify:    notify,
		busy:      map[string]bool{},
		degraded:  map[string]time.Time{},
		startTime: time.Now(),
	}
	s.mtPtr.Store(mt)
	s.buildDepsLocked()
	return s, nil
}

func (s *Syncer) buildDepsLocked() {
	httpClient := collectors.NewHTTP(s.cfg.External.HTTPTimeout.Duration(), s.cfg.Retry, s.cfg.External.MaxResponseMB, s.cfg.External.BGPToolsContact)
	res := resolver.NewResolver(s.cfg.External.Resolver, httpClient, s.cache, s.cfg.Scheduler.CacheTTL.Duration())

	deps := &collectors.Deps{
		HTTP:     httpClient,
		Resolver: res,
		Cache:    s.cache,
		TTL:      s.cfg.Scheduler.CacheTTL.Duration(),
	}
	reg := collectors.NewRegistry(deps)

	s.deps = deps
	s.resPtr.Store(res)
	s.regPtr.Store(reg)
}

func (s *Syncer) ApplyConfig() error {
	mt, err := mikrotik.NewClient(s.cfg, s.log)
	if err != nil {
		return fmt.Errorf("rebuild mikrotik client: %w", err)
	}
	s.mtPtr.Store(mt)
	s.buildDepsLocked()

	s.log.Info("configuration applied at runtime",
		"mikrotik_host", s.cfg.MikroTik.Host,
		"address_list", s.cfg.Firewall.AddressList,
		"external_timeout", s.cfg.External.HTTPTimeout.Duration().String(),
		"cache_ttl", s.cfg.Scheduler.CacheTTL.Duration().String())
	return nil
}

// ============================================================================
// Метрики состояния
// ============================================================================

func (s *Syncer) StartTime() time.Time { return s.startTime }

func (s *Syncer) LastSync() time.Time {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	return s.lastSync
}

func (s *Syncer) Version() string           { return version.Version }
func (s *Syncer) Config() *config.Config    { return s.cfg }
func (s *Syncer) Cache() *storage.Cache     { return s.cache }
func (s *Syncer) History() *history.History { return s.history }

func (s *Syncer) mt() *mikrotik.Client           { return s.mtPtr.Load() }
func (s *Syncer) resolver() *resolver.Resolver   { return s.resPtr.Load() }
func (s *Syncer) registry() *collectors.Registry { return s.regPtr.Load() }

func (s *Syncer) getNotify() notifier.Notifier {
	s.notifyMu.RLock()
	defer s.notifyMu.RUnlock()
	return s.notify
}

func (s *Syncer) SetNotifier(n notifier.Notifier) {
	s.notifyMu.Lock()
	s.notify = n
	s.notifyMu.Unlock()
}

func (s *Syncer) MarkDegraded(service string) {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	s.degraded[service] = time.Now()
}

func (s *Syncer) IsDegraded(service string) bool {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	_, ok := s.degraded[service]
	return ok
}

func (s *Syncer) ClearDegraded(service string) {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	delete(s.degraded, service)
}

func (s *Syncer) DegradedServices() []string {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	out := make([]string, 0, len(s.degraded))
	for svc := range s.degraded {
		out = append(out, svc)
	}
	return out
}

func (s *Syncer) SetEventHook(fn func(event string, data any)) {
	s.onEventMu.Lock()
	defer s.onEventMu.Unlock()
	s.onEvent = fn
}

func (s *Syncer) emit(event string, data any) {
	s.onEventMu.Lock()
	fn := s.onEvent
	s.onEventMu.Unlock()
	if fn != nil {
		fn(event, data)
	}
}

func (s *Syncer) SetReloadFunc(fn func() error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.reload = fn
}

func (s *Syncer) ResolveDomain(ctx context.Context, domain string) ([]string, error) {
	return s.resolver().ResolveDomain(ctx, domain)
}

func (s *Syncer) PurgeCache() error {
	return s.cache.PurgeExpired()
}

func (s *Syncer) PingMikroTik(ctx context.Context) error {
	return s.mt().Ping(ctx)
}

// ============================================================================
// Основная логика синхронизации
// ============================================================================

func (s *Syncer) SyncService(ctx context.Context, name string, dry, force bool) (Result, error) {
	start := time.Now()
	list := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name)

	res := Result{
		Service:   name,
		List:      list,
		Comment:   comment,
		DryRun:    dry,
		StartedAt: start,
		Version:   version.Version,
	}

	if !s.acquire(name) {
		msg := fmt.Sprintf("Синхронизация %s пропущена: предыдущий запуск ещё выполняется", name)
		s.log.Warn(msg, "service", name)
		s.emit("skipped", map[string]any{"service": name, "reason": "already running"})
		if !dry {
			_ = s.getNotify().SyncStart(ctx, []string{name}, "вручную", "пропущено: уже выполняется")
		}
		res.Error = msg
		res.FinishedAt = time.Now()
		res.Duration = res.FinishedAt.Sub(start).String()
		return res, fmt.Errorf("%s", msg)
	}
	defer s.release(name)

	log := s.log.With("service", name, "list", list, "comment", comment, "dry_run", dry, "force", force)
	log.Info("starting sync")
	s.emit("sync_start", map[string]any{"service": name, "list": list, "dry_run": dry})

	// 1. Управляемые записи сервиса (list + comment + dynamic=false).
	// ИЗМЕНЕНО: при недоступности Микротика в DRY-RUN режиме продолжаем с пустым списком
	existing, err := s.mt().ListServiceEntries(ctx, list, s.cfg.Firewall.CommentPrefix, name)
	if err != nil {
		if dry {
			log.Warn("MikroTik is unavailable in DRY-RUN mode. Assuming empty list for testing.", "err", err)
			existing = nil
		} else {
			res.Error = fmt.Sprintf("list entries: %v", err)
			log.Error("failed to list address entries", "err", err)
			s.notifyError(ctx, name, err)
			return res, fmt.Errorf("list entries: %w", err)
		}
	} else {
		log.Info("fetched existing managed entries", "count", len(existing))
	}

	// 2. Сбор IPv4 CIDR.
	ov := s.cfg.Overrides[name]
	raw, method, err := s.collect(ctx, name, ov)
	if err != nil {
		res.Error = fmt.Sprintf("collect: %v", err)
		log.Error("failed to collect prefixes", "err", err)
		s.notifyError(ctx, name, err)
		return res, fmt.Errorf("collect: %w", err)
	}
	log.Info("collected raw prefixes", "count", len(raw), "method", method)

	// 3. Валидация.
	v := validator.Validator{Safety: s.cfg.Safety}
	prefixes, err := v.Validate(prefixesToStrings(raw), ov)
	if err != nil {
		res.Error = fmt.Sprintf("validate: %v", err)
		log.Error("validation failed", "err", err)
		s.notifyError(ctx, name, err)
		return res, fmt.Errorf("validate: %w", err)
	}

	if len(prefixes) == 0 {
		res.Error = "no valid prefixes after validation, aborting sync (fail-closed)"
		log.Error("fail-closed: no valid prefixes collected")
		s.notifyError(ctx, name, fmt.Errorf("%s", res.Error))
		return res, fmt.Errorf("no valid prefixes after validation")
	}

	// 4. Агрегация.
	agg, err := aggregator.Aggregate(prefixes)
	if err != nil {
		log.Error("aggregation invariant violated, using unaggregated list", "err", err)
		agg = prefixes
	}
	log.Info("aggregated prefixes", "count", len(agg))

	desired := make([]string, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, p.String())
	}

	// 5. Diff по нормализованному address.
	diff, err := addresslist.ComputeDiff(name, list, comment, desired, existing)
	if err != nil {
		res.Error = fmt.Sprintf("compute diff: %v", err)
		log.Error("diff computation failed", "err", err)
		s.notifyError(ctx, name, err)
		return res, fmt.Errorf("compute diff: %w", err)
	}

	res.Added = len(diff.Add)
	res.Removed = len(diff.Remove)
	res.Unchanged = len(diff.Unchanged)
	log.Info("computed diff", "add", res.Added, "remove", res.Removed, "unchanged", res.Unchanged)

	// 6. SAFE-DIFF.
	if len(existing) > 0 && !force {
		if err := addresslist.CheckDeletion(len(existing), len(diff.Remove), addresslist.SafetyParams{
			MaxDeleteRatio:          s.cfg.Safety.MaxDeleteRatio,
			RequireConfirmationOver: s.cfg.Safety.RequireConfirmationOver,
		}); err != nil {
			res.Error = err.Error()
			log.Error("safety-check blocked sync", "err", err)
			s.notifyError(ctx, name, err)
			return res, err
		}
	}

	toRemove := addresslist.RemovalPlan(desired, existing)
	toAdd := make([]addresslist.Entry, 0, len(diff.Add))
	for _, a := range diff.Add {
		toAdd = append(toAdd, addresslist.Entry{Address: a})
	}

	// 7. Снапшот перед применением.
	snapshotID := s.createSnapshot(ctx, name, existing)
	if snapshotID == "" {
		log.Warn("snapshot creation failed, proceeding without rollback capability")
	} else {
		log.Info("snapshot created", "snapshot_id", snapshotID)
	}

	// 8. Dry run — только diff.
	if dry {
		log.Info("dry run completed", "add", res.Added, "remove", res.Removed)
		res.Success = true
		res.FinishedAt = time.Now()
		res.Duration = res.FinishedAt.Sub(start).String()
		return res, nil
	}

	// 9. Транзакционное применение.
	tx := mikrotik.NewTransaction(s.mt(), s.cfg, s.cache, name, log)
	if snapshotID != "" {
		tx.SetSnapshotID(snapshotID)
	}

	if err := tx.Apply(ctx, toAdd, toRemove); err != nil {
		res.Error = err.Error()
		log.Error("transaction failed", "err", err)
		if tx.State() == mikrotik.StateDegraded {
			s.MarkDegraded(name)
			_ = s.getNotify().Error(ctx, name, fmt.Errorf(
				"⚠️ Сервис %s в состоянии DEGRADED: откат (rollback) прошёл лишь частично, нужна ручная проверка. Снимок для восстановления: app restore %s --from-snapshot %s --force",
				name, name, snapshotID))
		} else {
			s.notifyError(ctx, name, err)
		}
		s.emit("sync_error", map[string]any{"service": name, "error": err.Error()})
		return res, fmt.Errorf("transaction: %w", err)
	}

	// 10. Финализация.
	res.Success = true
	s.ClearDegraded(name)
	res.FinishedAt = time.Now()
	res.Duration = res.FinishedAt.Sub(start).String()
	elapsed := time.Since(start)

	logRes := logging.SyncResult{
		Service:    res.Service,
		Success:    res.Success,
		Added:      res.Added,
		Removed:    res.Removed,
		Unchanged:  res.Unchanged,
		Error:      res.Error,
		DryRun:     res.DryRun,
		StartedAt:  res.StartedAt,
		FinishedAt: res.FinishedAt,
		Duration:   res.Duration,
	}
	logging.LogSyncResult(log, logRes, elapsed)

	_ = s.getNotify().Send(ctx, fmt.Sprintf(
		"✅ %s (list %s): +%d добавлено, −%d удалено, %d без изменений (%s)",
		name, list, res.Added, res.Removed, res.Unchanged, res.Duration))
	s.audit.Log(EntryToAudit(res))
	s.emit("sync_done", map[string]any{
		"service": name, "list": list,
		"added": res.Added, "removed": res.Removed, "unchanged": res.Unchanged,
	})

	s.serviceMu.Lock()
	s.history.Add(history.Record{
		Time:       res.StartedAt,
		Service:    name,
		Method:     method,
		Status:     "success",
		Collected:  len(raw),
		Aggregated: len(agg),
		Added:      res.Added,
		Removed:    res.Removed,
		Unchanged:  res.Unchanged,
		DurationMS: elapsed.Milliseconds(),
	})
	s.lastSync = time.Now()
	s.serviceMu.Unlock()

	log.Info("sync completed successfully", "duration", elapsed)
	return res, nil
}

func (s *Syncer) SyncMany(ctx context.Context, services []string, dry bool, force bool) error {
	if len(services) == 0 {
		return nil
	}

	start := time.Now()
	s.audit.LogSyncStart(services)
	_ = s.getNotify().SyncStart(ctx, services, "вручную", fmt.Sprintf("пробный режим: %v, принудительно: %v", dry, force))

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	results := make([]Result, 0, len(services))

	maxConcurrent := s.cfg.Scheduler.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 3
	}
	sem := make(chan struct{}, maxConcurrent)

	for _, name := range services {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("panic in sync goroutine", "service", n, "panic", fmt.Sprintf("%v", r))
					mu.Lock()
					results = append(results, Result{Service: n, Success: false, Error: fmt.Sprintf("panic: %v", r), DryRun: dry})
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: panic: %v", n, r)
					}
					mu.Unlock()
				}
			}()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				results = append(results, Result{Service: n, Success: false, Error: "context cancelled", DryRun: dry})
				mu.Unlock()
				return
			}

			res, err := s.SyncService(ctx, n, dry, force)
			mu.Lock()
			results = append(results, res)
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", n, err)
			}
			mu.Unlock()
		}(name)
	}

	wg.Wait()

	logResults := make([]logging.SyncResult, len(results))
	for i, r := range results {
		logResults[i] = logging.SyncResult{
			Service: r.Service, Success: r.Success, Added: r.Added, Removed: r.Removed,
			Unchanged: r.Unchanged, Error: r.Error, DryRun: r.DryRun,
			StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Duration: r.Duration,
		}
	}
	logging.LogSyncSummary(s.log, logResults)

	nr := make([]notifier.SyncResult, 0, len(results))
	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.Service
		nr = append(nr, notifier.SyncResult{
			Service:    r.Service,
			Success:    r.Success,
			Added:      r.Added,
			Removed:    r.Removed,
			Unchanged:  r.Unchanged,
			Error:      logging.RedactString(r.Error),
			DurationMs: r.FinishedAt.Sub(r.StartedAt).Milliseconds(),
		})
	}
	_ = s.getNotify().SyncDone(ctx, nr, time.Since(start), dry)
	s.audit.LogSyncComplete(names, time.Since(start), firstErr)
	return firstErr
}

func (s *Syncer) SyncOneResult(ctx context.Context, name string, dry bool) notifier.SyncResult {
	res, err := s.SyncService(ctx, name, dry, false)
	sr := notifier.SyncResult{
		Service:    res.Service,
		Success:    res.Success,
		Added:      res.Added,
		Removed:    res.Removed,
		Unchanged:  res.Unchanged,
		Error:      res.Error,
		DurationMs: res.FinishedAt.Sub(res.StartedAt).Milliseconds(),
	}
	if err != nil && sr.Error == "" {
		sr.Error = err.Error()
	}
	return sr
}

// ============================================================================
// CLI/Web-хелперы
// ============================================================================

func (s *Syncer) InfoService(ctx context.Context, name string) (*ServiceInfo, error) {
	list := s.cfg.Firewall.AddressList
	info := &ServiceInfo{
		Name:     name,
		List:     list,
		Comment:  addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name),
		Schedule: s.cfg.EffectiveSchedule(name),
		Override: s.cfg.Overrides[name],
	}

	entries, err := s.mt().ListServiceEntries(ctx, list, s.cfg.Firewall.CommentPrefix, name)
	if err != nil {
		s.log.Warn("failed to list entries for info", "service", name, "err", err)
	} else {
		info.EntryCount = len(entries)
	}

	snapshots, err := s.cache.ListSnapshots(name)
	if err != nil {
		s.log.Warn("failed to list snapshots for info", "service", name, "err", err)
	} else {
		info.SnapshotCount = len(snapshots)
	}

	return info, nil
}

func (s *Syncer) DiffService(ctx context.Context, name string) (*addresslist.Diff, error) {
	list := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name)

	existing, err := s.mt().ListServiceEntries(ctx, list, s.cfg.Firewall.CommentPrefix, name)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}

	ov := s.cfg.Overrides[name]
	raw, _, err := s.collect(ctx, name, ov)
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}

	v := validator.Validator{Safety: s.cfg.Safety}
	prefixes, err := v.Validate(prefixesToStrings(raw), ov)
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("no valid prefixes after validation")
	}

	agg, err := aggregator.Aggregate(prefixes)
	if err != nil {
		agg = prefixes
	}

	desired := make([]string, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, p.String())
	}
	return addresslist.ComputeDiff(name, list, comment, desired, existing)
}

func (s *Syncer) ListGlobalEntries(ctx context.Context) ([]addresslist.Entry, error) {
	return s.mt().ListEntries(ctx, s.cfg.Firewall.AddressList)
}

func (s *Syncer) ListServiceEntries(ctx context.Context, name string) ([]addresslist.Entry, error) {
	return s.mt().ListServiceEntries(ctx,
		s.cfg.Firewall.AddressList, s.cfg.Firewall.CommentPrefix, name)
}

func (s *Syncer) AuditAddressList(ctx context.Context) ([]OrphanEntry, error) {
	entries, err := s.ListGlobalEntries(ctx)
	if err != nil {
		return nil, err
	}

	prefix := s.cfg.Firewall.CommentPrefix
	active := make(map[string]bool, len(s.cfg.Services))
	for _, sv := range s.cfg.Services {
		active[sv] = true
	}

	var orphans []OrphanEntry
	for _, e := range entries {
		if e.IsDynamic() {
			continue
		}
		svc, ok := addresslist.ServiceFromComment(e.Comment, prefix)
		if !ok {
			continue
		}
		if active[svc] {
			continue
		}
		orphans = append(orphans, OrphanEntry{Addresslist: e, Service: svc})
	}
	return orphans, nil
}

func (s *Syncer) CleanupAutoEntries(ctx context.Context, force bool) (int, error) {
	if !force {
		return 0, fmt.Errorf("cleanup_auto_entries requires --force (orphan-записи не удаляются молча)")
	}
	orphans, err := s.AuditAddressList(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, o := range orphans {
		if err := s.mt().DeleteEntry(ctx, o.Addresslist.ID); err != nil {
			s.log.Error("failed to delete orphan entry",
				"id", o.Addresslist.ID, "address", o.Addresslist.Address, "err", err)
			continue
		}
		deleted++
	}
	s.log.Info("orphan AUTO entries cleanup completed", "deleted", deleted, "total", len(orphans))
	return deleted, nil
}

func (s *Syncer) RemoveServiceEntries(ctx context.Context, name string) (int, error) {
	entries, err := s.ListServiceEntries(ctx, name)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var failed []string
	for _, e := range entries {
		if err := s.mt().DeleteEntry(ctx, e.ID); err != nil {
			failed = append(failed, e.Address)
			s.log.Error("failed to delete entry", "service", name, "id", e.ID, "address", e.Address, "err", err)
			continue
		}
		deleted++
	}
	if len(failed) > 0 {
		return deleted, fmt.Errorf("удалено %d из %d записей, не удалены: %s",
			deleted, len(entries), strings.Join(failed, ", "))
	}
	return deleted, nil
}

// ============================================================================
// Сбор префиксов
// ============================================================================

func (s *Syncer) collect(ctx context.Context, name string, ov config.ServiceOverride) ([]netip.Prefix, string, error) {
	class := classifier.Classify(name, ov.Method)

	opts := collectors.Options{
		Exclude:     ov.Exclude,
		IncludeOnly: ov.IncludeOnly,
	}

	domains := ov.Domains
	if len(domains) == 0 {
		domains = class.Domains
	}
	staticURL := ov.StaticURL
	if staticURL == "" && len(class.StaticURLs) > 0 {
		staticURL = class.StaticURLs[0]
	}
	maxPrefixes := ov.MaxPrefixes
	if maxPrefixes == 0 {
		maxPrefixes = ov.MaxASNPrefixes
	}
	if maxPrefixes == 0 {
		maxPrefixes = s.cfg.Safety.MaxASNPrefixes
	}
	asn := 0
	if class.ASN != "" {
		if n, err := resolverParseASN(class.ASN); err == nil {
			asn = n
		}
	}
	if asn == 0 && (hasMethod(class.Methods, "asn") || hasMethod(class.Methods, "cdn")) && len(domains) > 0 {
		if resolved, err := s.resolver().ResolveASN(ctx, domains[0]); err == nil {
			asn = resolved
		} else {
			s.log.Warn("cannot resolve ASN", "service", name, "err", err)
		}
	}

	alsoCDN := make([]int, 0, len(ov.AlsoCDN))
	for _, c := range ov.AlsoCDN {
		if a, ok := cdnNameToASN[c]; ok {
			alsoCDN = append(alsoCDN, a)
		}
	}

	params := &collectors.Params{
		Service:     name,
		ASN:         asn,
		Domains:     domains,
		IPs:         class.IPs,
		StaticURL:   staticURL,
		MaxPrefixes: maxPrefixes,
		AlsoCDN:     alsoCDN,
	}

	var (
		acc     []netip.Prefix
		used    []string
		lastErr error
	)
	for _, method := range class.Methods {
		res, err := s.registry().Collect(ctx, method, params, opts)
		if err != nil {
			s.log.Warn("collector failed, trying next method", "method", method, "service", name, "err", err)
			lastErr = err
			continue
		}
		if res != nil && len(res.Prefixes) > 0 {
			acc = append(acc, res.Prefixes...)
			used = append(used, res.Method)
		}
	}

	if len(acc) == 0 {
		if lastErr != nil {
			return nil, "", lastErr
		}
		return nil, "", fmt.Errorf("no prefixes collected for service %s", name)
	}

	usedMethod := strings.Join(used, "+")

	if asn > 0 && (strings.Contains(usedMethod, "asn") || strings.Contains(usedMethod, "whois")) {
		acc = s.verifyASNOwnership(ctx, acc, asn)
	}

	return acc, usedMethod, nil
}

func resolverParseASN(s string) (int, error) {
	return resolver.ParseASN(s)
}

const rdapVerifySample = 32

func (s *Syncer) verifyASNOwnership(ctx context.Context, in []netip.Prefix, asn int) []netip.Prefix {
	if len(in) == 0 {
		return in
	}
	step := max(1, len(in)/rdapVerifySample)
	rejected := 0
	checked := 0
	dropped := make(map[netip.Prefix]bool)

	for i := 0; i < len(in); i += step {
		if ctx.Err() != nil {
			break
		}
		checked++
		ok, err := s.resolver().VerifyPrefixBelongsToASN(ctx, in[i], asn)
		if err == nil && !ok {
			dropped[in[i]] = true
			rejected++
		}
	}

	if rejected == 0 {
		return in
	}
	s.log.Warn("rdap asn-ownership check rejected some prefixes",
		"asn", asn, "checked", checked, "explicit_rejects", rejected)

	out := make([]netip.Prefix, 0, len(in))
	for _, p := range in {
		if dropped[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

var cdnNameToASN = map[string]int{
	"cloudflare": 13335,
	"aws":        16509,
	"google":     15169,
	"fastly":     54825,
}

func hasMethod(methods []string, want string) bool {
	for _, m := range methods {
		if m == want {
			return true
		}
	}
	return false
}

// ============================================================================
// Снапшоты / backup / restore
// ============================================================================

func (s *Syncer) createSnapshot(ctx context.Context, name string, entries []addresslist.Entry) string {
	if !s.cfg.Snapshots.Enabled {
		return ""
	}

	addresses := make([]string, 0, len(entries))
	skipped := 0
	for _, e := range entries {
		norm, err := addresslist.NormalizeAddress(e.Address)
		if err != nil {
			s.log.Error("invalid address in snapshot, skipping",
				"address", e.Address, "service", name, "err", err)
			skipped++
			continue
		}
		addresses = append(addresses, norm)
	}
	if skipped > 0 {
		s.log.Warn("some entries skipped in snapshot", "service", name, "skipped", skipped, "total", len(entries))
	}

	id, err := s.CreateSnapshot(ctx, name, addresses)
	if err != nil {
		s.log.Error("failed to create snapshot", "service", name, "err", err)
		return ""
	}
	return id
}

func (s *Syncer) CreateSnapshot(ctx context.Context, service string, addresses []string) (string, error) {
	if !s.cfg.Snapshots.Enabled {
		return "", fmt.Errorf("snapshots are disabled")
	}
	id, err := s.cache.CreateSnapshot(service, addresses)
	if err != nil {
		return "", fmt.Errorf("save snapshot: %w", err)
	}
	s.log.Info("created snapshot", "service", service, "id", id, "addresses", len(addresses))
	return id, nil
}

func (s *Syncer) GetSnapshot(ctx context.Context, service, id string, entries *[]addresslist.Entry) error {
	var addresses []string
	if err := s.cache.GetSnapshot(service, id, &addresses); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	list := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, service)
	out := make([]addresslist.Entry, 0, len(addresses))
	for _, a := range addresses {
		out = append(out, addresslist.Entry{Address: a, List: list, Comment: comment, Disabled: "false"})
	}
	*entries = out
	return nil
}

func (s *Syncer) ListSnapshots(ctx context.Context, service string) ([]SnapshotInfo, error) {
	infos, err := s.cache.ListSnapshots(service)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	result := make([]SnapshotInfo, 0, len(infos))
	for _, info := range infos {
		result = append(result, SnapshotInfo{
			ID:        info.ID,
			Service:   service,
			CreatedAt: info.CreatedAt,
			Count:     info.Count,
		})
	}
	return result, nil
}

func (s *Syncer) DeleteSnapshot(ctx context.Context, service, id string) error {
	return s.cache.DeleteSnapshot(service, id)
}

func (s *Syncer) CleanupSnapshots(ctx context.Context, service string, ttl time.Duration) (int, error) {
	ids, err := s.cache.ListSnapshots(service)
	if err != nil {
		return 0, fmt.Errorf("list snapshots: %w", err)
	}

	deleted := 0
	cutoff := time.Now().Add(-ttl)
	for _, info := range ids {
		if info.CreatedAt.Before(cutoff) {
			if err := s.cache.DeleteSnapshot(service, info.ID); err != nil {
				s.log.Warn("failed to delete expired snapshot", "service", service, "id", info.ID, "err", err)
				continue
			}
			deleted++
		}
	}

	remaining, err := s.cache.ListSnapshots(service)
	if err != nil {
		return deleted, nil
	}
	maxCount := s.cfg.Snapshots.MaxCount
	if maxCount > 0 && len(remaining) > maxCount {
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].CreatedAt.Before(remaining[j].CreatedAt)
		})
		toDelete := len(remaining) - maxCount
		for i := 0; i < toDelete && i < len(remaining); i++ {
			if err := s.cache.DeleteSnapshot(service, remaining[i].ID); err != nil {
				s.log.Warn("failed to delete excess snapshot", "service", service, "id", remaining[i].ID, "err", err)
				continue
			}
			deleted++
		}
	}

	s.log.Info("snapshot cleanup completed", "service", service, "deleted", deleted, "ttl", ttl, "max_count", maxCount)
	return deleted, nil
}

// ============================================================================
// Управление сервисами
// ============================================================================

func (s *Syncer) AddService(ctx context.Context, name string) error {
	return s.AddServiceWithConfig(ctx, name, config.ServiceOverride{})
}

func (s *Syncer) AddServiceWithConfig(ctx context.Context, name string, override config.ServiceOverride) error {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()

	name = strings.ToLower(strings.TrimSpace(name))
	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service name: %s (allowed: lowercase letters/digits with '-', '_', '.' — e.g. instagram, youtube.com, 8.8.8.8, AS13335)", name)
	}

	if err := validator.SanitizeComment(name); err != nil {
		return err
	}

	for _, p := range s.cfg.Services {
		if p == name {
			return fmt.Errorf("service %s already exists", name)
		}
	}

	s.cfg.Services = append(s.cfg.Services, name)

	if s.cfg.Overrides == nil {
		s.cfg.Overrides = make(map[string]config.ServiceOverride)
	}
	s.cfg.Overrides[name] = override

	if err := s.cfg.Save(); err != nil {
		return err
	}
	s.audit.LogServiceAdd("", "core", name)
	s.emit("service_added", map[string]any{"service": name})
	return nil
}

func (s *Syncer) RemoveService(ctx context.Context, name string, purgeRoutes bool) error {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()

	name = strings.ToLower(strings.TrimSpace(name))
	found := false
	var newServices []string
	for _, p := range s.cfg.Services {
		if p == name {
			found = true
			continue
		}
		newServices = append(newServices, p)
	}
	if !found {
		return fmt.Errorf("service %s not found", name)
	}

	if purgeRoutes {
		n, err := s.RemoveServiceEntries(ctx, name)
		if err != nil {
			return fmt.Errorf("purge address-list entries failed, сервис НЕ удалён из конфигурации: %w", err)
		}
		s.log.Info("purged service address entries", "service", name, "count", n)
	}

	ids, err := s.cache.ListSnapshots(name)
	if err == nil {
		for _, info := range ids {
			_ = s.cache.DeleteSnapshot(name, info.ID)
		}
	}

	s.cfg.Services = newServices
	delete(s.cfg.Overrides, name)

	if err := s.cfg.Save(); err != nil {
		return err
	}
	s.audit.LogServiceDelete("", "core", name)
	s.emit("service_removed", map[string]any{"service": name})
	return nil
}

func (s *Syncer) ListServices() []string {
	return s.cfg.Services
}

// ============================================================================
// Backup / Restore
// ============================================================================

func (s *Syncer) Backup(ctx context.Context, name string) (*BackupFile, error) {
	entries, err := s.ListServiceEntries(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	return &BackupFile{
		Service:   name,
		List:      s.cfg.Firewall.AddressList,
		Comment:   addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name),
		CreatedAt: time.Now().UTC(),
		Entries:   entries,
	}, nil
}

func (s *Syncer) Restore(ctx context.Context, name string, entries []addresslist.Entry) error {
	existing, err := s.ListServiceEntries(ctx, name)
	if err != nil {
		s.log.Warn("failed to list existing entries for duplicate check", "service", name, "err", err)
	}
	existingSet := make(map[string]bool, len(existing))
	for _, e := range existing {
		if n, err := addresslist.NormalizeAddress(e.Address); err == nil {
			existingSet[n] = true
		}
	}

	list := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name)

	restored, skipped, failures := 0, 0, 0
	for _, e := range entries {
		norm, err := addresslist.NormalizeAddress(e.Address)
		if err != nil {
			failures++
			s.log.Error("invalid address in restore", "service", name, "address", e.Address, "err", err)
			continue
		}
		if existingSet[norm] {
			skipped++
			continue
		}
		in := addresslist.Entry{Address: norm, List: list, Comment: comment, Disabled: "false"}
		if _, err := s.mt().AddEntry(ctx, in); err != nil {
			failures++
			s.log.Error("failed to restore entry", "service", name, "address", norm, "err", err)
			continue
		}
		restored++
		existingSet[norm] = true
	}

	s.log.Info("restore completed", "service", name,
		"restored", restored, "skipped_duplicates", skipped, "errors", failures, "total", len(entries))

	if failures > 0 {
		return fmt.Errorf("restore completed with errors: restored %d, skipped %d, errors %d out of %d",
			restored, skipped, failures, len(entries))
	}
	return nil
}

// ============================================================================
// Утилиты
// ============================================================================

func (s *Syncer) acquire(service string) bool {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busy[service] {
		return false
	}
	s.busy[service] = true
	return true
}

func (s *Syncer) release(service string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	delete(s.busy, service)
}

func (s *Syncer) notifyError(ctx context.Context, service string, err error) {
	if err == nil {
		return
	}
	_ = s.getNotify().Error(ctx, service, logging.RedactError(err))
}

func (s *Syncer) ReloadScheduler() error {
	s.reloadMu.Lock()
	fn := s.reload
	s.reloadMu.Unlock()
	s.log.Info("scheduler reload requested")
	if fn != nil {
		return fn()
	}
	return nil
}

// SetTelegramRestartFunc регистрирует функцию перезапуска Telegram-бота.
// Вызывается из главного процесса при инициализации демона.
func (s *Syncer) SetTelegramRestartFunc(fn func() error) {
	s.telegramRestartMu.Lock()
	defer s.telegramRestartMu.Unlock()
	s.telegramRestartFunc = fn
}

// RestartTelegramBot перезапускает бота с новыми настройками.
// Вызывается при изменении telegram.* в веб-интерфейсе.
func (s *Syncer) RestartTelegramBot() error {
	s.telegramRestartMu.Lock()
	fn := s.telegramRestartFunc
	s.telegramRestartMu.Unlock()
	
	if fn == nil {
		s.log.Debug("telegram restart function not registered, skipping bot restart")
		return nil
	}
	
	s.log.Info("restarting telegram bot with new configuration")
	return fn()
}

func (s *Syncer) GetLogs(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 100
	}
	logFile := s.cfg.Logging.File
	if logFile == "" {
		return []map[string]any{}, nil
	}
	entries, err := logging.TailLogFile(logFile, limit)
	if err != nil {
		s.log.Warn("failed to read log file", "err", err, "limit", limit)
		return []map[string]any{}, nil
	}
	return entries, nil
}

func EntryToAudit(res Result) audit.Entry {
	status := "success"
	if !res.Success {
		status = "failed"
	}
	return audit.Entry{
		Action: audit.ActionSyncComplete,
		Metadata: map[string]any{
			"service":   res.Service,
			"list":      res.List,
			"comment":   res.Comment,
			"added":     res.Added,
			"removed":   res.Removed,
			"unchanged": res.Unchanged,
			"status":    status,
			"error":     res.Error,
			"duration":  res.Duration,
		},
	}
}

func prefixesToStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}
