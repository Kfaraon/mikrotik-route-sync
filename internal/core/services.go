package core

// Управление жизненным циклом сервисов: добавление, удаление, информация,
// бэкап/восстановление записей.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/presets"
	"github.com/Kfaraon/mikrotik-route-sync/internal/validator"
)

// addDetectTimeout — предел на автоопределение CDN при добавлении сервиса.
// Детект best-effort: не уложился — сервис добавляется с обычным методом.
const addDetectTimeout = 20 * time.Second

// addDetectDomains — сколько доменов проверяет пробник при добавлении.
// У пресета бывает десятки доменов одного провайдера: подтвердить CDN
// достаточно по витринному и нескольким ближайшим, а полный обход не
// укладывался в addDetectTimeout и оставлял сервис на широком whois.
const addDetectDomains = 8

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
		s.log.Warn("не удалось получить записи для информации о сервисе", "service", name, "service_id", s.ServiceID(name), "err", err)
	} else {
		info.EntryCount = len(entries)
	}

	snapshots, err := s.cache.ListSnapshots(name)
	if err != nil {
		s.log.Warn("не удалось получить снапшоты для информации о сервисе", "service", name, "service_id", s.ServiceID(name), "err", err)
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
		return fmt.Errorf("unknown method %q (allowed: cdn, asn, dynamic, whois, static_url, preset; comma-separated lists like \"dynamic,whois\" accepted)", override.Method)
	}

	// Пресет навязывает метод сбора (домены → preset, IP-лист → static_url)
	// и приоритетнее переданного: сервис с пресетом собирается строго по его
	// содержимому, поэтому автоопределение CDN ниже не запускается.
	override.Method = effectiveMethod(override)

	// UUID текущего воплощения: присваивается этому добавлению (не сервису
	// вообще) и переживёт удаление только как чужая история/логи. Нужен уже
	// на этапе автоопределения, поэтому генерируем сразу (в конфиг попадёт
	// при успешном Save).
	newID := newServiceID()

	// Автоопределение источника: если метод не задан явно — проверяем, не
	// обслуживается ли домен за CDN (Cloudflare и др.), и сразу ставим
	// метод cdn. Ручной выбор (метод/static_url) не переопределяется;
	// сбой проверки не блокирует добавление (детект best-effort).
	//
	// Детект ограничен таймаутом и не выполняет дорогое whois-сравнение
	// (WithSkipWhois): иначе добавление крупного сервиса держало пользователя
	// минутами (скачивание bgp.tools на каждый ASN ради цифры в отчёте).
	var detected *CDNAdvice
	if override.Method == "" && override.StaticURL == "" {
		s.clearAddProgress(name)
		defer s.clearAddProgress(name)
		dctx, cancel := context.WithTimeout(ctx, addDetectTimeout)
		adv := s.AdviseCDN(dctx, name, override,
			WithSkipWhois(),
			WithProbeDomains(addDetectDomains),
			WithReporter(func(detail string) { s.setAddProgress(name, detail) }),
		)
		// dctx.Err() читаем ДО cancel(): после cancel() он всегда
		// context.Canceled, и предупреждение логировалось бы всегда.
		timedOut := errors.Is(dctx.Err(), context.DeadlineExceeded)
		cancel()
		if timedOut {
			s.log.Warn("автоопределение CDN прервано по таймауту, добавляем с методом по умолчанию",
				"service", name, "service_id", newID, "timeout", addDetectTimeout)
		}
		if adv.Detected {
			override.Method = "cdn"
			// Фиксируем домены проверки — см. ApplyCDNRecommendation.
			if len(override.Domains) == 0 && len(adv.Domains) > 0 {
				override.Domains = append([]string(nil), adv.Domains...)
			}
			detected = adv
			s.log.Info("источник CDN определён автоматически",
				"service", name, "service_id", newID, "cdn", adv.CDN, "asn", adv.ASN, "url", adv.URL,
				"cdn_prefixes", adv.PrefixCount, "whois_prefixes", adv.WhoisCount)
		} else {
			s.log.Debug("автоопределение CDN пропущено", "service", name, "service_id", newID, "reason", adv.Reason)
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

	// Новый UUID v4 — идентичность текущего воплощения сервиса. Старые
	// история/логи остаются с чужим id и в UI текущего сервиса не попадут.
	if s.cfg.ServiceIDs == nil {
		s.cfg.ServiceIDs = make(map[string]string)
	}
	prevID, hadID := s.cfg.ServiceIDs[name]
	s.cfg.ServiceIDs[name] = newID

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
		if hadID {
			s.cfg.ServiceIDs[name] = prevID
		} else {
			delete(s.cfg.ServiceIDs, name)
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
				s.log.Warn("уведомление telegram об определении CDN не доставлено", "service", name, "service_id", newID, "err", err)
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
	// id запоминаем до мутации конфига: логи чистки ссылаются на воплощение,
	// которое удаляем (после delete ServiceID(name) уже пуст).
	removedID := s.cfg.ServiceIDs[name]
	s.cfg.RUnlock()
	if !found {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}

	// Фаза 1: purge записей на роутере — БЕЗ блокировок (сеть на минуты
	// вешала всё управление). При ошибке сервис НЕ удаляется (как раньше).
	// Гоночное окно: синк, стартовавший во время purge, может вернуть записи
	// обратно — они останутся сиротами (управляемость уже снята).
	if purgeRoutes {
		n, err := s.RemoveServiceEntries(ctx, name)
		if err != nil {
			return fmt.Errorf("purge address-list entries failed, service NOT removed from config: %w", err)
		}
		s.log.Info("записи адресов сервиса удалены с роутера", "service", name, "service_id", removedID, "count", n)
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
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}
	prevServices := s.cfg.Services
	prevOverride, hadOverride := s.cfg.Overrides[name]
	s.cfg.Services = newServices
	delete(s.cfg.Overrides, name)
	prevID, hadID := s.cfg.ServiceIDs[name]
	delete(s.cfg.ServiceIDs, name)

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
		if hadID {
			s.cfg.ServiceIDs[name] = prevID
		}
		if hadSched {
			if s.cfg.Schedules.Services == nil {
				s.cfg.Schedules.Services = map[string]config.ServiceSchedule{}
			}
			s.cfg.Schedules.Services[name] = prevSched
		}
		s.cfg.Unlock()
		return fmt.Errorf("config not saved, service NOT removed: %w", err)
	}
	s.cfg.Unlock()

	// Фаза 3: чистка производного состояния — у каждого свои мьютексы.
	// Снапшоты удаляются только после успешного сохранения конфигурации.
	ids, err := s.cache.ListSnapshots(name)
	if err != nil {
		s.log.Warn("не удалось получить снапшоты удаляемого сервиса", "service", name, "service_id", removedID, "err", err)
	} else {
		for _, info := range ids {
			if err := s.cache.DeleteSnapshot(name, info.ID); err != nil {
				s.log.Warn("не удалось удалить снапшот удаляемого сервиса", "service", name, "service_id", removedID, "snapshot", info.ID, "err", err)
			}
		}
	}

	// Флаг деградации сбрасывается: удалённый сервис не должен светиться
	// в дашборде и боте. История при этом сохраняется (для страницы логов).
	s.ClearDegraded(name)

	// Троттлинг SNI-алертов: сбрасываем метку, чтобы новое воплощение сервиса
	// с тем же именем снова могло уведомить о провале, не дожидаясь суток.
	s.sniMu.Lock()
	delete(s.sniAlerted, name)
	s.sniMu.Unlock()

	s.audit.LogServiceDelete("", "core", name)
	s.emit("service_removed", map[string]any{"service": name})
	return nil
}

// MaxServiceDomains — предел доменов на сервис: больше — это уже bulk,
// который dynamic-конвейер физически не тянет (годы DNS),
// а config.yaml раздувается на десятки мегабайт (маршалинг при каждом Save).
// Проверяется и в ядре (UpdateServiceOverride), и в веб-хендлерах.
const MaxServiceDomains = 5000

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
		return nil, nil, fmt.Errorf("service %s has no preset bound (pass preset explicitly)", name)
	}
	p, ok := presets.Domains(slug)
	if !ok {
		return nil, nil, fmt.Errorf("preset %q unavailable (mirror not downloaded?)", slug)
	}
	full, _ := presets.Get(slug)
	if full.Large && !force {
		return nil, nil, fmt.Errorf("preset %q large (%d domains): confirm with force=true", slug, len(p))
	}
	if len(p) > MaxServiceDomains {
		return nil, nil, fmt.Errorf("preset %q too large (%d domains, max %d)", slug, len(p), MaxServiceDomains)
	}

	inPreset := make(map[string]bool, len(p))
	for _, d := range p {
		inPreset[strings.ToLower(d)] = true
	}

	// Мутация под Lock со свежим чтением: между снапшотом выше и записью
	// параллельный PUT мог поменять override — применяем diff к актуальному.
	s.cfg.Lock()
	ovFresh, _ := s.cfg.Overrides[name]
	stillThere := false
	for _, svc := range s.cfg.Services {
		if svc == name {
			stillThere = true
			break
		}
	}
	if !stillThere {
		s.cfg.Unlock()
		return nil, nil, fmt.Errorf("service %s was removed during refresh", name)
	}
	generated := make(map[string]bool, len(ovFresh.Generated))
	for _, d := range ovFresh.Generated {
		generated[strings.ToLower(d)] = true
	}
	var userAdded []string
	for _, d := range ovFresh.Domains {
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
	oldSet := make(map[string]bool, len(ovFresh.Domains))
	for _, d := range ovFresh.Domains {
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
	for _, d := range ovFresh.Domains {
		if !newSet[strings.ToLower(d)] {
			removed = append(removed, d)
		}
	}

	primary := ovFresh.Primary
	if primary == "" && len(p) > 0 {
		primary = p[0]
	}
	nov := ovFresh
	nov.Domains = newDomains
	nov.Generated = append([]string(nil), p...)
	nov.Preset = slug
	nov.Primary = primary
	// Метод навязывает пресет (домены → preset, IP-лист → static_url):
	// держим override.Method согласованным с тем, что применяет сбор.
	if m := presets.CollectMethod(full); m != "" {
		nov.Method = m
	}

	cur, hadCur := s.cfg.Overrides[name]
	if s.cfg.Overrides == nil {
		s.cfg.Overrides = make(map[string]config.ServiceOverride)
	}
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
	s.log.Info("пресет обновлён", "service", name, "service_id", s.ServiceID(name), "preset", slug,
		"added", len(added), "removed", len(removed))
	return added, removed, nil
}

func (s *Syncer) ListServices() []string {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	return append([]string(nil), s.cfg.Services...)
}

// ServiceID — UUID v4 текущего воплощения сервиса (пусто, если не задан:
// сервисы, добавленные до появления идентификаторов).
func (s *Syncer) ServiceID(name string) string {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	return s.cfg.ServiceIDs[name]
}

// logFor возвращает логгер с полями сервиса, включая service_id (UUID
// воплощения): по нему UI отличает логи текущего сервиса от логов прежнего
// воплощения с тем же именем (после «удалил → добавил заново»).
func (s *Syncer) logFor(name string) *slog.Logger {
	return s.log.With("service", name, "service_id", s.ServiceID(name))
}

// ServiceSchedules — снимок эффективных расписаний одним чтением
// (для бота: поштучные RLock в цикле здесь не нужны).
func (s *Syncer) ServiceSchedules() map[string]string {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	out := make(map[string]string, len(s.cfg.Services))
	for _, svc := range s.cfg.Services {
		out[svc] = s.cfg.EffectiveSchedule(svc)
	}
	return out
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
		if len(clean) > MaxServiceDomains {
			return config.ServiceOverride{}, fmt.Errorf("too many domains (%d, max %d)", len(clean), MaxServiceDomains)
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
	s.log.Info("override сервиса обновлён", "service", name, "service_id", s.ServiceID(name), "method", ov.Method, "domains", ov.Domains)
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
		s.log.Warn("не удалось получить текущие записи для проверки дублей", "service", name, "service_id", s.ServiceID(name), "err", err)
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
			s.log.Error("некорректный адрес при восстановлении", "service", name, "service_id", s.ServiceID(name), "address", e.Address, "err", err)
			continue
		}
		if existingSet[norm] {
			skipped++
			continue
		}
		in := addresslist.Entry{Address: norm, List: list, Comment: comment, Disabled: "false"}
		if _, err := s.mt().AddEntry(ctx, in); err != nil {
			failures++
			s.log.Error("не удалось восстановить запись", "service", name, "service_id", s.ServiceID(name), "address", norm, "err", err)
			continue
		}
		restored++
		existingSet[norm] = true
	}

	s.log.Info("восстановление завершено", "service", name, "service_id", s.ServiceID(name),
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
