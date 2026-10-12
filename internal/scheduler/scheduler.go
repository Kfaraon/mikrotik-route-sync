package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	cfg    *config.Config
	syncer *core.Syncer
	notify notifier.Notifier
	log    *slog.Logger

	mu      sync.Mutex
	cron    *cron.Cron
	cancel  context.CancelFunc // отменяет планировочную машинерию (cron/heap), не сами синки
	wg      sync.WaitGroup
	sem     chan struct{}
	started bool

	// basePub/baseCancel — базовый контекст тел синков. В отличие от s.cancel
	// он НЕ отменяется при Reload: иначе плановая перезагрузка (reload_interval,
	// SIGHUP, смена расписания) обрывала идущий синк на середине транзакции
	// («rate limit wait: context canceled»). Отменяется только в Stop.
	// basePub — atomic: dispatch читает его без s.mu.
	basePub    atomic.Value // syncBase
	baseCancel context.CancelFunc

	// semPub — публикация текущего семафора для dispatch без s.mu: иначе
	// Reload (держит s.mu) → stopLocked (ждёт cron.Stop) → dispatch (ждёт
	// s.mu) образовывали инверсию блокировок со ступором до 10 с.
	semPub atomic.Value // chan struct{}

	// runMu/runActive синхронизируют wg.Add (dispatch) с wg.Wait (Stop)
	// отдельно от s.mu: Add при нулевом счётчике, стартовавший после Wait,
	// — известная гонка ("WaitGroup misuse"). s.mu для этого не годится:
	// его держит Reload, ожидая завершения cron-задач.
	runMu     sync.Mutex
	runActive bool
	// Для interval-based расписаний (every N): единый таймер + куча
	// вместо горутины на сервис. Это предотвращает утечку горутин
	// при большом количестве сервисов.
	intervalHeap *intervalHeap
}

// syncBase — базовый контекст тел синков. invalid помечает устаревшее
// поколение (после Stop): dispatch, прочитавший такой снапшот, не запускает
// синк, а снимает место в wg.
type syncBase struct {
	ctx     context.Context
	invalid bool
}

func NewScheduler(cfg *config.Config, s *core.Syncer) *Scheduler {
	log := slog.Default()
	return &Scheduler{
		cfg:    cfg,
		syncer: s,
		notify: notifier.FromConfig(cfg.Telegram, cfg.Timezone, log),
		log:    log,
	}
}

func New(cfg *config.Config, s *core.Syncer, n notifier.Notifier, l *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, syncer: s, notify: n, log: l}
}

func (s *Scheduler) Start(ctx context.Context) error {
	s.StartNow()
	<-ctx.Done()
	s.Stop(context.Background())
	return nil
}

// ИЗМЕНЕНО: передаём quiet=false при первичном запуске
func (s *Scheduler) StartNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.startLocked(false)
	s.started = true
}

// ИЗМЕНЕНО: добавлен параметр quiet bool
func (s *Scheduler) startLocked(quiet bool) {
	// Снапшот читаемого: дальше кроны/куча без блокировок.
	s.cfg.RLock()
	timezone := s.cfg.Timezone
	limit := s.cfg.Scheduler.MaxConcurrent
	parallel := s.cfg.Scheduler.Parallel
	purgeSpec := s.cfg.Scheduler.CachePurge
	reloadInterval := s.cfg.Scheduler.ReloadInterval
	s.cfg.RUnlock()
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
		s.log.Warn("некорректная таймзона, используем UTC", "timezone", timezone)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	// Базовый контекст тел синков переживает Reload (см. поле basePub):
	// иначе смена расписания каждую минуту рвала бы идущий синк.
	// Переиспользуем прежний (валидный) — Stop инвалидирует его.
	if prev, ok := s.basePub.Load().(syncBase); !ok || prev.invalid {
		bctx, bcancel := context.WithCancel(context.Background())
		s.basePub.Store(syncBase{ctx: bctx})
		s.baseCancel = bcancel
	}

	// Разрешаем dispatch откладывать места в wg (до wg.Wait в Stop).
	s.runMu.Lock()
	s.runActive = true
	s.runMu.Unlock()

	if limit <= 0 {
		limit = 1
	}
	if !parallel {
		limit = 1
	}
	s.sem = make(chan struct{}, limit)
	s.semPub.Store(s.sem)
	s.cron = cron.New(cron.WithLocation(loc))

	// Инициализируем кучу для interval-based сервисов
	s.intervalHeap = newIntervalHeap()

	s.cfg.RLock()
	for _, svc := range s.cfg.Services {
		s.addLocked(ctx, svc, s.cfg.ScheduleFor(svc))
	}
	s.cfg.RUnlock()
	s.cron.Start()

	// Запускаем единую горутину для interval-based сервисов
	s.wg.Add(1)
	go s.intervalRunner(ctx)

	if d, ok := purgeInterval(purgeSpec); ok {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := s.syncer.PurgeCache(); err != nil {
						s.log.Warn("не удалось очистить кэш", "err", err)
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	if d := reloadInterval.Duration(); d > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := s.ReloadQuiet(); err != nil {
						s.log.Warn("не удалось перезагрузить по расписанию", "err", err)
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// ИЗМЕНЕНО: логируем только при тихом запуске (первый запуск или ручной Reload)
	if !quiet {
		s.log.Info("планировщик запущен", "max_concurrent", limit, "timezone", loc.String())
	}
}

func purgeInterval(spec string) (time.Duration, bool) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if spec == "" {
		return 0, false
	}
	if strings.HasPrefix(spec, "every ") {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(spec, "every ")))
		if err == nil && d > 0 {
			return d, true
		}
	}
	d, err := time.ParseDuration(spec)
	if err == nil && d > 0 {
		return d, true
	}
	return 0, false
}

func (s *Scheduler) addLocked(ctx context.Context, service, spec string) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if spec == "" || spec == "manual" || spec == "disabled" || spec == "inherit" {
		return
	}
	if c, ok := toCron(spec); ok {
		svc, original := service, spec
		_, err := s.cron.AddFunc(c, func() { s.dispatch(ctx, svc, original) })
		if err != nil {
			s.log.Error("не удалось добавить расписание", "service", service, "schedule", spec, "err", err)
		}
		return
	}
	if d, ok := interval(spec); ok {
		svc, original := service, spec
		// Добавляем в кучу вместо создания горутины на сервис.
		// Первый запуск — через интервал (как было в Ticker).
		s.intervalHeap.Push(intervalItem{
			service:  svc,
			spec:     original,
			interval: d,
			nextRun:  time.Now().Add(d),
		})
		return
	}
	s.log.Warn("неподдерживаемое расписание", "service", service, "schedule", spec)
}

func (s *Scheduler) dispatch(schedCtx context.Context, service, spec string) {
	// wg.Add(1) — до захвата семафора, под runMu: иначе Stop() успевал
	// вызвать wg.Wait() (при уже отменённом ctx), а горутина после этого
	// делала Add на счётчике с нулём — гонка "WaitGroup misuse".
	// base — контекст тела синка: переживает Reload, отменяется только Stop.
	// При невалидном поколении (после Stop) синк не запускаем.
	base, _ := s.basePub.Load().(syncBase)

	s.runMu.Lock()
	if !s.runActive || base.invalid || base.ctx == nil {
		s.runMu.Unlock()
		return
	}
	s.wg.Add(1)
	s.runMu.Unlock()
	started := false
	defer func() {
		if !started {
			s.wg.Done() // не запустились (ctx/timeout) — снимаем место
		}
	}()

	// Снапшот семафора из atomic.Value: без захвата s.mu (см. semPub).
	semRaw := s.semPub.Load()
	var sem chan struct{}
	if semRaw != nil {
		sem = semRaw.(chan struct{})
	}
	select {
	case sem <- struct{}{}:
		started = true
		go func(gate chan struct{}) {
			defer s.wg.Done()
			defer func() { <-gate }()
			s.run(base.ctx, service, spec)
		}(sem)
	case <-schedCtx.Done():
		s.log.Warn("синхронизация по расписанию пропущена: контекст отменён", "service", service, "schedule", spec)
	case <-time.After(5 * time.Minute):
		s.log.Warn("синхронизация по расписанию пропущена: достигнут лимит параллельных", "service", service, "schedule", spec)
	}
}

func (s *Scheduler) run(parent context.Context, service, spec string) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("паника при синхронизации по расписанию", "service", service, "schedule", spec, "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	s.log.Info("синхронизация по расписанию запущена", "service", service, "schedule", spec)
	res := s.syncer.SyncOneResult(ctx, service, false)
	if !res.Success {
		s.log.Warn("синхронизация по расписанию не удалась", "service", service, "error", res.Error)
	}
}

func (s *Scheduler) stopLocked(ctx context.Context) {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.cron != nil {
		done := s.cron.Stop()
		select {
		case <-done.Done():
		case <-ctx.Done():
		}
		s.cron = nil
	}
}

func (s *Scheduler) Stop(ctx context.Context) {
	// Запрещаем новые wg.Add(+dispatch) до stopLocked: dispatch получит
	// отменённый ctx и не будет запускаться; wg.Wait не гонится с Add.
	s.runMu.Lock()
	s.runActive = false
	s.runMu.Unlock()

	s.mu.Lock()
	s.stopLocked(ctx)
	// Инвалидируем базовый контекст синков и отменяем его: это единственное
	// место, где идущий синк обрывается (Reload его больше не трогает).
	if bc := s.baseCancel; bc != nil {
		s.basePub.Store(syncBase{invalid: true})
		bc()
		s.baseCancel = nil
	}
	s.started = false
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *Scheduler) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return fmt.Errorf("scheduler is not started")
	}
	return s.reloadLocked(false)
}

func (s *Scheduler) ReloadQuiet() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return fmt.Errorf("scheduler is not started")
	}
	return s.reloadLocked(true)
}

// ИЗМЕНЕНО: передаём quiet в startLocked
func (s *Scheduler) reloadLocked(quiet bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.stopLocked(ctx)
	s.startLocked(quiet)
	if quiet {
		s.log.Debug("планировщик тихо перезагружен")
	} else {
		s.log.Info("планировщик перезагружен")
	}
	return nil
}

// intervalHeap — min-heap для управления следующими запусками interval-based сервисов.
// Позволяет единому таймеру знать, когда ближайший запуск.
type intervalHeap struct {
	mu    sync.Mutex
	items []intervalItem
	// notify будит intervalRunner при добавлении/изменении элемента: без него
	// пустая куча опрашивалась busy-poll'ом каждые 100 мс (O3).
	notify chan struct{}
}

type intervalItem struct {
	service  string
	spec     string
	interval time.Duration
	nextRun  time.Time
}

func newIntervalHeap() *intervalHeap {
	return &intervalHeap{items: make([]intervalItem, 0), notify: make(chan struct{}, 1)}
}

// signal — неблокирующее пробуждение runner'а (канал буферизован на 1).
func (h *intervalHeap) signal() {
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

func (h *intervalHeap) Push(item intervalItem) {
	h.mu.Lock()
	h.items = append(h.items, item)
	h.siftUp(len(h.items) - 1)
	h.mu.Unlock()
	h.signal()
}

func (h *intervalHeap) Pop() (intervalItem, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.items) == 0 {
		return intervalItem{}, false
	}
	item := h.items[0]
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	if len(h.items) > 0 {
		h.items[0] = last
		h.siftDown(0)
	}
	return item, true
}

func (h *intervalHeap) Peek() (intervalItem, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.items) == 0 {
		return intervalItem{}, false
	}
	return h.items[0], true
}

func (h *intervalHeap) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.items)
}

func (h *intervalHeap) Remove(service string) {
	h.mu.Lock()
	for i, item := range h.items {
		if item.service == service {
			h.items[i] = h.items[len(h.items)-1]
			h.items = h.items[:len(h.items)-1]
			if i < len(h.items) {
				h.siftDown(i)
				h.siftUp(i)
			}
			break
		}
	}
	h.mu.Unlock()
	// Куча изменилась — возможно, освободился слот для других сервисов.
	h.signal()
}

func (h *intervalHeap) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if h.items[i].nextRun.After(h.items[parent].nextRun) {
			break
		}
		h.items[i], h.items[parent] = h.items[parent], h.items[i]
		i = parent
	}
}

func (h *intervalHeap) siftDown(i int) {
	for {
		smallest := i
		left := 2*i + 1
		right := 2*i + 2
		if left < len(h.items) && h.items[left].nextRun.Before(h.items[smallest].nextRun) {
			smallest = left
		}
		if right < len(h.items) && h.items[right].nextRun.Before(h.items[smallest].nextRun) {
			smallest = right
		}
		if smallest == i {
			break
		}
		h.items[i], h.items[smallest] = h.items[smallest], h.items[i]
		i = smallest
	}
}

// intervalRunner — единая горутина, которая просыпается на ближайшем nextRun,
// диспетчерит сервис и перепланирует его.
func (s *Scheduler) intervalRunner(ctx context.Context) {
	defer s.wg.Done()
	for {
		var nextRun time.Time
		var hasItem bool
		var item intervalItem

		// Снапшот указателя кучи: startLocked пересоздаёт её при Reload.
		s.mu.Lock()
		heap := s.intervalHeap
		s.mu.Unlock()

		// Берём ближайший элемент
		heap.mu.Lock()
		if len(heap.items) > 0 {
			item = heap.items[0]
			nextRun = item.nextRun
			hasItem = true
		}
		heap.mu.Unlock()

		if !hasItem {
			// Пустая куча — спим на сигнале о добавлении (O3: без busy-poll).
			select {
			case <-ctx.Done():
				return
			case <-heap.notify:
				continue
			}
		}

		now := time.Now()
		wait := nextRun.Sub(now)
		if wait <= 0 {
			// Время вышло — извлекаем, диспетчерим, перепланируем
			popped, ok := heap.Pop()
			if !ok {
				continue
			}
			s.dispatch(ctx, popped.service, popped.spec)
			// Перепланируем на следующий интервал
			popped.nextRun = time.Now().Add(popped.interval)
			heap.Push(popped)
			continue
		}

		// Ждём до следующего запуска или отмены
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// Таймер сработал, цикл повторится
		}
	}
}

func ValidSpec(spec string) bool {
	if err := Validate(spec); err == nil {
		if s := strings.TrimSpace(strings.ToLower(spec)); s == "every" || s == "daily" || s == "weekly" {
			return false
		}
		return true
	}
	return false
}

func toCron(spec string) (string, bool) {
	if strings.HasPrefix(spec, "daily at ") {
		h, m, ok := clock(strings.TrimPrefix(spec, "daily at "))
		if ok {
			return fmt.Sprintf("%d %d * * *", m, h), true
		}
	}
	if strings.HasPrefix(spec, "weekly on ") {
		parts := strings.SplitN(strings.TrimPrefix(spec, "weekly on "), " at ", 2)
		if len(parts) == 2 {
			dow := weekday(parts[0])
			h, m, ok := clock(parts[1])
			if dow != "" && ok {
				return fmt.Sprintf("%d %d * * %s", m, h, dow), true
			}
		}
	}
	if len(strings.Fields(spec)) == 5 {
		return spec, true
	}
	return "", false
}

func interval(spec string) (time.Duration, bool) {
	if !strings.HasPrefix(spec, "every ") {
		return 0, false
	}
	d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(spec, "every ")))
	return d, err == nil && d > 0
}

func clock(v string) (int, int, bool) {
	p := strings.Split(v, ":")
	if len(p) != 2 {
		return 0, 0, false
	}
	h, e1 := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	return h, m, e1 == nil && e2 == nil && h >= 0 && h < 24 && m >= 0 && m < 60
}

func weekday(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sun", "sunday":
		return "0"
	case "mon", "monday":
		return "1"
	case "tue", "tuesday":
		return "2"
	case "wed", "wednesday":
		return "3"
	case "thu", "thursday":
		return "4"
	case "fri", "friday":
		return "5"
	case "sat", "saturday":
		return "6"
	}
	return ""
}
