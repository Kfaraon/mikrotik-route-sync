package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type Config struct {
	path      string                       `yaml:"-" mapstructure:"-"`
	Timezone  string                       `yaml:"timezone" mapstructure:"timezone"`
	CachePath string                       `yaml:"cache_path" mapstructure:"cache_path"`
	Logging   LoggingConfig                `yaml:"logging" mapstructure:"logging"`
	MikroTik  MikroTikConfig               `yaml:"mikrotik" mapstructure:"mikrotik"`
	Telegram  TelegramConfig               `yaml:"telegram" mapstructure:"telegram"`
	Web       WebConfig                    `yaml:"web" mapstructure:"web"`
	Scheduler SchedulerConfig              `yaml:"scheduler" mapstructure:"scheduler"`
	Safety    SafetyConfig                 `yaml:"safety" mapstructure:"safety"`
	Retry     RetryConfig                  `yaml:"retry" mapstructure:"retry"`
	External  ExternalConfig               `yaml:"external" mapstructure:"external"`
	Snapshots SnapshotsConfig              `yaml:"snapshots" mapstructure:"snapshots"`
	Schedules SchedulesConfig              `yaml:"schedules" mapstructure:"schedules"`
	Services  []string                     `yaml:"services" mapstructure:"services"`
	Overrides map[string]ServiceOverride   `yaml:"overrides" mapstructure:"overrides"`
}

type LoggingConfig struct {
	Level      string `yaml:"level" mapstructure:"level"`
	File       string `yaml:"file" mapstructure:"file"`
	MaxSizeMB  int    `yaml:"max_size_mb" mapstructure:"max_size_mb"`
	MaxFiles   int    `yaml:"max_files" mapstructure:"max_files"`
	MaxTotalMB int    `yaml:"max_total_mb" mapstructure:"max_total_mb"`
	Compress   bool   `yaml:"compress" mapstructure:"compress"`
	AlsoStdout bool   `yaml:"also_stdout" mapstructure:"also_stdout"`
}

type MikroTikConfig struct {
	Host          string        `yaml:"host" mapstructure:"host"`
	Port          int           `yaml:"port" mapstructure:"port"`
	Username      string        `yaml:"username" mapstructure:"username"`
	Password      string        `yaml:"password" mapstructure:"password"`
	UseSSL        bool          `yaml:"use_ssl" mapstructure:"use_ssl"`
	VerifySSL     bool          `yaml:"verify_ssl" mapstructure:"verify_ssl"`
	Timeout       time.Duration `yaml:"timeout" mapstructure:"timeout"`
	Gateway       string        `yaml:"gateway" mapstructure:"gateway"`
	RoutingTable  string        `yaml:"routing_table" mapstructure:"routing_table"`
	Distance      int           `yaml:"distance" mapstructure:"distance"`
	CommentPrefix string        `yaml:"comment_prefix" mapstructure:"comment_prefix"`
	RateLimit     int           `yaml:"rate_limit" mapstructure:"rate_limit"`
}

type TelegramConfig struct {
	Enabled           bool               `yaml:"enabled" mapstructure:"enabled"`
	BotToken          string             `yaml:"bot_token" mapstructure:"bot_token"`
	ChatID            string             `yaml:"chat_id" mapstructure:"chat_id"`
	AuthorizedChatIDs []string           `yaml:"authorized_chat_ids" mapstructure:"authorized_chat_ids"`
	RateLimit         int                `yaml:"rate_limit" mapstructure:"rate_limit"`
	WeeklyReport      WeeklyReportConfig `yaml:"weekly_report" mapstructure:"weekly_report"`
	Buttons           ButtonsConfig      `yaml:"buttons" mapstructure:"buttons"`
}

type WeeklyReportConfig struct {
	Enabled  bool   `yaml:"enabled" mapstructure:"enabled"`
	Schedule string `yaml:"schedule" mapstructure:"schedule"`
}

type ButtonsConfig struct {
	Enabled             bool `yaml:"enabled" mapstructure:"enabled"`
	MaxSelectedServices int  `yaml:"max_selected_services" mapstructure:"max_selected_services"`
}

type WebConfig struct {
	Enabled         bool          `yaml:"enabled" mapstructure:"enabled"`
	Listen          string        `yaml:"listen" mapstructure:"listen"`
	AllowedCIDRs    []string      `yaml:"allowed_cidrs" mapstructure:"allowed_cidrs"`
	TrustedProxies  []string      `yaml:"trusted_proxies" mapstructure:"trusted_proxies"`
	Auth            WebAuthConfig `yaml:"auth" mapstructure:"auth"`
	SessionTimeout  time.Duration `yaml:"session_timeout" mapstructure:"session_timeout"`
	CSRFEnabled     bool          `yaml:"csrf_enabled" mapstructure:"csrf_enabled"`
	SecurityHeaders bool          `yaml:"security_headers" mapstructure:"security_headers"`
}

type WebAuthConfig struct {
	Enabled  bool   `yaml:"enabled" mapstructure:"enabled"`
	Username string `yaml:"username" mapstructure:"username"`
	Password string `yaml:"password" mapstructure:"password"`
}

type SchedulerConfig struct {
	Parallel       bool          `yaml:"parallel" mapstructure:"parallel"`
	MaxConcurrent  int           `yaml:"max_concurrent" mapstructure:"max_concurrent"`
	ReloadInterval time.Duration `yaml:"reload_interval" mapstructure:"reload_interval"`
	CacheTTL       time.Duration `yaml:"cache_ttl" mapstructure:"cache_ttl"`
	CachePurge     string        `yaml:"cache_purge" mapstructure:"cache_purge"`
}

type SafetyConfig struct {
	MaxDeleteRatio          float64 `yaml:"max_delete_ratio" mapstructure:"max_delete_ratio"`
	RequireConfirmationOver int     `yaml:"require_confirmation_over" mapstructure:"require_confirmation_over"`
	MinPrefixV4             int     `yaml:"min_prefix_v4" mapstructure:"min_prefix_v4"`
	MinPrefixV6             int     `yaml:"min_prefix_v6" mapstructure:"min_prefix_v6"`
	AllowHostRoutes         bool    `yaml:"allow_host_routes" mapstructure:"allow_host_routes"`
	MaxASNPrefixes          int     `yaml:"max_asn_prefixes" mapstructure:"max_asn_prefixes"`
}

type RetryConfig struct {
	MaxAttempts int           `yaml:"max_attempts" mapstructure:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay" mapstructure:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay" mapstructure:"max_delay"`
	Jitter      bool          `yaml:"jitter" mapstructure:"jitter"`
}

type ExternalConfig struct {
	HTTPTimeout   time.Duration `yaml:"http_timeout" mapstructure:"http_timeout"`
	MaxResponseMB int           `yaml:"max_response_mb" mapstructure:"max_response_mb"`
	BGPViewAPIKey string        `yaml:"bgpview_api_key" mapstructure:"bgpview_api_key"`
	AkamaiAPIKey  string        `yaml:"akamai_api_key" mapstructure:"akamai_api_key"`
	RDAPTimeout   time.Duration `yaml:"rdap_timeout" mapstructure:"rdap_timeout"`
	Resolver      string        `yaml:"resolver" mapstructure:"resolver"`
}

type SnapshotsConfig struct {
	Enabled  bool          `yaml:"enabled" mapstructure:"enabled"`
	TTL      time.Duration `yaml:"ttl" mapstructure:"ttl"`
	MaxCount int           `yaml:"max_count" mapstructure:"max_count"`
}

type SchedulesConfig struct {
	Global   string                     `yaml:"global" mapstructure:"global"`
	Groups   map[string]GroupConfig     `yaml:"groups" mapstructure:"groups"`
	Services map[string]ServiceSchedule `yaml:"services" mapstructure:"services"`
}

type GroupConfig struct {
	Schedule string   `yaml:"schedule" mapstructure:"schedule"`
	Services []string `yaml:"services" mapstructure:"services"`
}

type ServiceSchedule struct {
	Schedule string `yaml:"schedule" mapstructure:"schedule"`
}

type ServiceOverride struct {
	Method          string   `yaml:"method" mapstructure:"method"`
	Domains         []string `yaml:"domains" mapstructure:"domains"`
	StaticURL       string   `yaml:"static_url" mapstructure:"static_url"`
	AlsoCDN         []string `yaml:"also_cdn" mapstructure:"also_cdn"`
	MaxASNPrefixes  int      `yaml:"max_asn_prefixes" mapstructure:"max_asn_prefixes"`
	MaxPrefixes     int      `yaml:"max_prefixes" mapstructure:"max_prefixes"`
	Exclude         []string `yaml:"exclude" mapstructure:"exclude"`
	IncludeOnly     []string `yaml:"include_only" mapstructure:"include_only"`
	RefreshInterval string   `yaml:"refresh_interval" mapstructure:"refresh_interval"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var c Config
	c.path = path

	// DecodeHook для корректного парсинга time.Duration из строк ("30s", "5m")
	decoderConfig := viper.DecodeHook(
		mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	)

	if err := v.Unmarshal(&c, decoderConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	if fi, err := os.Stat(path); err == nil {
		if fi.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("config file %s must have 0600 permissions", path)
		}
	} else {
		return nil, fmt.Errorf("cannot stat config file: %w", err)
	}

	setDefaults(&c)

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &c, nil
}

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
	if c.Safety.MinPrefixV4 == 0 {
		c.Safety.MinPrefixV4 = 9
	}
	if c.Safety.MinPrefixV6 == 0 {
		c.Safety.MinPrefixV6 = 32
	}
	if c.MikroTik.Distance == 0 {
		c.MikroTik.Distance = 1
	}
	if c.MikroTik.CommentPrefix == "" {
		c.MikroTik.CommentPrefix = "AUTO"
	}
	if c.External.MaxResponseMB == 0 {
		c.External.MaxResponseMB = 50
	}
	if c.Snapshots.MaxCount == 0 {
		c.Snapshots.MaxCount = 10
	}
	if c.Snapshots.TTL == 0 {
		c.Snapshots.TTL = 7 * 24 * time.Hour
	}
	if c.Web.SessionTimeout == 0 {
		c.Web.SessionTimeout = 30 * time.Minute
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
}

func (c *Config) Validate() error {
	if c.MikroTik.Host == "" {
		return fmt.Errorf("mikrotik.host is required")
	}
	if c.MikroTik.Username == "" {
		return fmt.Errorf("mikrotik.username is required")
	}
	if err := c.Schedules.validate(); err != nil {
		return fmt.Errorf("schedules validation failed: %w", err)
	}
	if c.Logging.File != "" {
		dir := filepath.Dir(c.Logging.File)
		if fi, err := os.Stat(dir); err == nil {
			if fi.Mode().Perm()&0077 != 0 {
				return fmt.Errorf("config directory %s permissions too open (%o)", dir, fi.Mode().Perm())
			}
		}
	}
	return nil
}

func (s *SchedulesConfig) validate() error {
	if s.Global == "" {
		return fmt.Errorf("global schedule is required")
	}
	return nil
}

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

func (c *Config) ServicesInGroup(group string) []string {
	if g, ok := c.Schedules.Groups[group]; ok {
		return g.Services
	}
	return nil
}

func (c *Config) ScheduleFor(service string) string {
	return c.EffectiveSchedule(service)
}

var serviceNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func ValidateServiceName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	return serviceNameRE.MatchString(name)
}

func (c *Config) Set(path string, value any) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}

	switch {
	case strings.HasPrefix(path, "schedules.services."):
		parts := strings.Split(path, ".")
		if len(parts) != 4 || parts[3] != "schedule" {
			return fmt.Errorf("unsupported path: %s", path)
		}
		serviceName := parts[2]
		schedule, ok := value.(string)
		if !ok {
			return fmt.Errorf("value must be a string for schedule")
		}
		if c.Schedules.Services == nil {
			c.Schedules.Services = make(map[string]ServiceSchedule)
		}
		c.Schedules.Services[serviceName] = ServiceSchedule{Schedule: schedule}
		return nil

	case path == "web.listen":
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("value must be a string")
		}
		c.Web.Listen = s
		return nil

	case path == "web.enabled":
		b, ok := value.(bool)
		if !ok {
			return fmt.Errorf("value must be a bool")
		}
		c.Web.Enabled = b
		return nil

	default:
		return setByReflection(c, path, value)
	}
}

func setByReflection(obj any, path string, value any) error {
	v := reflect.ValueOf(obj)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return fmt.Errorf("unsupported object type")
	}

	parts := strings.Split(path, ".")
	current := v

	for _, part := range parts {
		if current.Kind() == reflect.Ptr {
			current = current.Elem()
		}
		field := current.FieldByNameFunc(func(name string) bool {
			return strings.EqualFold(name, part)
		})
		if !field.IsValid() || !field.CanSet() {
			return fmt.Errorf("field %q not found or not settable", part)
		}
		current = field
	}

	newVal := reflect.ValueOf(value)
	if !newVal.Type().AssignableTo(current.Type()) {
		return fmt.Errorf("cannot assign %T to %s", value, current.Type())
	}
	current.Set(newVal)
	return nil
}

// Save сохраняет конфигурацию обратно в файл.
// Используется при добавлении/удалении сервисов через API.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config path not set, cannot save")
	}

	// Маршалим конфигурацию в YAML
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Проверяем права на файл перед записью
	if fi, err := os.Stat(c.path); err == nil {
		if fi.Mode().Perm() != 0600 {
			return fmt.Errorf("config file %s must have 0600 permissions, got %o", c.path, fi.Mode().Perm())
		}
	} else {
		return fmt.Errorf("cannot stat config file: %w", err)
	}

	// Записываем файл с правами 0600
	if err := os.WriteFile(c.path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}
