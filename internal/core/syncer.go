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
	CIDR     string
	Gateway  string
	Table    string
	Distance int
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
	cfg       *config.Config
	mt        *mikrotik.Client
	cache     *storage.Cache
	log       *slog.Logger
	notify    notifier.Notifier
	audit     *audit.Logger
	history   *history.History
	http      *collectors.HTTP
	resolver  *resolver.Resolver
	mu        sync.Mutex
	startTime time.Time
	lastSync  time.Time
}

// ============================================================================
// Конструктор
// ============================================================================

// NewSyncer создает инициализированный экземпляр Syncer.
// Принимает уже сконфигурированные зависимости (cache, notify),
// что позволяет переиспользовать их между CLI, Web и Scheduler.
func NewSyncer(cfg *config.Config, log *slog.Logger, cache *storage.Cache, notify notifier.Notifier) (*Syncer, error) {
	mt, err := mikrotik.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("create mikrotik client: %w", err)
	}

	auditLog := audit.NewLogger(log)
	hist := history.NewHistory(1000)
	httpClient := collectors.NewHTTP(cfg.External.HTTPTimeout.Duration(), cfg.Retry, cfg.External.MaxResponseMB)
	res := resolver.NewResolver(cfg.External.Resolver, httpClient)

	return &Syncer{
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
	}, nil
}

// ============================================================================
// Метрики состояния
// ============================================================================

func (s *Syncer) StartTime() time.Time    { return s.startTime }
func (s *Syncer) LastSync() time.Time     { return s.lastSync }
func (s *Syncer) Version() string         { return version.Version }

// PingMikroTik проверяет доступность RouterOS API.
func (s *Syncer) PingMikroTik(ctx context.Context) error {
	return s.mt.Ping(ctx)
}

// ============================================================================
// Основная логика синхронизации
// ============================================================================

// SyncService выполняет полный цикл синхронизации одного сервиса:
// 1. Получает текущие маршруты из MikroTik (фильтр по comment=AUTO:<service>).
// 2. Собирает новые префиксы через classifier + collectors.
// 3. Валидирует и агрегирует (fail-closed при пустом результате).
// 4. Вычисляет diff и проверяет safe-delete ratio.
// 5. Создает снапшот и применяет изменения через транзакцию (с rollback при ошибке).
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

	// 1. Получаем существующие маршруты (строго по comment=AUTO:<name>)
	existing, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		res.Error = fmt.Sprintf("list routes: %v", err)
		log.Error("failed to list routes", "err", err)
		return res, fmt.Errorf("list routes: %w", err)
	}
	log.Info("fetched existing routes", "count", len(existing))

	// 2. Сбор префиксов
	ov := s.cfg.Overrides[name]
	raw, err := s.collect(ctx, name, ov)
	if err != nil {
		res.Error = fmt.Sprintf("collect: %v", err)
		log.Error("failed to collect routes", "err", err)
		return res, fmt.Errorf("collect: %w", err)
	}
	log.Info("collected raw prefixes", "count", len(raw))

	// 3. Валидация
	v := validator.Validator{Safety: s.cfg.Safety}
	prefixes, err := v.Validate(prefixesToStrings(raw), ov)
	if err != nil {
		res.Error = fmt.Sprintf("validate: %v", err)
		log.Error("validation failed", "err", err)
		return res, fmt.Errorf("validate: %w", err)
	}

	// FAIL-CLOSED: если валидация вернула 0 префиксов, ничего не меняем
	if len(prefixes) == 0 {
		res.Error = "no valid prefixes after validation, aborting sync (fail-closed)"
		log.Error("fail-closed: no valid prefixes collected")
		return res, fmt.Errorf("no valid prefixes after validation")
	}
	log.Info("validated prefixes", "count", len(prefixes))

	// 4. Агрегация в минимальный набор CIDR
	agg, err := aggregator.Aggregate(prefixes)
	if err != nil {
		res.Error = fmt.Sprintf("aggregate: %v", err)
		log.Error("aggregation failed", "err", err)
		return res, fmt.Errorf("aggregate: %w", err)
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
		ratio := float64(len(diff.Remove)) / float64(len(existing))
		if ratio > s.cfg.Safety.MaxDeleteRatio {
			res.Error = fmt.Sprintf(
				"delete ratio %.2f exceeds maximum %.2f (use --force to override)",
				ratio, s.cfg.Safety.MaxDeleteRatio,
			)
			log.Error("delete ratio exceeded", "ratio", ratio, "max", s.cfg.Safety.MaxDeleteRatio)
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
	tx := mikrotik.NewTransaction(s.mt, s.cfg, s.cache, name, log)
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
		return res, fmt.Errorf("transaction: %w", err)
	}

	// 9. Финализация
	res.Success = true
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
	s.notify.Send(ctx, fmt.Sprintf(
		"✅ Sync %s: +%d -%d =%d (%s)",
		name, res.Added, res.Removed, res.Unchanged, res.Duration,
	))
	s.audit.Log(EntryToAudit(res))
	s.history.Add(history.Record{
		Time:      res.StartedAt,
		Service:   name,
		Added:     res.Added,
		Removed:   res.Removed,
		Unchanged: res.Unchanged,
	})
	s.lastSync = time.Now()

	log.Info("sync completed successfully", "duration", elapsed)
	return res, nil
}

// SyncMany запускает параллельную синхронизацию группы сервисов с ограничением
// на количество одновременных потоков (из конфига `scheduler.max_concurrent`).
func (s *Syncer) SyncMany(ctx context.Context, services []string, dry bool) error {
	if len(services) == 0 {
		return nil
	}

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

			res, err := s.SyncService(ctx, n, dry, false)
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
	return firstErr
}

// ============================================================================
// Сбор префиксов (classifier + collectors)
// ============================================================================

// collect определяет метод сбора через classifier и последовательно
// опрашивает collectors до получения непустого результата.
func (s *Syncer) collect(ctx context.Context, name string, ov config.ServiceOverride) ([]netip.Prefix, error) {
	class := classifier.Classify(name, ov.Method)

	opts := collectors.Options{
		Exclude:     ov.Exclude,
		IncludeOnly: ov.IncludeOnly,
	}

	for _, method := range class.Methods {
		// Akamai (AS20940) обрабатывается как обычный ASN
		if method == "akamai" {
			method = "asn"
		}

		var res *collectors.Result
		var err error

		switch method {
		case "cdn":
			var asn int
			if class.ASN != "" {
				fmt.Sscanf(strings.TrimPrefix(strings.ToUpper(class.ASN), "AS"), "%d", &asn)
			}
			res, err = collectors.NewCDNCollector(asn).Collect(ctx, name, opts)

		case "asn":
			var asn int
			if class.ASN != "" {
				fmt.Sscanf(strings.TrimPrefix(strings.ToUpper(class.ASN), "AS"), "%d", &asn)
			}
			res, err = collectors.NewASNCollector(asn, s.http).Collect(ctx, name, opts)

		case "whois":
			maxP := ov.MaxPrefixes
			if maxP == 0 {
				maxP = 100
			}
			res, err = collectors.NewWHOISCollector(s.resolver, class.Domains, maxP).Collect(ctx, name, opts)

		case "static_url":
			url := ov.StaticURL
			if len(class.StaticURLs) > 0 {
				url = class.StaticURLs[0]
			}
			res, err = collectors.NewStaticCollector(url).Collect(ctx, name, opts)

		case "dynamic":
			res, err = collectors.NewDynamicCollector(s.resolver, class.Domains).Collect(ctx, name, opts)

		default:
			s.log.Warn("unknown collector method", "method", method, "service", name)
			continue
		}

		if err != nil {
			s.log.Warn("collector failed, trying next method", "method", method, "err", err)
			continue
		}

		if res != nil && len(res.Prefixes) > 0 {
			return res.Prefixes, nil
		}
	}

	return nil, fmt.Errorf("no prefixes collected for service %s", name)
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
		var prefixStrings []string
		count := 0
		if err := s.cache.GetSnapshot(service, info.ID, &prefixStrings); err == nil {
			count = len(prefixStrings)
		}
		result = append(result, SnapshotInfo{
			ID:        info.ID,
			Service:   service,
			CreatedAt: info.CreatedAt,
			Count:     count,
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
	return s.addServiceWithConfig(ctx, name, config.ServiceOverride{})
}

// addServiceWithConfig регистрирует сервис с кастомными override-настройками.
func (s *Syncer) addServiceWithConfig(ctx context.Context, name string, override config.ServiceOverride) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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

	return s.cfg.Save()
}

// RemoveService удаляет сервис из конфига и опционально очищает его маршруты.
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

	// Purge routes — удаляем все маршруты с comment=AUTO:<name>
	if purgeRoutes {
		routes, err := s.mt.ListServiceRoutes(ctx, name)
		if err != nil {
			s.log.Warn("failed to list routes for purge", "service", name, "err", err)
		} else {
			for _, r := range routes {
				if err := s.mt.DeleteRoute(ctx, r.ID); err != nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()

	routes, err := s.mt.ListServiceRoutes(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	return routes, nil
}

// Restore импортирует маршруты из backup, пропуская дубликаты.
func (s *Syncer) Restore(ctx context.Context, name string, routes []mikrotik.Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.mt.ListServiceRoutes(ctx, name)
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

		if _, err := s.mt.AddRoute(ctx, r); err != nil {
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

// ReloadScheduler сигналит планировщику перечитать конфиг.
func (s *Syncer) ReloadScheduler() error {
	s.log.Info("scheduler reload requested")
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
