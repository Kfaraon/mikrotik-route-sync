package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

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

func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

type Config struct {
	path          string                     `yaml:"-"`
	Timezone      string                     `yaml:"timezone"`
	CachePath     string                     `yaml:"cache_path"`
	Logging       LoggingConfig              `yaml:"logging"`
	MikroTik      MikroTikConfig             `yaml:"mikrotik"`
	Telegram      TelegramConfig             `yaml:"telegram"`
	Web           WebConfig                  `yaml:"web"`
	Scheduler     SchedulerConfig            `yaml:"scheduler"`
	Safety        SafetyConfig               `yaml:"safety"`
	Retry         RetryConfig                `yaml:"retry"`
	External      ExternalConfig             `yaml:"external"`
	Snapshots     SnapshotsConfig            `yaml:"snapshots"`
	Schedules     SchedulesConfig            `yaml:"schedules"`
	Services      []string                   `yaml:"services"`
	Overrides     map[string]ServiceOverride `yaml:"overrides"`
}

type LoggingConfig struct {
	Level      string `yaml:"level"`
	File       string `yaml:"file"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxFiles   int    `yaml:"max_files"`
	MaxTotalMB int    `yaml:"max_total_mb"`
	Compress   bool   `yaml:"compress"`
	AlsoStdout bool   `yaml:"also_stdout"`
}

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

type TelegramConfig struct {
	Enabled           bool               `yaml:"enabled"`
	BotToken          string             `yaml:"bot_token"`
	ChatID            string             `yaml:"chat_id"`
	AuthorizedChatIDs []string           `yaml:"authorized_chat_ids"`
	RateLimit         int                `yaml:"rate_limit"`
	WeeklyReport      WeeklyReportConfig `yaml:"weekly_report"`
	Buttons           ButtonsConfig      `yaml:"buttons"`
}

type WeeklyReportConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Schedule string `yaml:"schedule"`
}

type ButtonsConfig struct {
	Enabled             bool `yaml:"enabled"`
	MaxSelectedServices int  `yaml:"max_selected_services"`
}

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

type WebAuthConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type SchedulerConfig struct {
	Parallel       bool     `yaml:"parallel"`
	MaxConcurrent  int      `yaml:"max_concurrent"`
	ReloadInterval Duration `yaml:"reload_interval"`
	CacheTTL       Duration `yaml:"cache_ttl"`
	CachePurge     string   `yaml:"cache_purge"`
}

type SafetyConfig struct {
	MaxDeleteRatio          float64 `yaml:"max_delete_ratio"`
	RequireConfirmationOver int     `yaml:"require_confirmation_over"`
	MinPrefixV4             int     `yaml:"min_prefix_v4"`
	MinPrefixV6             int     `yaml:"min_prefix_v6"`
	AllowHostRoutes         bool    `yaml:"allow_host_routes"`
	MaxASNPrefixes          int     `yaml:"max_asn_prefixes"`
}

type RetryConfig struct {
	MaxAttempts int      `yaml:"max_attempts"`
	BaseDelay   Duration `yaml:"base_delay"`
	MaxDelay    Duration `yaml:"max_delay"`
	Jitter      bool     `yaml:"jitter"`
}

type ExternalConfig struct {
	HTTPTimeout   Duration `yaml:"http_timeout"`
	MaxResponseMB int      `yaml:"max_response_mb"`
	BGPViewAPIKey string   `yaml:"bgpview_api_key"`
	AkamaiAPIKey  string   `yaml:"akamai_api_key"`
	RDAPTimeout   Duration `yaml:"rdap_timeout"`
	Resolver      string   `yaml:"resolver"`
}

type SnapshotsConfig struct {
	Enabled  bool     `yaml:"enabled"`
	TTL      Duration `yaml:"ttl"`
	MaxCount int      `yaml:"max_count"`
}

type SchedulesConfig struct {
	Global   string                     `yaml:"global"`
	Groups   map[string]GroupConfig     `yaml:"groups"`
	Services map[string]ServiceSchedule `yaml:"services"`
}

type GroupConfig struct {
	Schedule string   `yaml:"schedule"`
	Services []string `yaml:"services"`
}

type ServiceSchedule struct {
	Schedule string `yaml:"schedule"`
}

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

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	c.path = path

	// ENV overrides (Приоритет: ENV > config.yaml)
	if p := os.Getenv("MRS_MIKROTIK_PASSWORD"); p != "" {
		c.MikroTik.Password = p
	}
	if t := os.Getenv("MRS_TELEGRAM_BOT_TOKEN"); t != "" {
		c.Telegram.BotToken = t
	}
	if wp := os.Getenv("MRS_WEB_PASSWORD"); wp != "" {
		c.Web.Auth.Password = wp
	}

	// ИСПРАВЛЕНО: Разрешены права 0400 (только чтение) и 0600 (чтение+запись)
	// Важно для production, где config.yaml может быть read-only
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf(
				"config file %s has insecure permissions %o (expected 600 or 400, owner-only access)",
				path, fi.Mode().Perm(),
			)
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
		c.Safety.MinPrefixV4 = 8
	}
	if c.Safety.MinPrefixV6 == 0 {
		c.Safety.MinPrefixV6 = 16
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
		c.Snapshots.MaxCount = 50
	}
	if c.Snapshots.TTL.Duration() == 0 {
		c.Snapshots.TTL = Duration(7 * 24 * time.Hour)
	}
	if c.Web.SessionTimeout.Duration() == 0 {
		c.Web.SessionTimeout = Duration(30 * time.Minute)
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

var serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ValidateServiceName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return serviceNameRE.MatchString(name)
}

func (c *Config) Save() error {
	return AtomicWrite(c.path, c)
}

func (c *Config) Set(path string, value any) error {
	return nil
}

func AtomicWrite(path string, c *Config) error {
	originalData, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read original config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(originalData, &doc); err != nil {
		return fmt.Errorf("failed to parse yaml: %w", err)
	}

	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		rootNode := doc.Content[0]
		if rootNode.Kind == yaml.MappingNode {
			updateServicesNode(rootNode, c.Services)
		}
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return fmt.Errorf("failed to encode yaml: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("failed to close encoder: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Fsync перед rename
	f, err := os.Open(tmpPath)
	if err == nil {
		f.Sync()
		f.Close()
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	return nil
}

func updateServicesNode(rootNode *yaml.Node, services []string) error {
	var servicesKeyNode *yaml.Node
	var servicesValueNode *yaml.Node
	for i := 0; i < len(rootNode.Content); i += 2 {
		if i+1 >= len(rootNode.Content) {
			break
		}
		keyNode := rootNode.Content[i]
		if keyNode.Kind == yaml.ScalarNode && keyNode.Value == "services" {
			servicesKeyNode = keyNode
			servicesValueNode = rootNode.Content[i+1]
			break
		}
	}
	if servicesKeyNode == nil {
		servicesKeyNode = &yaml.Node{Kind: yaml.ScalarNode, Value: "services", Tag: "!!str"}
		servicesValueNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		rootNode.Content = append(rootNode.Content, servicesKeyNode, servicesValueNode)
	}
	servicesValueNode.Content = make([]*yaml.Node, 0, len(services))
	for _, service := range services {
		node := &yaml.Node{Kind: yaml.ScalarNode, Value: service, Tag: "!!str"}
		servicesValueNode.Content = append(servicesValueNode.Content, node)
	}
	return nil
}
