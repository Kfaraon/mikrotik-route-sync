package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
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
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	sem     chan struct{}
	started bool

	// Для interval-based расписаний (every N): единый таймер + куча
	// вместо горутины на сервис. Это предотвращает утечку горутин
	// при большом количестве сервисов.
	intervalHeap *intervalHeap
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
		s.log.Warn("invalid timezone, using UTC", "timezone", timezone)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	if limit <= 0 {
		limit = 1
	}
	if !parallel {
		limit = 1
	}
	s.sem = make(chan struct{}, limit)
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
						s.log.Warn("cache purge failed", "err", err)
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
						s.log.Warn("scheduled reload failed", "err", err)
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// ИЗМЕНЕНО: логируем только при тихом запуске (первый запуск или ручной Reload)
	if !quiet {
		s.log.Info("scheduler started", "max_concurrent", limit, "timezone", loc.String())
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
			s.log.Error("schedule add failed", "service", service, "schedule", spec, "err", err)
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
	s.log.Warn("unsupported schedule", "service", service, "schedule", spec)
}

func (s *Scheduler) dispatch(ctx context.Context, service, spec string) {
	// Снапшот семафора: startLocked пересоздаёт его под s.mu при Reload.
	s.mu.Lock()
	sem := s.sem
	s.mu.Unlock()
	select {
	case sem <- struct{}{}:
		s.wg.Add(1)
		go func(gate chan struct{}) {
			defer s.wg.Done()
			defer func() { <-gate }()
			s.run(ctx, service, spec)
		}(sem)
	case <-ctx.Done():
		s.log.Warn("scheduled sync skipped: context cancelled", "service", service, "schedule", spec)
	case <-time.After(5 * time.Minute):
		s.log.Warn("scheduled sync skipped: concurrency limit reached", "service", service, "schedule", spec)
	}
}

func (s *Scheduler) run(parent context.Context, service, spec string) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("scheduled sync panicked", "service", service, "schedule", spec, "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	s.log.Info("scheduled sync triggered", "service", service, "schedule", spec)
	res := s.syncer.SyncOneResult(ctx, service, false)
	if !res.Success {
		s.log.Warn("scheduled sync failed", "service", service, "error", res.Error)
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
	s.mu.Lock()
	s.stopLocked(ctx)
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
		s.log.Debug("scheduler quietly reloaded")
	} else {
		s.log.Info("scheduler reloaded")
	}
	return nil
}

// intervalHeap — min-heap для управления следующими запусками interval-based сервисов.
// Позволяет единому таймеру знать, когда ближайший запуск.
type intervalHeap struct {
	mu    sync.Mutex
	items []intervalItem
}

type intervalItem struct {
	service  string
	spec     string
	interval time.Duration
	nextRun  time.Time
}

func newIntervalHeap() *intervalHeap {
	h := &intervalHeap{items: make([]intervalItem, 0)}
	return h
}

func (h *intervalHeap) Push(item intervalItem) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = append(h.items, item)
	h.siftUp(len(h.items) - 1)
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
	defer h.mu.Unlock()
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
			// Нет интервальных сервисов — ждём добавления или отмены
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
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
