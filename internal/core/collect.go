package core

// Конвейер сбора префиксов: методы коллекторов, RDAP-проверка владения ASN.

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

func (s *Syncer) collectOrStub(ctx context.Context, name string, ov config.ServiceOverride, maxASN int) ([]netip.Prefix, string, error) {
	if s.collectFn != nil {
		return s.collectFn(ctx, name, ov)
	}
	return s.collect(ctx, name, ov, maxASN)
}

// ============================================================================
// Сбор префиксов
// ============================================================================

func (s *Syncer) collect(ctx context.Context, name string, ov config.ServiceOverride, maxASN int) ([]netip.Prefix, string, error) {
	class := classifier.Classify(name, ov.Method)

	opts := collectors.Options{
		Exclude:     ov.Exclude,
		IncludeOnly: ov.IncludeOnly,
	}

	domains := serviceDomains(name, ov, class)
	staticURL := ov.StaticURL
	if staticURL == "" && len(class.StaticURLs) > 0 {
		staticURL = class.StaticURLs[0]
	}
	// CDN-сервис с явным методом static_url и без своего URL: берём
	// официальный список из каталога (раньше он приходил из classifier).
	if staticURL == "" && hasMethod(class.Methods, "static_url") {
		if _, _, u, ok := collectors.CDNSourceByName(name); ok {
			staticURL = u
		}
	}
	maxPrefixes := ov.MaxPrefixes
	if maxPrefixes == 0 {
		maxPrefixes = ov.MaxASNPrefixes
	}
	if maxPrefixes == 0 {
		maxPrefixes = maxASN
	}
	asn := 0
	if class.ASN != "" {
		if n, err := resolverParseASN(class.ASN); err == nil {
			asn = n
		}
	}
	// CDN-сервис без домена (cloudflare/aws/google/fastly): ASN берём из
	// каталога config.cdn_sources по имени сервиса, а не из кода.
	if asn == 0 && hasMethod(class.Methods, "cdn") && len(domains) == 0 {
		if n, _, _, ok := collectors.CDNSourceByName(name); ok {
			asn = n
		}
	}
	if asn == 0 && (hasMethod(class.Methods, "asn") || hasMethod(class.Methods, "cdn")) && len(domains) > 0 {
		if resolved, err := s.resolver().ResolveASN(ctx, domains[0]); err == nil {
			asn = resolved
		} else {
			s.log.Warn("cannot resolve ASN", "service", name, "err", err)
		}
	}

	alsoCDN := make([]int, 0, len(ov.AlsoCDN))
	for _, c := range ov.AlsoCDN {
		if a, _, _, ok := collectors.CDNSourceByName(c); ok {
			alsoCDN = append(alsoCDN, a)
		}
	}

	params := &collectors.Params{
		Service:       name,
		ASN:           asn,
		Domains:       domains,
		IPs:           class.IPs,
		StaticURL:     staticURL,
		MaxPrefixes:   maxPrefixes,
		AlsoCDN:       alsoCDN,
		IncludeGoogle: asn == collectors.GoogleASN,
	}

	var (
		acc     []netip.Prefix
		used    []string
		lastErr error
	)
	for _, method := range class.Methods {
		res, err := s.registry().Collect(ctx, method, params, opts)
		if err != nil {
			s.log.Warn("collector failed, trying next method", "method", method, "service", name, "err", err)
			lastErr = err
			continue
		}
		if res != nil && len(res.Prefixes) > 0 {
			acc = append(acc, res.Prefixes...)
			used = append(used, res.Method)
		}
	}

	if len(acc) == 0 {
		if lastErr != nil {
			return nil, "", lastErr
		}
		return nil, "", fmt.Errorf("no prefixes collected for service %s", name)
	}

	usedMethod := strings.Join(used, "+")

	// RDAP-проверка принадлежности одному ASN имеет смысл только для метода
	// asn (он собирает префиксы известного ASN). whois собирает префиксы
	// ВСЕХ ASN, найденных по резолвленным IP: проверка против одного
	// (основного) ASN отвергала бы легитимные префиксы вторичных ASN.
	if shouldVerifyASNOwnership(usedMethod, asn) {
		acc = s.verifyASNOwnership(ctx, acc, asn)
	}

	return acc, usedMethod, nil
}

// shouldVerifyASNOwnership — запускать ли RDAP-проверку принадлежности ASN.
// Только для метода asn: whois-набор покрывает несколько ASN, и проверка
// против одного отсеивала бы вторичные. Проверка всегда требует известного ASN.
func shouldVerifyASNOwnership(usedMethod string, asn int) bool {
	return asn > 0 &&
		hasMethod(splitMethods(usedMethod), "asn") &&
		!hasMethod(splitMethods(usedMethod), "whois")
}

func resolverParseASN(s string) (int, error) {
	return resolver.ParseASN(s)
}

const rdapVerifySample = 100

// rdapVerifyFullSize — наборы до этого размера проверяются целиком:
// whois-наборы типично 20–100 префиксов, и семплирование пропускало бы
// чужие сети в VPN-маршруты. Большие наборы — семплом, как раньше.

const rdapVerifyFullSize = 100

// rdapVerifyStep — шаг проверки: 1 (все подряд) для малых наборов,
// иначе равномерный семпл rdapVerifySample штук.

func rdapVerifyStep(n int) int {
	if n <= rdapVerifyFullSize {
		return 1
	}
	return max(1, n/rdapVerifySample)
}

func (s *Syncer) verifyASNOwnership(ctx context.Context, in []netip.Prefix, asn int) []netip.Prefix {
	if len(in) == 0 {
		return in
	}
	step := rdapVerifyStep(len(in))
	rejected := 0
	checked := 0
	dropped := make(map[netip.Prefix]bool)

	for i := 0; i < len(in); i += step {
		if ctx.Err() != nil {
			break
		}
		checked++
		ok, err := s.resolver().VerifyPrefixBelongsToASN(ctx, in[i], asn)
		if err == nil && !ok {
			dropped[in[i]] = true
			rejected++
		}
	}

	if rejected == 0 {
		return in
	}
	s.log.Warn("rdap asn-ownership check rejected some prefixes",
		"asn", asn, "checked", checked, "explicit_rejects", rejected)

	out := make([]netip.Prefix, 0, len(in))
	for _, p := range in {
		if dropped[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

func hasMethod(methods []string, want string) bool {
	for _, m := range methods {
		if m == want {
			return true
		}
	}
	return false
}

// ============================================================================
// Снапшоты / backup / restore
// ============================================================================
