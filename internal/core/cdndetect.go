package core

// Автоопределение источника сбора: проверка, не обслуживается ли сервис за
// CDN (Cloudflare, AWS CloudFront, Google, Fastly), и переключение на
// официальный публичный список диапазонов CDN (метод "cdn").
//
// Сигналы детекции (в порядке убывания точности):
//  1. A-записи доменов входят в официальный список CDN (точный признак);
//  2. ASN домена входит в таблицу cdnSources (pre-filter, экономит HTTP).
//
// Авто-переключение происходит при добавлении сервиса, если метод не задан
// явно; для существующих сервисов — через команду recommend / API.
// Ручной выбор (метод или static_url) никогда не переопределяется.

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/validator"
)

// CDNAdvice — результат проверки сервиса на обслуживание за CDN.
type CDNAdvice struct {
	Detected      bool            `json:"detected"`
	CDN           string          `json:"cdn,omitempty"`
	ASN           int             `json:"asn,omitempty"`
	URL           string          `json:"url,omitempty"`
	PrefixCount   int             `json:"prefix_count,omitempty"`
	WhoisCount    int             `json:"whois_count,omitempty"`
	Domains       []string        `json:"domains,omitempty"`
	IPs           []string        `json:"ips,omitempty"`
	Skipped       []string        `json:"skipped,omitempty"`
	CurrentMethod string          `json:"current_method"`
	Recommend     bool            `json:"recommend"`
	Reason        string          `json:"reason"`
	Coverage      *CoverageReport `json:"coverage,omitempty"`
	Broad         []string        `json:"broad,omitempty"`
	Verification  []*SNIVerdict   `json:"verification,omitempty"`
}

// cdnProbeDeps — зависимости пробника (внедряются в тестах).
type cdnProbeDeps struct {
	resolveDomain func(ctx context.Context, domain string) ([]string, error)
	resolveASN    func(ctx context.Context, domain string) (int, error)
	collectCDN    func(ctx context.Context, asn int) ([]netip.Prefix, string, error)
	collectWhois  func(ctx context.Context, domains []string) (int, error)
}

// AdviseCDN проверяет, обслуживается ли сервис за CDN, и возвращает
// рекомендацию по источнику. Чистая сетевая операция без побочных эффектов;
// никогда не возвращает ошибку (при сбоях — Detected=false с причиной).
func (s *Syncer) AdviseCDN(ctx context.Context, name string, ov config.ServiceOverride) *CDNAdvice {
	name = strings.ToLower(strings.TrimSpace(name))
	cur := currentMethodName(name, ov)
	adv := &CDNAdvice{CurrentMethod: cur, Domains: adviseDomains(name, ov)}

	if len(adv.Domains) == 0 {
		adv.Reason = "не домен (IP, ASN или имя без точки) — проверка CDN не применяется"
		return adv
	}

	var p = s.adviseProbe(ctx, name, adv.Domains)
	if p != nil {
		adv.Detected = p.Detected
		adv.CDN = p.CDN
		adv.ASN = p.ASN
		adv.URL = p.URL
		adv.PrefixCount = p.PrefixCount
		adv.WhoisCount = p.WhoisCount
		adv.IPs = p.IPs
		adv.Skipped = p.Skipped
		adv.Reason = p.Reason
	}
	if !adv.Detected {
		s.fillQuality(ctx, name, ov, adv)
		return adv
	}

	switch {
	case hasMethod(splitMethods(cur), "cdn"):
		adv.Reason += " — уже используется метод cdn"
	case ov.Method == "static_url":
		if ov.StaticURL == "" || ov.StaticURL == adv.URL {
			adv.Reason += " — static_url уже указывает на официальный список"
		} else {
			adv.Reason += " — указан собственный static_url, замена не предлагается"
		}
	default:
		adv.Recommend = true
	}
	s.fillQuality(ctx, name, ov, adv)
	return adv
}

// fillQuality — показатели качества текущего сбора для сервисов из конфига:
// покрытие A-записей, широкие префиксы, SNI-проба. При добавлении нового
// сервиса (его ещё нет в конфиге) пропускаем — детект и так всё проверил.
// Best-effort: сбой сбора качество не подменяет.
func (s *Syncer) fillQuality(ctx context.Context, name string, ov config.ServiceOverride, adv *CDNAdvice) {
	// Короткое чтение под cfgMu (было serviceMu на всю функцию).
	s.cfg.RLock()
	_, inCfg := s.cfg.Overrides[name]
	if !inCfg {
		for _, p := range s.cfg.Services {
			if p == name {
				inCfg = true
				break
			}
		}
	}
	maxASN := s.cfg.Safety.MaxASNPrefixes
	warnBroad := s.cfg.Safety.WarnBroadV4
	verifySNI := s.cfg.Safety.VerifySNIEnabled()
	s.cfg.RUnlock()
	if !inCfg || len(adv.Domains) == 0 {
		return
	}
	prefixes, _, err := s.collectOrStub(ctx, name, ov, maxASN)
	if err != nil {
		return
	}
	adv.Coverage = s.checkServiceCoverage(ctx, name, adv.Domains, prefixes)
	// Широкие префиксы текущего сбора (кроме cdn — см. SyncService).
	if !hasMethod(splitMethods(currentMethodName(name, ov)), "cdn") {
		adv.Broad = validator.BroadPrefixes(prefixes, warnBroad)
	}
	// SNI-проба текущего набора (только если включена).
	if verifySNI {
		adv.Verification = s.verifyServiceSNI(ctx, adv.Domains)
	}
}

// adviseProbe — точка подмены пробника в тестах (cdnProbeFn).
func (s *Syncer) adviseProbe(ctx context.Context, name string, domains []string) *CDNAdvice {
	if s.cdnProbeFn != nil {
		return s.cdnProbeFn(ctx, name, domains)
	}
	return s.probeCDN(ctx, name, domains)
}

// probeCDN — реальный пробник: DNS/ASN через resolver, списки через registry.
func (s *Syncer) probeCDN(ctx context.Context, name string, domains []string) *CDNAdvice {
	s.cfg.RLock()
	maxASN := s.cfg.Safety.MaxASNPrefixes
	s.cfg.RUnlock()
	deps := &cdnProbeDeps{
		resolveDomain: func(ctx context.Context, d string) ([]string, error) {
			return s.resolver().ResolveDomain(ctx, d)
		},
		resolveASN: func(ctx context.Context, d string) (int, error) {
			return s.resolver().ResolveASN(ctx, d)
		},
		collectCDN: func(ctx context.Context, asn int) ([]netip.Prefix, string, error) {
			res, err := s.registry().Collect(ctx, "cdn", &collectors.Params{Service: name, ASN: asn}, collectors.Options{})
			if err != nil {
				return nil, "", err
			}
			return res.Prefixes, res.Source, nil
		},
		collectWhois: func(ctx context.Context, doms []string) (int, error) {
			res, err := s.registry().Collect(ctx, "whois", &collectors.Params{
				Service:     name,
				Domains:     doms,
				MaxPrefixes: maxASN,
			}, collectors.Options{})
			if err != nil {
				return 0, err
			}
			return len(res.Prefixes), nil
		},
	}
	return runCDNProbe(ctx, name, domains, deps)
}

// runCDNProbe — чистая логика детекции (тестируется с фейковыми deps).
func runCDNProbe(ctx context.Context, name string, domains []string, deps *cdnProbeDeps) *CDNAdvice {
	adv := &CDNAdvice{Domains: domains}
	if err := ctx.Err(); err != nil {
		adv.Reason = "проверка прервана"
		return adv
	}

	var allIPs []string
	var asn int
	var resolved int
	for _, d := range domains {
		ips, err := deps.resolveDomain(ctx, d)
		if err != nil || len(ips) == 0 {
			// Apex без A-записей (ytimg.com) — не приговор всему сервису:
			// пропускаем домен, фиксируем в отчёте.
			adv.Skipped = append(adv.Skipped, d)
			continue
		}
		a, err := deps.resolveASN(ctx, d)
		if err != nil || a == 0 {
			adv.Reason = fmt.Sprintf("ASN домена %s не определён", d)
			return adv
		}
		if resolved == 0 {
			asn = a
		} else if a != asn {
			adv.Reason = fmt.Sprintf("домены в разных AS (AS%d и AS%d)", asn, a)
			return adv
		}
		resolved++
		allIPs = append(allIPs, ips...)
	}
	if resolved == 0 {
		adv.Reason = fmt.Sprintf("домены не разрешаются: %s", strings.Join(domains, ", "))
		return adv
	}
	adv.ASN = asn

	cdnName, url, ok := collectors.CDNSource(asn)
	if !ok {
		adv.Reason = fmt.Sprintf("AS%d не входит в список поддерживаемых CDN", asn)
		return adv
	}
	adv.CDN = cdnName
	adv.URL = url

	prefixes, src, err := deps.collectCDN(ctx, asn)
	if err != nil || len(prefixes) == 0 {
		adv.Reason = fmt.Sprintf("официальный список CDN недоступен (%s)", url)
		return adv
	}
	if src != "" {
		adv.URL = src
	}
	adv.PrefixCount = len(prefixes)

	// Проект работает только с IPv4: AAAA-записи в проверке не участвуют.
	var v4 []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, s := range allIPs {
		ip, perr := netip.ParseAddr(s)
		if perr != nil || !ip.Is4() || seen[ip] {
			continue
		}
		seen[ip] = true
		v4 = append(v4, ip)
	}
	if len(v4) == 0 {
		adv.Reason = "IPv4-адреса не найдены"
		return adv
	}

	var outside []string
	for _, ip := range v4 {
		contained := false
		for _, p := range prefixes {
			if p.Contains(ip) {
				contained = true
				break
			}
		}
		if !contained {
			outside = append(outside, ip.String())
		}
	}
	if len(outside) > 0 {
		adv.Reason = fmt.Sprintf("часть адресов вне списка CDN: %s", strings.Join(outside, ", "))
		return adv
	}

	adv.IPs = make([]string, 0, len(v4))
	for _, ip := range v4 {
		adv.IPs = append(adv.IPs, ip.String())
	}
	adv.Detected = true
	adv.Reason = fmt.Sprintf("все A-записи входят в официальный список %s", url)
	if len(adv.Skipped) > 0 {
		adv.Reason += fmt.Sprintf(" (пропущены неразрешённые: %s)", strings.Join(adv.Skipped, ", "))
	}

	// Сравнение с whois — best-effort (в отчёте цифра «15 против 86»).
	if deps.collectWhois != nil {
		if n, werr := deps.collectWhois(ctx, domains); werr == nil && n > 0 {
			adv.WhoisCount = n
		}
	}
	return adv
}

// ApplyCDNRecommendation переключает существующий сервис на метод cdn:
// удаляет старые AUTO-записи (см. covered-семантику — сами они не чистятся)
// и сохраняет override с method=cdn. Возвращает число удалённых записей.
func (s *Syncer) ApplyCDNRecommendation(ctx context.Context, name string, adv *CDNAdvice) (int, error) {
	if adv == nil || !adv.Detected {
		return 0, fmt.Errorf("CDN not detected for %s, nothing to apply", name)
	}
	name = strings.ToLower(strings.TrimSpace(name))

	// Фаза 0: быстрая проверка (короткое чтение) — понятная ошибка раньше сети.
	s.cfg.RLock()
	found := false
	for _, p := range s.cfg.Services {
		if p == name {
			found = true
			break
		}
	}
	orig := s.cfg.Overrides[name]
	s.cfg.RUnlock()
	if !found {
		return 0, fmt.Errorf("service %s not found — add it first (CDN is detected automatically on add)", name)
	}
	if orig.Method == "cdn" {
		return 0, fmt.Errorf("service %s already uses method cdn", name)
	}
	if orig.Method == "static_url" && (orig.StaticURL == "" || orig.StaticURL == adv.URL) {
		return 0, fmt.Errorf("service %s already uses official CDN list via static_url", name)
	}

	// Фаза 1: purge сети БЕЗ блокировок (было под serviceMu — вешало всё).
	// При ошибке конфигурация не меняется (как раньше).
	purged, perr := s.RemoveServiceEntries(ctx, name)
	if perr != nil {
		return 0, fmt.Errorf("purge failed, override unchanged: %w", perr)
	}

	// Фаза 2: мутация под Lock (коротко, без сети).
	prev := orig
	prevMethod := prev.Method
	if prevMethod == "" {
		prevMethod = "(auto)"
	}
	prev.Method = "cdn"
	// Фиксируем домены проверки: без них cdn на произвольном домене не сможет
	// резолвить ASN при сборе (см. serviceDomains) — метод станет мёртвым.
	if len(prev.Domains) == 0 && len(adv.Domains) > 0 {
		prev.Domains = append([]string(nil), adv.Domains...)
	}
	s.cfg.Lock()
	if s.cfg.Overrides == nil {
		s.cfg.Overrides = make(map[string]config.ServiceOverride)
	}
	// Сервис могли удалить, пока шёл purge, — перепроверяем.
	stillThere := false
	for _, p := range s.cfg.Services {
		if p == name {
			stillThere = true
			break
		}
	}
	if !stillThere {
		s.cfg.Unlock()
		return 0, fmt.Errorf("service %s was removed during purge, override unchanged", name)
	}
	cur := s.cfg.Overrides[name]
	s.cfg.Overrides[name] = prev

	if err := s.cfg.Save(); err != nil {
		s.cfg.Overrides[name] = cur
		s.cfg.Unlock()
		return 0, fmt.Errorf("config not saved, override unchanged: %w", err)
	}
	s.cfg.Unlock()

	s.audit.LogConfigChange("", "core", map[string]string{
		"service": name,
		"method":  prevMethod + " -> cdn",
	})
	s.emit("service_updated", map[string]any{"service": name, "method": "cdn"})
	s.log.Info("service source switched to cdn",
		"service", name, "prev_method", prevMethod, "cdn", adv.CDN,
		"asn", adv.ASN, "purged", purged)

	msg := cdnSwitchedMessage(name, adv, prevMethod, purged)
	if n := s.getNotify(); n != nil {
		if err := n.Send(context.WithoutCancel(ctx), msg); err != nil {
			s.log.Warn("telegram cdn notice not delivered", "service", name, "err", err)
		}
	}
	return purged, nil
}

// GetServiceOverride возвращает override сервиса (пустой, если нет).
func (s *Syncer) GetServiceOverride(name string) config.ServiceOverride {
	return s.cfg.Overrides[strings.ToLower(strings.TrimSpace(name))]
}

// serviceDomains — домены для сбора: override → классификатор → само имя,
// если это домен. Последний шаг страхует явный метод cdn/asn на произвольном
// домене: без домена ASN нечем резолвить, и сбор всегда падает
// ("ASN is required", 0 префиксов).
func serviceDomains(name string, ov config.ServiceOverride, class classifier.Result) []string {
	if len(ov.Domains) > 0 {
		return ov.Domains
	}
	if len(class.Domains) > 0 {
		return class.Domains
	}
	return adviseDomains(name, ov)
}

// adviseDomains — домены для проверки: override.Domains, затем домены
// классификатора (известные сервисы вида "youtube"), затем само имя,
// если это домен (содержит точку и не является IP-адресом).
func adviseDomains(name string, ov config.ServiceOverride) []string {
	if len(ov.Domains) > 0 {
		return append([]string(nil), ov.Domains...)
	}
	if class := classifier.Classify(name, ""); len(class.Domains) > 0 {
		return append([]string(nil), class.Domains...)
	}
	if name == "" {
		return nil
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return nil
	}
	if !strings.Contains(name, ".") {
		return nil
	}
	return []string{name}
}

// currentMethodName — текущий метод сервиса: override или дефолт классификатора.
func currentMethodName(name string, ov config.ServiceOverride) string {
	if ov.Method != "" {
		return ov.Method
	}
	return strings.Join(classifier.Classify(name, "").Methods, ",")
}

// splitMethods разбивает строку методов ("dynamic,whois" из конфига,
// "dynamic+whois" из usedMethod после сбора) на список.
func splitMethods(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '+'
	})
}

// cdnDetectedMessage — уведомление Telegram при автоопределении при добавлении.
func cdnDetectedMessage(name string, adv *CDNAdvice) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: определён за %s (AS%d) — метод сбора: cdn.\n", name, adv.CDN, adv.ASN)
	fmt.Fprintf(&b, "Источник: официальный список %s — %d префиксов", adv.URL, adv.PrefixCount)
	if adv.WhoisCount > 0 {
		fmt.Fprintf(&b, " (вместо whois — %d)", adv.WhoisCount)
	}
	return b.String()
}

// cdnSwitchedMessage — уведомление Telegram при ручном переключении (recommend).
func cdnSwitchedMessage(name string, adv *CDNAdvice, prevMethod string, purged int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: источник изменён на cdn (%s, AS%d), было: %s.\n", name, adv.CDN, adv.ASN, prevMethod)
	fmt.Fprintf(&b, "Источник: официальный список %s — %d префиксов", adv.URL, adv.PrefixCount)
	if adv.WhoisCount > 0 {
		fmt.Fprintf(&b, " (вместо whois — %d)", adv.WhoisCount)
	}
	if purged > 0 {
		fmt.Fprintf(&b, ". Удалено старых AUTO-записей: %d", purged)
	}
	return b.String()
}
