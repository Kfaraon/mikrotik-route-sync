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
}

// NewScheduler создаёт планировщик. Notifier и logger выводятся из конфига,
// что позволяет запускать планировщик из CLI одной строкой.
func NewScheduler(cfg *config.Config, s *core.Syncer) *Scheduler {
	log := slog.Default()
	return &Scheduler{
		cfg:    cfg,
		syncer: s,
		notify: notifier.FromConfig(cfg.Telegram, log),
		log:    log,
	}
}

// New создаёт планировщик с явными зависимостями (DI, PROMPT X.10.2).
func New(cfg *config.Config, s *core.Syncer, n notifier.Notifier, l *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, syncer: s, notify: n, log: l}
}

// Start запускает планировщик и блокирует до отмены ctx.
func (s *Scheduler) Start(ctx context.Context) error {
	s.StartNow()
	<-ctx.Done()
	s.Stop(context.Background())
	return nil
}

// StartNow неблокируемо запускает планировщик.
func (s *Scheduler) StartNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.startLocked()
	s.started = true
}

func (s *Scheduler) startLocked() {
	loc, err := time.LoadLocation(s.cfg.Timezone)
	if err != nil {
		loc = time.UTC
		s.log.Warn("invalid timezone, using UTC", "timezone", s.cfg.Timezone)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	limit := s.cfg.Scheduler.MaxConcurrent
	if limit <= 0 {
		limit = 1
	}
	if !s.cfg.Scheduler.Parallel {
		limit = 1
	}
	s.sem = make(chan struct{}, limit)
	s.cron = cron.New(cron.WithLocation(loc))
	for _, svc := range s.cfg.Services {
		s.addLocked(ctx, svc, s.cfg.ScheduleFor(svc))
	}
	s.cron.Start()

	// Периодическая очистка кэша: scheduler.cache_purge (по умолчанию every 1h).
	if d, ok := purgeInterval(s.cfg.Scheduler.CachePurge); ok {
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

	// Периодический перечитающий перезагрузк расписаний: scheduler.reload_interval.
	if d := s.cfg.Scheduler.ReloadInterval.Duration(); d > 0 {
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

	s.log.Info("scheduler started", "max_concurrent", limit, "timezone", loc.String())
}

// purgeInterval разбирает "every 1h" из scheduler.cache_purge.
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
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					s.dispatch(ctx, svc, original)
				case <-ctx.Done():
					return
				}
			}
		}()
		return
	}
	s.log.Warn("unsupported schedule", "service", service, "schedule", spec)
}

func (s *Scheduler) dispatch(ctx context.Context, service, spec string) {
	// Capture the semaphore for this scheduler generation. Reload replaces
	// s.sem; releasing through the field could otherwise target the new channel.
	sem := s.sem
	select {
	case sem <- struct{}{}:
		go func(gate chan struct{}) {
			defer func() { <-gate }()
			s.run(ctx, service, spec)
		}(sem)
	default:
		s.log.Warn("scheduled sync skipped: concurrency limit reached", "service", service, "schedule", spec)
	}
}

// run выполняет одну синхронизацию по расписанию.
// Уведомления о старте/результате/ошибке шлёт сам SyncService.
func (s *Scheduler) run(parent context.Context, service, spec string) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	s.log.Info("scheduled sync triggered", "service", service, "schedule", spec)
	s.syncer.SyncOneResult(ctx, service, false)
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

// Reload rebuilds all schedules from the current in-memory config. It is safe
// to call after config changes made by the web UI or another controller.
func (s *Scheduler) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return fmt.Errorf("scheduler is not started")
	}
	return s.reloadLocked(false)
}

// ReloadQuiet перестраивает расписания без Info-логирования (периодическая
// перезагрузка по scheduler.reload_interval не должна шуметь в логах).
func (s *Scheduler) ReloadQuiet() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return fmt.Errorf("scheduler is not started")
	}
	return s.reloadLocked(true)
}

func (s *Scheduler) reloadLocked(quiet bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.stopLocked(ctx)
	s.startLocked()
	if quiet {
		s.log.Debug("scheduler quietly reloaded")
	} else {
		s.log.Info("scheduler reloaded")
	}
	return nil
}

// ValidSpec reports whether a schedule is one of the supported special values,
// a 5-field cron expression, or a supported human-readable expression.
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
	// Human-readable forms first: their word count can collide with 5-field cron.
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
