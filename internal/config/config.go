package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Timezone  string              `mapstructure:"timezone" yaml:"timezone"`
	Logging   LoggingConfig       `mapstructure:"logging" yaml:"logging"`
	MikroTik  MikroTikConfig      `mapstructure:"mikrotik" yaml:"mikrotik"`
	Telegram  TelegramConfig      `mapstructure:"telegram" yaml:"telegram"`
	Web       WebConfig           `mapstructure:"web" yaml:"web"`
	Scheduler SchedulerConfig     `mapstructure:"scheduler" yaml:"scheduler"`
	Schedules SchedulesConfig     `mapstructure:"schedules" yaml:"schedules"`
	Services  []string            `mapstructure:"services" yaml:"services"`
	Overrides map[string]Override `mapstructure:"overrides" yaml:"overrides"`
	path      string
}

type LoggingConfig struct {
	Level, File string `mapstructure:"level,file" yaml:"level,file"`
	MaxSizeMB, MaxFiles, MaxTotalMB int `mapstructure:"max_size_mb,max_files,max_total_mb" yaml:"max_size_mb,max_files,max_total_mb"`
	Compress, AlsoStdout bool `mapstructure:"compress,also_stdout" yaml:"compress,also_stdout"`
}
type MikroTikConfig struct {
	Host, Username, Password, Gateway, RoutingTable, CommentPrefix string `mapstructure:"host,username,password,gateway,routing_table,comment_prefix" yaml:"host,username,password,gateway,routing_table,comment_prefix"`
	UseSSL, VerifySSL bool `mapstructure:"use_ssl,verify_ssl" yaml:"use_ssl,verify_ssl"`
	Distance          int  `mapstructure:"distance" yaml:"distance"`
}
type TelegramConfig struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`
	BotToken, ChatID, WeeklyReport string `mapstructure:"bot_token,chat_id,weekly_report" yaml:"bot_token,chat_id,weekly_report"`
	AuthorizedChatIDs []string `mapstructure:"authorized_chat_ids" yaml:"authorized_chat_ids"`
	Buttons struct { Enabled bool; MaxSelectedServices int } `mapstructure:"buttons" yaml:"buttons"`
}
type WebConfig struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`
	Listen, SessionTimeout string `mapstructure:"listen,session_timeout" yaml:"listen,session_timeout"`
	Auth struct { Enabled bool; Username, Password string } `mapstructure:"auth" yaml:"auth"`
}
type SchedulerConfig struct {
	Parallel bool `mapstructure:"parallel" yaml:"parallel"`
	MaxConcurrent int `mapstructure:"max_concurrent" yaml:"max_concurrent"`
	ReloadInterval, CacheTTL, CachePurge string `mapstructure:"reload_interval,cache_ttl,cache_purge" yaml:"reload_interval,cache_ttl,cache_purge"`
}
type SchedulesConfig struct {
	Global   string `mapstructure:"global" yaml:"global"`
	Groups   map[string]struct { Schedule string; Services []string } `mapstructure:"groups" yaml:"groups"`
	Services map[string]struct { Schedule string } `mapstructure:"services" yaml:"services"`
}
type Override struct {
	Domains []string `mapstructure:"domains" yaml:"domains"`
	MaxASNPrefixes int `mapstructure:"max_asn_prefixes" yaml:"max_asn_prefixes"`
	Exclude []string `mapstructure:"exclude" yaml:"exclude"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	if err := v.ReadInConfig(); err != nil { return nil, fmt.Errorf("read config: %w", err) }
	
	var c Config
	if err := v.Unmarshal(&c); err != nil { return nil, fmt.Errorf("unmarshal: %w", err) }
	
	c.path = path
	c.applyDefaults()
	if err := os.MkdirAll(filepath.Dir(c.Logging.File), 0o755); err != nil { return nil, err }
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Timezone == "" { c.Timezone = "UTC" }
	if c.Logging.Level == "" { c.Logging.Level = "info" }
	if c.Logging.File == "" { c.Logging.File = "/var/log/mikrotik-route-sync/app.log" }
	if c.Logging.MaxSizeMB == 0 { c.Logging.MaxSizeMB = 10 }
	if c.Logging.MaxFiles == 0 { c.Logging.MaxFiles = 5 }
	if c.Logging.MaxTotalMB == 0 { c.Logging.MaxTotalMB = 50 }
	if c.MikroTik.Distance == 0 { c.MikroTik.Distance = 2 }
	if c.MikroTik.CommentPrefix == "" { c.MikroTik.CommentPrefix = "AUTO" }
	if c.MikroTik.RoutingTable == "" { c.MikroTik.RoutingTable = "main" }
	if c.Web.Listen == "" { c.Web.Listen = ":8080" }
	if c.Scheduler.CacheTTL == "" { c.Scheduler.CacheTTL = "24h" }
}

func (c *Config) ScheduleFor(service string) string {
	if s, ok := c.Schedules.Services[service]; ok && s.Schedule != "" && s.Schedule != "inherit" { return s.Schedule }
	for _, g := range c.Schedules.Groups {
		for _, s := range g.Services {
			if s == service && g.Schedule != "" && g.Schedule != "inherit" { return g.Schedule }
		}
	}
	if c.Schedules.Global != "" && c.Schedules.Global != "inherit" { return c.Schedules.Global }
	return "every 6h"
}

func (c *Config) ServicesInGroup(group string) []string {
	if g, ok := c.Schedules.Groups[group]; ok { return g.Services }
	return nil
}

func (c *Config) Comment(service string) string { return fmt.Sprintf("%s:%s", c.MikroTik.CommentPrefix, service) }

func (c *Config) Get(path string) (string, error) {
	switch path {
	case "mikrotik.host": return c.MikroTik.Host, nil
	case "mikrotik.username": return c.MikroTik.Username, nil
	case "mikrotik.password": return c.MikroTik.Password, nil
	case "mikrotik.gateway": return c.MikroTik.Gateway, nil
	case "telegram.bot_token": return c.Telegram.BotToken, nil
	case "telegram.chat_id": return c.Telegram.ChatID, nil
	case "web.auth.username": return c.Web.Auth.Username, nil
	case "web.auth.password": return c.Web.Auth.Password, nil
	case "timezone": return c.Timezone, nil
	}
	return "", fmt.Errorf("unknown key: %s", path)
}

func (c *Config) Set(path, value string) error {
	switch path {
	case "mikrotik.host": c.MikroTik.Host = value
	case "mikrotik.username": c.MikroTik.Username = value
	case "mikrotik.password": c.MikroTik.Password = value
	case "mikrotik.gateway": c.MikroTik.Gateway = value
	case "telegram.bot_token": c.Telegram.BotToken = value
	case "telegram.chat_id": c.Telegram.ChatID = value
	case "web.auth.username": c.Web.Auth.Username = value
	case "web.auth.password": c.Web.Auth.Password = value
	case "timezone": c.Timezone = value
	default: return fmt.Errorf("unknown key: %s", path)
	}
	return nil
}

func IsSecret(key string) bool {
	return strings.HasSuffix(key, ".password") || strings.HasSuffix(key, ".bot_token")
}