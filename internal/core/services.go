package core

// Управление жизненным циклом сервисов: добавление, удаление, информация,
// бэкап/восстановление записей.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/presets"
	"github.com/Kfaraon/mikrotik-route-sync/internal/validator"
)

type ServiceInfo struct {
	Name          string                 `json:"name"`
	List          string                 `json:"list"`
	Comment       string                 `json:"comment"`
	Schedule      string                 `json:"schedule"`
	Override      config.ServiceOverride `json:"override"`
	EntryCount    int                    `json:"entry_count"`
	SnapshotCount int                    `json:"snapshot_count"`
}

type BackupFile struct {
	Service   string              `json:"service"`
	List      string              `json:"list"`
	Comment   string              `json:"comment"`
	CreatedAt time.Time           `json:"created_at"`
	Entries   []addresslist.Entry `json:"entries"`
}

func (s *Syncer) InfoService(ctx context.Context, name string) (*ServiceInfo, error) {
	// Снапшот читаемого под RLock, сеть — после (вызывающие блокировок
	// не держат: бот — секциями, веб — секцией serviceRows, CLI один).
	s.cfg.RLock()
	list := s.cfg.Firewall.AddressList
	commentPrefix := s.cfg.Firewall.CommentPrefix
	schedule := s.cfg.EffectiveSchedule(name)
	override := s.cfg.Overrides[name]
	s.cfg.RUnlock()
	info := &ServiceInfo{
		Name:     name,
		List:     list,
		Comment:  addresslist.CommentFor(commentPrefix, name),
		Schedule: schedule,
		Override: override,
	}

	entries, err := s.mt().ListServiceEntries(ctx, list, commentPrefix, name)
	if err != nil {
		s.log.Warn("failed to list entries for info", "service", name, "err", err)
	} else {
		info.EntryCount = len(entries)
	}

	snapshots, err := s.cache.ListSnapshots(name)
	if err != nil {
		s.log.Warn("failed to list snapshots for info", "service", name, "err", err)
	} else {
		info.SnapshotCount = len(snapshots)
	}

	return info, nil
}

func (s *Syncer) AddService(ctx context.Context, name string) error {
	return s.AddServiceWithConfig(ctx, name, config.ServiceOverride{})
}

func (s *Syncer) AddServiceWithConfig(ctx context.Context, name string, override config.ServiceOverride) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if !config.ValidateServiceName(name) {
		return fmt.Errorf("invalid service name: %s (allowed: lowercase letters/digits with '-', '_', '.' — e.g. instagram, youtube.com, 8.8.8.8, AS13335)", name)
	}

	if err := validator.SanitizeComment(name); err != nil {
		return err
	}

	if override.Method != "" && !config.IsValidMethod(override.Method) {
		return fmt.Errorf("unknown method %q (allowed: cdn, asn, dynamic, whois, static_url; comma-separated lists like \"dynamic,whois\" accepted)", override.Method)
	}

	// Автоопределение источника: если метод не задан явно — проверяем, не
	// обслуживается ли домен за CDN (Cloudflare и др.), и сразу ставим
	// метод cdn. Ручной выбор (метод/static_url) не переопределяется;
	// сбой проверки не блокирует добавление (детект best-effort).
	var detected *CDNAdvice
	if override.Method == "" && override.StaticURL == "" {
		if adv := s.AdviseCDN(ctx, name, override); adv.Detected {
			override.Method = "cdn"
			// Фиксируем домены проверки — см. ApplyCDNRecommendation.
			if len(override.Domains) == 0 && len(adv.Domains) > 0 {
				override.Domains = append([]string(nil), adv.Domains...)
			}
			detected = adv
			s.log.Info("cdn source auto-detected",
				"service", name, "cdn", adv.CDN, "asn", adv.ASN, "url", adv.URL,
				"cdn_prefixes", adv.PrefixCount, "whois_prefixes", adv.WhoisCount)
		} else {
			s.log.Debug("cdn auto-detection skipped", "service", name, "reason", adv.Reason)
		}
	}

	s.cfg.Lock()
	for _, p := range s.cfg.Services {
		if p == name {
			s.cfg.Unlock()
			return fmt.Errorf("service %s already exists", name)
		}
	}

	s.cfg.Services = append(s.cfg.Services, name)

	if s.cfg.Overrides == nil {
		s.cfg.Overrides = make(map[string]config.ServiceOverride)
	}
	prevOv, hadOv := s.cfg.Overrides[name]
	s.cfg.Overrides[name] = override

	if err := s.cfg.Save(); err != nil {
		// Откат: файл не изменился — память тоже не должна.
		kept := s.cfg.Services[:0]
		for _, p := range s.cfg.Services {
			if p != name {
				kept = append(kept, p)
			}
		}
		s.cfg.Services = kept
		if hadOv {
			s.cfg.Overrides[name] = prevOv
		} else {
			delete(s.cfg.Overrides, name)
		}
		s.cfg.Unlock()
		return err
	}
	s.cfg.Unlock()

	// Уведомления и аудит — вне блокировки (сеть).
	s.audit.LogServiceAdd("", "core", name)
	s.emit("service_added", map[string]any{"service": name})

	if detected != nil {
		if n := s.getNotify(); n != nil {
			if err := n.Send(context.WithoutCancel(ctx), cdnDetectedMessage(name, detected)); err != nil {
				s.log.Warn("telegram cdn notice not delivered", "service", name, "err", err)
			}
		}
	}
	return nil
}

func (s *Syncer) RemoveService(ctx context.Context, name string, purgeRoutes bool) error {
	name = strings.ToLower(strings.TrimSpace(name))

	// Фаза 0: быстрая проверка наличия (короткое чтение).
	s.cfg.RLock()
	found := false
	for _, p := range s.cfg.Services {
		if p == name {
			found = true
			break
		}
	}
	s.cfg.RUnlock()
	if !found {
		return fmt.Errorf("service %s not found", name)
	}

	// Фаза 1: purge записей на роутере — БЕЗ блокировок (сеть на минуты
	// вешала всё управление). При ошибке сервис НЕ удаляется (как раньше).
	// Гоночное окно: синк, стартовавший во время purge, может вернуть записи
	// обратно — они останутся сиротами (управляемость уже снята).
	if purgeRoutes {
		n, err := s.RemoveServiceEntries(ctx, name)
		if err != nil {
			return fmt.Errorf("purge address-list entries failed, сервис НЕ удалён из конфигурации: %w", err)
		}
		s.log.Info("purged service address entries", "service", name, "count", n)
	}

	// Фаза 2: мутация конфига под Lock (коротко, без сети).
	s.cfg.Lock()
	found = false
	var newServices []string
	for _, p := range s.cfg.Services {
		if p == name {
			found = true
			continue
		}
		newServices = append(newServices, p)
	}
	if !found {
		s.cfg.Unlock()
		return fmt.Errorf("service %s not found", name)
	}
	prevServices := s.cfg.Services
	prevOverride, hadOverride := s.cfg.Overrides[name]
	s.cfg.Services = newServices
	delete(s.cfg.Overrides, name)

	// Расписание сервиса: удалённый сервис не должен оставаться
	// в schedules.services (иначе светится на странице расписаний).
	var prevSched config.ServiceSchedule
	hadSched := false
	if s.cfg.Schedules.Services != nil {
		prevSched, hadSched = s.cfg.Schedules.Services[name]
		delete(s.cfg.Schedules.Services, name)
	}

	if err := s.cfg.Save(); err != nil {
		// Откат состояния в памяти: файл конфигурации не изменился —
		// сервис должен остаться и в памяти, и в файле, иначе после
		// reload он неожиданно воскреснет (а в памяти его уже нет).
		s.cfg.Services = prevServices
		if hadOverride {
			s.cfg.Overrides[name] = prevOverride
		}
		if hadSched {
			if s.cfg.Schedules.Services == nil {
				s.cfg.Schedules.Services = map[string]config.ServiceSchedule{}
			}
			s.cfg.Schedules.Services[name] = prevSched
		}
		s.cfg.Unlock()
		return fmt.Errorf("config not saved, сервис НЕ удалён: %w", err)
	}
	s.cfg.Unlock()

	// Фаза 3: чистка производного состояния — у каждого свои мьютексы.
	// Снапшоты удаляются только после успешного сохранения конфигурации.
	ids, err := s.cache.ListSnapshots(name)
	if err != nil {
		s.log.Warn("failed to list snapshots for removed service", "service", name, "err", err)
	} else {
		for _, info := range ids {
			if err := s.cache.DeleteSnapshot(name, info.ID); err != nil {
				s.log.Warn("failed to delete snapshot of removed service", "service", name, "snapshot", info.ID, "err", err)
			}
		}
	}

	// Флаг деградации сбрасывается: удалённый сервис не должен светиться
	// в дашборде и боте. История при этом сохраняется (для страницы логов).
	s.ClearDegraded(name)

	s.audit.LogServiceDelete("", "core", name)
	s.emit("service_removed", map[string]any{"service": name})
	return nil
}

// maxPresetApplyDomains — предел доменов пресета при применении на сервис.
// См. maxServiceDomains в web: bulk dynamic-конвейер не тянет физически.
const maxPresetApplyDomains = 5000

// ApplyPresetRefresh пересчитывает Domains сервиса из пресета с сохранением
// ручных доменов: ручные = текущие Domains минус прошлый снимок Generated.
// Возвращает diff для показа перед dry-run. Большие пресеты (rublock)
// требуют force=true. Действующие маршруты не трогает (только конфиг).
func (s *Syncer) ApplyPresetRefresh(ctx context.Context, name string, presetSlug string, force bool) (added, removed []string, err error) {
	name = strings.ToLower(strings.TrimSpace(name))

	s.cfg.RLock()
	ov, _ := s.cfg.Overrides[name]
	inServices := false
	for _, p := range s.cfg.Services {
		if p == name {
			inServices = true
			break
		}
	}
	s.cfg.RUnlock()
	if !inServices {
		return nil, nil, fmt.Errorf("service %s not found", name)
	}
	slug := strings.ToLower(strings.TrimSpace(presetSlug))
	if slug == "" {
		slug = ov.Preset
	}
	if slug == "" {
		return nil, nil, fmt.Errorf("service %s has no preset bound (передайте preset явно)", name)
	}
	p, ok := presets.Domains(slug)
	if !ok {
		return nil, nil, fmt.Errorf("preset %q unavailable (зеркало не скачано?)", slug)
	}
	full, _ := presets.Get(slug)
	if full.Large && !force {
		return nil, nil, fmt.Errorf("preset %q large (%d domains): подтвердите force=true", slug, len(p))
	}
	if len(p) > maxPresetApplyDomains {
		return nil, nil, fmt.Errorf("preset %q too large (%d domains, max %d)", slug, len(p), maxPresetApplyDomains)
	}

	generated := make(map[string]bool, len(ov.Generated))
	for _, d := range ov.Generated {
		generated[strings.ToLower(d)] = true
	}
	inPreset := make(map[string]bool, len(p))
	for _, d := range p {
		inPreset[strings.ToLower(d)] = true
	}
	var userAdded []string
	for _, d := range ov.Domains {
		if !generated[strings.ToLower(d)] && !inPreset[strings.ToLower(d)] {
			userAdded = append(userAdded, d)
		}
	}
	newDomains := append([]string(nil), p...)
	for _, d := range userAdded {
		if !inPreset[strings.ToLower(d)] {
			newDomains = append(newDomains, d)
		}
	}
	oldSet := make(map[string]bool, len(ov.Domains))
	for _, d := range ov.Domains {
		oldSet[strings.ToLower(d)] = true
	}
	newSet := make(map[string]bool, len(newDomains))
	for _, d := range newDomains {
		newSet[strings.ToLower(d)] = true
	}
	for _, d := range newDomains {
		if !oldSet[strings.ToLower(d)] {
			added = append(added, d)
		}
	}
	for _, d := range ov.Domains {
		if !newSet[strings.ToLower(d)] {
			removed = append(removed, d)
		}
	}

	primary := ov.Primary
	if primary == "" && len(p) > 0 {
		primary = p[0]
	}
	nov := ov
	nov.Domains = newDomains
	nov.Generated = append([]string(nil), p...)
	nov.Preset = slug
	nov.Primary = primary

	s.cfg.Lock()
	cur, hadCur := s.cfg.Overrides[name]
	s.cfg.Overrides[name] = nov
	if err := s.cfg.Save(); err != nil {
		if hadCur {
			s.cfg.Overrides[name] = cur
		} else {
			delete(s.cfg.Overrides, name)
		}
		s.cfg.Unlock()
		return nil, nil, fmt.Errorf("config not saved, preset unchanged: %w", err)
	}
	s.cfg.Unlock()

	s.audit.LogConfigChange("", "core", map[string]string{
		"service": name,
		"preset":  slug,
	})
	s.emit("service_updated", map[string]any{"service": name, "preset": slug})
	s.log.Info("preset refreshed", "service", name, "preset", slug,
		"added", len(added), "removed", len(removed))
	return added, removed, nil
}

func (s *Syncer) ListServices() []string {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	return append([]string(nil), s.cfg.Services...)
}

// UpdateServiceOverride меняет method и/или domains сервиса (scope Фазы 2:
// только эти два поля; остальное — через конфиг).
// method == nil — не трогать; пустая строка — сбросить в авто (классификатор).
// domains == nil — не трогать; иначе заменить целиком (пустой список чистит).
// Ручной выбор не переопределяется авто-детектом; scheduler не затрагивается.
func (s *Syncer) UpdateServiceOverride(ctx context.Context, name string, method *string, domains []string) (config.ServiceOverride, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !config.ValidateServiceName(name) {
		return config.ServiceOverride{}, fmt.Errorf("invalid service name: %s", name)
	}

	// Только конфиг: serviceMu не трогаем (порядок cfgMu → serviceMu,
	// обратного захвата нигде нет). Разблокировка явная до аудита/emit:
	// с defer они висели бы под Lock на дисковом I/O логгера.
	s.cfg.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			s.cfg.Unlock()
		}
	}()

	found := false
	for _, p := range s.cfg.Services {
		if p == name {
			found = true
			break
		}
	}
	if !found {
		return config.ServiceOverride{}, fmt.Errorf("service %s not found", name)
	}

	ov := s.cfg.Overrides[name]
	if method != nil {
		m := strings.ToLower(strings.TrimSpace(*method))
		if m != "" {
			if err := config.ValidateMethods(m); err != nil {
				return config.ServiceOverride{}, err
			}
		}
		ov.Method = m
	}
	if domains != nil {
		clean := make([]string, 0, len(domains))
		for _, d := range domains {
			if d = strings.TrimSpace(d); d != "" {
				clean = append(clean, d)
			}
		}
		ov.Domains = clean
	}

	if s.cfg.Overrides == nil {
		s.cfg.Overrides = make(map[string]config.ServiceOverride)
	}
	prev, hadPrev := s.cfg.Overrides[name]
	s.cfg.Overrides[name] = ov
	// Пустой override не храним: сброс всех полей удаляет запись,
	// иначе UI-откаты мусорят конфигом и зажигают бейдж «есть».
	if ov.Method == "" && len(ov.Domains) == 0 && ov.StaticURL == "" &&
		len(ov.AlsoCDN) == 0 && ov.MaxASNPrefixes == 0 && ov.MaxPrefixes == 0 &&
		len(ov.Exclude) == 0 && len(ov.IncludeOnly) == 0 && ov.RefreshInterval == "" {
		delete(s.cfg.Overrides, name)
	}
	if err := s.cfg.Save(); err != nil {
		if hadPrev {
			s.cfg.Overrides[name] = prev
		} else {
			delete(s.cfg.Overrides, name)
		}
		return config.ServiceOverride{}, fmt.Errorf("config not saved, override unchanged: %w", err)
	}
	s.cfg.Unlock()
	unlocked = true

	s.audit.LogConfigChange("", "core", map[string]string{
		"service": name,
		"method":  prev.Method + " -> " + ov.Method,
	})
	s.emit("service_updated", map[string]any{"service": name, "method": ov.Method})
	s.log.Info("service override updated", "service", name, "method", ov.Method, "domains", ov.Domains)
	return ov, nil
}

// ============================================================================
// Backup / Restore
// ============================================================================

func (s *Syncer) Backup(ctx context.Context, name string) (*BackupFile, error) {
	entries, err := s.ListServiceEntries(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	return &BackupFile{
		Service:   name,
		List:      s.cfg.Firewall.AddressList,
		Comment:   addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name),
		CreatedAt: time.Now().UTC(),
		Entries:   entries,
	}, nil
}

func (s *Syncer) Restore(ctx context.Context, name string, entries []addresslist.Entry) error {
	existing, err := s.ListServiceEntries(ctx, name)
	if err != nil {
		s.log.Warn("failed to list existing entries for duplicate check", "service", name, "err", err)
	}
	existingSet := make(map[string]bool, len(existing))
	for _, e := range existing {
		if n, err := addresslist.NormalizeAddress(e.Address); err == nil {
			existingSet[n] = true
		}
	}

	list := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name)

	restored, skipped, failures := 0, 0, 0
	for _, e := range entries {
		norm, err := addresslist.NormalizeAddress(e.Address)
		if err != nil {
			failures++
			s.log.Error("invalid address in restore", "service", name, "address", e.Address, "err", err)
			continue
		}
		if existingSet[norm] {
			skipped++
			continue
		}
		in := addresslist.Entry{Address: norm, List: list, Comment: comment, Disabled: "false"}
		if _, err := s.mt().AddEntry(ctx, in); err != nil {
			failures++
			s.log.Error("failed to restore entry", "service", name, "address", norm, "err", err)
			continue
		}
		restored++
		existingSet[norm] = true
	}

	s.log.Info("restore completed", "service", name,
		"restored", restored, "skipped_duplicates", skipped, "errors", failures, "total", len(entries))

	if failures > 0 {
		return fmt.Errorf("restore completed with errors: restored %d, skipped %d, errors %d out of %d",
			restored, skipped, failures, len(entries))
	}
	return nil
}

// ============================================================================
// Утилиты
// ============================================================================
