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

func New(cfg *config.Config, s *core.Syncer, n notifier.Notifier, l *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, syncer: s, notify: n, log: l}
}

func (s *Scheduler) Start() {
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
	s.log.Info("scheduler started", "max_concurrent", limit, "timezone", loc.String())
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

func (s *Scheduler) run(parent context.Context, service, spec string) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	start := time.Now()
	s.notify.SyncStart(ctx, []string{service}, "schedule", spec)
	r, err := s.syncer.SyncOneResult(ctx, service, false)
	if err != nil {
		r.Error = err.Error()
		s.notify.Error(ctx, service, err)
	}
	s.notify.SyncDone(ctx, []notifier.SyncResult{r}, time.Since(start), false)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.stopLocked(ctx)
	s.startLocked()
	s.log.Info("scheduler reloaded")
	return nil
}

// ValidSpec reports whether a schedule is one of the supported special values,
// a 5-field cron expression, or a supported human-readable expression.
func ValidSpec(spec string) bool {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if spec == "manual" || spec == "disabled" || spec == "inherit" {
		return true
	}
	if _, ok := toCron(spec); ok {
		return true
	}
	_, ok := interval(spec)
	return ok
}

func toCron(spec string) (string, bool) {
	if len(strings.Fields(spec)) == 5 {
		return spec, true
	}
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
