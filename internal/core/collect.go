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

func (s *Syncer) collectOrStub(ctx context.Context, name string, ov config.ServiceOverride, maxASN int, prog collectors.ProgressFunc) ([]netip.Prefix, string, error) {
	if s.collectFn != nil {
		return s.collectFn(ctx, name, ov)
	}
	return s.collect(ctx, name, ov, maxASN, prog)
}

// collectProgress* — диапазон процента фазы сбора в общей шкале синка
// (совпадает с setProgress("сбор адресов из источников", 15)). Подшаги
// двигают процент внутри [15,40], не выходя за границы фазы.
const (
	collectProgressLo = 15
	collectProgressHi = 40
	// Поддиапазоны внутри фазы сбора.
	collectASNLo, collectASNHi         = 15, 19 // определение ASN
	collectMethodsLo, collectMethodsHi = 19, 36
	collectVerifyLo, collectVerifyHi   = 36, 40 // RDAP-проверка владения ASN
)

// ============================================================================
// Сбор префиксов
// ============================================================================

func (s *Syncer) collect(ctx context.Context, name string, ov config.ServiceOverride, maxASN int, prog collectors.ProgressFunc) ([]netip.Prefix, string, error) {
	if prog != nil {
		ctx = collectors.WithProgress(ctx, collectProgressLo, collectProgressHi, prog)
	}

	// Эффективный метод: у сервиса с привязанным пресетом он навязан пресетом
	// (preset/static_url) и приоритетнее сохранённого override.Method.
	class := classifier.Classify(name, effectiveMethod(ov))

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
		ctxASN, restore := collectors.Scope(ctx, collectASNLo, collectASNHi)
		collectors.ReportCreep(ctxASN, "определение ASN: "+domains[0])
		if resolved, err := s.resolver().ResolveASN(ctxASN, domains[0]); err == nil {
			asn = resolved
		} else {
			s.log.Warn("не удалось определить ASN", "service", name, "service_id", s.ServiceID(name), "err", err)
		}
		restore()
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
	nMethods := len(class.Methods)
	for i, method := range class.Methods {
		lo := collectMethodsLo + (collectMethodsHi-collectMethodsLo)*i/nMethods
		hi := collectMethodsLo + (collectMethodsHi-collectMethodsLo)*(i+1)/nMethods
		mctx, restore := collectors.Scope(ctx, lo, hi)
		collectors.ReportCreep(mctx, "сбор: "+method)
		res, err := s.registry().Collect(mctx, method, params, opts)
		restore()
		if err != nil {
			s.log.Warn("сборщик не сработал, пробуем следующий метод", "method", method, "service", name, "service_id", s.ServiceID(name), "err", err)
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
		vctx, restore := collectors.Scope(ctx, collectVerifyLo, collectVerifyHi)
		acc = s.verifyASNOwnership(vctx, acc, asn)
		restore()
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
	checkedTotal := (len(in) + step - 1) / step
	dropped := make(map[netip.Prefix]bool)

	for i := 0; i < len(in); i += step {
		if ctx.Err() != nil {
			break
		}
		checked++
		collectors.Report(ctx, fmt.Sprintf("проверка AS%d: %d/%d", asn, checked, checkedTotal), checked, checkedTotal)
		ok, err := s.resolver().VerifyPrefixBelongsToASN(ctx, in[i], asn)
		if err == nil && !ok {
			dropped[in[i]] = true
			rejected++
		}
	}

	if rejected == 0 {
		return in
	}
	s.log.Warn("проверка принадлежности ASN по rdap отклонила часть префиксов",
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
