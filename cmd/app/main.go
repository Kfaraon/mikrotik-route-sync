package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kfaraon/mikrotik-route-sync/internal/bot"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	"github.com/Kfaraon/mikrotik-route-sync/internal/version"
	webui "github.com/Kfaraon/mikrotik-route-sync/internal/web"
)

// ============================================================================
// Глобальные флаги
// ============================================================================

var (
	cfgPath      string
	dryRun       bool
	forceOp      bool
	svcName      string
	grpName      string
	purge        bool
	snapTTLHours int
	logLimit     int
	logFollow    bool
)

// ============================================================================
// Точка входа
// ============================================================================

func main() {
	root := newRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "app",
		Short: "Автоматическое управление маршрутами на MikroTik RouterOS v7",
		Long: `mikrotik-route-sync — production-ready система управления маршрутами MikroTik.

Пользователь указывает только название сервиса (instagram, youtube, cloudflare),
домен, IP или ASN. Система сама определяет метод сбора, собирает CIDR,
валидирует, агрегирует и инкрементально синхронизирует маршруты через REST API.`,
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "config.yaml", "путь к файлу конфигурации")

	root.AddCommand(
		syncCmd(),
		diffCmd(),
		listCmd(),
		infoCmd(),
		addServiceCmd(),
		removeServiceCmd(),
		backupCmd(),
		restoreCmd(),
		snapshotsCmd(),
		scheduleCmd(),
		botCmd(),
		webCmd(),
		daemonCmd(),
		configCmd(),
		logsCmd(),
		testTelegramCmd(),
		testMikrotikCmd(),
		testDNSCmd(),
		versionCmd(),
	)
	return root
}

// ============================================================================
// sync / diff / list / info
// ============================================================================

func syncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Синхронизировать маршруты (все сервисы, группу или один сервис)",
		Example: `  app sync
  app sync --service instagram
  app sync --group social
  app sync --dry-run
  app sync --service youtube --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				services, err := resolveTargets(cfg)
				if err != nil {
					return err
				}

				// Dry-run по одному сервису: машиночитаемый diff (PROMPT III.3)
				if dryRun && svcName != "" {
					diff, err := s.DiffService(ctx, svcName)
					if err != nil {
						return err
					}
					return printJSON(diff)
				}

				fmt.Printf("Sync: %d сервисов (dry_run=%v force=%v)\n", len(services), dryRun, forceOp)
				if err := s.SyncMany(ctx, services, dryRun, forceOp); err != nil {
					return err
				}
				fmt.Println("Sync completed")
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "расчёт изменений без применения")
	cmd.Flags().BoolVar(&forceOp, "force", false, "обход защиты от массового удаления")
	cmd.Flags().StringVarP(&svcName, "service", "s", "", "один сервис")
	cmd.Flags().StringVarP(&grpName, "group", "g", "", "группа сервисов")
	return cmd
}

func resolveTargets(cfg *config.Config) ([]string, error) {
	switch {
	case svcName != "":
		return []string{svcName}, nil
	case grpName != "":
		gs := cfg.ServicesInGroup(grpName)
		if len(gs) == 0 {
			return nil, fmt.Errorf("group %q not found or empty", grpName)
		}
		return gs, nil
	default:
		if len(cfg.Services) == 0 {
			return nil, fmt.Errorf("no services configured; use 'add-service' first")
		}
		return cfg.Services, nil
	}
}

func diffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <service>",
		Short: "Показать diff сервиса в JSON (без изменений)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				diff, err := s.DiffService(ctx, args[0])
				if err != nil {
					return err
				}
				return printJSON(diff)
			})
		},
	}
}

func listCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"list-services"},
		Short:   "Список сервисов и их эффективных расписаний",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				services := s.ListServices()
				if len(services) == 0 {
					fmt.Println("No services configured")
					return nil
				}
				w := tabWriter()
				fmt.Fprintln(w, "SERVICE\tSCHEDULE\tROUTES")
				for _, name := range services {
					routes := 0
					if rs, err := s.Backup(ctx, name); err == nil {
						routes = len(rs)
					}
					fmt.Fprintf(w, "%s\t%s\t%d\n", name, cfg.EffectiveSchedule(name), routes)
				}
				w.Flush()
				return nil
			})
		},
	}
	return cmd
}

func infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <service>",
		Short: "Информация о сервисе (расписание, overrides, маршруты)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				info, err := s.InfoService(ctx, args[0])
				if err != nil {
					return err
				}
				return printJSON(info)
			})
		},
	}
}

// ============================================================================
// add-service / remove-service
// ============================================================================

func addServiceCmd() *cobra.Command {
	var (
		method    string
		staticURL string
		domains   []string
		exclude   []string
		alsoCDN   []string
		maxASN    int
		noSync    bool
	)

	cmd := &cobra.Command{
		Use:   "add-service <service>",
		Short: "Добавить сервис и первоначально синхронизировать его",
		Args:  cobra.ExactArgs(1),
		Example: `  app add-service instagram
  app add-service custom --method static_url --static-url https://example.org/prefixes.txt
  app add-service youtube --method dynamic --domains youtube.com,googlevideo.com`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				ov := config.ServiceOverride{
					Method:         method,
					StaticURL:      staticURL,
					Domains:        domains,
					Exclude:        exclude,
					AlsoCDN:        alsoCDN,
					MaxASNPrefixes: maxASN,
				}
				if err := s.AddServiceWithConfig(ctx, name, ov); err != nil {
					return err
				}
				fmt.Printf("Service '%s' added\n", name)

				if noSync {
					return nil
				}
				res, err := s.SyncService(ctx, name, false, false)
				if err != nil {
					return fmt.Errorf("service added but initial sync failed: %w", err)
				}
				fmt.Printf("Initial sync: +%d -%d =%d (%s)\n", res.Added, res.Removed, res.Unchanged, res.Duration)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&method, "method", "", "метод сбора (cdn, asn, dynamic, whois, static_url)")
	cmd.Flags().StringVar(&staticURL, "static-url", "", "URL для static_url метода")
	cmd.Flags().StringSliceVar(&domains, "domains", nil, "домены сервиса")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "CIDR-сети для исключения")
	cmd.Flags().StringSliceVar(&alsoCDN, "also-cdn", nil, "доп. CDN (cloudflare, google, aws, fastly)")
	cmd.Flags().IntVar(&maxASN, "max-asn-prefixes", 0, "лимит префиксов ASN/WHOIS")
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "только добавить, без первоначальной синхронизации")
	return cmd
}

func removeServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-service <service>",
		Short: "Удалить сервис (только его AUTO-маршруты)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if err := s.RemoveService(ctx, args[0], purge || forceOp); err != nil {
					return err
				}
				fmt.Printf("Service '%s' removed (routes purged: %v)\n", args[0], purge || forceOp)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "удалить AUTO-маршруты сервиса")
	cmd.Flags().BoolVar(&forceOp, "force", false, "то же, что --purge")
	return cmd
}

// ============================================================================
// backup / restore / snapshots
// ============================================================================

func backupCmd() *cobra.Command {
	var outputFile string

	cmd := &cobra.Command{
		Use:   "backup <service>",
		Short: "Экспорт маршрутов сервиса в JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				routes, err := s.Backup(ctx, args[0])
				if err != nil {
					return err
				}
				data, err := json.MarshalIndent(routes, "", "  ")
				if err != nil {
					return err
				}
				if outputFile != "" {
					if err := os.WriteFile(outputFile, data, 0o600); err != nil {
						return err
					}
					fmt.Printf("Backup saved to %s (%d routes)\n", outputFile, len(routes))
					return nil
				}
				fmt.Println(string(data))
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "файл для сохранения")
	return cmd
}

func restoreCmd() *cobra.Command {
	var fromFile, fromSnapshot string

	cmd := &cobra.Command{
		Use:   "restore <service>",
		Short: "Восстановление маршрутов сервиса из файла или снапшота",
		Args:  cobra.ExactArgs(1),
		Example: `  app restore instagram --from-file backup.json --force
  app restore instagram --from-snapshot 171234567890 --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				name := args[0]
				if !forceOp {
					return fmt.Errorf("restore replaces routes and requires --force")
				}

				var routes []mikrotik.Route
				switch {
				case fromFile != "":
					data, err := os.ReadFile(fromFile)
					if err != nil {
						return fmt.Errorf("read backup: %w", err)
					}
					if err := json.Unmarshal(data, &routes); err != nil {
						return fmt.Errorf("parse backup: %w", err)
					}
				case fromSnapshot != "":
					if err := s.GetSnapshot(ctx, name, fromSnapshot, &routes); err != nil {
						return err
					}
				default:
					return fmt.Errorf("--from-file или --from-snapshot обязателен")
				}

				if err := s.Restore(ctx, name, routes); err != nil {
					return err
				}
				fmt.Printf("Restored '%s' from %d routes\n", name, len(routes))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "JSON-файл бэкапа")
	cmd.Flags().StringVar(&fromSnapshot, "from-snapshot", "", "ID снапшота")
	cmd.Flags().BoolVar(&forceOp, "force", false, "подтверждение восстановления")
	return cmd
}

func snapshotsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "snapshots",
		Aliases: []string{"snapshot"},
		Short:   "Управление снапшотами маршрутов",
	}

	listCmd := &cobra.Command{
		Use:   "list <service>",
		Short: "Список снапшотов сервиса",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				infos, err := s.ListSnapshots(ctx, args[0])
				if err != nil {
					return err
				}
				if len(infos) == 0 {
					fmt.Println("No snapshots found")
					return nil
				}
				return printJSON(infos)
			})
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete <service> <snapshot-id>",
		Short: "Удалить снапшот",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if err := s.DeleteSnapshot(ctx, args[0], args[1]); err != nil {
					return err
				}
				fmt.Printf("Snapshot '%s' deleted\n", args[1])
				return nil
			})
		},
	}

	var createCmd = &cobra.Command{
		Use:   "create <service>",
		Short: "Создать снапшот текущих маршрутов сервиса",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				routes, err := s.Backup(ctx, args[0])
				if err != nil {
					return err
				}
				prefixes := make([]netip.Prefix, 0, len(routes))
				for _, r := range routes {
					if p, err := netip.ParsePrefix(r.DstAddress); err == nil {
						prefixes = append(prefixes, p)
					}
				}
				id, err := s.CreateSnapshot(ctx, args[0], prefixes)
				if err != nil {
					return err
				}
				fmt.Printf("Snapshot created: %s (routes: %d)\n", id, len(prefixes))
				return nil
			})
		},
	}

	cleanupCmd := &cobra.Command{
		Use:   "cleanup <service>",
		Short: "Удалить старые снапшоты по TTL (и сверх max_count)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				n, err := s.CleanupSnapshots(ctx, args[0], time.Duration(snapTTLHours)*time.Hour)
				if err != nil {
					return err
				}
				fmt.Printf("Deleted %d snapshots\n", n)
				return nil
			})
		},
	}
	cleanupCmd.Flags().IntVar(&snapTTLHours, "ttl", 168, "TTL снапшотов в часах")

	cmd.AddCommand(createCmd, listCmd, deleteCmd, cleanupCmd)
	return cmd
}

// ============================================================================
// schedule / bot / web / daemon
// ============================================================================

func scheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "schedule",
		Aliases: []string{"scheduler"},
		Short:   "Расписания синхронизации",
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "Показать эффективные расписания",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				fmt.Printf("global: %s\n", cfg.Schedules.Global)
				for _, name := range s.ListServices() {
					fmt.Printf("  %-20s → %s\n", name, cfg.EffectiveSchedule(name))
				}
				return nil
			})
		},
	}

	reloadCmd := &cobra.Command{
		Use:   "reload",
		Short: "Перечитать расписания (SIGHUP для daemon)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if err := s.ReloadScheduler(); err != nil {
					return err
				}
				fmt.Println("Scheduler reload requested")
				return nil
			})
		},
	}

	cmd.AddCommand(listCmd, reloadCmd)
	return cmd
}

func botCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bot",
		Short: "Запустить Telegram-бота",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				b, err := bot.NewBot(cfg, s)
				if err != nil {
					return err
				}
				fmt.Println("Telegram bot started (Ctrl+C to stop)")
				return b.Run(ctx)
			})
		},
	}
}

func webCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "web",
		Short: "Запустить Web UI и REST API",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				srv, err := webui.NewServer(cfg, s, slogLogger())
				if err != nil {
					return err
				}
				fmt.Printf("Web UI: http://%s (Ctrl+C to stop)\n", cfg.Web.Listen)
				return srv.Start(ctx)
			})
		},
	}
}

func daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Запустить всё: scheduler + web + bot (SIGHUP — reload)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cache, log, err := bootstrap()
			if cache != nil {
				defer cache.Close()
			}
			if err != nil {
				return err
			}

			notify := notifier.FromConfig(cfg.Telegram, log)
			s, err := core.NewSyncer(cfg, log, cache, notify)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			pidFile := cfgPath + ".pid"
			if err := os.WriteFile(pidFile, []byte(fmt.Sprint(os.Getpid())), 0o600); err == nil {
				defer os.Remove(pidFile)
			}

			sched := scheduler.New(cfg, s, notify, log)
			s.SetReloadFunc(func() error {
				newCfg, err := config.Load(cfgPath)
				if err != nil {
					return fmt.Errorf("reload config: %w", err)
				}
				*cfg = *newCfg
				return sched.Reload()
			})

			sched.StartNow()

			// SIGHUP → перечитать конфиг и перестроить расписания.
			hup := make(chan os.Signal, 1)
			if supportsSIGHUP() {
				signal.Notify(hup, sighupSignal)
				go func() {
					for range hup {
						log.Info("SIGHUP received, reloading schedules")
						if err := s.ReloadScheduler(); err != nil {
							log.Error("reload failed", "err", err)
						}
					}
				}()
			}

			errCh := make(chan error, 3)
			if cfg.Web.Enabled {
				srv, err := webui.NewServer(cfg, s, log)
				if err != nil {
					return fmt.Errorf("web init: %w", err)
				}
				go func() { errCh <- srv.Start(ctx) }()
			}
			if cfg.Telegram.Enabled {
				b, err := bot.New(cfg.Telegram, s, log)
				if err != nil {
					log.Error("telegram bot init failed, continuing without bot", "err", err)
				} else {
					go func() { errCh <- b.Run(ctx) }()
				}
			}

			log.Info("daemon started", "version", version.Version, "services", cfg.Services)

			select {
			case <-ctx.Done():
				log.Info("shutdown signal received, stopping gracefully")
			case err := <-errCh:
				if err != nil {
					return err
				}
			}

			sched.Stop(context.Background())
			log.Info("daemon stopped")
			return nil
		},
	}
}

// ============================================================================
// config
// ============================================================================

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Работа с конфигурацией",
	}

	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Проверить конфигурацию без применения",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			// Проверка парсинга всех расписаний
			for _, name := range cfg.Services {
				spec := cfg.EffectiveSchedule(name)
				if err := scheduler.Validate(spec); err != nil {
					return fmt.Errorf("schedule %q for %s: %w", spec, name, err)
				}
			}
			fmt.Println("OK: configuration is valid")
			return nil
		},
	}

	reloadCmd := &cobra.Command{
		Use:   "reload",
		Short: "Отправить SIGHUP запущенному daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !supportsSIGHUP() {
				return fmt.Errorf("SIGHUP is not supported on this platform")
			}
			return sighupSelf()
		},
	}

	editCmd := &cobra.Command{
		Use:   "edit",
		Short: "Открыть конфигурацию в $EDITOR",
		RunE: func(cmd *cobra.Command, args []string) error {
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vi"
			}
			c := exec.Command(editor, cfgPath)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}

	getCmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Прочитать параметр (секреты маскируются)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			val, err := getConfigValue(cfg, args[0])
			if err != nil {
				return err
			}
			if config.IsSensitiveKey(args[0]) {
				val = webui.Mask(val)
			}
			fmt.Printf("%s = %s\n", args[0], val)
			return nil
		},
	}

	setCmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Изменить параметр конфигурации (атомарно)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if err := cfg.Set(args[0], args[1]); err != nil {
				return err
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("%s = %s saved\n", args[0], args[1])
			return nil
		},
	}

	cmd.AddCommand(validateCmd, reloadCmd, editCmd, getCmd, setCmd)
	return cmd
}

// getConfigValue читает значение параметра по точечному пути.
func getConfigValue(cfg *config.Config, key string) (string, error) {
	val, err := cfg.GetPath(key)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", val), nil
}

// ============================================================================
// logs
// ============================================================================

func logsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Просмотр логов",
	}

	tailCmd := &cobra.Command{
		Use:   "tail",
		Short: "Последние строки лога (-f — слежение)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if cfg.Logging.File == "" {
				return fmt.Errorf("logging.file is empty")
			}
			if logFollow {
				return tailLogFollow(cfg.Logging.File)
			}
			entries, err := logging.TailLogFile(cfg.Logging.File, logLimit)
			if err != nil {
				return err
			}
			for _, e := range entries {
				line, _ := json.Marshal(e)
				fmt.Println(string(line))
			}
			return nil
		},
	}
	tailCmd.Flags().IntVarP(&logLimit, "lines", "n", 100, "количество строк")
	tailCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "следить за файлом")

	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Очистить лог-файл",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if cfg.Logging.File == "" {
				return fmt.Errorf("logging.file is empty")
			}
			if err := os.WriteFile(cfg.Logging.File, nil, 0o600); err != nil {
				return err
			}
			fmt.Println("Log cleared")
			return nil
		},
	}

	sizeCmd := &cobra.Command{
		Use:   "size",
		Short: "Размер и количество файлов логов",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if cfg.Logging.File == "" {
				return fmt.Errorf("logging.file is empty")
			}
			dir, base := filepath.Split(cfg.Logging.File)
			matches, err := filepath.Glob(filepath.Join(dir, base+"*"))
			if err != nil {
				return err
			}
			var total int64
			for _, m := range matches {
				fi, err := os.Stat(m)
				if err != nil {
					continue
				}
				total += fi.Size()
				fmt.Printf("%10.2f MB  %s\n", float64(fi.Size())/(1<<20), m)
			}
			fmt.Printf("files: %d, total: %.2f MB\n", len(matches), float64(total)/(1<<20))
			return nil
		},
	}

	cmd.AddCommand(tailCmd, clearCmd, sizeCmd)
	return cmd
}

// tailLogFollow — простой tail -f: читаем новые данные с poll.
func tailLogFollow(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	partial := ""
	for {
		n, err := f.Read(buf)
		if n > 0 {
			partial += string(buf[:n])
			for {
				i := strings.IndexByte(partial, '\n')
				if i < 0 {
					break
				}
				fmt.Println(partial[:i])
				partial = partial[i+1:]
			}
		}
		if err != nil && err != io.EOF {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ============================================================================
// test-*
// ============================================================================

func testTelegramCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test-telegram",
		Short: "Проверить Telegram",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			n, err := notifier.NewTelegram(cfg.Telegram, slogLogger())
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := n.Send(ctx, "test message from mikrotik-route-sync"); err != nil {
				return err
			}
			fmt.Println("OK: telegram message sent")
			return nil
		},
	}
	return cmd
}

func testMikrotikCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test-mikrotik",
		Short: "Проверить RouterOS REST API",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if err := s.PingMikroTik(ctx); err != nil {
					return err
				}
				fmt.Println("OK: RouterOS REST API reachable")
				return nil
			})
		},
	}
}

func testDNSCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test-dns <domain>",
		Short: "Проверить DNS-резолвинг",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				ips, err := s.ResolveDomain(ctx, args[0])
				if err != nil {
					return err
				}
				fmt.Printf("%s → %v\n", args[0], ips)
				return nil
			})
		},
	}
}

// ============================================================================
// version
// ============================================================================

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Версия и информация о сборке",
		Run: func(cmd *cobra.Command, args []string) {
			info := version.Get()
			fmt.Printf("mikrotik-route-sync %s\ncommit: %s\nbuilt: %s\ngo: %s\nos/arch: %s/%s\n",
				info.Version, info.Commit, info.BuildDate, info.GoVersion, info.OS, info.Arch)
		},
	}
}

// ============================================================================
// DI-хелпер: withSyncer
// ============================================================================

// bootstrap загружает конфиг, открывает bbolt-кэш и инициализирует логгер.
func bootstrap() (*config.Config, *storage.Cache, *slog.Logger, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}
	log, err := logging.Setup(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("logging setup: %w", err)
	}
	cachePath := cfg.CachePath
	if cachePath == "" {
		cachePath = "cache.db"
	}
	cache, err := storage.Open(cachePath)
	if err != nil {
		return cfg, nil, log, fmt.Errorf("open cache: %w", err)
	}
	return cfg, cache, log, nil
}

// withSyncer — загружает конфиг, открывает кэш, создаёт Syncer,
// настраивает graceful shutdown и вызывает fn.
func withSyncer(fn func(context.Context, *config.Config, *core.Syncer) error) error {
	cfg, cache, log, err := bootstrap()
	if cache != nil {
		defer cache.Close()
	}
	if err != nil {
		return err
	}

	notify := notifier.FromConfig(cfg.Telegram, log)
	s, err := core.NewSyncer(cfg, log, cache, notify)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return fn(ctx, cfg, s)
}

func slogLogger() *slog.Logger { return slog.Default() }

// tabWriter — общий табличный writer для списков.
func tabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

// printJSON — машиночитаемый вывод.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
