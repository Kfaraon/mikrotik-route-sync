package core

// Авто-совет по расширению доменного пресета до "preset,asn".
//
// Проблема: доменный пресет собирает /32 доменов (узко). У части сервисов
// (Meta: instagram/facebook/whatsapp) реальные сети живут в собственном ASN
// (AS32934) и не выводятся из списка доменов: DNS может отдавать разные
// адреса (anycast), а бренд-сквоттеры в пресете вообще на чужих хостингах.
// Поэтому расширение возможно ТОЛЬКО по объявленному (проверенному
// мейнтейнером) ASN, а не по рантайм-резолву.
//
// Три предохранителя, все обязательны:
//  1. ASN объявлен в classifier.knownServices (человек его подтвердил);
//  2. ASN не входит в cdn_sources (иначе это Cloudflare/AWS/Google и
//     расширять нечего — у них уже есть метод cdn);
//  3. набор объявленного ASN «узкий» (≤ presets.expand_max_prefixes).
//
// Ручной override.Method/ASN приоритетнее: авто-расширение не перезаписывает
// явный выбор пользователя (как и CDN-детект).

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kfaraon/mikrotik-list-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-list-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-list-sync/internal/config"
)

// AdviseExpand считает совет по расширению доменного пресета до "preset,asn"
// без CDN-пробы (она для пресет-сервиса бессмысленна). Используется в пути
// добавления: сервис приходит с уже выставленным method=preset, и общий детект
// не запускается. Всегда измеряет размер объявленного ASN (expand=true).
func (s *Syncer) AdviseExpand(ctx context.Context, name string, ov config.ServiceOverride) *CDNAdvice {
	adv := &CDNAdvice{CurrentMethod: currentMethodName(name, ov), Domains: adviseDomains(name, ov)}
	cfg := &adviseConfig{expand: true}
	s.adviseExpand(ctx, name, ov, cfg, adv)
	return adv
}

// adviseExpand считает совет по расширению доменного пресета. Заполняет
// Expand*-поля adv. Best-effort: сбой измерения не валит детект.
func (s *Syncer) adviseExpand(ctx context.Context, name string, ov config.ServiceOverride, cfg *adviseConfig, adv *CDNAdvice) {
	// Расширение имеет смысл только для доменного пресета: у IP-пресета
	// (static_url) доп. методы отбрасываются, у сервиса без пресета —
	// действует обычный override.Method.
	if presetMethod(ov) != "preset" {
		return
	}
	// Уже расширено (есть метод asn и ASN) — совет не нужен.
	if hasMethod(splitMethods(effectiveMethod(ov)), "asn") && ov.ASN > 0 {
		adv.ExpandCurrent = true
		adv.ExpandASN = ov.ASN
		return
	}
	// Явный ASN без метода asn — странное состояние, не трогаем авто.
	if ov.ASN > 0 {
		adv.ExpandCurrent = true
		adv.ExpandASN = ov.ASN
		return
	}

	asn, ok := classifier.DeclaredASN(name)
	if !ok {
		return
	}
	adv.ExpandASN = asn

	// Условие 2: ASN не должен быть известным CDN-провайдером.
	if cdnName, _, isCDN := collectors.CDNSource(asn); isCDN {
		adv.ExpandReason = fmt.Sprintf("AS%d — CDN-провайдер (%s), расширение не применяется", asn, cdnName)
		return
	}

	// Без опции (путь добавления без авто) ограничиваемся объявлением ASN,
	// размер не измеряем — это сетевой шаг.
	if cfg == nil || !cfg.expand {
		adv.ExpandReason = fmt.Sprintf("объявлен AS%d; расширить метод до \"preset,asn\"?", asn)
		return
	}

	// Условие 3: размер набора ASN в пределах порога.
	count, err := s.countASNPrefixes(ctx, asn)
	if err != nil {
		adv.ExpandReason = fmt.Sprintf("AS%d: не удалось измерить набор (%v)", asn, err)
		return
	}
	adv.ExpandPrefixes = count

	limit := s.expandMaxPrefixes()
	if count > limit {
		adv.ExpandReason = fmt.Sprintf("AS%d слишком широкий: %d префиксов > лимита %d", asn, count, limit)
		return
	}
	adv.ExpandRecommend = true
	adv.ExpandReason = fmt.Sprintf("набор узкий: AS%d добавит %d сетей к доменному пресету", asn, count)
}

// countASNPrefixes измеряет число агрегированных IPv4-префиксов ASN
// (кэш коллектора делает повторный вызов дешёвым).
func (s *Syncer) countASNPrefixes(ctx context.Context, asn int) (int, error) {
	if s.expandCountFn != nil {
		return s.expandCountFn(ctx, asn)
	}
	res, err := s.registry().Collect(ctx, "asn", &collectors.Params{Service: "expand-probe", ASN: asn}, collectors.Options{})
	if err != nil {
		return 0, err
	}
	return len(res.Prefixes), nil
}

// expandMaxPrefixes возвращает порог «узости» из конфига (0 → дефолт).
func (s *Syncer) expandMaxPrefixes() int {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	if s.cfg.Presets.ExpandMaxPrefixes > 0 {
		return s.cfg.Presets.ExpandMaxPrefixes
	}
	return 300
}

// expandAutoEnabled — включено ли авто-применение расширения конфигом.
func (s *Syncer) expandAutoEnabled() bool {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	return s.cfg.Presets.ExpandMethodAuto
}

// ApplyExpandRecommendation применяет расширение доменного пресета до
// "preset,asn": выставляет ov.ASN и method с суффиксом asn. Маршруты при этом
// не purge-ятся: точные /32 остаются, набор только дополняется широкими
// сетями (следующий синк применит).
func (s *Syncer) ApplyExpandRecommendation(ctx context.Context, name string, asn int) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if asn <= 0 {
		return fmt.Errorf("invalid ASN for %s", name)
	}

	s.cfg.RLock()
	found := false
	for _, p := range s.cfg.Services {
		if p == name {
			found = true
			break
		}
	}
	ov := s.cfg.Overrides[name]
	s.cfg.RUnlock()
	if !found {
		return fmt.Errorf("service %s not found", name)
	}
	if presetMethod(ov) != "preset" {
		return fmt.Errorf("service %s has no domain preset to expand", name)
	}
	// Ручной ASN не переопределяем молча: если пользователь задал другой —
	// это осознанный выбор. Совпадающий ASN — идемпотентно.
	if ov.ASN != 0 && ov.ASN != asn {
		return fmt.Errorf("service %s already has explicit asn %d, refusing to override with %d", name, ov.ASN, asn)
	}

	s.cfg.Lock()
	if s.cfg.Overrides == nil {
		s.cfg.Overrides = make(map[string]config.ServiceOverride)
	}
	stillThere := false
	for _, p := range s.cfg.Services {
		if p == name {
			stillThere = true
			break
		}
	}
	if !stillThere {
		s.cfg.Unlock()
		return fmt.Errorf("service %s was removed during expand", name)
	}
	cur := s.cfg.Overrides[name]
	next := cur
	next.ASN = asn
	// Сохраняем привязанный пресет и домены; метод получает суффикс asn через
	// effectiveMethod, но и в override.Method пишем согласованно (UI/refresh).
	next.Method = withMethod(next.Method, "asn")
	s.cfg.Overrides[name] = next
	if err := s.cfg.Save(); err != nil {
		s.cfg.Overrides[name] = cur
		s.cfg.Unlock()
		return fmt.Errorf("config not saved, expand unchanged: %w", err)
	}
	s.cfg.Unlock()

	s.audit.LogConfigChange("", "core", map[string]string{
		"service": name,
		"expand":  fmt.Sprintf("preset,asn AS%d", asn),
	})
	s.emit("service_updated", map[string]any{"service": name, "method": next.Method, "asn": asn})
	s.log.Info("метод сервиса расширен до preset,asn",
		"service", name, "service_id", s.ServiceID(name), "asn", asn, "method", next.Method)
	return nil
}

// withMethod добавляет метод m в строку методов (если его ещё нет).
func withMethod(methods, m string) string {
	parts := splitMethods(methods)
	if hasMethod(parts, m) {
		return methods
	}
	parts = append(parts, m)
	return strings.Join(parts, ",")
}
