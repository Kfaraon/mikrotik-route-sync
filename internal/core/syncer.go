package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/aggregator"
	"github.com/Kfaraon/mikrotik-route-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	"github.com/Kfaraon/mikrotik-route-sync/internal/validator"
)

type Syncer struct {
	cfg        *config.Config
	log        *slog.Logger
	cache      *storage.Cache
	notify     notifier.Notifier
	router     *mikrotik.Client
	resolver   *resolver.Resolver
	classifier *classifier.Classifier
	collector  *collectors.Collector
	mu         sync.Mutex
	busy       map[string]bool
	lastMu     sync.RWMutex
	last       map[string]notifier.SyncResult
}

func NewSyncer(cfg *config.Config, log *slog.Logger, cache *storage.Cache, n notifier.Notifier) *Syncer {
	ttl, _ := time.ParseDuration(cfg.Scheduler.CacheTTL)
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	r := resolver.New(cache, ttl, cfg.External)
	return &Syncer{cfg: cfg, log: log, cache: cache, notify: n, router: mikrotik.New(cfg.MikroTik), resolver: r, classifier: classifier.New(cfg, r), collector: collectors.New(cfg, r), busy: map[string]bool{}, last: map[string]notifier.SyncResult{}}
}
func (s *Syncer) acquire(service string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[service] {
		return false
	}
	s.busy[service] = true
	return true
}
func (s *Syncer) release(service string) { s.mu.Lock(); delete(s.busy, service); s.mu.Unlock() }
func (s *Syncer) SyncOneResult(ctx context.Context, service string, dryRun bool) (notifier.SyncResult, error) {
	service = config.NormalizeService(service)
	res := notifier.SyncResult{Service: service}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()
	if !s.acquire(service) {
		return res, fmt.Errorf("service %s is already syncing", service)
	}
	defer s.release(service)
	d, err := s.classifier.Classify(ctx, service)
	if err != nil {
		return res, err
	}
	res.Method = string(d.Method)
	raw, err := s.collector.Collect(ctx, service, d)
	if err != nil {
		return res, err
	}
	ov := s.cfg.Overrides[service]
	raw = append(raw, ov.Include...)
	valid, err := validator.Prefixes(raw, ov.Exclude)
	if err != nil {
		return res, err
	}
	if len(valid) == 0 {
		return res, fmt.Errorf("no valid prefixes collected; routes were left untouched")
	}
	agg, err := aggregator.Aggregate(valid)
	if err != nil {
		return res, err
	}
	if len(agg) == 0 {
		return res, fmt.Errorf("aggregation returned zero prefixes; routes were left untouched")
	}
	res.Prefixes = len(agg)
	routes := make([]mikrotik.Route, 0, len(agg))
	for _, p := range agg {
		routes = append(routes, mikrotik.Route{DstAddress: p.String(), Gateway: s.cfg.MikroTik.Gateway, Distance: fmt.Sprint(s.cfg.MikroTik.Distance), Comment: s.cfg.Comment(service), RoutingTable: s.cfg.MikroTik.RoutingTable})
	}
	res.Added, res.Removed, err = s.router.Apply(ctx, s.cfg.Comment(service), routes, dryRun)
	if err != nil {
		return res, err
	}
	s.lastMu.Lock()
	s.last[service] = res
	s.lastMu.Unlock()
	s.log.Info("service sync complete", "service", service, "method", res.Method, "prefixes", res.Prefixes, "added", res.Added, "removed", res.Removed, "dry_run", dryRun)
	return res, nil
}
func (s *Syncer) SyncMany(ctx context.Context, services []string, dryRun bool) error {
	start := time.Now()
	s.notify.SyncStart(ctx, services, "manual", "manual")
	results := make([]notifier.SyncResult, 0, len(services))
	var first error
	for _, svc := range services {
		r, e := s.SyncOneResult(ctx, svc, dryRun)
		if e != nil {
			r.Error = e.Error()
			s.notify.Error(ctx, svc, e)
			if first == nil {
				first = e
			}
		}
		results = append(results, r)
	}
	s.notify.SyncDone(ctx, results, time.Since(start), dryRun)
	return first
}
func (s *Syncer) AddService(ctx context.Context, service string) error {
	service = config.NormalizeService(service)
	if s.cfg.HasService(service) {
		return fmt.Errorf("service %s already exists", service)
	}
	r, err := s.SyncOneResult(ctx, service, false)
	if err != nil {
		return err
	}
	if err := s.cfg.AddService(service); err != nil {
		return fmt.Errorf("routes synced but config update failed: %w", err)
	}
	s.log.Info("service added", "service", service, "prefixes", r.Prefixes)
	return nil
}
func (s *Syncer) RemoveService(ctx context.Context, service string) error {
	service = config.NormalizeService(service)
	routes, err := s.router.ListRoutes(ctx, s.cfg.Comment(service))
	if err != nil {
		return err
	}
	for _, r := range routes {
		if err := s.router.RemoveRoute(ctx, r.ID); err != nil {
			return err
		}
	}
	return s.cfg.RemoveService(service)
}
func (s *Syncer) ListRoutes(ctx context.Context, service string) ([]mikrotik.Route, error) {
	return s.router.ListRoutes(ctx, s.cfg.Comment(service))
}
func (s *Syncer) Snapshot(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, svc := range s.cfg.Services {
		r, e := s.ListRoutes(ctx, svc)
		if e != nil {
			return nil, e
		}
		out[svc] = len(r)
	}
	return out, nil
}
func (s *Syncer) Info(ctx context.Context, service string, w io.Writer) error {
	d, e := s.classifier.Classify(ctx, service)
	if e != nil {
		return e
	}
	routes, e := s.ListRoutes(ctx, service)
	if e != nil {
		return e
	}
	fmt.Fprintf(w, "service: %s\nmethod: %s\nasn: %d\nschedule: %s\nroutes: %d\n", service, d.Method, d.ASN, s.cfg.ScheduleFor(service), len(routes))
	return nil
}
func (s *Syncer) RouterPing(ctx context.Context) error { return s.router.Ping(ctx) }
func (s *Syncer) Status(ctx context.Context) map[string]any {
	ok := s.RouterPing(ctx) == nil
	snap, _ := s.Snapshot(ctx)
	s.lastMu.RLock()
	last := map[string]notifier.SyncResult{}
	for k, v := range s.last {
		last[k] = v
	}
	s.lastMu.RUnlock()
	return map[string]any{"mikrotik": ok, "routes": snap, "last": last, "services": append([]string(nil), s.cfg.Services...), "timezone": s.cfg.Timezone}
}
func PrefixStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	sort.Strings(out)
	return out
}
func NormalizeServices(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		s = config.NormalizeService(s)
		if s != "" {
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

var _ = strings.Builder{}
