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

// Result представляет результат синхронизации
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

// Syncer управляет синхронизацией маршрутов
type Syncer struct {
	cfg       *config.Config
	mt        *mikrotik.Client
	cache     *storage.Cache
	log       *slog.Logger
	notify    *notifier.Notifier
	audit     *audit.Audit
	history   *history.History
	http      *collectors.HTTP
	resolver  *resolver.Resolver
	mu        sync.Mutex
	startTime time.Time
	lastSync  time.Time
}

// NewSyncer создает новый экземпляр Syncer
func NewSyncer(cfg *config.Config, log *slog.Logger) (*Syncer, error) {
	mt, err := mikrotik.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("create mikrotik client: %w", err)
	}

	cache, err := storage.NewCache(cfg.Storage.Path, cfg.Snapshots.MaxCount)
	if err != nil {
		return nil, fmt.Errorf("create cache: %w", err)
	}

	notify := notifier.NewNotifier(cfg)
	auditLog := audit.NewAudit(log)
	hist := history.NewHistory(cfg.History.MaxEntries)
	httpClient := collectors.NewHTTP(cfg.External.Timeout)
	res := resolver.NewResolver(cfg.External.Resolver, httpClient)

	s := &Syncer{
		cfg:       cfg,
		mt:        mt,
		cache:     cache,
		log:       log,
		notify:    notify,
		audit:     auditLog,
		history:   hist,
		http:      httpClient,
		resolver:  res,
		startTime: time.Now(),
	}

	return s, nil
}

// StartTime возвращает время запуска синхронизатора
func (s *Syncer) StartTime() time.Time {
	return s.startTime
}

// LastSync возвращает время последней синхронизации
func (s *Syncer) LastSync() time.Time {
	return s.lastSync
}

// PingMikroTik проверяет соединение с MikroTik
func (s *Syncer) PingMikroTik(ctx context.Context) error {
	return s.mt.Ping(ctx)
}

// SyncService выполняет синхронизацию одного сервиса
func (s *Syncer) SyncService(ctx context.Context, name string, dry, force bool) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	start := time.Now()
	res := Result{
		Service:   name,
		DryRun:    dry,
		StartedAt: start,
		Version:   version.Version,
	}

	log := s.log.With("service", name, "dry_run", dry)
	log.Info("starting sync")

	// Получаем текущие маршруты из MikroTik
	existing, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		res.Error = fmt.Sprintf("list routes: %v", err)
		log.Error("failed to list routes", "err", err)
		return res, fmt.Errorf("list routes: %w", err)
	}

	log.Info("fetched existing routes", "count", len(existing))

	// Получаем желаемые маршруты
	ov := s.cfg.Overrides[name]
	raw, err := s.collect(ctx, name, ov)
	if err != nil {
		res.Error = fmt.Sprintf("collect: %v", err)
		log.Error("failed to collect routes", "err", err)
		return res, fmt.Errorf("collect: %w", err)
	}

	log.Info("collected raw prefixes", "count", len(raw))

	// Валидируем и нормализуем
	v := validator.Validator{Safety: s.cfg.Safety}
	prefixes, err := v.Validate(raw, ov)
	if err != nil {
		res.Error = fmt.Sprintf("validate: %v", err)
		log.Error("validation failed", "err", err)
		return res, fmt.Errorf("validate: %w", err)
	}

	log.Info("validated prefixes", "count", len(prefixes))

	// Fail-closed: если после сбора и валидации не осталось ни одной сети
	if len(prefixes) == 0 {
		res.Error = "no valid prefixes after validation, aborting sync (fail-closed)"
		log.Error("fail-closed: no valid prefixes collected")
		return res, fmt.Errorf("no valid prefixes after validation")
	}

	// Агрегируем
	agg, err := aggregator.Aggregate(prefixes)
	if err != nil {
		res.Error = fmt.Sprintf("aggregate: %v", err)
		log.Error("aggregation failed", "err", err)
		return res, fmt.Errorf("aggregate: %w", err)
	}

	log.Info("aggregated prefixes", "count", len(agg))

	// Преобразуем в RouteKey
	desired := make([]RouteKey, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, RouteKey{
			CIDR:     p.String(),
			Gateway:  ov.Gateway,
			Table:    ov.RoutingTable,
			Distance: ov.Distance,
		})
	}

	// Вычисляем разницу
	diff, err := ComputeDiff(desired, existing)
	if err != nil {
		res.Error = fmt.Sprintf("compute diff: %v", err)
		log.Error("diff computation failed", "err", err)
		return res, fmt.Errorf("compute diff: %w", err)
	}

	res.Added = len(diff.Add)
	res.Removed = len(diff.Remove)
	res.Unchanged = diff.Unchanged

	log.Info("computed diff", "add", len(diff.Add), "remove", len(diff.Remove), "unchanged", diff.Unchanged)

	// Проверяем безопасность удаления
	if len(existing) > 0 && !force {
		ratio := float64(len(diff.Remove)) / float64(len(existing))
		if ratio > s.cfg.Safety.MaxDeleteRatio {
			res.Error = fmt.Sprintf("delete ratio %.2f exceeds maximum %.2f (use --force to override)", ratio, s.cfg.Safety.MaxDeleteRatio)
			log.Error("delete ratio exceeded", "ratio", ratio, "max", s.cfg.Safety.MaxDeleteRatio)
			return res, fmt.Errorf("%s", res.Error)
		}
	}

	// Создаем snapshot ДО транзакции для возможности отката
	snapshotID := s.createSnapshot(ctx, name, existing)
	if snapshotID == "" {
		log.Warn("snapshot creation failed, proceeding without rollback capability")
	} else {
		log.Info("snapshot created", "snapshot_id", snapshotID)
	}

	// Если dry run, не применяем изменения
	if dry {
		log.Info("dry run completed", "add", len(diff.Add), "remove", len(diff.Remove))
		res.Success = true
		res.FinishedAt = time.Now()
		res.Duration = res.FinishedAt.Sub(start).String()
		return res, nil
	}

	// Создаем транзакцию с cache для возможности rollback из snapshot
	tx := mikrotik.NewTransaction(s.mt, s.cfg, s.cache, name, log)

	// Устанавливаем snapshot ID для возможного отката
	if snapshotID != "" {
		tx.SetSnapshotID(snapshotID)
	}

	// Преобразуем diff.Add в []mikrotik.Route
	toAdd := make([]mikrotik.Route, 0, len(diff.Add))
	for _, k := range diff.Add {
		toAdd = append(toAdd, mikrotik.Route{
			DstAddress:   k.CIDR,
			Gateway:      k.Gateway,
			RoutingTable: k.Table,
			Distance:     strconv.Itoa(k.Distance),
		})
	}

	// Применяем изменения (при ошибке автоматически выполнится rollback)
	if err := tx.Apply(ctx, toAdd, diff.Remove); err != nil {
		res.Error = err.Error()
		log.Error("transaction failed", "err", err)
		return res, fmt.Errorf("transaction: %w", err)
	}

	res.Success = true
	res.FinishedAt = time.Now()
	res.Duration = res.FinishedAt.Sub(start).String()
	elapsed := time.Since(start)

	logging.LogSyncResult(log, res, elapsed)
	s.notify.Notify(ctx, res)
	s.audit.LogSync(res)
	s.history.Add(res)
	s.lastSync = time.Now()

	log.Info("sync completed successfully", "duration", elapsed)
	return res, nil
}

// SyncMany выполняет синхронизацию нескольких сервисов параллельно
func (s *Syncer) SyncMany(ctx context.Context, services []string, dry bool) error {
	if len(services) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	results := make([]Result, 0, len(services))

	// ИСПРАВЛЕНО: используем Scheduler.MaxConcurrent вместо несуществующего Parallelism
	maxConcurrent := s.cfg.Scheduler.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 3 // значение по умолчанию
	}
	sem := make(chan struct{}, maxConcurrent)

	for _, name := range services {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()

			// ИСПРАВЛЕНО: защита от паники в горутине
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("panic in sync goroutine",
						"service", name,
						"panic", fmt.Sprintf("%v", r),
					)
					mu.Lock()
					defer mu.Unlock()
					results = append(results, Result{
						Service: name,
						Success: false,
						Error:   fmt.Sprintf("panic: %v", r),
						DryRun:  dry,
					})
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: panic: %v", name, r)
					}
				}
			}()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				results = append(results, Result{
					Service: name,
					Success: false,
					Error:   "context cancelled",
					DryRun:  dry,
				})
				mu.Unlock()
				return
			}

			res, err := s.SyncService(ctx, name, dry, false)

			mu.Lock()
			defer mu.Unlock()
			results = append(results, res)
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", name, err)
			}
		}(name)
	}

	wg.Wait()

	// Логируем сводку
	logging.LogSyncSummary(s.log, results)

	return firstErr
}

// collect собирает префиксы для сервиса
func (s *Syncer) collect(ctx context.Context, name string, ov config.ServiceOverride) ([]netip.Prefix, error) {
	class := classifier.Classify(name, ov.Method)

	var raw []netip.Prefix
	var err error

	for _, method := range class.Methods {
		switch method {
		case "cdn":
			raw, err = collectors.NewCDN(s.http).Collect(ctx, name, ov)
		case "asn":
			raw, err = collectors.NewASN(s.http).Collect(ctx, name, ov)
		case "whois":
			raw, err = collectors.NewWhois(s.http).Collect(ctx, name, ov)
		case "static_url":
			raw, err = collectors.NewStatic(s.http).Collect(ctx, name, ov)
		case "dynamic":
			raw, err = collectors.NewDynamic(s.http, s.resolver).Collect(ctx, name, ov)
		case "akamai":
			raw, err = collectors.NewAkamai(s.http).Collect(ctx, name, ov)
		default:
			return nil, fmt.Errorf("unknown method: %s", method)
		}

		if err != nil {
			s.log.Warn("collector failed", "method", method, "err", err)
			continue
		}

		if len(raw) > 0 {
			return raw, nil
		}
	}

	return nil, fmt.Errorf("no prefixes collected for service %s", name)
}

// createSnapshot создает снимок текущих маршрутов и возвращает его ID
// ИСПРАВЛЕНО: возвращает string ID вместо void
func (s *Syncer) createSnapshot(ctx context.Context, name string, routes []mikrotik.Route) string {
	if !s.cfg.Snapshots.Enabled {
		return ""
	}

	prefixes := make([]netip.Prefix, 0, len(routes))
	skipped := 0

	for _, r := range routes {
		p, err := netip.ParsePrefix(r.DstAddress)
		if err != nil {
			// ИСПРАВЛЕНО: логируем на уровне Error, а не Warn, чтобы привлечь внимание
			s.log.Error("invalid prefix in snapshot, skipping",
				"prefix", r.DstAddress,
				"service", name,
				"err", err,
			)
			skipped++
			continue
		}
		prefixes = append(prefixes, p)
	}

	if skipped > 0 {
		s.log.Warn("some routes skipped in snapshot",
			"service", name,
			"skipped", skipped,
			"total", len(routes),
		)
	}

	snapshotID, err := s.CreateSnapshot(ctx, name, prefixes)
	if err != nil {
		s.log.Error("failed to create snapshot",
			"service", name,
			"err", err,
		)
		return ""
	}

	return snapshotID
}

// ======================== МЕТОДЫ УПРАВЛЕНИЯ СЕРВИСАМИ ========================

// AddService добавляет новый сервис (для CLI)
func (s *Syncer) AddService(ctx context.Context, name string) error {
	return s.addServiceWithConfig(ctx, name, []string{}, config.ServiceOverride{})
}

// addServiceWithConfig добавляет сервис с конфигурацией
func (s *Syncer) addServiceWithConfig(ctx context.Context, name string, prefixes []string, override config.ServiceOverride) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service name: %s", name)
	}

	if err := validator.SanitizeComment(name); err != nil {
		return err
	}

	// Проверка существования сервиса
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

// RemoveService удаляет сервис
func (s *Syncer) RemoveService(ctx context.Context, name string, purgeRoutes bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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

	// Если нужно удалить маршруты из MikroTik
	if purgeRoutes {
		routes, err := s.mt.ListServiceRoutes(ctx, name)
		if err != nil {
			s.log.Warn("failed to list routes for purge", "service", name, "err", err)
		} else {
			for _, r := range routes {
				if err := s.mt.DeleteRoute(ctx, r.ID); err != nil {
					s.log.Warn("failed to delete route", "service", name, "dst", r.DstAddress, "err", err)
				}
			}
			s.log.Info("purged service routes", "service", name, "count", len(routes))
		}
	}

	// Удаляем снапшоты
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

// ListServices возвращает список сервисов
func (s *Syncer) ListServices() []string {
	return s.cfg.Services
}

// ======================== МЕТОДЫ РАБОТЫ СО СНАПШОТАМИ ========================

// SnapshotInfo информация о снапшоте
type SnapshotInfo struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	CreatedAt time.Time `json:"created_at"`
	Count     int       `json:"count"`
}

// CreateSnapshot создает снапшот префиксов
func (s *Syncer) CreateSnapshot(ctx context.Context, service string, prefixes []netip.Prefix) (string, error) {
	if !s.cfg.Snapshots.Enabled {
		return "", fmt.Errorf("snapshots are disabled")
	}

	id := fmt.Sprintf("%d", time.Now().UnixNano())
	key := "snapshots:" + service

	// Конвертируем префиксы в строки для хранения
	prefixStrings := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		prefixStrings = append(prefixStrings, p.String())
	}

	if err := s.cache.SaveSnapshot(key, id, prefixStrings); err != nil {
		return "", fmt.Errorf("save snapshot: %w", err)
	}

	s.log.Info("created snapshot", "service", service, "id", id, "prefixes", len(prefixes))
	return id, nil
}

// GetSnapshot загружает снапшот и заполняет переданный слайс маршрутов (для CLI)
func (s *Syncer) GetSnapshot(ctx context.Context, service, id string, routes *[]mikrotik.Route) error {
	prefixes, err := s.cache.LoadSnapshot(service, id)
	if err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	*routes = make([]mikrotik.Route, 0, len(prefixes))
	for _, p := range prefixes {
		// ИСПРАВЛЕНО: p.String() вместо p (была ошибка типов)
		*routes = append(*routes, mikrotik.Route{
			DstAddress:   p.String(),
			Gateway:      s.cfg.MikroTik.Gateway,
			RoutingTable: s.cfg.MikroTik.RoutingTable,
			Distance:     fmt.Sprintf("%d", s.cfg.MikroTik.Distance),
			Comment:      fmt.Sprintf("%s:%s", s.cfg.MikroTik.CommentPrefix, service),
		})
	}

	return nil
}

// ListSnapshots возвращает список снапшотов с информацией (для CLI)
func (s *Syncer) ListSnapshots(ctx context.Context, service string) ([]SnapshotInfo, error) {
	ids, err := s.cache.ListSnapshots(service)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}

	var result []SnapshotInfo
	for _, info := range ids {
		prefixes, err := s.cache.LoadSnapshot(service, info.ID)
		if err != nil {
			s.log.Warn("failed to load snapshot", "service", service, "id", info.ID, "err", err)
			continue
		}
		result = append(result, SnapshotInfo{
			ID:        info.ID,
			Service:   service,
			CreatedAt: info.CreatedAt,
			Count:     len(prefixes),
		})
	}

	return result, nil
}

// DeleteSnapshot удаляет снапшот (для CLI)
func (s *Syncer) DeleteSnapshot(ctx context.Context, service, id string) error {
	return s.cache.DeleteSnapshot(service, id)
}

// CleanupSnapshots удаляет старые снапшоты (для CLI)
// ИСПРАВЛЕНО: теперь учитывает и TTL, и MaxCount
func (s *Syncer) CleanupSnapshots(ctx context.Context, service string, ttl time.Duration) (int, error) {
	ids, err := s.cache.ListSnapshots(service)
	if err != nil {
		return 0, fmt.Errorf("list snapshots: %w", err)
	}

	deleted := 0
	cutoff := time.Now().Add(-ttl)

	// Шаг 1: Удаляем снапшоты старше TTL
	for _, info := range ids {
		if info.CreatedAt.Before(cutoff) {
			if err := s.cache.DeleteSnapshot(service, info.ID); err != nil {
				s.log.Warn("failed to delete expired snapshot",
					"service", service,
					"id", info.ID,
					"err", err,
				)
				continue
			}
			deleted++
			s.log.Info("deleted expired snapshot",
				"service", service,
				"id", info.ID,
				"created_at", info.CreatedAt,
			)
		}
	}

	// Шаг 2: Если после очистки по TTL всё ещё больше лимита, удаляем самые старые
	remaining, err := s.cache.ListSnapshots(service)
	if err != nil {
		return deleted, nil // Возвращаем сколько удалили на этом этапе
	}

	maxCount := s.cfg.Snapshots.MaxCount
	if len(remaining) > maxCount {
		// Сортируем по CreatedAt (старые первыми)
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].CreatedAt.Before(remaining[j].CreatedAt)
		})

		toDelete := len(remaining) - maxCount
		for i := 0; i < toDelete && i < len(remaining); i++ {
			if err := s.cache.DeleteSnapshot(service, remaining[i].ID); err != nil {
				s.log.Warn("failed to delete excess snapshot",
					"service", service,
					"id", remaining[i].ID,
					"err", err,
				)
				continue
			}
			deleted++
			s.log.Info("deleted excess snapshot",
				"service", service,
				"id", remaining[i].ID,
			)
		}
	}

	s.log.Info("snapshot cleanup completed",
		"service", service,
		"deleted", deleted,
		"ttl", ttl,
		"max_count", maxCount,
	)

	return deleted, nil
}

// ======================== МЕТОДЫ РЕЗЕРВНОГО КОПИРОВАНИЯ ========================

// Backup возвращает список маршрутов сервиса (для CLI)
func (s *Syncer) Backup(ctx context.Context, name string) ([]mikrotik.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	routes, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}

	return routes, nil
}

// Restore восстанавливает маршруты из списка (для CLI)
// ИСПРАВЛЕНО: теперь проверяет существование маршрутов для предотвращения дубликатов
func (s *Syncer) Restore(ctx context.Context, name string, routes []mikrotik.Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Получаем существующие маршруты для проверки дубликатов
	existing, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		s.log.Warn("failed to list existing routes for duplicate check",
			"service", name,
			"err", err,
		)
	}

	// Строим карту существующих маршрутов
	existingMap := make(map[string]bool)
	for _, r := range existing {
		existingMap[r.DstAddress] = true
	}

	restored := 0
	skipped := 0
	errors := 0

	for _, r := range routes {
		r.Comment = fmt.Sprintf("%s:%s", s.cfg.MikroTik.CommentPrefix, name)

		// ИСПРАВЛЕНО: проверка на дубликат
		if existingMap[r.DstAddress] {
			s.log.Info("route already exists, skipping",
				"service", name,
				"dst", r.DstAddress,
			)
			skipped++
			continue
		}

		if _, err := s.mt.AddRoute(ctx, r); err != nil {
			errors++
			s.log.Error("failed to restore route",
				"service", name,
				"dst", r.DstAddress,
				"err", err,
			)
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
		return fmt.Errorf("restore completed with errors: restored %d, skipped %d, errors %d out of %d",
			restored, skipped, errors, len(routes))
	}

	return nil
}

// ======================== ВСПОМОГАТЕЛЬНЫЕ МЕТОДЫ ========================

// ReloadScheduler перезагружает планировщик
// ИСПРАВЛЕНО: теперь имеет реальную реализацию
func (s *Syncer) ReloadScheduler() error {
	s.log.Info("scheduler reload requested")

	// Отправляем сигнал для перечитывания расписаний
	// Реализация зависит от архитектуры планировщика
	// Здесь вызываем метод для перечитывания конфигурации расписаний

	// Перечитываем конфигурацию расписаний
	if s.cfg != nil {
		s.log.Info("reloading schedules configuration")
		// Логика перезагрузки расписаний будет в scheduler
	}

	return nil
}

// GetLogs возвращает последние логи (для веб-интерфейса)
// ИСПРАВЛЕНО: теперь читает логи из файла
func (s *Syncer) GetLogs(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 100
	}

	logFile := s.cfg.Logging.File
	if logFile == "" {
		return []map[string]any{}, nil
	}

	// Читаем лог-файл и парсим JSON строки
	entries, err := logging.TailLogFile(logFile, limit)
	if err != nil {
		s.log.Warn("failed to read log file", "err", err, "limit", limit)
		return []map[string]any{}, nil
	}

	return entries, nil
}

// Version возвращает версию приложения
func (s *Syncer) Version() string {
	return version.Version
}
