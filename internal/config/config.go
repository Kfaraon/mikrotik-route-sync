package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config представляет основную конфигурацию приложения.
type Config struct {
	Timezone string `yaml:"timezone" mapstructure:"timezone"`
	CachePath string `yaml:"cache_path" mapstructure:"cache_path"` // Путь к файлу кэша
	Logging  LoggingConfig  `yaml:"logging" mapstructure:"logging"`
	MikroTik MikroTikConfig `yaml:"mikrotik" mapstructure:"mikrotik"`
	Telegram TelegramConfig `yaml:"telegram" mapstructure:"telegram"`
	Web      WebConfig      `yaml:"web" mapstructure:"web"`
	Scheduler SchedulerConfig `yaml:"scheduler" mapstructure:"scheduler"`
	Safety    SafetyConfig    `yaml:"safety" mapstructure:"safety"`
	Retry     RetryConfig     `yaml:"retry" mapstructure:"retry"`
	External  ExternalConfig  `yaml:"external" mapstructure:"external"`
	Snapshots SnapshotsConfig `yaml:"snapshots" mapstructure:"snapshots"`
	Schedules SchedulesConfig `yaml:"schedules" mapstructure:"schedules"`
	Services  []string        `yaml:"services" mapstructure:"services"`
	Overrides map[string]ServiceOverride `yaml:"overrides" mapstructure:"overrides"`
}

// LoggingConfig ...
type LoggingConfig struct {
	Level      string `yaml:"level" mapstructure:"level"`
	File       string `yaml:"file" mapstructure:"file"`
	MaxSizeMB  int    `yaml:"max_size_mb" mapstructure:"max_size_mb"`
	MaxFiles   int    `yaml:"max_files" mapstructure:"max_files"`
	MaxTotalMB int    `yaml:"max_total_mb" mapstructure:"max_total_mb"`
	Compress   bool   `yaml:"compress" mapstructure:"compress"`
	AlsoStdout bool   `yaml:"also_stdout" mapstructure:"also_stdout"`
}

// MikroTikConfig ...
type MikroTikConfig struct {
	Host          string `yaml:"host" mapstructure:"host"`
	Port          int    `yaml:"port" mapstructure:"port"`
	Username      string `yaml:"username" mapstructure:"username"`
	Password      string `yaml:"password" mapstructure:"password"`
	UseSSL        bool   `yaml:"use_ssl" mapstructure:"use_ssl"`
	VerifySSL     bool   `yaml:"verify_ssl" mapstructure:"verify_ssl"`
	Timeout       time.Duration `yaml:"timeout" mapstructure:"timeout"`
	Gateway       string `yaml:"gateway" mapstructure:"gateway"`
	RoutingTable  string `yaml:"routing_table" mapstructure:"routing_table"`
	Distance      int    `yaml:"distance" mapstructure:"distance"`
	CommentPrefix string `yaml:"comment_prefix" mapstructure:"comment_prefix"`
	RateLimit     int    `yaml:"rate_limit" mapstructure:"rate_limit"`
}

// TelegramConfig ...
type TelegramConfig struct {
	Enabled           bool     `yaml:"enabled" mapstructure:"enabled"`
	BotToken          string   `yaml:"bot_token" mapstructure:"bot_token"`
	ChatID            string   `yaml:"chat_id" mapstructure:"chat_id"`
	AuthorizedChatIDs []string `yaml:"authorized_chat_ids" mapstructure:"authorized_chat_ids"`
	RateLimit         int      `yaml:"rate_limit" mapstructure:"rate_limit"`
	WeeklyReport      WeeklyReportConfig `yaml:"weekly_report" mapstructure:"weekly_report"`
	Buttons           ButtonsConfig      `yaml:"buttons" mapstructure:"buttons"`
}

// WeeklyReportConfig ...
type WeeklyReportConfig struct {
	Enabled  bool   `yaml:"enabled" mapstructure:"enabled"`
	Schedule string `yaml:"schedule" mapstructure:"schedule"`
}

// ButtonsConfig ...
type ButtonsConfig struct {
	Enabled             bool `yaml:"enabled" mapstructure:"enabled"`
	MaxSelectedServices int  `yaml:"max_selected_services" mapstructure:"max_selected_services"`
}

// WebConfig ...
type WebConfig struct {
	Enabled         bool     `yaml:"enabled" mapstructure:"enabled"`
	Listen          string   `yaml:"listen" mapstructure:"listen"`
	AllowedCIDRs    []string `yaml:"allowed_cidrs" mapstructure:"allowed_cidrs"`
	TrustedProxies  []string `yaml:"trusted_proxies" mapstructure:"trusted_proxies"`
	Auth            WebAuthConfig `yaml:"auth" mapstructure:"auth"`
	SessionTimeout  time.Duration `yaml:"session_timeout" mapstructure:"session_timeout"`
	CSRFEnabled     bool          `yaml:"csrf_enabled" mapstructure:"csrf_enabled"`
	SecurityHeaders bool          `yaml:"security_headers" mapstructure:"security_headers"`
}

// WebAuthConfig ...
type WebAuthConfig struct {
	Enabled  bool   `yaml:"enabled" mapstructure:"enabled"`
	Username string `yaml:"username" mapstructure:"username"`
	Password string `yaml:"password" mapstructure:"password"`
}

// SchedulerConfig ...
type SchedulerConfig struct {
	Parallel       bool          `yaml:"parallel" mapstructure:"parallel"`
	MaxConcurrent  int           `yaml:"max_concurrent" mapstructure:"max_concurrent"`
	ReloadInterval time.Duration `yaml:"reload_interval" mapstructure:"reload_interval"`
	CacheTTL       time.Duration `yaml:"cache_ttl" mapstructure:"cache_ttl"`
	CachePurge     string        `yaml:"cache_purge" mapstructure:"cache_purge"`
}

// SafetyConfig ...
type SafetyConfig struct {
	MaxDeleteRatio          float64 `yaml:"max_delete_ratio" mapstructure:"max_delete_ratio"`
	RequireConfirmationOver int     `yaml:"require_confirmation_over" mapstructure:"require_confirmation_over"`
	MinPrefixV4             int     `yaml:"min_prefix_v4" mapstructure:"min_prefix_v4"`
	MinPrefixV6             int     `yaml:"min_prefix_v6" mapstructure:"min_prefix_v6"`
	AllowHostRoutes         bool    `yaml:"allow_host_routes" mapstructure:"allow_host_routes"`
	MaxASNPrefixes          int     `yaml:"max_asn_prefixes" mapstructure:"max_asn_prefixes"`
}

// RetryConfig ...
type RetryConfig struct {
	MaxAttempts int           `yaml:"max_attempts" mapstructure:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay" mapstructure:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay" mapstructure:"max_delay"`
	Jitter      bool          `yaml:"jitter" mapstructure:"jitter"`
}

// ExternalConfig ...
type ExternalConfig struct {
	HTTPTimeout   time.Duration `yaml:"http_timeout" mapstructure:"http_timeout"`
	MaxResponseMB int           `yaml:"max_response_mb" mapstructure:"max_response_mb"`
	BGPViewAPIKey string        `yaml:"bgpview_api_key" mapstructure:"bgpview_api_key"`
	AkamaiAPIKey   string        `yaml:"akamai_api_key"`
	RDAPTimeout   time.Duration `yaml:"rdap_timeout" mapstructure:"rdap_timeout"`
	Resolver      string        `yaml:"resolver" mapstructure:"resolver"`
}

// SnapshotsConfig ...
type SnapshotsConfig struct {
	Enabled  bool          `yaml:"enabled" mapstructure:"enabled"`
	TTL      time.Duration `yaml:"ttl" mapstructure:"ttl"`
	MaxCount int           `yaml:"max_count" mapstructure:"max_count"`
}

// SchedulesConfig ...
type SchedulesConfig struct {
	Global   string                 `yaml:"global" mapstructure:"global"`
	Groups   map[string]GroupConfig `yaml:"groups" mapstructure:"groups"`
	Services map[string]ServiceSchedule `yaml:"services" mapstructure:"services"`
}

// GroupConfig ...
type GroupConfig struct {
	Schedule string   `yaml:"schedule" mapstructure:"schedule"`
	Services []string `yaml:"services" mapstructure:"services"`
}

// ServiceSchedule ...
type ServiceSchedule struct {
	Schedule string `yaml:"schedule" mapstructure:"schedule"`
}

// ServiceOverride ...
type ServiceOverride struct {
	Method          string   `yaml:"method" mapstructure:"method"`
	Domains         []string `yaml:"domains" mapstructure:"domains"`
	StaticURL       string   `yaml:"static_url" mapstructure:"static_url"`
	AlsoCDN         []string `yaml:"also_cdn" mapstructure:"also_cdn"`
	MaxASNPrefixes  int      `yaml:"max_asn_prefixes" mapstructure:"max_asn_prefixes"`
	Exclude         []string `yaml:"exclude" mapstructure:"exclude"`
	IncludeOnly     []string `yaml:"include_only" mapstructure:"include_only"`
	RefreshInterval string   `yaml:"refresh_interval" mapstructure:"refresh_interval"`
}

// Load загружает конфигурацию из файла по указанному пути.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Проверка прав доступа к файлу конфигурации
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("config file %s must have 0600 permissions", path)
		}
	} else {
		return nil, fmt.Errorf("cannot stat config file: %w", err)
	}

	// Установка значений по умолчанию
	setDefaults(&c)

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &c, nil
}

// setDefaults устанавливает значения по умолчанию для полей конфигурации.
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
	// ... другие значения по умолчанию
}

// Validate проверяет корректность конфигурации.
func (c *Config) Validate() error {
	if c.MikroTik.Host == "" {
		return fmt.Errorf("mikrotik.host is required")
	}
	if c.MikroTik.Username == "" {
		return fmt.Errorf("mikrotik.username is required")
	}
	// Проверка расписаний
	if err := c.Schedules.validate(); err != nil {
		return fmt.Errorf("schedules validation failed: %w", err)
	}
	// Проверка прав доступа к директории конфига
	dir := filepath.Dir(c.Logging.File)
	if fi, err := os.Stat(dir); err == nil {
		if fi.Mode().Perm() != 0700 {
			return fmt.Errorf("config directory %s must have 0700 permissions", dir)
		}
	}
	return nil
}

// SchedulesConfig.validate проверяет корректность расписаний.
func (s *SchedulesConfig) validate() error {
	// TODO: Реализовать парсинг и валидацию cron-выражений и
	// человекочитаемых расписаний (например, "every 6h").
	// Пока просто проверяем, что они не пустые.
	if s.Global == "" {
		return fmt.Errorf("global schedule is required")
	}
	return nil
}

// EffectiveSchedule возвращает эффективное расписание для сервиса
// с учетом приоритета: сервис -> группа -> глобальное.
func (c *Config) EffectiveSchedule(service string) string {
	if s, ok := c.Schedules.Services[service]; ok && s.Schedule != "" {
		return s.Schedule
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

// ServicesInGroup возвращает список сервисов, входящих в указанную группу.
func (c *Config) ServicesInGroup(group string) []string {
	if g, ok := c.Schedules.Groups[group]; ok {
		return g.Services
	}
	return nil
}

// AtomicWrite атомарно записывает конфигурацию в файл.
func AtomicWrite(path string, c *Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
