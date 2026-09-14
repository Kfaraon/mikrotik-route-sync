package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"github.com/robfig/cron/v3"
	"github.com/example/mikrotik-route-sync/internal/config"
	"github.com/example/mikrotik-route-sync/internal/core"
	"github.com/example/mikrotik-route-sync/internal/notifier"
)

type Scheduler struct {
	cfg *config.Config; syncer *core.Syncer; notifier notifier.Notifier; log *slog.Logger
	cron *cron.Cron; tickers []*time.Ticker; stopCh chan struct{}
	wg sync.WaitGroup; mtx sync.Mutex; busy map[string]bool
}

func New(cfg *config.Config, syncer *core.Syncer, n notifier.Notifier, log *slog.Logger) *Scheduler {
	loc, _ := time.LoadLocation(cfg.Timezone)
	return &Scheduler{cfg: cfg, syncer: syncer, notifier: n, log: log, cron: cron.New(cron.WithLocation(loc)), stopCh: make(chan struct{}), busy: map[string]bool{}}
}

func (s *Scheduler) Start() {
	for _, svc := range s.cfg.Services {
		sched := s.cfg.ScheduleFor(svc)
		if sched == "manual" || sched == "disabled" { continue }
		
		if spec, ok := intervalToCron(sched); ok {
			svc := svc
			if _, err := s.cron.AddFunc(spec, func() { s.run(svc, "schedule", sched) }); err != nil { s.log.Error("cron add failed", "service", svc, "err", err) }
			continue
		}
		if d, ok := parseInterval(sched); ok {
			svc := svc; t := time.NewTicker(d); s.tickers = append(s.tickers, t); s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				for { select { case <-t.C: s.run(svc, "schedule", sched); case <-s.stopCh: return } }
			}()
			continue
		}
		s.log.Warn("unknown schedule format", "service", svc, "schedule", sched)
	}

	if s.cfg.Telegram.Enabled && s.cfg.Telegram.WeeklyReport != "" {
		if spec, ok := intervalToCron(s.cfg.Telegram.WeeklyReport); ok {
			s.cron.AddFunc(spec, s.weeklyReport)
		}
	}
	s.cron.Start()
}

func (s *Scheduler) Stop(ctx context.Context) {
	close(s.stopCh)
	for _, t := range s.tickers { t.Stop() }
	done := s.cron.Stop(); select { case <-done.Done(): case <-ctx.Done(): }
	s.wg.Wait()
}

func (s *Scheduler) run(service, trigger, schedule string) {
	s.mtx.Lock()
	if s.busy[service] { s.mtx.Unlock(); s.log.Warn("service already running, skip", "service", service); return }
	s.busy[service] = true; s.mtx.Unlock()
	defer func() { s.mtx.Lock(); s.busy[service] = false; s.mtx.Unlock() }()

	s.log.Info("scheduled sync start", "service", service, "trigger", trigger)
	start := time.Now()
	s.notifier.SyncStart(context.Background(), []string{service}, trigger, schedule)
	res, err := s.syncer.SyncOneResult(context.Background(), service, false)
	if err != nil { s.notifier.Error(context.Background(), service, err); return }
	s.notifier.SyncDone(context.Background(), []notifier.SyncResult{res}, time.Since(start).Round(time.Millisecond).String(), false)
}

func (s *Scheduler) weeklyReport() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second); defer cancel()
	snap, err := s.syncer.Snapshot(ctx)
	if err != nil { return }
	var b strings.Builder; b.WriteString("📅 *Weekly Report*\n\n"); total := 0
	for _, svc := range s.cfg.Services { fmt.Fprintf(&b, "• %s: %d\n", svc, snap[svc]); total += snap[svc] }
	fmt.Fprintf(&b, "\nTotal: %d routes", total)
	s.notifier.Report(ctx, s.cfg, b.String())
}

func intervalToCron(spec string) (string, bool) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if strings.HasPrefix(spec, "daily at ") {
		t := strings.TrimPrefix(spec, "daily at "); parts := strings.Split(t, ":")
		if len(parts) == 2 {
			h, _ := strconv.Atoi(parts[0]); m, _ := strconv.Atoi(parts[1])
			if h >= 0 && h <= 23 && m >= 0 && m <= 59 { return fmt.Sprintf("%d %d * * *", m, h), true }
		}
	}
	if strings.HasPrefix(spec, "weekly on ") {
		rest := strings.TrimPrefix(spec, "weekly on "); parts := strings.SplitN(rest, " at ", 2)
		if len(parts) == 2 {
			dow := weekdayToCron(parts[0]); t := strings.Split(parts[1], ":")
			if dow != "" && len(t) == 2 {
				h, _ := strconv.Atoi(t[0]); m, _ := strconv.Atoi(t[1])
				return fmt.Sprintf("%d %d * * %s", m, h, dow), true
			}
		}
	}
	if len(strings.Fields(spec)) == 5 { return spec, true }
	return "", false
}

// parseInterval использует нативный time.ParseDuration для надёжности ("every 1h30m", "every 500ms")
func parseInterval(spec string) (time.Duration, bool) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if !strings.HasPrefix(spec, "every ") { return 0, false }
	raw := strings.TrimSpace(strings.TrimPrefix(spec, "every "))
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 { return 0, false }
	return d, true
}

func weekdayToCron(s string) string {
	switch strings.TrimSpace(s) {
	case "sunday", "sun": return "0"; case "monday", "mon": return "1"; case "tuesday", "tue": return "2"
	case "wednesday", "wed": return "3"; case "thursday", "thu": return "4"; case "friday", "fri": return "5"; case "saturday", "sat": return "6"
	}
	return ""
}