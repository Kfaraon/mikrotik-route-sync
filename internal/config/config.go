package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Timezone  string          `mapstructure:"timezone" yaml:"timezone"`
	Logging   LoggingConfig   `mapstructure:"logging" yaml:"logging"`
	MikroTik  MikroTikConfig  `mapstructure:"mikrotik" yaml:"mikrotik"`
	Telegram  TelegramConfig  `mapstructure:"telegram" yaml:"telegram"`
	Web       WebConfig       `mapstructure:"web" yaml:"web"`
	Scheduler SchedulerConfig `mapstructure:"scheduler" yaml:"scheduler"`
	Schedules SchedulesConfig `mapstructure:"schedules" yaml:"schedules"`
	Services  []string        `mapstructure:"services" yaml:"services"`
	Overrides map[string]ServiceOverride `mapstructure:"overrides" yaml:"overrides"`
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
	Host         string `mapstructure:"host" yaml:"host"`
	Username     string `mapstructure:"username" yaml:"username"`
	Password     string `mapstructure:"password" yaml:"password"`
	UseSSL       bool   `mapstructure:"use_ssl" yaml:"use_ssl"`
	VerifySSL    bool   `mapstructure:"verify_ssl" yaml:"verify_ssl"`
	Gateway      string `mapstructure:"gateway" yaml:"gateway"`
	RoutingTable string `mapstructure:"routing_table" yaml:"routing_table"`
	Distance     int    `mapstructure:"distance" yaml:"distance"`
	CommentPrefix string `mapstructure:"comment_prefix" yaml:"comment_prefix"`
}

type TelegramConfig struct {
	Enabled           bool     `mapstructure:"enabled" yaml:"enabled"`
	BotToken          string   `mapstructure:"bot_token" yaml:"bot_token"`
	ChatID            string   `mapstructure:"chat_id" yaml:"chat_id"`
	AuthorizedChatIDs []string `mapstructure:"authorized_chat_ids" yaml:"authorized_chat_ids"`
	Buttons           struct {
		Enabled             bool `mapstructure:"enabled" yaml:"enabled"`
		MaxSelectedServices int  `mapstructure:"max_selected_services" yaml:"max_selected_services"`
	} `mapstructure:"buttons" yaml:"buttons"`
	WeeklyReport string `mapstructure:"weekly_report" yaml:"weekly_report"`
}

type WebConfig struct {
	Enabled        bool   `mapstructure:"enabled" yaml:"enabled"`
	Listen         string `mapstructure:"listen" yaml:"listen"`
	Auth           struct {
		Enabled  bool   `mapstructure:"enabled" yaml:"enabled"`
		Username string `mapstructure:"username" yaml:"username"`
		Password string `mapstructure:"password" yaml:"password"`
	} `mapstructure:"auth" yaml:"auth"`
	SessionTimeout string `mapstructure:"session_timeout" yaml:"session_timeout"`
}

type SchedulerConfig struct {
	Parallel      bool   `mapstructure:"parallel" yaml:"parallel"`
	MaxConcurrent int    `mapstructure:"max_concurrent" yaml:"max_concurrent"`
	ReloadInterval string `mapstructure:"reload_interval" yaml:"reload_interval"`
	CacheTTL      string `mapstructure:"cache_ttl" yaml:"cache_ttl"`
	CachePurge    string `mapstructure:"cache_purge" yaml:"cache_purge"`
}

type SchedulesConfig struct {
	Global   string                          `mapstructure:"global" yaml:"global"`
	Groups   map[string]GroupSchedule        `mapstructure:"groups" yaml:"groups"`
	Services map[string]struct{ Schedule string } `mapstructure:"services" yaml:"services"`
}

type GroupSchedule struct {
	Schedule string   `mapstructure:"schedule" yaml:"schedule"`
	Services []string `mapstructure:"services" yaml:"services"`
}

type ServiceOverride struct {
	Domains        []string `mapstructure:"domains" yaml:"domains"`
	MaxASNPrefixes int      `mapstructure:"max_asn_prefixes" yaml:"max_asn_prefixes"`
	Exclude        []string `mapstructure:"exclude" yaml:"exclude"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	// Поддержка переменных окружения с префиксом MRS_ (MikroTik Route Sync)
	// Например: MRS_MIKROTIK_PASSWORD, MRS_TELEGRAM_BOT_TOKEN
	v.SetEnvPrefix("MRS")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

	// Явный биндинг секретов, если их нет в yaml
	_ = v.BindEnv("mikrotik.password", "MRS_MIKROTIK_PASSWORD")
	_ = v.BindEnv("telegram.bot_token", "MRS_TELEGRAM_BOT_TOKEN")
	_ = v.BindEnv("web.auth.password", "MRS_WEB_PASSWORD")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			// Если файл существует, но есть ошибка парсинга - возвращаем её
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Валидация обязательных полей
	if cfg.MikroTik.Host == "" {
		return nil, fmt.Errorf("mikrotik.host is required")
	}

	return &cfg, nil
}

func (c *Config) Comment(service string) string {
	return fmt.Sprintf("%s:%s", c.MikroTik.CommentPrefix, service)
}

func (c *Config) ScheduleFor(service string) string {
	if s, ok := c.Schedules.Services[service]; ok && s.Schedule != "" {
		return s.Schedule
	}
	for _, g := range c.Schedules.Groups {
		for _, s := range g.Services {
			if s == service {
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
