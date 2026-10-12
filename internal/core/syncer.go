package core

// Syncer — ядро оркестратора синхронизации (сервисы, история, снапшоты, кэш):
// структура, конструктор, движок синхронизации (SyncService/SyncMany),
// прогресс, служебные доступы.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
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

// ErrServiceNotFound — сервиса нет в конфиге (Services). Синк такого сервиса
// не должен выполняться: иначе на роутер писались бы записи AUTO:<name> без
// записи в конфиге (сироты в address-list).
var ErrServiceNotFound = errors.New("service not found")

type Result struct {
	Service   string `json:"service"`
	List      string `json:"list"`
	Comment   string `json:"comment"`
	Success   bool   `json:"success"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Unchanged int    `json:"unchanged"`
	Updated   int    `json:"updated"` // re-enable выключенных управляемых записей
	Error     string `json:"error,omitempty"`
	DryRun    bool   `json:"dry_run"`
	// MikroTikDown — роутер был недоступен: в dry-run результат посчитан
	// от пустого списка (fail-open для тестов), ему нельзя доверять.
	MikroTikDown bool            `json:"mikrotik_down,omitempty"`
	Coverage     *CoverageReport `json:"coverage,omitempty"`
	Broad        []string        `json:"broad,omitempty"`
	Verification []*SNIVerdict   `json:"verification,omitempty"`
	// Rejected — сколько префиксов валидатор отбросил и по каким причинам
	// (ставится, только если отсев был).
	Rejected   *validator.RejectStats `json:"rejected,omitempty"`
	StartedAt  time.Time              `json:"started_at"`
	FinishedAt time.Time              `json:"finished_at"`
	Duration   string                 `json:"duration"`
	Version    string                 `json:"version,omitempty"`
}

type Syncer struct {
	cfg     *config.Config
	cache   *storage.Cache
	log     *slog.Logger
	audit   *audit.Logger
	history *history.History

	mtPtr  atomic.Pointer[mikrotik.Client]
	resPtr atomic.Pointer[resolver.Resolver]
	deps   atomic.Pointer[collectors.Deps]
	regPtr atomic.Pointer[collectors.Registry]

	notifyMu sync.RWMutex
	notify   notifier.Notifier

	serviceMu sync.Mutex
	startTime time.Time
	lastSync  time.Time

	busyMu sync.Mutex
	busy   map[string]bool

	progressMu sync.RWMutex
	progress   map[string]SyncProgress

	// addProgress — подшаги добавления сервиса (детект CDN): UI поллит статус,
	// пока POST /services не вернул ответ. Отдельно от progress: добавление
	// не является синхронизацией и не должно попадать в таблицу прогрессов.
	addProgressMu sync.RWMutex
	addProgress   map[string]string

	degradedMu sync.Mutex
	degraded   map[string]time.Time

	onEventMu sync.Mutex
	onEvent   func(event string, data any)

	reloadMu sync.Mutex
	reload   func() error

	// cdnProbeFn — подмена пробника CDN-детекции в тестах (nil = реальный).
	cdnProbeFn func(ctx context.Context, name string, domains []string) *CDNAdvice

	// collectFn — подмена сбора префиксов в тестах (nil = реальный collect).
	collectFn func(ctx context.Context, name string, ov config.ServiceOverride) ([]netip.Prefix, string, error)

	// sniMu защищает SNI-кэш и троттлинг алертов.
	sniMu      sync.Mutex
	sniCache   map[string]sniCacheEntry
	sniAlerted map[string]time.Time

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
	hist.SetLogger(log)

	s := &Syncer{
		cfg:         cfg,
		cache:       cache,
		log:         log,
		audit:       auditLog,
		history:     hist,
		notify:      notify,
		busy:        map[string]bool{},
		progress:    map[string]SyncProgress{},
		addProgress: map[string]string{},
		degraded:    map[string]time.Time{},
		startTime:   time.Now(),
		sniCache:    map[string]sniCacheEntry{},
		sniAlerted:  map[string]time.Time{},
	}
	s.mtPtr.Store(mt)
	s.buildDepsLocked()
	return s, nil
}

func (s *Syncer) buildDepsLocked() {
	httpClient := collectors.NewHTTP(s.cfg.External.HTTPTimeout.Duration(), s.cfg.Retry, s.cfg.External.MaxResponseMB, s.cfg.External.BGPToolsContact)
	res := resolver.NewResolver(s.cfg.External.Resolver, httpClient, s.cache, s.cfg.Scheduler.CacheTTL.Duration())
	res.WithFallbacks(s.cfg.External.FallbackResolvers...)

	deps := &collectors.Deps{
		HTTP:     httpClient,
		Resolver: res,
		Cache:    s.cache,
		TTL:      s.cfg.Scheduler.CacheTTL.Duration(),
	}
	reg := collectors.NewRegistry(deps)

	// Каталог CDN-источников живёт в конфиге: публикуем снимок для
	// classifier-сервисов без домена и метода cdn (cloudflare/aws/...).
	collectors.SetCDNSources(s.cfg.CDNSources)

	s.deps.Store(deps)
	s.resPtr.Store(res)
	s.regPtr.Store(reg)
}

func (s *Syncer) ApplyConfig() error {
	// Перестроение клиентов по текущему конфигу: чтение под RLock,
	// вызывающие (веб, SIGHUP-путь) блокировок не держат.
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	mt, err := mikrotik.NewClient(s.cfg, s.log)
	if err != nil {
		return fmt.Errorf("rebuild mikrotik client: %w", err)
	}
	s.mtPtr.Store(mt)
	s.buildDepsLocked()

	s.log.Info("конфигурация применена на лету",
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

func (s *Syncer) Version() string { return version.Version }

func (s *Syncer) Config() *config.Config { return s.cfg }

// RLockConfig/RUnlockConfig — чтение конфига для внешних владельцев
// (бот, планировщик вызывает напрямую через *Config). Правило то же:
// только точки входа, без вложенности.
func (s *Syncer) RLockConfig()   { s.cfg.RLock() }
func (s *Syncer) RUnlockConfig() { s.cfg.RUnlock() }

func (s *Syncer) Cache() *storage.Cache { return s.cache }

func (s *Syncer) History() *history.History { return s.history }

func (s *Syncer) mt() *mikrotik.Client { return s.mtPtr.Load() }

func (s *Syncer) resolver() *resolver.Resolver { return s.resPtr.Load() }

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

// SyncProgress — прогресс текущей синхронизации сервиса (для UI).

type SyncProgress struct {
	Service string `json:"service"`
	Phase   string `json:"phase"`
	Percent int    `json:"percent"`
	DryRun  bool   `json:"dry_run"`
	Detail  string `json:"detail,omitempty"`
}

// setProgress фиксирует текущую фазу синхронизации и рассылает событие
// sync_progress (уходит в WS-hub и поллер /api/v1/progress).

func (s *Syncer) setProgress(name, phase string, percent int, dry bool) {
	s.setProgressDetail(name, phase, percent, dry, "")
}

// setProgressDetail — как setProgress, но с текстовой детализацией текущего
// подшага (например, «DNS: x.com (3/7)»). Деталь показывается в UI второй
// строкой; при переходе к следующей вехе сбрасывается.
func (s *Syncer) setProgressDetail(name, phase string, percent int, dry bool, detail string) {
	s.progressMu.Lock()
	s.progress[name] = SyncProgress{Service: name, Phase: phase, Percent: percent, DryRun: dry, Detail: detail}
	s.progressMu.Unlock()
	s.emit("sync_progress", map[string]any{
		"service": name, "phase": phase, "percent": percent, "dry_run": dry, "detail": detail,
	})
}

// clearProgress убирает сервис из активных прогрессов (defer в SyncService).
func (s *Syncer) clearProgress(name string) {
	s.progressMu.Lock()
	delete(s.progress, name)
	s.progressMu.Unlock()
}

// Отрезки процента best-effort проверок после агрегации: контроль покрытия
// занимает [50,52], SNI-проба — [52,55]. Внутри отрезка виден текущий домен
// (done/total); без этого фаза «агрегация префиксов · 50%» замирала бы на
// минуты, пока резолвятся домены и делаются handshake.
const (
	coverageProgressLo, coverageProgressHi = 50, 52
	sniProgressLo, sniProgressHi           = 52, 55
)

// reportVerificationStep двигает прогресс best-effort проверки (покрытие/SNI)
// внутри отрезка [lo, hi]: доля done/total доменов и подпись текущего домена.
func (s *Syncer) reportVerificationStep(name, phase string, dry bool, lo, hi, done, total int, detail string) {
	if total <= 0 {
		return
	}
	pct := lo + (hi-lo)*done/total
	if pct > hi {
		pct = hi
	}
	s.setProgressDetail(name, phase, pct, dry, fmt.Sprintf("%s (%d/%d)", detail, done, total))
}

// collectProgressFunc возвращает приёмник прогресса сбора: подшаги коллекторов
// обновляют Detail текущей фазы, не меняя её название.
func (s *Syncer) collectProgressFunc(name string, dry bool) collectors.ProgressFunc {
	return func(percent int, label string) {
		s.setProgressDetail(name, "сбор адресов из источников", percent, dry, label)
	}
}

// ProgressSnapshot — копия карты активных синхронизаций (GET /api/v1/progress).

func (s *Syncer) ProgressSnapshot() map[string]SyncProgress {
	s.progressMu.RLock()
	defer s.progressMu.RUnlock()
	out := make(map[string]SyncProgress, len(s.progress))
	for k, v := range s.progress {
		out[k] = v
	}
	return out
}

// AddProgress — текущий подшаг добавления сервиса (для UI-статуса).
// Второй результат — есть ли активное добавление с таким именем.
func (s *Syncer) AddProgress(name string) (string, bool) {
	s.addProgressMu.RLock()
	defer s.addProgressMu.RUnlock()
	detail, ok := s.addProgress[name]
	return detail, ok
}

func (s *Syncer) setAddProgress(name, detail string) {
	s.addProgressMu.Lock()
	s.addProgress[name] = detail
	s.addProgressMu.Unlock()
}

func (s *Syncer) clearAddProgress(name string) {
	s.addProgressMu.Lock()
	delete(s.addProgress, name)
	s.addProgressMu.Unlock()
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

	// Снапшот конфига: дальше тело работает с локальными копиями —
	// сохранение настроек или удаление сервиса параллельно синк не тормозит
	// и не даёт гонок (мутации берут Lock).
	safety := s.cfg.SnapshotSafety()
	s.cfg.RLock()
	list := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name)
	ov := s.cfg.Overrides[name]
	// Членство в конфиге: без него синк писал бы AUTO:<name> на роутер
	// для несуществующего сервиса (в память и в файл ничего не попадает —
	// сироты в address-list). Проверки нет у внутренних вызовов (SyncMany
	// берёт список из конфига, SyncOneResult — для существующего сервиса).
	inCfg := false
	for _, p := range s.cfg.Services {
		if p == name {
			inCfg = true
			break
		}
	}
	manageDisabled := s.cfg.Firewall.ManageDisabled
	maxDeleteRatio := s.cfg.Safety.MaxDeleteRatio
	requireConfirmationOver := s.cfg.Safety.RequireConfirmationOver
	warnBroadV4 := s.cfg.Safety.WarnBroadV4
	maxASNPrefixes := s.cfg.Safety.MaxASNPrefixes
	verifySNI := safety.VerifySNIEnabled()
	s.cfg.RUnlock()

	if !inCfg {
		return Result{
			Service:    name,
			DryRun:     dry,
			StartedAt:  start,
			FinishedAt: time.Now(),
			Version:    version.Version,
		}, fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}

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
		s.logFor(name).Warn(msg)
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

	log := s.logFor(name).With("list", list, "comment", comment, "dry_run", dry, "force", force)
	log.Info("запуск синхронизации")
	s.emit("sync_start", map[string]any{"service": name, "list": list, "dry_run": dry})

	// Прогресс для UI: карта очищается через defer при любом выходе.
	defer s.clearProgress(name)
	s.setProgress(name, "чтение записей списка", 5, dry)

	// 1. Все записи списка: управляемые сервиса (list + comment + dynamic=false)
	// и чужие (manual, другие сервисы, dynamic) — чужие учитываются ниже, чтобы
	// не создавать пересекающиеся адреса (PROMPT II.1 — изоляция).
	// ИЗМЕНЕНО: при недоступности Микротика в DRY-RUN режиме продолжаем с пустым списком
	all, err := s.mt().ListEntries(ctx, list)
	if err != nil {
		if dry {
			log.Warn("MikroTik недоступен в режиме DRY-RUN, считаем список пустым", "err", err)
			all = nil
			res.MikroTikDown = true
		} else {
			res.Error = fmt.Sprintf("list entries: %v", err)
			log.Error("не удалось получить записи адресов", "err", err)
			s.notifyError(ctx, name, err)
			return res, fmt.Errorf("list entries: %w", err)
		}
	}
	existing := addresslist.FilterManaged(all, list, comment)
	foreign := addresslist.ForeignEntries(all, list, comment)
	log.Info("записи адресов получены", "managed", len(existing), "foreign", len(foreign))

	s.setProgress(name, "сбор адресов из источников", 15, dry)

	// 2. Сбор IPv4 CIDR (лимит ASN — из стартового снапшота конфига).
	// Детализация: подшаги сбора (DNS/ASN/загрузки) двигают процент в [15,40]
	// и обновляют вторую строку прогресса в UI.
	raw, method, err := s.collectOrStub(ctx, name, ov, maxASNPrefixes, s.collectProgressFunc(name, dry))
	if err != nil {
		res.Error = fmt.Sprintf("collect: %v", err)
		log.Error("не удалось собрать префиксы", "err", err)
		s.notifyError(ctx, name, err)
		return res, fmt.Errorf("collect: %w", err)
	}
	log.Info("собраны исходные префиксы", "count", len(raw), "method", method)

	s.setProgress(name, "валидация адресов", 40, dry)

	// 3. Валидация.
	v := validator.Validator{Safety: safety}
	prefixes, rej, err := v.ValidateWithStats(prefixesToStrings(raw), ov)
	if err != nil {
		res.Error = fmt.Sprintf("validate: %v", err)
		log.Error("валидация не пройдена", "err", err)
		s.notifyError(ctx, name, err)
		return res, fmt.Errorf("validate: %w", err)
	}
	if rej.Total() > 0 {
		res.Rejected = &rej
		log.Warn("валидатор отклонил часть префиксов",
			"ipv6", rej.IPv6, "blocked", rej.Blocked, "too_broad", rej.TooBroad,
			"host_routes", rej.HostRoute, "excluded", rej.Excluded, "not_included", rej.NotIncluded)
	}

	if len(prefixes) == 0 {
		res.Error = "no valid prefixes after validation, aborting sync (fail-closed)"
		log.Error("fail-closed: не собрано ни одного валидного префикса")
		s.notifyError(ctx, name, fmt.Errorf("%s", res.Error))
		return res, fmt.Errorf("no valid prefixes after validation")
	}

	s.setProgress(name, "агрегация префиксов", 50, dry)

	// 4. Агрегация.
	agg, err := aggregator.Aggregate(prefixes)
	if err != nil {
		log.Error("нарушен инвариант агрегации, используем неагрегированный список", "err", err)
		agg = prefixes
	}
	log.Info("префиксы агрегированы", "count", len(agg))

	// 4a. Контроль покрытия: текущие A-записи доменов должны входить в набор,
	// иначе сервис через VPN работать не будет. Проверка best-effort —
	// сбор не блокируется, только WARN и поле в результате.
	// Долгая фаза (резолв десятков доменов): прогресс двигается по доменам
	// в отрезке [50,52], иначе UI замирал бы на «агрегация префиксов · 50%».
	svcDomains := serviceDomains(name, ov, classifier.Classify(name, ov.Method))
	cov := s.checkServiceCoverageProgress(ctx, svcDomains, agg,
		func(done, total int, domain string) {
			s.reportVerificationStep(name, "контроль покрытия", dry,
				coverageProgressLo, coverageProgressHi, done, total, "DNS: "+domain)
		})
	res.Coverage = cov
	if !cov.Complete() {
		log.Warn("часть разрешённых адресов вне собранного набора, сервис может не работать через VPN",
			"checked", cov.Checked, "covered", cov.Covered, "uncovered", cov.Uncovered)
	}

	// 4b. Широкие префиксы: warn-отчёт (кроме cdn — официальные списки
	// широкие по построению, это нормально).
	if !hasMethod(splitMethods(method), "cdn") {
		if broad := validator.BroadPrefixes(agg, warnBroadV4); len(broad) > 0 {
			res.Broad = broad
			log.Warn("в наборе есть широкие префиксы, лишний трафик может идти через VPN",
				"broad", broad)
		}
	}

	// 4c. SNI-проба: доказывает, что адреса обслуживают домены.
	// Best-effort: только варнинги и поле в результате, сбор не блокируется.
	// Шумят только провалы primary-домена (первый в списке — домен самого
	// сервиса); aux-домены (googlevideo.com и т.п.) склонны к ложным
	// срабатываниям и видны лишь в Verification.
	// Прогресс по доменам в отрезке [52,55].
	if verifySNI {
		vds := s.verifyServiceSNIProgress(ctx, svcDomains,
			func(done, total int, domain string) {
				s.reportVerificationStep(name, "SNI-проба", dry,
					sniProgressLo, sniProgressHi, done, total, "TLS: "+domain)
			})
		res.Verification = vds
		if primary := primarySNIFailed(vds); len(primary) > 0 {
			var ips []string
			for _, vd := range primary {
				ips = append(ips, vd.Failed...)
			}
			log.Warn("SNI-проба не прошла, сервис может не работать через VPN",
				"failed_ips", ips)
			if !dry {
				s.maybeAlertSNI(ctx, name, primary)
			}
		}
	}

	desired := make([]string, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, p.String())
	}

	// Пересечения с уже существующими записями MikroTik: чужие сети вычитаются
	// из желаемого набора — дубликаты и перекрытия чужих адресов не создаются.
	s.setProgress(name, "исключение пересечений", 55, dry)
	preExclusion := len(desired)
	desired = applyForeignExclusions(desired, foreign, log)

	// Весь желаемый набор уже покрыт записями списка (ручные записи, другие
	// сервисы, dynamic): сеть находится в нужном состоянии. Чужие записи из-за
	// изоляции (PROMPT II.1) не переиспользуются и не дублируются — это не
	// ошибка, а «уже покрыто»: фиксируем успех с нулём изменений.
	covered := len(desired) == 0 && preExclusion > 0
	if covered {
		log.Info("желаемый набор полностью покрыт существующими записями, изменений не нужно",
			"foreign", len(foreign), "managed", len(existing))
	}
	if len(desired) == 0 && !covered {
		res.Error = "all desired prefixes overlap existing MikroTik entries, aborting sync (fail-closed)"
		log.Error("fail-closed: после исключения пересечений с чужими записями ничего не осталось",
			"foreign", len(foreign))
		s.notifyError(ctx, name, fmt.Errorf("%s", res.Error))
		return res, fmt.Errorf("%s", res.Error)
	}

	s.setProgress(name, "сравнение списков", 60, dry)

	// 5. Diff по нормализованному address.
	var diff *addresslist.Diff
	if covered {
		res.Unchanged = len(existing)
	} else {
		d, derr := addresslist.ComputeDiff(name, list, comment, desired, existing)
		if derr != nil {
			res.Error = fmt.Sprintf("compute diff: %v", derr)
			log.Error("не удалось вычислить diff", "err", derr)
			s.notifyError(ctx, name, derr)
			return res, fmt.Errorf("compute diff: %w", derr)
		}
		diff = d
		res.Added = len(diff.Add)
		res.Removed = len(diff.Remove)
		res.Unchanged = len(diff.Unchanged)
	}

	s.setProgress(name, "план обновлений", 65, dry)

	// 6. Re-enable выключенных управляемых записей (PROMPT II.7: сравнение
	// учитывает disabled; PROMPT I: disabled=false, если не задано иное).
	// firewall.manage_disabled=false — пользователь управляет полем сам:
	// записи остаются выключенными, но не считаются unchanged.
	toUpdate := addresslist.UpdatePlan(desired, existing)
	if manageDisabled {
		res.Updated = len(toUpdate)
	} else {
		if len(toUpdate) > 0 {
			log.Warn("выключенные управляемые записи не тронуты (firewall.manage_disabled=false)",
				"count", len(toUpdate))
		}
		toUpdate = nil
	}
	if covered {
		log.Info("изменений нет: желаемый набор покрыт существующими записями")
	} else {
		log.Info("вычислен diff",
			"add", res.Added, "remove", res.Removed, "unchanged", res.Unchanged, "update", res.Updated)
	}

	s.setProgress(name, "проверка безопасности", 70, dry)

	// 7. SAFE-DIFF.
	if !covered && len(existing) > 0 && !force {
		if err := addresslist.CheckDeletion(len(existing), len(diff.Remove), addresslist.SafetyParams{
			MaxDeleteRatio:          maxDeleteRatio,
			RequireConfirmationOver: requireConfirmationOver,
		}); err != nil {
			res.Error = err.Error()
			log.Error("проверка безопасности заблокировала синхронизацию", "err", err)
			s.notifyError(ctx, name, err)
			return res, err
		}
	}

	var toRemove []addresslist.Entry
	var toAdd []addresslist.Entry
	if !covered {
		toRemove = addresslist.RemovalPlan(desired, existing)
		toAdd = make([]addresslist.Entry, 0, len(diff.Add))
		for _, a := range diff.Add {
			toAdd = append(toAdd, addresslist.Entry{Address: a})
		}
	}

	s.setProgress(name, "подготовка применений", 75, dry)

	// 8. Dry run — только diff, без изменения состояния (bbolt в том числе).
	if dry {
		log.Info("DRY-RUN завершён", "add", res.Added, "remove", res.Removed, "update", res.Updated)
		res.Success = true
		res.FinishedAt = time.Now()
		res.Duration = res.FinishedAt.Sub(start).String()
		return res, nil
	}

	snapshotID := ""
	if !covered {
		s.setProgress(name, "создание снапшота", 80, dry)

		// 9. Снапшот перед применением (PROMPT II.5, IV.5).
		snapshotID = s.createSnapshot(ctx, name, existing)
		if snapshotID == "" {
			log.Warn("не удалось создать снапшот, продолжаем без возможности отката")
		} else {
			log.Info("снапшот создан", "snapshot_id", snapshotID)
		}

		s.setProgress(name, "применение изменений", 85, dry)

		// 10. Транзакционное применение (add → update → remove).
		tx := mikrotik.NewTransaction(s.mt(), s.cfg, s.cache, name, log)
		if snapshotID != "" {
			tx.SetSnapshotID(snapshotID)
		}

		if err := tx.Apply(ctx, toAdd, toUpdate, toRemove); err != nil {
			res.Error = err.Error()
			log.Error("транзакция не выполнена", "err", err)
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
	} else {
		log.Info("применять нечего: желаемый набор покрыт существующими записями")
	}

	s.setProgress(name, "завершение", 95, dry)

	// 11. Финализация.
	res.Success = true
	s.ClearDegraded(name)
	res.FinishedAt = time.Now()
	res.Duration = res.FinishedAt.Sub(start).String()
	elapsed := time.Since(start)

	logRes := logging.SyncResult{
		Service:    res.Service,
		ServiceID:  s.ServiceID(name),
		Success:    res.Success,
		Added:      res.Added,
		Removed:    res.Removed,
		Unchanged:  res.Unchanged,
		Updated:    res.Updated,
		Error:      res.Error,
		DryRun:     res.DryRun,
		StartedAt:  res.StartedAt,
		FinishedAt: res.FinishedAt,
		Duration:   res.Duration,
	}
	logging.LogSyncResult(log, logRes, elapsed)

	var msg string
	if covered {
		msg = fmt.Sprintf("✅ %s (список %s): все адреса уже покрыты записями списка — изменений не требуется",
			name, list)
	} else {
		msg = fmt.Sprintf("✅ %s (список %s): +%d добавлено, −%d удалено, %d без изменений",
			name, list, res.Added, res.Removed, res.Unchanged)
		if res.Updated > 0 {
			msg += fmt.Sprintf(", включено %d", res.Updated)
		}
	}
	msg += fmt.Sprintf(" (%s)", notifier.FormatDur(elapsed))
	if err := s.getNotify().Send(ctx, msg); err != nil {
		log.Warn("сводка синхронизации в telegram не доставлена", "err", err)
	}
	s.audit.Log(EntryToAudit(res))
	s.emit("sync_done", map[string]any{
		"service": name, "list": list,
		"added": res.Added, "removed": res.Removed, "unchanged": res.Unchanged,
		"updated": res.Updated,
	})

	// Финализация: rec собирается без блокировок, запись истории (bbolt I/O
	// под своим мьютексом) — ВНЕ serviceMu, иначе каждый синк держит все
	// операции с сервисами. lastSync под serviceMu — коротко.
	rec := history.Record{
		Service:    name,
		ServiceID:  s.ServiceID(name),
		Method:     method,
		Status:     "success",
		Collected:  len(raw),
		Aggregated: len(agg),
		Added:      res.Added,
		Removed:    res.Removed,
		Unchanged:  res.Unchanged,
		DurationMS: elapsed.Milliseconds(),
	}
	// Сводка качества для UI-health (персист; старые записи без неё).
	if res.Coverage != nil {
		rec.CovChecked = res.Coverage.Checked
		rec.CovCovered = res.Coverage.Covered
	}
	if len(primarySNIFailed(res.Verification)) > 0 {
		rec.SNIPrimaryFailed = true
	}
	if res.Rejected != nil {
		rec.RejectedTotal = res.Rejected.Total()
	}
	s.history.Add(rec)
	s.serviceMu.Lock()
	s.lastSync = time.Now()
	s.serviceMu.Unlock()

	log.Info("синхронизация успешно завершена", "duration", elapsed)
	return res, nil
}

func (s *Syncer) SyncMany(ctx context.Context, services []string, dry bool, force bool) error {
	if len(services) == 0 {
		return nil
	}

	start := time.Now()
	s.audit.LogSyncStart(services)
	mode := "обычный режим"
	if dry {
		mode = "пробный запуск (изменения не применяются)"
	}
	if force {
		mode += ", принудительно"
	}
	_ = s.getNotify().SyncStart(ctx, services, "вручную", mode)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	results := make([]Result, 0, len(services))

	maxConcurrent := func() int {
		s.cfg.RLock()
		defer s.cfg.RUnlock()
		return s.cfg.Scheduler.MaxConcurrent
	}()
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
					s.log.Error("паника в горутине синхронизации", "service", n, "service_id", s.ServiceID(n), "panic", fmt.Sprintf("%v", r))
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
			Service: r.Service, ServiceID: s.ServiceID(r.Service),
			Success: r.Success, Added: r.Added, Removed: r.Removed,
			Unchanged: r.Unchanged, Updated: r.Updated, Error: r.Error, DryRun: r.DryRun,
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
			Updated:    r.Updated,
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
		Updated:    res.Updated,
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
	s.log.Info("запрошена перезагрузка планировщика")
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
		s.log.Warn("перезапуск telegram пропущен: в этом режиме (web) бот не запущен, перезапустите демон для применения настроек telegram")
		return nil
	}

	s.log.Info("перезапуск telegram-бота с новой конфигурацией")
	return fn()
}

// TelegramRestartAvailable сообщает, обслуживается ли бот в этом режиме:
// в web-режиме бота нет, и смена telegram.* требует перезапуска демона.
func (s *Syncer) TelegramRestartAvailable() bool {
	s.telegramRestartMu.Lock()
	defer s.telegramRestartMu.Unlock()
	return s.telegramRestartFunc != nil
}

func (s *Syncer) GetLogs(ctx context.Context, limit int, match func(map[string]any) bool) ([]map[string]any, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	logFile := func() string {
		s.cfg.RLock()
		defer s.cfg.RUnlock()
		return s.cfg.Logging.File
	}()
	if logFile == "" {
		return []map[string]any{}, false, nil
	}
	// Фильтр применяется при чтении с конца файла (а не после жёсткого
	// лимита): старые записи при активных фильтрах не теряются.
	entries, truncated, err := logging.TailLogFileFiltered(logFile, limit, match)
	if err != nil {
		s.log.Warn("не удалось прочитать файл логов", "err", err, "limit", limit)
		return []map[string]any{}, false, nil
	}
	return entries, truncated, nil
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
			"updated":   res.Updated,
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
