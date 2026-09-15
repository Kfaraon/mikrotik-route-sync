package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var serviceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Config struct {
	Timezone  string              `yaml:"timezone"`
	Logging   Logging             `yaml:"logging"`
	MikroTik  MikroTik            `yaml:"mikrotik"`
	Telegram  Telegram            `yaml:"telegram"`
	Web       Web                 `yaml:"web"`
	Scheduler Scheduler           `yaml:"scheduler"`
	Safety    Safety              `yaml:"safety"`
	Retry     Retry               `yaml:"retry"`
	External  External            `yaml:"external"`
	Snapshots Snapshots           `yaml:"snapshots"`
	Schedules Schedules           `yaml:"schedules"`
	Services  []string            `yaml:"services"`
	Overrides map[string]Override `yaml:"overrides"`
	path      string              // Путь к файлу конфига для сохранения
}

type Logging struct {
	Level      string `yaml:"level"`
	File       string `yaml:"file"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxFiles   int    `yaml:"max_files"`
	MaxTotalMB int    `yaml:"max_total_mb"`
	Compress   bool   `yaml:"compress"`
	AlsoStdout bool   `yaml:"also_stdout"`
}

type MikroTik struct {
	Host          string        `yaml:"host"`
	Port          int           `yaml:"port"`
	Username      string        `yaml:"username"`
	Password      string        `yaml:"password"`
	UseSSL        bool          `yaml:"use_ssl"`
	VerifySSL     bool          `yaml:"verify_ssl"`
	Timeout       time.Duration `yaml:"-"`
	TimeoutText   string        `yaml:"timeout"`
	Gateway       string        `yaml:"gateway"`
	RoutingTable  string        `yaml:"routing_table"`
	Distance      int           `yaml:"distance"`
	CommentPrefix string        `yaml:"comment_prefix"`
	RateLimit     int           `yaml:"rate_limit"`
}

type Telegram struct {
	Enabled           bool     `yaml:"enabled"`
	BotToken          string   `yaml:"bot_token"`
	ChatID            string   `yaml:"chat_id"`
	AuthorizedChatIDs []string `yaml:"authorized_chat_ids"`
	RateLimit         int      `yaml:"rate_limit"`
}

type Web struct {
	Enabled            bool     `yaml:"enabled"`
	Listen             string   `yaml:"listen"`
	AllowedCIDRs       []string `yaml:"allowed_cidrs"`
	TrustedProxies     []string `yaml:"trusted_proxies"`
	Auth               WebAuth  `yaml:"auth"`
	SessionTimeoutText string   `yaml:"session_timeout"`
	CSRFEnabled        bool     `yaml:"csrf_enabled"`
	SecurityHeaders    bool     `yaml:"security_headers"`
}

type WebAuth struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Scheduler struct {
	Parallel       bool   `yaml:"parallel"`
	MaxConcurrent  int    `yaml:"max_concurrent"`
	ReloadInterval string `yaml:"reload_interval"`
	CacheTTL       string `yaml:"cache_ttl"`
	CachePurge     string `yaml:"cache_purge"`
}

type Safety struct {
	MaxDeleteRatio          float64 `yaml:"max_delete_ratio"`
	RequireConfirmationOver int     `yaml:"require_confirmation_over"`
	MinPrefixV4             int     `yaml:"min_prefix_v4"`
	MinPrefixV6             int     `yaml:"min_prefix_v6"`
	AllowHostRoutes         bool    `yaml:"allow_host_routes"`
	MaxASNPrefixes          int     `yaml:"max_asn_prefixes"`
}

type Retry struct {
	MaxAttempts int    `yaml:"max_attempts"`
	BaseDelay   string `yaml:"base_delay"`
	MaxDelay    string `yaml:"max_delay"`
	Jitter      bool   `yaml:"jitter"`
}

type External struct {
	HTTPTimeout   string `yaml:"http_timeout"`
	MaxResponseMB int    `yaml:"max_response_mb"`
	BGPViewAPIKey string `yaml:"bgpview_api_key"`
	RDAPTimeout   string `yaml:"rdap_timeout"`
	Resolver      string `yaml:"resolver"`
}

type Snapshots struct {
	Enabled  bool   `yaml:"enabled"`
	TTL      string `yaml:"ttl"`
	MaxCount int    `yaml:"max_count"`
}

type Schedules struct {
	Global   string                     `yaml:"global"`
	Groups   map[string]GroupSchedule   `yaml:"groups"`
	Services map[string]ServiceSchedule `yaml:"services"`
}

type GroupSchedule struct {
	Schedule string   `yaml:"schedule"`
	Services []string `yaml:"services"`
}

type ServiceSchedule struct {
	Schedule string `yaml:"schedule"`
}

type Override struct {
	Method         string   `yaml:"method"`
	Domains        []string `yaml:"domains"`
	StaticURL      string   `yaml:"static_url"`
	MaxASNPrefixes int      `yaml:"max_asn_prefixes"`
	MaxPrefixes    int      `yaml:"max_prefixes"`
	Exclude        []string `yaml:"exclude"`
	IncludeOnly    []string `yaml:"include_only"`
	AlsoCDN        []string `yaml:"also_cdn"`
}

func defaults() Config {
	return Config{
		Timezone: "UTC",
		Logging: Logging{
			Level: "info", File: "/var/log/mikrotik-sync/app.log",
			MaxSizeMB: 10, MaxFiles: 5, MaxTotalMB: 50, Compress: true, AlsoStdout: true,
		},
		MikroTik: MikroTik{
			Port: 443, UseSSL: true, VerifySSL: true, TimeoutText: "30s",
			RoutingTable: "main", Distance: 2, CommentPrefix: "AUTO", RateLimit: 20,
		},
		Web: Web{
			Listen: "127.0.0.1:8080", SessionTimeoutText: "24h",
			CSRFEnabled: true, SecurityHeaders: true,
		},
		Scheduler: Scheduler{
			Parallel: true, MaxConcurrent: 3, ReloadInterval: "1m",
			CacheTTL: "24h", CachePurge: "every 1h",
		},
		Safety: Safety{
			MaxDeleteRatio: .5, RequireConfirmationOver: 100,
			MinPrefixV4: 8, MinPrefixV6: 16, MaxASNPrefixes: 100,
		},
		Retry: Retry{MaxAttempts: 3, BaseDelay: "1s", MaxDelay: "30s", Jitter: true},
		External: External{
			HTTPTimeout: "15s", MaxResponseMB: 50,
			RDAPTimeout: "10s", Resolver: "1.1.1.1:53",
		},
		Snapshots: Snapshots{Enabled: true, TTL: "168h", MaxCount: 50},
		Overrides: map[string]Override{},
		Schedules: Schedules{
			Global: "every 6h",
			Groups: map[string]GroupSchedule{},
			Services: map[string]ServiceSchedule{},
		},
	}
}

func Load(path string) (*Config, error) {
	if err := CheckSecurePermissions(path); err != nil {
		return nil, err
	}
	cfg := defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err = yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	applyEnv(&cfg)
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	if d, err := time.ParseDuration(cfg.MikroTik.TimeoutText); err == nil {
		cfg.MikroTik.Timeout = d
	} else {
		return nil, fmt.Errorf("mikrotik.timeout: %w", err)
	}
	cfg.path = path // Сохраняем путь для последующего Save()
	return &cfg, nil
}

func applyEnv(c *Config) {
	env := func(k string) *string {
		if f := os.Getenv(k + "_FILE"); f != "" {
			if b, e := os.ReadFile(f); e == nil {
				s := strings.TrimSpace(string(b))
				return &s
			}
		}
		if v, ok := os.LookupEnv(k); ok {
			return &v
		}
		return nil
	}
	if v := env("MRS_MIKROTIK_PASSWORD"); v != nil {
		c.MikroTik.Password = *v
	}
	if v := env("MRS_TELEGRAM_BOT_TOKEN"); v != nil {
		c.Telegram.BotToken = *v
	}
	if v := env("MRS_WEB_PASSWORD"); v != nil {
		c.Web.Auth.Password = *v
	}
	if v := env("MRS_BGPVIEW_API_KEY"); v != nil {
		c.External.BGPViewAPIKey = *v
	}
}

func (c *Config) Validate() error {
	if c.MikroTik.Host == "" {
		return errors.New("mikrotik.host is required")
	}
	if c.MikroTik.Password == "CHANGE_ME" || c.Web.Auth.Password == "CHANGE_ME" || c.Telegram.BotToken == "CHANGE_ME" {
		return errors.New("default CHANGE_ME secrets are forbidden")
	}
	if c.Safety.MaxDeleteRatio < 0 || c.Safety.MaxDeleteRatio > 1 {
		return errors.New("safety.max_delete_ratio must be 0..1")
	}
	if c.Scheduler.MaxConcurrent < 1 {
		return errors.New("scheduler.max_concurrent must be >=1")
	}
	seen := map[string]bool{}
	for _, s := range c.Services {
		if !serviceRE.MatchString(s) {
			return fmt.Errorf("invalid service name %q", s)
		}
		if seen[s] {
			return fmt.Errorf("duplicate service %q", s)
		}
		seen[s] = true
	}
	if _, e := time.LoadLocation(c.Timezone); e != nil {
		return fmt.Errorf("timezone: %w", e)
	}
	for _, x := range append(append([]string{}, c.Web.AllowedCIDRs...), c.Web.TrustedProxies...) {
		if _, _, e := net.ParseCIDR(x); e != nil {
			return fmt.Errorf("invalid CIDR %q", x)
		}
	}
	return nil
}

func (c *Config) ServicesInGroup(name string) []string {
	return append([]string(nil), c.Schedules.Groups[name].Services...)
}

func (c *Config) EffectiveSchedule(service string) string {
	if s, ok := c.Schedules.Services[service]; ok && s.Schedule != "" && s.Schedule != "inherit" {
		return s.Schedule
	}
	for _, g := range c.Schedules.Groups {
		for _, x := range g.Services {
			if x == service && g.Schedule != "" && g.Schedule != "inherit" {
				return g.Schedule
			}
		}
	}
	return c.Schedules.Global
}

func ValidateServiceName(s string) bool { return serviceRE.MatchString(s) }

// AtomicWrite удалена отсюда, находится в save.go

func CheckSecurePermissions(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("config %s permissions must be 0600 or stricter", path)
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err == nil && dst.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("config directory permissions must be 0700 or stricter")
	}
	return nil
}

func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "••••"
	}
	return "••••••••" + s[len(s)-4:]
}

func ParseIntEnv(k string, dst *int) {
	if v := os.Getenv(k); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			*dst = n
		}
	}
}
