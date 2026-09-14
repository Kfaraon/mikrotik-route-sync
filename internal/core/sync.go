package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/aggregator"
	"github.com/Kfaraon/mikrotik-route-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

const (
	syncTimeout     = 5 * time.Minute
	mikrotikTimeout = 30 * time.Second
)

type Syncer struct {
	cfg        *config.Config
	log        *slog.Logger
	mikrotik   *mikrotik.Client
	resolver   *resolver.Resolver
	classifier *classifier.Classifier
	notifier   notifier.Notifier
}

func NewSyncer(cfg *config.Config, log *slog.Logger, cache *storage.Cache, n notifier.Notifier) *Syncer {
	r := resolver.New(cache, 24*time.Hour)
	if n == nil {
		n = notifier.Nop{}
	}
	return &Syncer{
		cfg:        cfg,
		log:        log,
		mikrotik:   mikrotik.New(cfg.MikroTik),
		resolver:   r,
		classifier: classifier.New(cfg, r),
		notifier:   n,
	}
}

func (s *Syncer) SyncMany(ctx context.Context, services []string, dryRun bool) error {
	ctx, cancel := context.WithTimeout(ctx, syncTimeout*time.Duration(len(services)))
	defer cancel()

	start := time.Now()
	s.notifier.SyncStart(ctx, services, "manual", "")
	results := make([]notifier.SyncResult, 0, len(services))
	for _, svc := range services {
		res, err := s.syncOne(ctx, svc, dryRun)
		results = append(results, res)
		if err != nil {
			s.log.Error("sync failed", "service", svc, "err", err)
			s.notifier.Error(ctx, svc, err)
		}
	}
	s.notifier.SyncDone(ctx, results, time.Since(start).Round(time.Millisecond).String(), dryRun)
	return nil
}

func (s *Syncer) SyncOne(ctx context.Context, service string, dryRun bool) error {
	_, err := s.syncOne(ctx, service, dryRun)
	return err
}

func (s *Syncer) syncOne(ctx context.Context, service string, dryRun bool) (notifier.SyncResult, error) {
	start := time.Now()
	res := notifier.SyncResult{Service: service, DryRun: dryRun}
	s.log.Info("sync start", "service", service, "dry_run", dryRun)

	cidrs, method, err := s.collect(ctx, service)
	if err != nil {
		res.Errors++
		res.Duration = time.Since(start).String()
		return res, err
	}

	cidrs = s.filter(cidrs)
	if len(cidrs) == 0 {
		s.log.Warn("no valid CIDRs collected", "service", service, "method", method)
		res.Duration = time.Since(start).String()
		return res, fmt.Errorf("no CIDRs collected for %s", service)
	}

	agg, err := aggregator.Aggregate(cidrs)
	if err != nil {
		res.Errors++
		res.Duration = time.Since(start).String()
		return res, fmt.Errorf("aggregate: %w", err)
	}

	comment := s.cfg.Comment(service)

	// Create backup before any changes
	mikCtx, mikCancel := context.WithTimeout(ctx, mikrotikTimeout)
	defer mikCancel()

	backup, err := s.mikrotik.BackupRoutes(mikCtx, comment)
	if err != nil {
		s.log.Error("backup failed", "service", service, "err", err)
		// Continue anyway, but log
	}

	existing := backup
	existingByDst := map[string]mikrotik.Route{}
	for _, r := range existing {
		existingByDst[r.DstAddress] = r
	}

	aggSet := map[string]struct{}{}
	for _, a := range agg {
		aggSet[a] = struct{}{}
	}

	var toAdd, toRemove []string
	for _, a := range agg {
		if _, ok := existingByDst[a]; !ok {
			toAdd = append(toAdd, a)
		}
	}
	for dst := range existingByDst {
		if _, ok := aggSet[dst]; !ok {
			toRemove = append(toRemove, dst)
		}
	}

	s.log.Info("plan", "service", service, "method", method, "collected", len(cidrs), "aggregated", len(agg), "add", len(toAdd), "remove", len(toRemove))
	res.Added, res.Removed = len(toAdd), len(toRemove)

	if dryRun {
		for _, a := range toAdd {
			s.log.Info("would add", "service", service, "cidr", a)
		}
		for _, r := range toRemove {
			s.log.Info("would remove", "service", service, "cidr", r)
		}
		res.Duration = time.Since(start).String()
		return res, nil
	}

	// Transaction: remove old, then add new
	// If add fails, we don't rollback automatically (routes already removed),
	// but we log the backup for manual recovery
	var addErrors, removeErrors int

	for _, dst := range toRemove {
		mikCtx, mikCancel := context.WithTimeout(ctx, mikrotikTimeout)
		if err := s.mikrotik.RemoveRoute(mikCtx, existingByDst[dst].ID); err != nil {
			s.log.Error("remove failed", "cidr", dst, "err", err)
			removeErrors++
			res.Errors++
		}
		mikCancel()
	}

	for _, a := range toAdd {
		mikCtx, mikCancel := context.WithTimeout(ctx, mikrotikTimeout)
		r := mikrotik.Route{
			DstAddress:   a,
			Gateway:      s.cfg.MikroTik.Gateway,
			Distance:     fmt.Sprintf("%d", s.cfg.MikroTik.Distance),
			Comment:      comment,
			RoutingTable: s.cfg.MikroTik.RoutingTable,
		}
		if err := s.mikrotik.AddRoute(mikCtx, r); err != nil {
			s.log.Error("add failed", "cidr", a, "err", err)
			addErrors++
			res.Errors++
		}
		mikCancel()
	}

	if addErrors > 0 || removeErrors > 0 {
		s.log.Warn("sync completed with errors", "service", service, "add_errors", addErrors, "remove_errors", removeErrors, "backup_count", len(backup))
	}

	res.Duration = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

func (s *Syncer) collect(ctx context.Context, service string) ([]netip.Prefix, classifier.Method, error) {
	d, err := s.classifier.Classify(ctx, service)
	if err != nil {
		return nil, "", err
	}

	opts := collectors.Options{
		Domains:        d.Domains,
		ASN:            d.ASN,
		URL:            d.URL,
		MaxASNPrefixes: 200,
	}
	if ov, ok := s.cfg.Overrides[strings.ToLower(service)]; ok {
		if ov.MaxASNPrefixes > 0 {
			opts.MaxASNPrefixes = ov.MaxASNPrefixes
		}
		// Parse exclude prefixes
		for _, ex := range ov.Exclude {
			if p, err := netip.ParsePrefix(ex); err == nil {
				opts.Exclude = append(opts.Exclude, p)
			}
		}
	}

	var col collectors.Collector
	switch d.Method {
	case classifier.MethodASN:
		col = collectors.NewASNCollector(s.resolver, d.ASN)
	case classifier.MethodCDN:
		col = collectors.NewCDNCollector(d.ASN)
	case classifier.MethodDynamic:
		col = collectors.NewDynamicCollector(s.resolver, d.Domains)
	case classifier.MethodWHOIS:
		col = collectors.NewWHOISCollector(s.resolver, d.Domains, opts.MaxASNPrefixes)
	case classifier.MethodStaticURL:
		col = collectors.NewStaticCollector(d.URL)
	default:
		return nil, d.Method, fmt.Errorf("unsupported method %s", d.Method)
	}

	collectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	result, err := col.Collect(collectCtx, service, opts)
	if err != nil {
		return nil, d.Method, err
	}
	return result.Prefixes, d.Method, nil
}

func (s *Syncer) filter(in []netip.Prefix) []netip.Prefix {
	out := in[:0]
	for _, p := range in {
		if !p.IsValid() {
			continue
		}
		bits := p.Bits()
		addr := p.Addr()

		// Skip too broad prefixes (IPv4: /0-/9, IPv6: /0-/16)
		if addr.Is4() && bits <= 9 {
			s.log.Warn("skip too broad prefix", "cidr", p.String())
			continue
		}
		if addr.Is6() && bits <= 16 {
			s.log.Warn("skip too broad IPv6 prefix", "cidr", p.String())
			continue
		}

		// Skip private/reserved ranges (IPv4 only)
		if addr.Is4() {
			if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() ||
				addr.IsMulticast() || addr.IsUnspecified() {
				continue
			}
			// Additional checks for reserved ranges
			b := addr.As4()
			if b[0] == 127 || b[0] == 169 && b[1] == 254 || b[0] == 224 || b[0] >= 240 {
				continue
			}
		}

		out = append(out, p)
	}
	return out
}

func (s *Syncer) AddService(ctx context.Context, service string) error {
	return s.SyncOne(ctx, service, false)
}

func (s *Syncer) RemoveService(ctx context.Context, service string) error {
	mikCtx, cancel := context.WithTimeout(ctx, mikrotikTimeout)
	defer cancel()

	existing, err := s.mikrotik.ListRoutes(mikCtx, s.cfg.Comment(service))
	if err != nil {
		return fmt.Errorf("list routes: %w", err)
	}
	for _, r := range existing {
		if err := s.mikrotik.RemoveRoute(mikCtx, r.ID); err != nil {
			s.log.Error("remove failed", "route", r.DstAddress, "err", err)
		}
	}
	return nil
}

func (s *Syncer) Info(ctx context.Context, service string, w io.Writer) error {
	d, _ := s.classifier.Classify(ctx, service)
	fmt.Fprintf(w, "Service: %s\nMethod: %s\nASN: %d\nDomains: %s\nSchedule: %s\n",
		service, d.Method, d.ASN, strings.Join(d.Domains, ", "), s.cfg.ScheduleFor(service))

	mikCtx, cancel := context.WithTimeout(ctx, mikrotikTimeout)
	defer cancel()
	routes, _ := s.mikrotik.ListRoutes(mikCtx, s.cfg.Comment(service))
	fmt.Fprintf(w, "Routes: %d\n", len(routes))
	return nil
}

func (s *Syncer) ListRoutes(ctx context.Context, service string) ([]mikrotik.Route, error) {
	mikCtx, cancel := context.WithTimeout(ctx, mikrotikTimeout)
	defer cancel()
	return s.mikrotik.ListRoutes(mikCtx, s.cfg.Comment(service))
}

func (s *Syncer) PingMikroTik(ctx context.Context) error {
	mikCtx, cancel := context.WithTimeout(ctx, mikrotikTimeout)
	defer cancel()
	return s.mikrotik.Ping(mikCtx)
}

func (s *Syncer) Snapshot(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, svc := range s.cfg.Services {
		mikCtx, cancel := context.WithTimeout(ctx, mikrotikTimeout)
		routes, _ := s.mikrotik.ListRoutes(mikCtx, s.cfg.Comment(svc))
		cancel()
		out[svc] = len(routes)
	}
	return out, nil
}

func (s *Syncer) SyncOneResult(ctx context.Context, service string, dryRun bool) (notifier.SyncResult, error) {
	return s.syncOne(ctx, service, dryRun)
}
