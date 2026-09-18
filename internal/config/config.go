package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
type Config struct {
	path      string `yaml:"-"`
	Timezone  string `yaml:"timezone"`
	CachePath string `yaml:"cache_path"`

	Logging   LoggingConfig    `yaml:"logging"`
	MikroTik  MikroTikConfig   `yaml:"mikrotik"`
	Telegram  TelegramConfig   `yaml:"telegram"`
	Web       WebConfig        `yaml:"web"`
	Scheduler SchedulerConfig  `yaml:"scheduler"`
	Safety    SafetyConfig     `yaml:"safety"`
	Retry     RetryConfig      `yaml:"retry"`
	External  ExternalConfig   `yaml:"external"`
	Snapshots SnapshotsConfig  `yaml:"snapshots"`
	Schedules SchedulesConfig  `yaml:"schedules"`

	Services  []string                   `yaml:"services"`
	Overrides map[string]ServiceOverride `yaml:"overrides"`
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
	Host          string   `yaml:"host"`
	Port          int      `yaml:"port"`
	Username      string   `yaml:"username"`
	Password      string   `yaml:"password"`
	UseSSL        bool     `yaml:"use_ssl"`
	VerifySSL     bool     `yaml:"verify_ssl"`
	Timeout       Duration `yaml:"timeout"`
	Gateway       string   `yaml:"gateway"`
	RoutingTable  string   `yaml:"routing_table"`
	Distance      int      `yaml:"distance"`
	CommentPrefix string   `yaml:"comment_prefix"`
	RateLimit     int      `yaml:"rate_limit"`
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
type SafetyConfig struct {
	MaxDeleteRatio          float64 `yaml:"max_delete_ratio"`
	RequireConfirmationOver int     `yaml:"require_confirmation_over"`
	MinPrefixV4             int     `yaml:"min_prefix_v4"`
	AllowHostRoutes         bool    `yaml:"allow_host_routes"`
	MaxASNPrefixes          int     `yaml:"max_asn_prefixes"`
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
	BGPViewAPIKey string   `yaml:"bgpview_api_key"`
	AkamaiAPIKey  string   `yaml:"akamai_api_key"`
	RDAPTimeout   Duration `yaml:"rdap_timeout"`
	Resolver      string   `yaml:"resolver"`
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

// ServiceOverride — переопределения для конкретного сервиса.
type ServiceOverride struct {
	Method          string   `yaml:"method"`
	Domains         []string `yaml:"domains"`
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
	if bg := os.Getenv("MRS_BGPVIEW_API_KEY"); bg != "" {
		c.External.BGPViewAPIKey = bg
	}
	if ak := os.Getenv("MRS_AKAMAI_API_KEY"); ak != "" {
		c.External.AkamaiAPIKey = ak
	}

	// Проверка прав файла (обязательно 0600)
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("config %s: required 0600, got %o", path, fi.Mode().Perm())
	}

	setDefaults(&c)

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}
	return &c, nil
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
	// Только IPv4: минимум /8 (PROMPT XIV)
	if c.Safety.MinPrefixV4 == 0 {
		c.Safety.MinPrefixV4 = 8
	}
	if c.MikroTik.Distance == 0 {
		c.MikroTik.Distance = 1
	}
	if c.MikroTik.CommentPrefix == "" {
		c.MikroTik.CommentPrefix = "AUTO"
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
}

// Validate проверяет корректность конфигурации.
func (c *Config) Validate() error {
	if c.MikroTik.Host == "" {
		return fmt.Errorf("mikrotik.host required")
	}
	if c.MikroTik.Username == "" {
		return fmt.Errorf("mikrotik.username required")
	}
	if c.Schedules.Global == "" {
		return fmt.Errorf("schedules.global required")
	}
	if c.Logging.File != "" {
		dir := filepath.Dir(c.Logging.File)
		if fi, err := os.Stat(dir); err == nil {
			if fi.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("log dir %s too open: %o", dir, fi.Mode().Perm())
			}
		}
	}
	if c.Web.Enabled && c.Web.Auth.Enabled {
		if c.Web.Auth.Username == "" || c.Web.Auth.Password == "" {
			return fmt.Errorf("web.auth.username/password required")
		}
	}
	return nil
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
var serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidateServiceName проверяет корректность имени сервиса.
func ValidateServiceName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return serviceNameRE.MatchString(name)
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
