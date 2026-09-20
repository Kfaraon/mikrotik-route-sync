package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// ============================================================================
// Типы данных
// ============================================================================

// Result описывает итог синхронизации одного сервиса.
type Result struct {
	Service    string    `json:"service"`
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

// RouteKey — нормализованный ключ маршрута для идемпотентного сравнения.
type RouteKey struct {
	CIDR     string `json:"cidr"`
	Gateway  string `json:"gateway"`
	Table    string `json:"table"`
	Distance int    `json:"distance"`
}

// DiffResult описывает вычисленную разницу для команды `app diff`.
type DiffResult struct {
	Service   string     `json:"service"`
	Add       []RouteKey `json:"add"`
	Remove    []RouteKey `json:"remove"`
	Unchanged int        `json:"unchanged"`
}

// ServiceInfo — метаинформация о сервисе для команды `app info`.
type ServiceInfo struct {
	Name          string                 `json:"name"`
	Schedule      string                 `json:"schedule"`
	Override      config.ServiceOverride `json:"override"`
	RouteCount    int                    `json:"route_count"`
	SnapshotCount int                    `json:"snapshot_count"`
}

// SnapshotInfo — метаинформация о снапшоте для CLI/Web/Telegram.
type SnapshotInfo struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	CreatedAt time.Time `json:"created_at"`
	Count     int       `json:"count"`
}

// Syncer — главный оркестратор синхронизации маршрутов.
type Syncer struct {
	cfg     *config.Config
	cache   *storage.Cache
	log     *slog.Logger
	audit   *audit.Logger
	history *history.History

	// Горяче-заменяемые зависимости (ApplyConfig) — атомарные указатели,
	// чтобы синхронизация видела обновлённые клиенты без блокировок.
	mtPtr  atomic.Pointer[mikrotik.Client]
	resPtr atomic.Pointer[resolver.Resolver]
	deps   *collectors.Deps
	regPtr atomic.Pointer[collectors.Registry]

	notifyMu sync.RWMutex
	notify   notifier.Notifier

	// serviceMu защищает ТОЛЬКО мутации конфига (add/remove service, save)
	// и запись в history/lastSync. НЕ блокирует параллельную синхронизацию.
	serviceMu sync.Mutex
	startTime time.Time
	lastSync  time.Time

	// busyMu — защита от наложения запусков по ОДНОМУ сервису (PROMPT III.2).
	busyMu sync.Mutex
	busy   map[string]bool

	// degradedMu — статусы сервисов с неуспешным rollback (PROMPT I.3).
	degradedMu sync.Mutex
	degraded   map[string]time.Time

	// onEvent — опциональный хук live-событий (WebSocket в web UI).
	onEventMu sync.Mutex
	onEvent   func(event string, data any)

	// reload — хук перезагрузки планировщика (устанавливается в daemon).
	reloadMu sync.Mutex
	reload   func() error
}

// mt возвращает текущий MikroTik-клиент (горяче заменяемый).
func (s *Syncer) mt() *mikrotik.Client { return s.mtPtr.Load() }

// resolver возвращает текущий резолвер.
func (s *Syncer) resolver() *resolver.Resolver { return s.resPtr.Load() }

// registry возвращает текущий реестр коллекторов.
func (s *Syncer) registry() *collectors.Registry { return s.regPtr.Load() }

// getNotify возвращает нотификатор (потокобезопасно).
func (s *Syncer) getNotify() notifier.Notifier {
	s.notifyMu.RLock()
	defer s.notifyMu.RUnlock()
	return s.notify
}

// SetNotifier подменяет канал уведомлений (например, после включения Telegram в web UI).
func (s *Syncer) SetNotifier(n notifier.Notifier) {
	s.notifyMu.Lock()
	s.notify = n
	s.notifyMu.Unlock()
}

// ============================================================================
// Конструктор
// ============================================================================

// NewSyncer создает инициализированный экземпляр Syncer.
// Принимает уже сконфигурированные зависимости (cache, notify),
// что позволяет переиспользовать их между CLI, Web и Scheduler.
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

// buildDepsLocked пересоздаёт http-клиент, резолвер и реестр коллекторов
// из текущей конфигурации (вызывается из конструктора и ApplyConfig).
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

// ApplyConfig перечитывает конфигурацию (s.cfg уже обновлён externally) и
// пересоздаёт клиенты: MikroTik (host/port/ssl/rate-limit/retry), внешний
// HTTP (таймауты/зеркала), резолвер (DNS/TTL кэша). Вызывается после
// сохранения настроек из web UI — изменения применяются без рестарта.
//
// Не применяются на лету (требуют рестарта процесса): web.listen,
// logging.file, cache_path — это отмечено в подсказках UI.
func (s *Syncer) ApplyConfig() error {
	mt, err := mikrotik.NewClient(s.cfg, s.log)
	if err != nil {
		return fmt.Errorf("rebuild mikrotik client: %w", err)
	}
	s.mtPtr.Store(mt)
	s.buildDepsLocked()

	s.log.Info("configuration applied at runtime",
		"mikrotik_host", s.cfg.MikroTik.Host,
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

// MarkDegraded помечает сервис как degraded (частично неуспешный rollback).
func (s *Syncer) MarkDegraded(service string) {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	s.degraded[service] = time.Now()
}

// IsDegraded возвращает статус degraded сервиса.
func (s *Syncer) IsDegraded(service string) bool {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	_, ok := s.degraded[service]
	return ok
}

// ClearDegraded снимает флаг degraded (успешная синхронизация).
func (s *Syncer) ClearDegraded(service string) {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	delete(s.degraded, service)
}

// DegradedServices — список сервисов в состоянии degraded.
func (s *Syncer) DegradedServices() []string {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	out := make([]string, 0, len(s.degraded))
	for svc := range s.degraded {
		out = append(out, svc)
	}
	return out
}

// SetEventHook регистрирует callback для live-событий (web WebSocket).
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

// SetReloadFunc регистрирует хук перечитывания расписаний (daemon/web).
func (s *Syncer) SetReloadFunc(fn func() error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.reload = fn
}

// ResolveDomain — диагностика для `app test-dns`.
func (s *Syncer) ResolveDomain(ctx context.Context, domain string) ([]string, error) {
	return s.resolver().ResolveDomain(ctx, domain)
}

// PurgeCache удаляет просроченные записи bbolt-кэша (scheduler.cache_purge).
func (s *Syncer) PurgeCache() error {
	return s.cache.PurgeExpired()
}

// PingMikroTik проверяет доступность RouterOS API.
func (s *Syncer) PingMikroTik(ctx context.Context) error {
	return s.mt().Ping(ctx)
}

// ============================================================================
// Основная логика синхронизации
// ============================================================================

// SyncService выполняет полный цикл синхронизации одного сервиса:
// 1. Защита от наложения (второй запуск того же сервиса пропускается с WARN).
// 2. Получает текущие маршруты из MikroTik (фильтр по comment=AUTO:<service>).
// 3. Собирает новые префиксы через classifier + collectors.
// 4. Валидирует и агрегирует (fail-closed при пустом результате).
// 5. Вычисляет diff и проверяет safe-delete ratio + RequireConfirmationOver.
// 6. Создает снапшот и применяет изменения через транзакцию (с rollback при ошибке).
//
// NOTE: Метод НЕ использует глобальный мьютекс, что позволяет параллельную
// синхронизацию нескольких сервисов через SyncMany.
func (s *Syncer) SyncService(ctx context.Context, name string, dry, force bool) (Result, error) {
	start := time.Now()
	res := Result{
		Service:   name,
		DryRun:    dry,
		StartedAt: start,
		Version:   version.Version,
	}

	// Mutex на сервис: повторный запуск пропускается (PROMPT III.2).
	if !s.acquire(name) {
		msg := fmt.Sprintf("sync skipped: %s is already running", name)
		s.log.Warn(msg, "service", name)
		s.emit("skipped", map[string]any{"service": name, "reason": "already running"})
		if !dry {
			_ = s.getNotify().SyncStart(ctx, []string{name}, "manual", "skipped: already running")
		}
		res.Error = msg
		res.FinishedAt = time.Now()
		res.Duration = res.FinishedAt.Sub(start).String()
		return res, fmt.Errorf("%s", msg)
	}
	defer s.release(name)

	log := s.log.With("service", name, "dry_run", dry, "force", force)
	log.Info("starting sync")
	s.emit("sync_start", map[string]any{"service": name, "dry_run": dry})

	// 1. Получаем существующие маршруты (строго по comment=AUTO:<name>)
	existing, err := s.mt().ListServiceRoutes(ctx, name)
	if err != nil {
		res.Error = fmt.Sprintf("list routes: %v", err)
		log.Error("failed to list routes", "err", err)
		s.notifyError(ctx, name, err)
		return res, fmt.Errorf("list routes: %w", err)
	}
	log.Info("fetched existing routes", "count", len(existing))

	// 2. Сбор префиксов
	ov := s.cfg.Overrides[name]
	raw, method, err := s.collect(ctx, name, ov)
	if err != nil {
		res.Error = fmt.Sprintf("collect: %v", err)
		log.Error("failed to collect routes", "err", err)
		s.notifyError(ctx, name, err)
		return res, fmt.Errorf("collect: %w", err)
	}
	log.Info("collected raw prefixes", "count", len(raw), "method", method)

	// 3. Валидация
	v := validator.Validator{Safety: s.cfg.Safety}
	prefixes, err := v.Validate(prefixesToStrings(raw), ov)
	if err != nil {
		res.Error = fmt.Sprintf("validate: %v", err)
		log.Error("validation failed", "err", err)
		s.notifyError(ctx, name, err)
		return res, fmt.Errorf("validate: %w", err)
	}

	// FAIL-CLOSED: если валидация вернула 0 префиксов, ничего не меняем
	if len(prefixes) == 0 {
		res.Error = "no valid prefixes after validation, aborting sync (fail-closed)"
		log.Error("fail-closed: no valid prefixes collected")
		s.notifyError(ctx, name, fmt.Errorf("%s", res.Error))
		return res, fmt.Errorf("no valid prefixes after validation")
	}
	log.Info("validated prefixes", "count", len(prefixes))

	// 4. Агрегация в минимальный набор CIDR
	agg, err := aggregator.Aggregate(prefixes)
	if err != nil {
		// Нарушение инварианта — безопасный откат к неагрегированному списку
		log.Error("aggregation invariant violated, using unaggregated list", "err", err)
		agg = prefixes
	}
	log.Info("aggregated prefixes", "count", len(agg))

	// Формируем желаемый набор маршрутов (используем глобальные настройки MikroTik)
	desired := make([]RouteKey, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, RouteKey{
			CIDR:     p.String(),
			Gateway:  s.cfg.MikroTik.Gateway,
			Table:    s.cfg.MikroTik.RoutingTable,
			Distance: s.cfg.MikroTik.Distance,
		})
	}

	// 5. Вычисление diff
	diff, err := ComputeDiff(desired, existing)
	if err != nil {
		res.Error = fmt.Sprintf("compute diff: %v", err)
		log.Error("diff computation failed", "err", err)
		return res, fmt.Errorf("compute diff: %w", err)
	}

	res.Added = len(diff.Add)
	res.Removed = len(diff.Remove)
	res.Unchanged = diff.Unchanged
	log.Info("computed diff", "add", res.Added, "remove", res.Removed, "unchanged", res.Unchanged)

	// SAFE-DIFF: защита от массового удаления маршрутов
	if len(existing) > 0 && !force {
		// Проверка 1: Относительный ratio
		ratio := float64(len(diff.Remove)) / float64(len(existing))
		if ratio > s.cfg.Safety.MaxDeleteRatio {
			res.Error = fmt.Sprintf(
				"delete ratio %.2f exceeds maximum %.2f (use --force to override)",
				ratio, s.cfg.Safety.MaxDeleteRatio,
			)
			log.Error("delete ratio exceeded", "ratio", ratio, "max", s.cfg.Safety.MaxDeleteRatio)
			return res, fmt.Errorf("%s", res.Error)
		}

		// Проверка 2: Абсолютный порог RequireConfirmationOver
		if s.cfg.Safety.RequireConfirmationOver > 0 &&
			len(diff.Remove) > s.cfg.Safety.RequireConfirmationOver {
			res.Error = fmt.Sprintf(
				"delete count %d exceeds require_confirmation_over %d (use --force to override)",
				len(diff.Remove), s.cfg.Safety.RequireConfirmationOver,
			)
			log.Error("delete count exceeded absolute threshold",
				"remove_count", len(diff.Remove),
				"threshold", s.cfg.Safety.RequireConfirmationOver)
			return res, fmt.Errorf("%s", res.Error)
		}
	}

	// 6. Снапшот перед применением (для rollback)
	snapshotID := s.createSnapshot(ctx, name, existing)
	if snapshotID == "" {
		log.Warn("snapshot creation failed, proceeding without rollback capability")
	} else {
		log.Info("snapshot created", "snapshot_id", snapshotID)
	}

	// 7. Dry run — только логируем diff
	if dry {
		log.Info("dry run completed", "add", res.Added, "remove", res.Removed)
		res.Success = true
		res.FinishedAt = time.Now()
		res.Duration = res.FinishedAt.Sub(start).String()
		return res, nil
	}

	// 8. Транзакционное применение (с автоматическим rollback при ошибке)
	tx := mikrotik.NewTransaction(s.mt(), s.cfg, s.cache, name, log)
	if snapshotID != "" {
		tx.SetSnapshotID(snapshotID)
	}

	toAdd := make([]mikrotik.Route, 0, len(diff.Add))
	for _, k := range diff.Add {
		toAdd = append(toAdd, mikrotik.Route{
			DstAddress:   k.CIDR,
			Gateway:      k.Gateway,
			RoutingTable: k.Table,
			Distance:     strconv.Itoa(k.Distance),
			Comment:      fmt.Sprintf("%s:%s", s.cfg.MikroTik.CommentPrefix, name),
		})
	}

	if err := tx.Apply(ctx, toAdd, diff.Remove); err != nil {
		res.Error = err.Error()
		log.Error("transaction failed", "err", err)
		// Если транзакция завершилась в degraded (rollback частично неудачен) —
		// помечаем сервис и шлём алерт высоким приоритетом (PROMPT I.3).
		if tx.State() == mikrotik.StateDegraded {
			s.MarkDegraded(name)
			_ = s.getNotify().Error(ctx, name, fmt.Errorf(
				"DEGRADED: rollback partially failed for %s, manual intervention required (snapshot %s)",
				name, snapshotID))
		} else {
			s.notifyError(ctx, name, err)
		}
		s.emit("sync_error", map[string]any{"service": name, "error": err.Error()})
		return res, fmt.Errorf("transaction: %w", err)
	}

	// 9. Финализация
	res.Success = true
	s.ClearDegraded(name)
	res.FinishedAt = time.Now()
	res.Duration = res.FinishedAt.Sub(start).String()
	elapsed := time.Since(start)

	// Логирование (приведение к logging.SyncResult)
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

	// Уведомления, аудит, история
	s.getNotify().Send(ctx, fmt.Sprintf(
		"✅ Sync %s: +%d -%d =%d (%s)",
		name, res.Added, res.Removed, res.Unchanged, res.Duration,
	))
	s.audit.Log(EntryToAudit(res))
	s.emit("sync_done", map[string]any{
		"service": name, "added": res.Added, "removed": res.Removed, "unchanged": res.Unchanged,
	})

	// КРИТИЧЕСКАЯ СЕКЦИЯ: запись в историю и lastSync
	s.serviceMu.Lock()
	s.history.Add(history.Record{
		Time:       res.StartedAt,
		Service:    name,
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

// SyncMany запускает ПАРАЛЛЕЛЬНУЮ синхронизацию группы сервисов с ограничением
// на количество одновременных потоков (из конфига `scheduler.max_concurrent`).
//
// ВАЖНО: force параметр пробрасывается в SyncService.
// Итоговый отчёт агрегируется и отправляется в Telegram (PROMPT III.2).
func (s *Syncer) SyncMany(ctx context.Context, services []string, dry bool, force bool) error {
	if len(services) == 0 {
		return nil
	}

	start := time.Now()
	s.audit.LogSyncStart(services)
	_ = s.getNotify().SyncStart(ctx, services, "manual", fmt.Sprintf("dry_run=%v force=%v", dry, force))

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
					results = append(results, Result{
						Service: n, Success: false,
						Error: fmt.Sprintf("panic: %v", r), DryRun: dry,
					})
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
				results = append(results, Result{
					Service: n, Success: false,
					Error: "context cancelled", DryRun: dry,
				})
				mu.Unlock()
				return
			}

			// ВАЖНО: передаём force в SyncService
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

	// Итоговое логирование группы
	logResults := make([]logging.SyncResult, len(results))
	for i, r := range results {
		logResults[i] = logging.SyncResult{
			Service:    r.Service,
			Success:    r.Success,
			Added:      r.Added,
			Removed:    r.Removed,
			Unchanged:  r.Unchanged,
			Error:      r.Error,
			DryRun:     r.DryRun,
			StartedAt:  r.StartedAt,
			FinishedAt: r.FinishedAt,
			Duration:   r.Duration,
		}
	}
	logging.LogSyncSummary(s.log, logResults)

	nr := make([]notifier.SyncResult, 0, len(results))
	for _, r := range results {
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
	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.Service
	}
	_ = s.getNotify().SyncDone(ctx, nr, time.Since(start), dry)
	s.audit.LogSyncComplete(names, time.Since(start), firstErr)
	return firstErr
}

// ============================================================================
// Команды info / diff для CLI
// ============================================================================

// InfoService возвращает детальную информацию о сервисе.
func (s *Syncer) InfoService(ctx context.Context, name string) (*ServiceInfo, error) {
	info := &ServiceInfo{
		Name:     name,
		Schedule: s.cfg.EffectiveSchedule(name),
		Override: s.cfg.Overrides[name],
	}

	// Количество маршрутов в MikroTik
	routes, err := s.mt().ListServiceRoutes(ctx, name)
	if err != nil {
		s.log.Warn("failed to list routes for info", "service", name, "err", err)
	} else {
		info.RouteCount = len(routes)
	}

	// Количество снапшотов
	snapshots, err := s.cache.ListSnapshots(name)
	if err != nil {
		s.log.Warn("failed to list snapshots for info", "service", name, "err", err)
	} else {
		info.SnapshotCount = len(snapshots)
	}

	return info, nil
}

// DiffService вычисляет diff без применения (аналог --dry-run, но без применения).
// ИСПРАВЛЕНО: не использует приватные поля из других файлов.
func (s *Syncer) DiffService(ctx context.Context, name string) (*DiffResult, error) {
	existing, err := s.mt().ListServiceRoutes(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
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
		return nil, fmt.Errorf("aggregate: %w", err)
	}

	desired := make([]RouteKey, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, RouteKey{
			CIDR:     p.String(),
			Gateway:  s.cfg.MikroTik.Gateway,
			Table:    s.cfg.MikroTik.RoutingTable,
			Distance: s.cfg.MikroTik.Distance,
		})
	}

	diff, err := ComputeDiff(desired, existing)
	if err != nil {
		return nil, fmt.Errorf("compute diff: %w", err)
	}

	// Конвертируем diff.Remove ([]mikrotik.Route) в []RouteKey
	removeKeys := make([]RouteKey, 0, len(diff.Remove))
	for _, r := range diff.Remove {
		cidr := r.DstAddress
		if p, err := netip.ParsePrefix(cidr); err == nil {
			cidr = p.Masked().String()
		}

		distance := 1
		if r.Distance != "" {
			if d, err := strconv.Atoi(r.Distance); err == nil {
				distance = d
			}
		}

		removeKeys = append(removeKeys, RouteKey{
			CIDR:     cidr,
			Gateway:  r.Gateway,
			Table:    r.RoutingTable,
			Distance: distance,
		})
	}

	return &DiffResult{
		Service:   name,
		Add:       diff.Add,
		Remove:    removeKeys,
		Unchanged: diff.Unchanged,
	}, nil
}

// ============================================================================
// Сбор префиксов (classifier + collectors)
// ============================================================================

// collect определяет методы сбора через classifier и опрашивает collectors.
// Для совмещённых методов результаты объединяются; при ошибке метода
// пробуется следующий. Возвращает сырые префиксы и имя сработавшего метода.
func (s *Syncer) collect(ctx context.Context, name string, ov config.ServiceOverride) ([]netip.Prefix, string, error) {
	class := classifier.Classify(name, ov.Method)

	opts := collectors.Options{
		Exclude:     ov.Exclude,
		IncludeOnly: ov.IncludeOnly,
	}

	// Override-параметрики имеют приоритет над словарём классификатора.
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
		if n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(class.ASN), "AS")); err == nil {
			asn = n
		}
	}
	// Для unknown-сервисов без override пробуем определить ASN через RDAP/Cymru
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

	// Уровень 3 валидации (PROMPT II.3): проверка принадлежности ASN через RDAP.
	// Best-effort и ограничена по количеству запросов: явное несовпадение ASN
	// отбрасывает префикс, недоступность RDAP — оставляет (префиксы уже
	// подтверждены BGP-анонсами источника).
	if asn > 0 && (strings.Contains(usedMethod, "asn") || strings.Contains(usedMethod, "whois")) {
		acc = s.verifyASNOwnership(ctx, acc, asn)
	}

	return acc, usedMethod, nil
}

// verifyASNOwnership проверяет RDAP-принадлежность выборки префиксов ASN.
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

// cdnNameToASN — словарь имён CDN из also_cdn.
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
// Работа со снапшотами (Rollback infrastructure)
// ============================================================================

// createSnapshot — внутренний хелпер: создает снапшот из существующих маршрутов.
func (s *Syncer) createSnapshot(ctx context.Context, name string, routes []mikrotik.Route) string {
	if !s.cfg.Snapshots.Enabled {
		return ""
	}

	prefixes := make([]netip.Prefix, 0, len(routes))
	skipped := 0

	for _, r := range routes {
		p, err := netip.ParsePrefix(r.DstAddress)
		if err != nil {
			s.log.Error("invalid prefix in snapshot, skipping",
				"prefix", r.DstAddress, "service", name, "err", err)
			skipped++
			continue
		}
		prefixes = append(prefixes, p)
	}

	if skipped > 0 {
		s.log.Warn("some routes skipped in snapshot",
			"service", name, "skipped", skipped, "total", len(routes))
	}

	snapshotID, err := s.CreateSnapshot(ctx, name, prefixes)
	if err != nil {
		s.log.Error("failed to create snapshot", "service", name, "err", err)
		return ""
	}
	return snapshotID
}

// CreateSnapshot сохраняет текущий набор префиксов сервиса в bbolt.
func (s *Syncer) CreateSnapshot(ctx context.Context, service string, prefixes []netip.Prefix) (string, error) {
	if !s.cfg.Snapshots.Enabled {
		return "", fmt.Errorf("snapshots are disabled")
	}

	prefixStrings := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		prefixStrings = append(prefixStrings, p.String())
	}

	id, err := s.cache.CreateSnapshot(service, prefixStrings)
	if err != nil {
		return "", fmt.Errorf("save snapshot: %w", err)
	}

	s.log.Info("created snapshot", "service", service, "id", id, "prefixes", len(prefixes))
	return id, nil
}

// GetSnapshot загружает снапшот и конвертирует его обратно в []mikrotik.Route.
func (s *Syncer) GetSnapshot(ctx context.Context, service, id string, routes *[]mikrotik.Route) error {
	var prefixStrings []string
	if err := s.cache.GetSnapshot(service, id, &prefixStrings); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	prefixes := make([]netip.Prefix, 0, len(prefixStrings))
	for _, ps := range prefixStrings {
		if p, err := netip.ParsePrefix(ps); err == nil {
			prefixes = append(prefixes, p)
		}
	}

	*routes = make([]mikrotik.Route, 0, len(prefixes))
	for _, p := range prefixes {
		*routes = append(*routes, mikrotik.Route{
			DstAddress:   p.String(),
			Gateway:      s.cfg.MikroTik.Gateway,
			RoutingTable: s.cfg.MikroTik.RoutingTable,
			Distance:     strconv.Itoa(s.cfg.MikroTik.Distance),
			Comment:      fmt.Sprintf("%s:%s", s.cfg.MikroTik.CommentPrefix, service),
		})
	}

	return nil
}

// ListSnapshots возвращает список всех снапшотов сервиса с количеством префиксов.
func (s *Syncer) ListSnapshots(ctx context.Context, service string) ([]SnapshotInfo, error) {
	infos, err := s.cache.ListSnapshots(service)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}

	var result []SnapshotInfo
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

// DeleteSnapshot удаляет конкретный снапшот сервиса.
func (s *Syncer) DeleteSnapshot(ctx context.Context, service, id string) error {
	return s.cache.DeleteSnapshot(service, id)
}

// CleanupSnapshots удаляет старые снапшоты по TTL и ограничивает их количество.
func (s *Syncer) CleanupSnapshots(ctx context.Context, service string, ttl time.Duration) (int, error) {
	ids, err := s.cache.ListSnapshots(service)
	if err != nil {
		return 0, fmt.Errorf("list snapshots: %w", err)
	}

	deleted := 0
	cutoff := time.Now().Add(-ttl)

	// Удаляем просроченные
	for _, info := range ids {
		if info.CreatedAt.Before(cutoff) {
			if err := s.cache.DeleteSnapshot(service, info.ID); err != nil {
				s.log.Warn("failed to delete expired snapshot",
					"service", service, "id", info.ID, "err", err)
				continue
			}
			deleted++
		}
	}

	// Удаляем лишние, если превышен max_count
	remaining, err := s.cache.ListSnapshots(service)
	if err != nil {
		return deleted, nil
	}

	maxCount := s.cfg.Snapshots.MaxCount
	if len(remaining) > maxCount {
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].CreatedAt.Before(remaining[j].CreatedAt)
		})

		toDelete := len(remaining) - maxCount
		for i := 0; i < toDelete && i < len(remaining); i++ {
			if err := s.cache.DeleteSnapshot(service, remaining[i].ID); err != nil {
				s.log.Warn("failed to delete excess snapshot",
					"service", service, "id", remaining[i].ID, "err", err)
				continue
			}
			deleted++
		}
	}

	s.log.Info("snapshot cleanup completed",
		"service", service, "deleted", deleted, "ttl", ttl, "max_count", maxCount)
	return deleted, nil
}

// ============================================================================
// Управление сервисами (CLI / Web / Telegram Bot)
// ============================================================================

// AddService регистрирует новый сервис в конфиге с дефолтными настройками.
func (s *Syncer) AddService(ctx context.Context, name string) error {
	return s.AddServiceWithConfig(ctx, name, config.ServiceOverride{})
}

// AddServiceWithConfig регистрирует сервис с кастомными override-настройками.
// КРИТИЧЕСКАЯ СЕКЦИЯ: мутация конфига требует мьютекса.
func (s *Syncer) AddServiceWithConfig(ctx context.Context, name string, override config.ServiceOverride) error {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()

	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service name: %s (must match [a-z0-9-])", name)
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

// RemoveService удаляет сервис из конфига и опционально очищает его маршруты.
// КРИТИЧЕСКАЯ СЕКЦИЯ: мутация конфига требует мьютекса.
func (s *Syncer) RemoveService(ctx context.Context, name string, purgeRoutes bool) error {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()

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

	// Purge routes — удаляем все маршруты с comment=AUTO:<name>
	if purgeRoutes {
		routes, err := s.mt().ListServiceRoutes(ctx, name)
		if err != nil {
			s.log.Warn("failed to list routes for purge", "service", name, "err", err)
		} else {
			for _, r := range routes {
				if err := s.mt().DeleteRoute(ctx, r.ID); err != nil {
					s.log.Warn("failed to delete route",
						"service", name, "dst", r.DstAddress, "err", err)
				}
			}
			s.log.Info("purged service routes", "service", name, "count", len(routes))
		}
	}

	// Удаляем все снапшоты сервиса
	ids, err := s.cache.ListSnapshots(name)
	if err != nil {
		s.log.Warn("failed to list snapshots", "service", name, "err", err)
	} else {
		for _, info := range ids {
			_ = s.cache.DeleteSnapshot(name, info.ID)
		}
		s.log.Info("deleted service snapshots", "service", name, "count", len(ids))
	}

	s.cfg.Services = newServices
	delete(s.cfg.Overrides, name)

	return s.cfg.Save()
}

// ListServices возвращает список зарегистрированных сервисов.
func (s *Syncer) ListServices() []string {
	return s.cfg.Services
}

// ============================================================================
// Backup / Restore
// ============================================================================

// Backup экспортирует текущие маршруты сервиса в формате []mikrotik.Route.
func (s *Syncer) Backup(ctx context.Context, name string) ([]mikrotik.Route, error) {
	routes, err := s.mt().ListServiceRoutes(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	return routes, nil
}

// Restore импортирует маршруты из backup, пропуская дубликаты.
func (s *Syncer) Restore(ctx context.Context, name string, routes []mikrotik.Route) error {
	existing, err := s.mt().ListServiceRoutes(ctx, name)
	if err != nil {
		s.log.Warn("failed to list existing routes for duplicate check",
			"service", name, "err", err)
	}

	existingMap := make(map[string]bool)
	for _, r := range existing {
		existingMap[r.DstAddress] = true
	}

	restored := 0
	skipped := 0
	errors := 0

	for _, r := range routes {
		r.Comment = fmt.Sprintf("%s:%s", s.cfg.MikroTik.CommentPrefix, name)

		if existingMap[r.DstAddress] {
			skipped++
			continue
		}

		if _, err := s.mt().AddRoute(ctx, r); err != nil {
			errors++
			s.log.Error("failed to restore route",
				"service", name, "dst", r.DstAddress, "err", err)
			continue
		}
		restored++
		existingMap[r.DstAddress] = true
	}

	s.log.Info("restore completed",
		"service", name,
		"restored", restored,
		"skipped_duplicates", skipped,
		"errors", errors,
		"total", len(routes),
	)

	if errors > 0 {
		return fmt.Errorf(
			"restore completed with errors: restored %d, skipped %d, errors %d out of %d",
			restored, skipped, errors, len(routes),
		)
	}
	return nil
}

// ============================================================================
// Утилиты
// ============================================================================

// acquire помечает сервис как «занятый». Возвращает false, если синхронизация
// этого сервиса уже выполняется (защита от наложения, PROMPT III.2).
func (s *Syncer) acquire(service string) bool {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busy[service] {
		return false
	}
	s.busy[service] = true
	return true
}

// release снимает метку занятости сервиса.
func (s *Syncer) release(service string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	delete(s.busy, service)
}

// notifyError отправляет уведомление об ошибке с redaction секретов.
func (s *Syncer) notifyError(ctx context.Context, service string, err error) {
	if err == nil {
		return
	}
	_ = s.getNotify().Error(ctx, service, logging.RedactError(err))
}

// SyncOneResult — синхронизация одного сервиса с возвратом notifier.SyncResult
// (используется планировщиком для уведомлений).
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

// ReloadScheduler сигналит планировщику перечитать конфиг.
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

// GetLogs возвращает последние N записей из лог-файла.
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

// ============================================================================
// Вспомогательные функции
// ============================================================================

// EntryToAudit конвертирует Result в формат для audit-лога.
func EntryToAudit(res Result) audit.Entry {
	status := "success"
	if !res.Success {
		status = "failed"
	}
	return audit.Entry{
		Action: audit.ActionSyncComplete,
		Metadata: map[string]any{
			"service":   res.Service,
			"added":     res.Added,
			"removed":   res.Removed,
			"unchanged": res.Unchanged,
			"status":    status,
			"error":     res.Error,
			"duration":  res.Duration,
		},
	}
}

// prefixesToStrings конвертирует []netip.Prefix в []string для валидатора.
func prefixesToStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}
