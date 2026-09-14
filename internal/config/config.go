package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Timezone  string                     `mapstructure:"timezone" yaml:"timezone"`
	Logging   LoggingConfig              `mapstructure:"logging" yaml:"logging"`
	MikroTik  MikroTikConfig             `mapstructure:"mikrotik" yaml:"mikrotik"`
	Telegram  TelegramConfig             `mapstructure:"telegram" yaml:"telegram"`
	Web       WebConfig                  `mapstructure:"web" yaml:"web"`
	Scheduler SchedulerConfig            `mapstructure:"scheduler" yaml:"scheduler"`
	External  ExternalConfig             `mapstructure:"external" yaml:"external"`
	Schedules SchedulesConfig            `mapstructure:"schedules" yaml:"schedules"`
	Services  []string                   `mapstructure:"services" yaml:"services"`
	Overrides map[string]ServiceOverride `mapstructure:"overrides" yaml:"overrides"`
	path      string
	mu        sync.RWMutex
}

type LoggingConfig struct {
	Level      string `mapstructure:"level" yaml:"level"`
	File       string `mapstructure:"file" yaml:"file"`
	MaxSizeMB  int    `mapstructure:"max_size_mb" yaml:"max_size_mb"`
	MaxFiles   int    `mapstructure:"max_files" yaml:"max_files"`
	MaxTotalMB int    `mapstructure:"max_total_mb" yaml:"max_total_mb"`
	Compress   bool   `mapstructure:"compress" yaml:"compress"`
	Stdout     bool   `mapstructure:"also_stdout" yaml:"also_stdout"`
}

type MikroTikConfig struct {
	Host          string `mapstructure:"host" yaml:"host"`
	Port          int    `mapstructure:"port" yaml:"port"`
	Username      string `mapstructure:"username" yaml:"username"`
	Password      string `mapstructure:"password" yaml:"password"`
	UseSSL        bool   `mapstructure:"use_ssl" yaml:"use_ssl"`
	VerifySSL     bool   `mapstructure:"verify_ssl" yaml:"verify_ssl"`
	Gateway       string `mapstructure:"gateway" yaml:"gateway"`
	RoutingTable  string `mapstructure:"routing_table" yaml:"routing_table"`
	Distance      int    `mapstructure:"distance" yaml:"distance"`
	CommentPrefix string `mapstructure:"comment_prefix" yaml:"comment_prefix"`
	Timeout       string `mapstructure:"timeout" yaml:"timeout"`
}

type TelegramConfig struct {
	Enabled           bool     `mapstructure:"enabled" yaml:"enabled"`
	BotToken          string   `mapstructure:"bot_token" yaml:"bot_token"`
	ChatID            string   `mapstructure:"chat_id" yaml:"chat_id"`
	AuthorizedChatIDs []string `mapstructure:"authorized_chat_ids" yaml:"authorized_chat_ids"`
	WeeklyReport      string   `mapstructure:"weekly_report" yaml:"weekly_report"`
	Buttons           struct {
		Enabled             bool `mapstructure:"enabled" yaml:"enabled"`
		MaxSelectedServices int  `mapstructure:"max_selected_services" yaml:"max_selected_services"`
	} `mapstructure:"buttons" yaml:"buttons"`
}

type WebConfig struct {
	Enabled        bool     `mapstructure:"enabled" yaml:"enabled"`
	Listen         string   `mapstructure:"listen" yaml:"listen"`
	SessionTimeout string   `mapstructure:"session_timeout" yaml:"session_timeout"`
	AllowedCIDRs   []string `mapstructure:"allowed_cidrs" yaml:"allowed_cidrs"`
	Auth           struct {
		Enabled  bool   `mapstructure:"enabled" yaml:"enabled"`
		Username string `mapstructure:"username" yaml:"username"`
		Password string `mapstructure:"password" yaml:"password"`
	} `mapstructure:"auth" yaml:"auth"`
}

type SchedulerConfig struct {
	Parallel       bool   `mapstructure:"parallel" yaml:"parallel"`
	MaxConcurrent  int    `mapstructure:"max_concurrent" yaml:"max_concurrent"`
	ReloadInterval string `mapstructure:"reload_interval" yaml:"reload_interval"`
	CacheTTL       string `mapstructure:"cache_ttl" yaml:"cache_ttl"`
	CachePurge     string `mapstructure:"cache_purge" yaml:"cache_purge"`
}

type ExternalConfig struct {
	BGPViewBase  string `mapstructure:"bgpview_base" yaml:"bgpview_base"`
	RIPEStatBase string `mapstructure:"ripestat_base" yaml:"ripestat_base"`
	UserAgent    string `mapstructure:"user_agent" yaml:"user_agent"`
}

type SchedulesConfig struct {
	Global   string                     `mapstructure:"global" yaml:"global"`
	Groups   map[string]GroupSchedule   `mapstructure:"groups" yaml:"groups"`
	Services map[string]ServiceSchedule `mapstructure:"services" yaml:"services"`
}

type GroupSchedule struct {
	Schedule string   `mapstructure:"schedule" yaml:"schedule"`
	Services []string `mapstructure:"services" yaml:"services"`
}

type ServiceSchedule struct {
	Schedule string `mapstructure:"schedule" yaml:"schedule"`
}

type ServiceOverride struct {
	Domains        []string `mapstructure:"domains" yaml:"domains"`
	ASN            int      `mapstructure:"asn" yaml:"asn"`
	Method         string   `mapstructure:"method" yaml:"method"`
	StaticURL      string   `mapstructure:"static_url" yaml:"static_url"`
	MaxASNPrefixes int      `mapstructure:"max_asn_prefixes" yaml:"max_asn_prefixes"`
	Exclude        []string `mapstructure:"exclude" yaml:"exclude"`
	Include        []string `mapstructure:"include" yaml:"include"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	v.SetEnvPrefix("MRS")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	_ = v.BindEnv("mikrotik.password", "MRS_MIKROTIK_PASSWORD")
	_ = v.BindEnv("telegram.bot_token", "MRS_TELEGRAM_BOT_TOKEN")
	_ = v.BindEnv("web.auth.password", "MRS_WEB_PASSWORD")
	setDefaults(v)
	if err := v.ReadInConfig(); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config: %w", err)
		}
	} else {
		// The config contains credentials by design. Keep permissions restrictive
		// even when the file was created manually with a permissive umask.
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("chmod config: %w", err)
		}
	}
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	cfg.path = path
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("timezone", "UTC")
	v.SetDefault("mikrotik.port", 443)
	v.SetDefault("mikrotik.routing_table", "main")
	v.SetDefault("mikrotik.distance", 2)
	v.SetDefault("mikrotik.comment_prefix", "AUTO")
	v.SetDefault("mikrotik.timeout", "30s")
	v.SetDefault("scheduler.max_concurrent", 3)
	v.SetDefault("scheduler.reload_interval", "1m")
	v.SetDefault("scheduler.cache_ttl", "24h")
	v.SetDefault("external.bgpview_base", "https://api.bgpview.io")
	v.SetDefault("external.ripestat_base", "https://stat.ripe.net/data")
	v.SetDefault("external.user_agent", "mikrotik-route-sync/1.0")
	v.SetDefault("web.listen", ":8080")
}

func (c *Config) Validate() error {
	if c.MikroTik.Host == "" {
		return fmt.Errorf("mikrotik.host is required")
	}
	if c.MikroTik.Username == "" {
		return fmt.Errorf("mikrotik.username is required")
	}
	if c.MikroTik.Gateway == "" {
		return fmt.Errorf("mikrotik.gateway is required")
	}
	if c.MikroTik.CommentPrefix == "" {
		c.MikroTik.CommentPrefix = "AUTO"
	}
	if c.Schedules.Groups == nil {
		c.Schedules.Groups = map[string]GroupSchedule{}
	}
	if c.Schedules.Services == nil {
		c.Schedules.Services = map[string]ServiceSchedule{}
	}
	if c.Overrides == nil {
		c.Overrides = map[string]ServiceOverride{}
	}
	seen := map[string]struct{}{}
	for i, s := range c.Services {
		s = NormalizeService(s)
		if s == "" {
			return fmt.Errorf("services[%d] is empty", i)
		}
		if _, ok := seen[s]; ok {
			return fmt.Errorf("duplicate service %q", s)
		}
		seen[s] = struct{}{}
		c.Services[i] = s
	}
	return nil
}

func NormalizeService(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// A user may paste a URL even though the service identity is its host.
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
			s = u.Hostname()
		}
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "."))
	s = strings.TrimSuffix(s, "/")
	return strings.ToLower(s)
}

func (c *Config) Comment(service string) string {
	return c.MikroTik.CommentPrefix + ":" + NormalizeService(service)
}

func (c *Config) ScheduleFor(service string) string {
	service = NormalizeService(service)
	if v, ok := c.Schedules.Services[service]; ok {
		s := strings.TrimSpace(strings.ToLower(v.Schedule))
		if s != "" && s != "inherit" {
			return v.Schedule
		}
	}
	groups := make([]string, 0, len(c.Schedules.Groups))
	for name := range c.Schedules.Groups {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	for _, name := range groups {
		g := c.Schedules.Groups[name]
		for _, s := range g.Services {
			if NormalizeService(s) == service && strings.TrimSpace(g.Schedule) != "" {
				return g.Schedule
			}
		}
	}
	if strings.TrimSpace(c.Schedules.Global) == "" {
		return "manual"
	}
	return c.Schedules.Global
}

func (c *Config) ServicesInGroup(group string) []string {
	if g, ok := c.Schedules.Groups[group]; ok {
		return append([]string(nil), g.Services...)
	}
	return nil
}

func (c *Config) HasService(service string) bool {
	service = NormalizeService(service)
	for _, s := range c.Services {
		if s == service {
			return true
		}
	}
	return false
}

func (c *Config) AddService(service string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	service = NormalizeService(service)
	if service == "" {
		return fmt.Errorf("empty service")
	}
	for _, s := range c.Services {
		if s == service {
			return nil
		}
	}
	c.Services = append(c.Services, service)
	sort.Strings(c.Services)
	return c.saveLocked()
}

func (c *Config) RemoveService(service string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	service = NormalizeService(service)
	out := c.Services[:0]
	for _, s := range c.Services {
		if s != service {
			out = append(out, s)
		}
	}
	c.Services = out
	delete(c.Schedules.Services, service)
	delete(c.Overrides, service)
	for name, g := range c.Schedules.Groups {
		ss := g.Services[:0]
		for _, s := range g.Services {
			if NormalizeService(s) != service {
				ss = append(ss, s)
			}
		}
		g.Services = ss
		c.Schedules.Groups[name] = g
	}
	return c.saveLocked()
}

func (c *Config) Save() error { c.mu.Lock(); defer c.mu.Unlock(); return c.saveLocked() }
func (c *Config) saveLocked() error {
	if c.path == "" {
		return fmt.Errorf("config path not set")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil && filepath.Dir(c.path) != "." {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func IsSecret(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "password") || strings.Contains(key, "token") || strings.Contains(key, "secret")
}
