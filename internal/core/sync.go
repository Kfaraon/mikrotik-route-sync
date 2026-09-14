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

	agg, err := aggregator.Aggregate(cidrs)
	if err != nil {
		res.Errors++
		res.Duration = time.Since(start).String()
		return res, fmt.Errorf("aggregate: %w", err)
	}

	comment := s.cfg.Comment(service)
	existing, err := s.mikrotik.ListRoutes(ctx, comment)
	if err != nil {
		res.Errors++
		res.Duration = time.Since(start).String()
		return res, fmt.Errorf("list routes: %w", err)
	}

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

	s.log.Info("plan",
		"service", service,
		"method", method,
		"collected", len(cidrs),
		"aggregated", len(agg),
		"add", len(toAdd),
		"remove", len(toRemove),
	)

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

	for _, dst := range toRemove {
		if err := s.mikrotik.RemoveRoute(ctx, existingByDst[dst].ID); err != nil {
			s.log.Error("remove failed", "cidr", dst, "err", err)
			res.Errors++
		}
	}

	for _, a := range toAdd {
		r := mikrotik.Route{
			DstAddress:   a,
			Gateway:      s.cfg.MikroTik.Gateway,
			Distance:     fmt.Sprintf("%d", s.cfg.MikroTik.Distance),
			Comment:      comment,
			RoutingTable: s.cfg.MikroTik.RoutingTable,
		}
		if err := s.mikrotik.AddRoute(ctx, r); err != nil {
			s.log.Error("add failed", "cidr", a, "err", err)
			res.Errors++
		}
	}

	res.Duration = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

func (s *Syncer) collect(ctx context.Context, service string) ([]string, classifier.Method, error) {
	d, err := s.classifier.Classify(ctx, service)
	if err != nil {
		return nil, "", err
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
		maxASN := 200
		if ov, ok := s.cfg.Overrides[service]; ok && ov.MaxASNPrefixes > 0 {
			maxASN = ov.MaxASNPrefixes
		}
		col = collectors.NewWHOISCollector(s.res
