package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Duration — обёртка над time.Duration для YAML-парсинга строк вида "30s", "1h".
type Duration time.Duration

// UnmarshalYAML декодирует строку вида "30s" в Duration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(dur)
	return nil
}

// MarshalYAML кодирует Duration в строку для YAML.
func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// Duration возвращает нативный time.Duration.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// Config — корневая структура конфигурации приложения.
//
// Потокобезопасность: все обращения к полям идут под mu (RLock для чтения,
// Lock для записи). Правило: мьютекс берут только точки входа (веб-хендлеры,
// методы Syncer/Scheduler/Bot, SIGHUP), но никогда — вложенные хелперы,
// иначе возможен дедлок при ожидающем писателе (рекурсивный RLock).
type Config struct {
	mu        sync.RWMutex `yaml:"-"`
	path      string       `yaml:"-"`
	Timezone  string       `yaml:"timezone"`
	CachePath string       `yaml:"cache_path"`

	Logging   LoggingConfig   `yaml:"logging"`
	MikroTik  MikroTikConfig  `yaml:"mikrotik"`
	Firewall  FirewallConfig  `yaml:"firewall"`
	Telegram  TelegramConfig  `yaml:"telegram"`
	Web       WebConfig       `yaml:"web"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Safety    SafetyConfig    `yaml:"safety"`
	Retry     RetryConfig     `yaml:"retry"`
	External  ExternalConfig  `yaml:"external"`
	Snapshots SnapshotsConfig `yaml:"snapshots"`
	Schedules SchedulesConfig `yaml:"schedules"`
	Suggest   SuggestConfig   `yaml:"suggest"`
	Presets   PresetsConfig   `yaml:"presets"`

	// CDNSources — каталог официальных списков CDN: по ASN выбирается URL и
	// парсер ответа. Пусто — встроенные дефолты (setDefaults). Вынесен из
	// кода, чтобы менять/добавлять провайдеров без пересборки.
	CDNSources []CDNSourceConfig `yaml:"cdn_sources"`

	Services  []string                   `yaml:"services"`
	Overrides map[string]ServiceOverride `yaml:"overrides"`
}

// CDNSourceConfig — один официальный источник диапазонов CDN-провайдера.
// Service — имя сервиса-алиаса (cloudflare/aws/google/fastly): по нему
// classifier-сервис без домена получает ASN. ASN — ключ каталога.
// Parser — text|google|aws|fastly: какой формат ответа разбирать.
type CDNSourceConfig struct {
	Service string `yaml:"service"`
	Name    string `yaml:"name"`
	ASN     int    `yaml:"asn"`
	URL     string `yaml:"url"`
	Parser  string `yaml:"parser"`
}

// DefaultCDNSources возвращает копию встроенного каталога CDN. Используется
// как значение по умолчанию, когда cdn_sources в конфиге не задан.
// Akamai здесь нет: у него нет официального списка, сбор идёт методом asn.
func DefaultCDNSources() []CDNSourceConfig {
	return []CDNSourceConfig{
		{Service: "cloudflare", Name: "Cloudflare", ASN: 13335,
			URL: "https://www.cloudflare.com/ips-v4", Parser: "text"},
		{Service: "aws", Name: "AWS CloudFront", ASN: 16509,
			URL: "https://ip-ranges.amazonaws.com/ip-ranges.json", Parser: "aws"},
		{Service: "google", Name: "Google", ASN: 15169,
			URL: "https://www.gstatic.com/ipranges/goog.json", Parser: "google"},
		{Service: "fastly", Name: "Fastly", ASN: 54113,
			URL: "https://api.fastly.com/public-ip-list", Parser: "fastly"},
	}
}

// LoggingConfig — настройки логирования (slog + lumberjack).
type LoggingConfig struct {
	Level      string `yaml:"level"`
	File       string `yaml:"file"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxFiles   int    `yaml:"max_files"`
	MaxTotalMB int    `yaml:"max_total_mb"`
	Compress   bool   `yaml:"compress"`
	AlsoStdout bool   `yaml:"also_stdout"`
}

// MikroTikConfig — параметры подключения к RouterOS REST API.
type MikroTikConfig struct {
	Host      string   `yaml:"host"`
	Port      int      `yaml:"port"`
	Username  string   `yaml:"username"`
	Password  string   `yaml:"password"`
	UseSSL    bool     `yaml:"use_ssl"`
	VerifySSL bool     `yaml:"verify_ssl"`
	Timeout   Duration `yaml:"timeout"`
	RateLimit int      `yaml:"rate_limit"`
}

// FirewallConfig — целевая модель управления: Firewall Address List.
// Один глобальный address-list для всех сервисов; изоляция по comment.
type FirewallConfig struct {
	AddressList    string `yaml:"address_list"`    // например TO-VPN
	CommentPrefix  string `yaml:"comment_prefix"`  // например AUTO
	IgnoreDynamic  bool   `yaml:"ignore_dynamic"`  // не трогать dynamic-записи
	ManageDisabled bool   `yaml:"manage_disabled"` // создавать записи disabled=false
}

// TelegramConfig — настройки Telegram-бота.
type TelegramConfig struct {
	Enabled           bool               `yaml:"enabled"`
	BotToken          string             `yaml:"bot_token"`
	ChatID            string             `yaml:"chat_id"`
	AuthorizedChatIDs []string           `yaml:"authorized_chat_ids"`
	RateLimit         int                `yaml:"rate_limit"`
	WeeklyReport      WeeklyReportConfig `yaml:"weekly_report"`
	Buttons           ButtonsConfig      `yaml:"buttons"`
}

// WeeklyReportConfig — настройки еженедельного отчёта.
type WeeklyReportConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Schedule string `yaml:"schedule"`
}

// ButtonsConfig — настройки inline-кнопок бота.
type ButtonsConfig struct {
	Enabled             bool `yaml:"enabled"`
	MaxSelectedServices int  `yaml:"max_selected_services"`
}

// WebConfig — настройки веб-интерфейса и REST API.
type WebConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Listen          string        `yaml:"listen"`
	AllowedCIDRs    []string      `yaml:"allowed_cidrs"`
	TrustedProxies  []string      `yaml:"trusted_proxies"`
	Auth            WebAuthConfig `yaml:"auth"`
	SessionTimeout  Duration      `yaml:"session_timeout"`
	CSRFEnabled     bool          `yaml:"csrf_enabled"`
	SecurityHeaders bool          `yaml:"security_headers"`
}

// WebAuthConfig — настройки HTTP Basic Auth.
type WebAuthConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// SchedulerConfig — настройки планировщика.
type SchedulerConfig struct {
	Parallel       bool     `yaml:"parallel"`
	MaxConcurrent  int      `yaml:"max_concurrent"`
	ReloadInterval Duration `yaml:"reload_interval"`
	CacheTTL       Duration `yaml:"cache_ttl"`
	CachePurge     string   `yaml:"cache_purge"`
}

// SafetyConfig — параметры безопасности (только IPv4).
// Проект сознательно не поддерживает IPv6.
type SafetyConfig struct {
	MaxDeleteRatio          float64 `yaml:"max_delete_ratio"`
	RequireConfirmationOver int     `yaml:"require_confirmation_over"`
	MinPrefixV4             int     `yaml:"min_prefix_v4"`
	// AllowHostRoutes — разрешить хостовые /32 из dynamic/whois-сбора.
	// Указатель, чтобы дефолт был «разрешено»: nil = true. dynamic отдаёт
	// только /32, whois гарантирует покрытие резолвленных IP хостами —
	// запрет по умолчанию ломал бы оба метода целиком.
	AllowHostRoutes *bool `yaml:"allow_host_routes"`
	MaxASNPrefixes  int   `yaml:"max_asn_prefixes"`
	// WarnBroadV4 — warn-порог ширины префикса (только отчёт, не отсев:
	// официальные списки CDN содержат /13–/19 — это нормально и не
	// должно ломать сбор; дефолт 20).
	WarnBroadV4 int `yaml:"warn_broad_v4"`
	// VerifySNI — SNI-проба после сбора (TLS-handshake доказывает, что адреса
	// обслуживают домен). Только варнинги, сбор не блокируется.
	// Указатель, чтобы дефолт был «включено»: nil = true.
	VerifySNI *bool `yaml:"verify_sni"`
}

// RetryConfig — настройки повторных попыток (exponential backoff).
type RetryConfig struct {
	MaxAttempts int      `yaml:"max_attempts"`
	BaseDelay   Duration `yaml:"base_delay"`
	MaxDelay    Duration `yaml:"max_delay"`
	Jitter      bool     `yaml:"jitter"`
}

// ExternalConfig — настройки внешних API.
type ExternalConfig struct {
	HTTPTimeout   Duration `yaml:"http_timeout"`
	MaxResponseMB int      `yaml:"max_response_mb"`
	// BGPToolsContact — email или "название + URL/почта" для User-Agent
	// запросов к bgp.tools (требование их API: описательный UA со способом связи).
	BGPToolsContact string   `yaml:"bgp_tools_contact"`
	AkamaiAPIKey    string   `yaml:"akamai_api_key"`
	UrlscanAPIKey   string   `yaml:"urlscan_api_key"`
	RDAPTimeout     Duration `yaml:"rdap_timeout"`
	Resolver        string   `yaml:"resolver"`
	// FallbackResolvers — резервные DNS при недоступности основного
	// (пусто — публичные 8.8.8.8:53 и 1.1.1.1:53).
	FallbackResolvers []string `yaml:"fallback_resolvers"`
}

// SnapshotsConfig — настройки снапшотов маршрутов.
type SnapshotsConfig struct {
	Enabled  bool     `yaml:"enabled"`
	TTL      Duration `yaml:"ttl"`
	MaxCount int      `yaml:"max_count"`
}

// SchedulesConfig — расписания (три уровня: service -> group -> global).
type SchedulesConfig struct {
	Global   string                     `yaml:"global"`
	Groups   map[string]GroupConfig     `yaml:"groups"`
	Services map[string]ServiceSchedule `yaml:"services"`
}

// GroupConfig — конфигурация группы сервисов.
type GroupConfig struct {
	Schedule string   `yaml:"schedule"`
	Services []string `yaml:"services"`
}

// ServiceSchedule — расписание конкретного сервиса.
type ServiceSchedule struct {
	Schedule string `yaml:"schedule"`
}

// SuggestConfig — подбор доменов для сервисов (советник, не синк).
type SuggestConfig struct {
	// V2flyEnabled — каталог пресетов v2fly в выборе.
	V2flyEnabled bool `yaml:"v2fly_enabled"`
	// RuEnabled — RU-пресет (antifilter) в выборе (список огромный).
	RuEnabled bool `yaml:"ru_enabled"`
	// UrlscanMode — "off"|"key"|"anonymous" (ключ — external.urlscan_api_key).
	UrlscanMode string `yaml:"urlscan_mode"`
	// Sources — порядок источников: preset, urlscan (manual доступен всегда).
	Sources []string `yaml:"sources"`
}

// PresetsConfig — локальное зеркало пресетов.
type PresetsConfig struct {
	// UpdateSchedule — расписание обновления зеркала (cron/every/daily,
	// пусто — только вручную).
	UpdateSchedule string `yaml:"update_schedule"`
}

// ServiceOverride — переопределения для конкретного сервиса.
type ServiceOverride struct {
	Method  string   `yaml:"method"`
	Domains []string `yaml:"domains"`
	// Primary — витринный домен (первый для ASN-резолва и SNI); пусто — domains[0].
	Primary string `yaml:"primary,omitempty"`
	// Preset — слаг пресета, из которого скопированы Generated.
	Preset string `yaml:"preset,omitempty"`
	// Generated — последний применённый снимок пресета (для refresh без
	// потери ручных доменов: ручные = Domains минус Generated).
	Generated       []string `yaml:"generated,omitempty"`
	StaticURL       string   `yaml:"static_url"`
	AlsoCDN         []string `yaml:"also_cdn"`
	MaxASNPrefixes  int      `yaml:"max_asn_prefixes"`
	MaxPrefixes     int      `yaml:"max_prefixes"`
	Exclude         []string `yaml:"exclude"`
	IncludeOnly     []string `yaml:"include_only"`
	RefreshInterval string   `yaml:"refresh_interval"`
}

// Load читает YAML-файл, применяет ENV, устанавливает defaults, валидирует.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	c.path = path

	// ENV overrides (приоритет выше config.yaml)
	if p := os.Getenv("MRS_MIKROTIK_PASSWORD"); p != "" {
		c.MikroTik.Password = p
	}
	if t := os.Getenv("MRS_TELEGRAM_BOT_TOKEN"); t != "" {
		c.Telegram.BotToken = t
	}
	if wp := os.Getenv("MRS_WEB_PASSWORD"); wp != "" {
		c.Web.Auth.Password = wp
	}
	if contact := os.Getenv("MRS_BGP_TOOLS_CONTACT"); contact != "" {
		c.External.BGPToolsContact = contact
	}
	if ak := os.Getenv("MRS_AKAMAI_API_KEY"); ak != "" {
		c.External.AkamaiAPIKey = ak
	}
	if uk := os.Getenv("MRS_URLSCAN_API_KEY"); uk != "" {
		c.External.UrlscanAPIKey = uk
	}

	// Проверка прав файла (обязательно 0600).
	// На Windows файловые права NTFS не отображаются в unix-бите,
	// поэтому проверка выполняется только на POSIX-системах
	// либо когда явная проверка включена через MRS_REQUIRE_FILE_PERMS=1.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if err := checkFilePermissions(path); err != nil {
		return nil, err
	}

	setDefaults(&c)

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}
	return &c, nil
}

// checkFilePermissions требует права 0600 на POSIX-системах.
// На Windows (где NTFS-права не отображаются в unix-бите) проверка
// пропускается, если явно не включена через MRS_REQUIRE_FILE_PERMS=1.
// MRS_REQUIRE_FILE_PERMS=0 отключает проверку принудительно (например,
// в Docker Desktop на Windows bind-mount всегда показывает 0777).
func checkFilePermissions(path string) error {
	switch os.Getenv("MRS_REQUIRE_FILE_PERMS") {
	case "0":
		return nil
	case "1":
	default:
		if runtime.GOOS == "windows" {
			return nil
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("config %s: required 0600, got %o", path, fi.Mode().Perm())
	}
	return nil
}

// setDefaults устанавливает значения по умолчанию для пропущенных параметров.
func setDefaults(c *Config) {
	if c.CachePath == "" {
		c.CachePath = "cache.db"
	}
	if c.Scheduler.MaxConcurrent == 0 {
		c.Scheduler.MaxConcurrent = 3
	}
	if c.Safety.MaxDeleteRatio == 0 {
		c.Safety.MaxDeleteRatio = 0.5
	}
	// Только IPv4: минимум /8 (PROMPT XIV). IPv6 не поддерживается.
	// Дефолт осознанно широкий: официальные списки CDN (Cloudflare /13)
	// должны собираться; сужать — только явно под свою ответственность.
	if c.Safety.MinPrefixV4 == 0 {
		c.Safety.MinPrefixV4 = 8
	}
	// Warn-порог ширины (только отчёт): префиксы шире — в отчёт sync/recommend.
	if c.Safety.WarnBroadV4 == 0 {
		c.Safety.WarnBroadV4 = 20
	}
	if c.Safety.MaxASNPrefixes == 0 {
		c.Safety.MaxASNPrefixes = 100
	}
	// Хостовые /32 разрешены по умолчанию (nil = true): материализуем,
	// чтобы GetPath/UI показывали фактическое значение, а Save фиксировал.
	if c.Safety.AllowHostRoutes == nil {
		t := true
		c.Safety.AllowHostRoutes = &t
	}
	if c.Firewall.CommentPrefix == "" {
		c.Firewall.CommentPrefix = "AUTO"
	}
	if c.MikroTik.Port == 0 {
		if c.MikroTik.UseSSL {
			c.MikroTik.Port = 443
		} else {
			c.MikroTik.Port = 80
		}
	}
	if c.External.MaxResponseMB == 0 {
		c.External.MaxResponseMB = 50
	}
	if c.Snapshots.MaxCount == 0 {
		c.Snapshots.MaxCount = 50
	}
	if c.Snapshots.TTL.Duration() == 0 {
		c.Snapshots.TTL = Duration(7 * 24 * time.Hour)
	}
	if c.Web.SessionTimeout.Duration() == 0 {
		c.Web.SessionTimeout = Duration(24 * time.Hour)
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if c.Web.Listen == "" {
		c.Web.Listen = "127.0.0.1:8080"
	}
	if c.Scheduler.CacheTTL.Duration() == 0 {
		c.Scheduler.CacheTTL = Duration(24 * time.Hour)
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 3
	}
	if c.Retry.BaseDelay.Duration() == 0 {
		c.Retry.BaseDelay = Duration(time.Second)
	}
	if c.Retry.MaxDelay.Duration() == 0 {
		c.Retry.MaxDelay = Duration(30 * time.Second)
	}
	if c.External.HTTPTimeout.Duration() == 0 {
		c.External.HTTPTimeout = Duration(15 * time.Second)
	}
	if c.External.RDAPTimeout.Duration() == 0 {
		c.External.RDAPTimeout = Duration(10 * time.Second)
	}
	if c.MikroTik.Timeout.Duration() == 0 {
		c.MikroTik.Timeout = Duration(30 * time.Second)
	}
	// Подбор доменов по умолчанию выключен: внешние вызовы (urlscan) и
	// гигантский RU-список включаются осознанно (bool по нулям — off).
	if c.Suggest.UrlscanMode == "" {
		c.Suggest.UrlscanMode = "off"
	}
	if len(c.Suggest.Sources) == 0 {
		c.Suggest.Sources = []string{"preset", "urlscan"}
	}
	// Каталог CDN вынесен в конфиг: пусто — вкомпилированные дефолты.
	// Так встроенные CDN-сервисы (cloudflare/aws/...) не остаются без
	// источника, если секцию убрали/очистили.
	if len(c.CDNSources) == 0 {
		c.CDNSources = DefaultCDNSources()
	}
}

// RLock/RUnlock — чтение полей под разделяемой блокировкой.
// Lock/Unlock — запись. См. правило в комментарии к Config.
func (c *Config) RLock()   { c.mu.RLock() }
func (c *Config) RUnlock() { c.mu.RUnlock() }
func (c *Config) Lock()    { c.mu.Lock() }
func (c *Config) Unlock()  { c.mu.Unlock() }

// ReplaceFromLocked — замена без блокировки (вызывающий держит Lock).
// Отдельного ReplaceFrom с внутренним Lock нет специально: вызов его
// из-под уже взятого Lock дал бы self-deadlock.
func (c *Config) ReplaceFromLocked(src *Config) {
	c.replaceFromLocked(src)
}

func (c *Config) replaceFromLocked(src *Config) {
	c.Timezone = src.Timezone
	c.CachePath = src.CachePath
	c.Logging = src.Logging
	c.MikroTik = src.MikroTik
	c.Firewall = src.Firewall
	c.Telegram = src.Telegram
	c.Web = src.Web
	c.Scheduler = src.Scheduler
	c.Safety = src.Safety
	c.Retry = src.Retry
	c.External = src.External
	c.Snapshots = src.Snapshots
	c.Schedules = src.Schedules
	c.Suggest = src.Suggest
	c.Presets = src.Presets
	c.CDNSources = src.CDNSources
	c.Services = src.Services
	c.Overrides = src.Overrides
}

// SnapshotSafety возвращает копию Safety с собственными флагами:
// Set пишет в pointee напрямую, поэтому сами указатели копировать нельзя.
func (c *Config) SnapshotSafety() SafetyConfig {
	c.RLock()
	defer c.RUnlock()
	out := c.Safety
	if c.Safety.AllowHostRoutes != nil {
		v := *c.Safety.AllowHostRoutes
		out.AllowHostRoutes = &v
	}
	if c.Safety.VerifySNI != nil {
		v := *c.Safety.VerifySNI
		out.VerifySNI = &v
	}
	return out
}

// Validate проверяет корректность конфигурации.
func (c *Config) Validate() error {
	if c.MikroTik.Host == "" {
		return fmt.Errorf("mikrotik.host required")
	}
	if c.MikroTik.Username == "" {
		return fmt.Errorf("mikrotik.username required")
	}
	// Firewall Address List — единственный целевой объект управления.
	if c.Firewall.AddressList == "" {
		return fmt.Errorf("firewall.address_list required (например TO-VPN)")
	}
	if !ValidateAddressListName(c.Firewall.AddressList) {
		return fmt.Errorf("firewall.address_list %q: allowed ^[A-Za-z0-9_.-]{1,63}$", c.Firewall.AddressList)
	}
	if c.Schedules.Global == "" {
		return fmt.Errorf("schedules.global required")
	}
	// Диапазоны safety: мусор (отрицательные, нулевые маски) ловим здесь,
	// а не молчаливым отключением защит (MaxDeleteRatio<=0, например,
	// незаметно выключал бы safe-diff).
	if c.Safety.MinPrefixV4 < 8 || c.Safety.MinPrefixV4 > 32 {
		return fmt.Errorf("safety.min_prefix_v4 must be 8..32, got %d", c.Safety.MinPrefixV4)
	}
	if c.Safety.WarnBroadV4 < 8 || c.Safety.WarnBroadV4 > 32 {
		return fmt.Errorf("safety.warn_broad_v4 must be 8..32, got %d", c.Safety.WarnBroadV4)
	}
	if c.Safety.MaxASNPrefixes < 1 || c.Safety.MaxASNPrefixes > 10000 {
		return fmt.Errorf("safety.max_asn_prefixes must be 1..10000, got %d", c.Safety.MaxASNPrefixes)
	}
	if c.Safety.MaxDeleteRatio < 0 || c.Safety.MaxDeleteRatio > 1 {
		return fmt.Errorf("safety.max_delete_ratio must be 0..1, got %v", c.Safety.MaxDeleteRatio)
	}
	if c.Safety.RequireConfirmationOver < 0 {
		return fmt.Errorf("safety.require_confirmation_over must be >= 0, got %d", c.Safety.RequireConfirmationOver)
	}
	// Типичная ошибка: http:// на TLS-порт (или наоборот) — коннект виснет по
	// таймауту и circuit breaker открывается; ловим на этапе валидации.
	if !c.MikroTik.UseSSL && c.MikroTik.Port == 443 {
		return fmt.Errorf("mikrotik: use_ssl=false с port=443 — http на HTTPS-порту не заработает (поставьте use_ssl: true или port: 80)")
	}
	if c.Logging.File != "" && runtime.GOOS != "windows" && os.Getenv("MRS_REQUIRE_FILE_PERMS") != "0" {
		dir := filepath.Dir(c.Logging.File)
		if fi, err := os.Stat(dir); err == nil {
			// «Слишком открыто» = запись для группы/окружающих (world/group writable)
			if fi.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("log dir %s too open: %o", dir, fi.Mode().Perm())
			}
		}
	}
	if c.Web.Enabled && c.Web.Auth.Enabled {
		if c.Web.Auth.Username == "" || c.Web.Auth.Password == "" {
			return fmt.Errorf("web.auth.username/password required")
		}
	}
	// Валидация имён сервисов и overrides (PROMPT XI.9).
	for _, name := range c.Services {
		if !ValidateServiceName(name) {
			return fmt.Errorf("invalid service name %q (allowed ^[a-z0-9][a-z0-9_-]{0,63}$)", name)
		}
	}
	for name, ov := range c.Overrides {
		if !ValidateServiceName(name) {
			return fmt.Errorf("invalid override service name %q", name)
		}
		if ov.Method != "" {
			if err := ValidateMethods(ov.Method); err != nil {
				return fmt.Errorf("service %q: %v", name, err)
			}
		}
		if ov.StaticURL != "" {
			u, err := url.Parse(ov.StaticURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("service %q: static_url must be http/https", name)
			}
		}
	}
	switch c.Suggest.UrlscanMode {
	case "", "off", "key", "anonymous":
	default:
		return fmt.Errorf("suggest.urlscan_mode must be off|key|anonymous, got %q", c.Suggest.UrlscanMode)
	}
	for _, src := range c.Suggest.Sources {
		if src != "preset" && src != "urlscan" && src != "manual" {
			return fmt.Errorf("suggest.sources must be preset|urlscan|manual, got %q", src)
		}
	}
	if c.Suggest.UrlscanMode == "key" && c.External.UrlscanAPIKey == "" {
		return fmt.Errorf("suggest.urlscan_mode=key requires external.urlscan_api_key")
	}
	if err := validateCDNSources(c.CDNSources); err != nil {
		return err
	}
	return nil
}

// cdnParsers — поддерживаемые парсеры ответа CDN-источника.
var cdnParsers = map[string]bool{"text": true, "google": true, "aws": true, "fastly": true}

// validateCDNSources проверяет каталог cdn_sources: корректные service/name,
// ASN, URL, парсер; без дублей service и ASN (иначе lookup неоднозначен).
func validateCDNSources(list []CDNSourceConfig) error {
	seenSvc := map[string]bool{}
	seenASN := map[int]bool{}
	for i, s := range list {
		svc := strings.ToLower(strings.TrimSpace(s.Service))
		if !ValidateServiceName(svc) {
			return fmt.Errorf("cdn_sources[%d]: invalid service %q (allowed ^[a-z0-9][a-z0-9_-]{0,63}$)", i, s.Service)
		}
		if s.Name == "" {
			return fmt.Errorf("cdn_sources[%d] (%s): name required", i, svc)
		}
		if s.ASN <= 0 || s.ASN > 4294967295 {
			return fmt.Errorf("cdn_sources[%d] (%s): asn must be 1..4294967295, got %d", i, svc, s.ASN)
		}
		if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("cdn_sources[%d] (%s): url must be http/https", i, svc)
		}
		if !cdnParsers[s.Parser] {
			return fmt.Errorf("cdn_sources[%d] (%s): parser must be text|google|aws|fastly, got %q", i, svc, s.Parser)
		}
		if seenSvc[svc] {
			return fmt.Errorf("cdn_sources: duplicate service %q", svc)
		}
		seenSvc[svc] = true
		if seenASN[s.ASN] {
			return fmt.Errorf("cdn_sources: duplicate asn %d", s.ASN)
		}
		seenASN[s.ASN] = true
	}
	return nil
}

// knownMethods — допустимые методы сбора (PROMPT II.2).
var knownMethods = map[string]bool{
	"cdn": true, "asn": true, "dynamic": true,
	"whois": true, "static_url": true,
}

// SplitMethods разбивает строку методов на части. Составные методы
// ("dynamic,whois", "dynamic+cdn") собираются по очереди, результат
// склеивается через "+" в usedMethod. Разделители: запятая, точка
// с запятой, плюс, пробельные символы.
func SplitMethods(s string) []string {
	var out []string
	for _, m := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '+' || unicode.IsSpace(r)
	}) {
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

// ValidateMethods проверяет список методов: каждая часть должна входить
// в известный набор (cdn, asn, dynamic, whois, static_url). Пустые
// сегменты ("dynamic,,whois") — ошибка: обычно это опечатка.
func ValidateMethods(s string) error {
	parts := SplitMethods(s)
	if len(parts) == 0 {
		return fmt.Errorf("empty method list")
	}
	// Ловим пустые сегменты явно (опечатки вида "dynamic,,whois").
	seps := 0
	for _, r := range s {
		if r == ',' || r == ';' || r == '+' {
			seps++
		}
	}
	if seps >= len(parts) {
		return fmt.Errorf("empty method in list %q", s)
	}
	for _, m := range parts {
		if !knownMethods[m] {
			return fmt.Errorf("unknown method %q in list %q (allowed: cdn, asn, dynamic, whois, static_url)", m, s)
		}
	}
	return nil
}

// IsValidMethod проверяет, что метод (или составной список через ",;+")
// входит в известный набор (cdn, asn, dynamic, whois, static_url).
func IsValidMethod(m string) bool { return ValidateMethods(m) == nil }

// VerifySNIEnabled — включена ли SNI-проба (дефолт true: nil = включено).
func (s SafetyConfig) VerifySNIEnabled() bool {
	return s.VerifySNI == nil || *s.VerifySNI
}

// AllowHostRoutesEnabled — разрешены ли хостовые /32 (дефолт true:
// nil = разрешено). Явный false отбрасывает /32 в валидации.
func (s SafetyConfig) AllowHostRoutesEnabled() bool {
	return s.AllowHostRoutes == nil || *s.AllowHostRoutes
}

// EffectiveSchedule возвращает эффективное расписание сервиса.
// Приоритет: service -> group -> global.
func (c *Config) EffectiveSchedule(service string) string {
	if c.Schedules.Services != nil {
		if s, ok := c.Schedules.Services[service]; ok && s.Schedule != "" {
			return s.Schedule
		}
	}
	for _, g := range c.Schedules.Groups {
		for _, s := range g.Services {
			if s == service && g.Schedule != "" {
				return g.Schedule
			}
		}
	}
	return c.Schedules.Global
}

// ServicesInGroup возвращает список сервисов в группе.
func (c *Config) ServicesInGroup(group string) []string {
	if g, ok := c.Schedules.Groups[group]; ok {
		return g.Services
	}
	return nil
}

// ScheduleFor — алиас для EffectiveSchedule.
func (c *Config) ScheduleFor(service string) string {
	return c.EffectiveSchedule(service)
}

// serviceNameRE — регулярное выражение для валидации имени сервиса.
// Точка разрешена: сервис называют доменом (youtube.com, instagram.com) или
// IPv4-адресом (8.8.8.8) — classifier сам определит тип по имени (PROMPT I).
// Дефис/undercore в конце запрещены, чтобы AUTO:-комментарии и ключи bbolt
// оставались предсказуемыми.
var serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}(?:\.[a-z0-9][a-z0-9_-]{0,62})*$`)

// ValidateServiceName проверяет корректность имени сервиса (1..253 символов,
// как доменное имя: метки через точку).
func ValidateServiceName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	return serviceNameRE.MatchString(name)
}

// addressListNameRE — безопасное имя Firewall Address List (PROMPT XII.12.9).
var addressListNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

// ValidateAddressListName проверяет имя глобального address-list.
func ValidateAddressListName(name string) bool {
	return addressListNameRE.MatchString(name)
}

// commentPrefixRE — префикс AUTO-комментария: без пробелов/кавычек/слэшей.
var commentPrefixRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,16}$`)

// ValidateCommentPrefix проверяет firewall.comment_prefix.
func ValidateCommentPrefix(prefix string) bool {
	return commentPrefixRE.MatchString(prefix)
}

// IsSensitiveKey проверяет, является ли ключ чувствительным (для redaction).
func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	sensitive := []string{"password", "token", "secret", "key", "api_key",
		"bot_token", "authorization", "cookie", "session"}
	for _, s := range sensitive {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}
