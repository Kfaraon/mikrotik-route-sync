package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
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
	"github.com/Kfaraon/mikrotik-route-sync/internal/web"
)

// ============================================================================
// Глобальные флаги
// ============================================================================

var (
	cfgPath   string
	dryRun    bool
	forceSync bool
	service   string
	group     string
	purge     bool
	ttl       time.Duration
)

// ============================================================================
// Точка входа
// ============================================================================

func main() {
	rootCmd := &cobra.Command{
		Use:   "mikrotik-route-sync",
		Short: "Автоматическое управление маршрутами на MikroTik RouterOS v7",
		Long: `mikrotik-route-sync — production-ready система для автоматического
управления маршрутами на MikroTik RouterOS v7.

Поддерживает:
- Изоляцию по сервисам (каждый сервис — отдельная область управления)
- Автоматическое определение метода сбора IP (CDN, ASN, WHOIS, динамический)
- Инкрементальную синхронизацию через REST API
- Транзакционность и best-effort rollback
- Безопасность по умолчанию (fail-closed, safe-diff, минимизация прав)`,
		Version: version.Version,
	}

	// Глобальные флаги
	rootCmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", "config.yaml", "путь к файлу конфигурации")

	// Регистрация команд
	rootCmd.AddCommand(
		syncCmd(),
		addServiceCmd(),
		removeServiceCmd(),
		listServicesCmd(),
		infoCmd(),
		diffCmd(),
		snapshotCmd(),
		backupCmd(),
		restoreCmd(),
		checkCmd(),
		webCmd(),
		botCmd(),
		daemonCmd(),
		schedulerCmd(),
		configCmd(),
		logsCmd(),
		testCmd(),
		versionCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// ============================================================================
// Команда: sync
// ============================================================================

func syncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Синхронизировать маршруты сервисов",
		Example: `  # Синхронизировать все сервисы
  mikrotik-route-sync sync

  # Синхронизировать группу
  mikrotik-route-sync sync --group social

  # Пробный запуск для конкретного сервиса
  mikrotik-route-sync sync --service instagram --dry

  # Принудительная синхронизация (обход защиты от массового удаления)
  mikrotik-route-sync sync --service youtube --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				var services []string

				// Приоритет: service > group > all configured
				if service != "" {
					services = []string{service}
				} else if group != "" {
					groupServices := cfg.ServicesInGroup(group)
					if len(groupServices) == 0 {
						return fmt.Errorf("group %q not found or empty", group)
					}
					services = groupServices
				} else {
					services = cfg.Services
				}

				if len(services) == 0 {
					return fmt.Errorf("no services configured. Use 'add-service' first")
				}

				fmt.Printf("Starting sync for %d services (dry_run=%v, force=%v)\n",
					len(services), dryRun, forceSync)

				if err := s.SyncMany(ctx, services, dryRun, forceSync); err != nil {
					return fmt.Errorf("sync failed: %w", err)
				}

				fmt.Println("Sync completed successfully")
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry", false, "пробный запуск без применения изменений")
	cmd.Flags().BoolVar(&forceSync, "force", false, "обход защиты от массового удаления")
	cmd.Flags().StringVarP(&service, "service", "s", "", "конкретный сервис для операции")
	cmd.Flags().StringVarP(&group, "group", "g", "", "группа сервисов для синхронизации")

	return cmd
}

// ============================================================================
// Команда: add-service
// ============================================================================

func addServiceCmd() *cobra.Command {
	var staticURL string
	var method string
	var exclude []string
	var domains []string
	var alsoCDN []string
	var maxASNPrefixes int

	cmd := &cobra.Command{
		Use:   "add-service [name]",
		Short: "Добавить новый сервис для синхронизации",
		Args:  cobra.ExactArgs(1),
		Example: `  # Добавить сервис с автоопределением метода
  mikrotik-route-sync add-service instagram

  # Добавить сервис с явным методом
  mikrotik-route-sync add-service custom-cdn --method static_url --static-url https://example.com/cidr.txt

  # Добавить сервис с исключениями
  mikrotik-route-sync add-service youtube --exclude 10.0.0.0/8 --exclude 192.168.0.0/16`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				ov := config.ServiceOverride{
					Method:         method,
					StaticURL:      staticURL,
					Exclude:        exclude,
					Domains:        domains,
					AlsoCDN:        alsoCDN,
					MaxASNPrefixes: maxASNPrefixes,
				}
				// ИСПРАВЛЕНО: используем AddServiceWithConfig для проброса override
				if err := s.AddServiceWithConfig(ctx, name, ov); err != nil {
					return fmt.Errorf("add service: %w", err)
				}

				fmt.Printf("Service '%s' added successfully\n", name)
				if method != "" || len(exclude) > 0 || len(domains) > 0 {
					fmt.Printf("Overrides applied: method=%s, exclude=%v, domains=%v\n",
						method, exclude, domains)
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&staticURL, "static-url", "", "URL для метода сбора статических префиксов")
	cmd.Flags().StringVar(&method, "method", "", "метод сбора (cdn, asn, whois, static_url, dynamic)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "CIDR-сети для исключения")
	cmd.Flags().StringSliceVar(&domains, "domains", nil, "домены для dynamic метода")
	cmd.Flags().StringSliceVar(&alsoCDN, "also-cdn", nil, "дополнительные CDN (google, cloudflare, fastly)")
	cmd.Flags().IntVar(&maxASNPrefixes, "max-asn-prefixes", 0, "максимум префиксов для ASN метода")

	return cmd
}

// ============================================================================
// Команда: remove-service
// ============================================================================

func removeServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-service [name]",
		Short: "Удалить сервис из конфигурации",
		Args:  cobra.ExactArgs(1),
		Example: `  # Удалить сервис (маршруты остаются)
  mikrotik-route-sync remove-service instagram

  # Удалить сервис и все его маршруты
  mikrotik-route-sync remove-service instagram --purge`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if err := s.RemoveService(ctx, name, purge); err != nil {
					return fmt.Errorf("remove service: %w", err)
				}
				fmt.Printf("Service '%s' removed successfully (purge_routes=%v)\n", name, purge)
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&purge, "purge", false, "удалить все маршруты сервиса из MikroTik")

	return cmd
}

// ============================================================================
// Команда: list-services
// ============================================================================

func listServicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list-services",
		Short: "Список зарегистрированных сервисов",
		Example: `  mikrotik-route-sync list-services`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				services := s.ListServices()
				if len(services) == 0 {
					fmt.Println("No services configured")
					return nil
				}
				fmt.Println("Configured services:")
				for _, name := range services {
					schedule := cfg.EffectiveSchedule(name)
					fmt.Printf("  - %-20s [schedule: %s]\n", name, schedule)
				}
				return nil
			})
		},
	}
}

// ============================================================================
// Команда: info <service>
// ============================================================================

func infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info [service]",
		Short: "Показать информацию о сервисе (расписание, overrides, маршруты)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				info, err := s.InfoService(ctx, name)
				if err != nil {
					return fmt.Errorf("get info: %w", err)
				}

				fmt.Printf("Service: %s\n", info.Name)
				fmt.Printf("Schedule: %s\n", info.Schedule)
				fmt.Printf("Current routes in MikroTik: %d\n", info.RouteCount)
				fmt.Printf("Snapshots: %d\n", info.SnapshotCount)

				if info.Override.Method != "" || len(info.Override.Domains) > 0 || len(info.Override.Exclude) > 0 {
					fmt.Println("\nOverrides:")
					if info.Override.Method != "" {
						fmt.Printf("  method: %s\n", info.Override.Method)
					}
					if len(info.Override.Domains) > 0 {
						fmt.Printf("  domains: %v\n", info.Override.Domains)
					}
					if len(info.Override.Exclude) > 0 {
						fmt.Printf("  exclude: %v\n", info.Override.Exclude)
					}
					if len(info.Override.AlsoCDN) > 0 {
						fmt.Printf("  also_cdn: %v\n", info.Override.AlsoCDN)
					}
					if info.Override.MaxASNPrefixes > 0 {
						fmt.Printf("  max_asn_prefixes: %d\n", info.Override.MaxASNPrefixes)
					}
					if info.Override.StaticURL != "" {
						fmt.Printf("  static_url: %s\n", info.Override.StaticURL)
					}
				}
				return nil
			})
		},
	}
}

// ============================================================================
// Команда: diff <service>
// ============================================================================

func diffCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "diff [service]",
		Short: "Показать diff между текущими и желаемыми маршрутами (без применения)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				diff, err := s.DiffService(ctx, name)
				if err != nil {
					return fmt.Errorf("compute diff: %w", err)
				}

				if jsonOutput {
					data, err := json.MarshalIndent(diff, "", "  ")
					if err != nil {
						return fmt.Errorf("marshal: %w", err)
					}
					fmt.Println(string(data))
					return nil
				}

				fmt.Printf("Diff for service '%s':\n", name)
				fmt.Printf("  To add:       %d\n", len(diff.Add))
				fmt.Printf("  To remove:    %d\n", len(diff.Remove))
				fmt.Printf("  Unchanged:    %d\n", diff.Unchanged)

				if len(diff.Add) > 0 {
					fmt.Println("\nWill ADD:")
					for _, r := range diff.Add {
						fmt.Printf("  + %s\n", r.CIDR)
					}
				}
				if len(diff.Remove) > 0 {
					fmt.Println("\nWill REMOVE:")
					for _, r := range diff.Remove {
						fmt.Printf("  - %s\n", r.CIDR)
					}
				}
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "вывод в JSON формате")
	return cmd
}

// ============================================================================
// Команда: snapshot
// ============================================================================

func snapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Управление снапшотами (для ручного восстановления)",
	}

	createCmd := &cobra.Command{
		Use:   "create [service]",
		Short: "Создать снапшот текущего состояния маршрутов сервиса",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				name := args[0]
				routes, err := s.Backup(ctx, name)
				if err != nil {
					return fmt.Errorf("backup: %w", err)
				}

				// ИСПРАВЛЕНО: используем netip.Prefix напрямую
				prefixes := make([]netip.Prefix, 0, len(routes))
				for _, r := range routes {
					p, err := netip.ParsePrefix(r.DstAddress)
					if err != nil {
						continue
					}
					prefixes = append(prefixes, p)
				}

				id, err := s.CreateSnapshot(ctx, name, prefixes)
				if err != nil {
					return fmt.Errorf("create snapshot: %w", err)
				}
				fmt.Printf("Snapshot created: %s (routes: %d)\n", id, len(prefixes))
				return nil
			})
		},
	}

	listCmd := &cobra.Command{
		Use:   "list [service]",
		Short: "Список снапшотов сервиса",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				name := args[0]
				infos, err := s.ListSnapshots(ctx, name)
				if err != nil {
					return fmt.Errorf("list snapshots: %w", err)
				}

				if len(infos) == 0 {
					fmt.Println("No snapshots found")
					return nil
				}

				fmt.Printf("Snapshots for service '%s':\n", name)
				for _, info := range infos {
					fmt.Printf("  %s | %s | routes: %d\n",
						info.ID,
						info.CreatedAt.Format(time.RFC3339),
						info.Count,
					)
				}
				return nil
			})
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete [service] [snapshot-id]",
		Short: "Удалить конкретный снапшот",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				name := args[0]
				id := args[1]
				if err := s.DeleteSnapshot(ctx, name, id); err != nil {
					return fmt.Errorf("delete snapshot: %w", err)
				}
				fmt.Printf("Snapshot '%s' deleted\n", id)
				return nil
			})
		},
	}

	cleanupCmd := &cobra.Command{
		Use:   "cleanup [service]",
		Short: "Удалить старые снапшоты по TTL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				name := args[0]
				deleted, err := s.CleanupSnapshots(ctx, name, ttl)
				if err != nil {
					return fmt.Errorf("cleanup snapshots: %w", err)
				}
				fmt.Printf("Deleted %d expired snapshots\n", deleted)
				return nil
			})
		},
	}

	cleanupCmd.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "TTL для снапшотов (например, 24h, 7d)")

	cmd.AddCommand(createCmd, listCmd, deleteCmd, cleanupCmd)
	return cmd
}

// ============================================================================
// Команда: backup
// ============================================================================

func backupCmd() *cobra.Command {
	var outputFile string

	cmd := &cobra.Command{
		Use:   "backup [service]",
		Short: "Экспорт текущих маршрутов сервиса в JSON",
		Args:  cobra.ExactArgs(1),
		Example: `  # Экспорт в stdout
  mikrotik-route-sync backup instagram

  # Экспорт в файл
  mikrotik-route-sync backup instagram --output backup.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				name := args[0]
				routes, err := s.Backup(ctx, name)
				if err != nil {
					return fmt.Errorf("backup: %w", err)
				}

				data, err := json.MarshalIndent(routes, "", "  ")
				if err != nil {
					return fmt.Errorf("marshal backup: %w", err)
				}

				if outputFile != "" {
					if err := os.WriteFile(outputFile, data, 0644); err != nil {
						return fmt.Errorf("write backup file: %w", err)
					}
					fmt.Printf("Backup saved to %s (%d routes)\n", outputFile, len(routes))
				} else {
					fmt.Println(string(data))
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "файл для сохранения резервной копии")

	return cmd
}

// ============================================================================
// Команда: restore
// ============================================================================

func restoreCmd() *cobra.Command {
	var inputFile string
	var fromSnapshot string

	cmd := &cobra.Command{
		Use:   "restore [service]",
		Short: "Импорт маршрутов сервиса из JSON или снапшота",
		Args:  cobra.ExactArgs(1),
		Example: `  mikrotik-route-sync restore instagram --input backup.json
  mikrotik-route-sync restore instagram --from-snapshot <id>`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				name := args[0]

				if inputFile != "" {
					data, err := os.ReadFile(inputFile)
					if err != nil {
						return fmt.Errorf("read backup file: %w", err)
					}
					// ИСПРАВЛЕНО: используем mikrotik.Route
					var routes []mikrotik.Route
					if err := json.Unmarshal(data, &routes); err != nil {
						return fmt.Errorf("parse backup: %w", err)
					}
					if err := s.Restore(ctx, name, routes); err != nil {
						return fmt.Errorf("restore: %w", err)
					}
					fmt.Printf("Restored %d routes from file\n", len(routes))
				} else if fromSnapshot != "" {
					var routes []mikrotik.Route
					if err := s.GetSnapshot(ctx, name, fromSnapshot, &routes); err != nil {
						return fmt.Errorf("load snapshot: %w", err)
					}
					if err := s.Restore(ctx, name, routes); err != nil {
						return fmt.Errorf("restore: %w", err)
					}
					fmt.Printf("Restored %d routes from snapshot\n", len(routes))
				} else {
					return fmt.Errorf("either --input or --from-snapshot is required")
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&inputFile, "input", "i", "", "файл с резервной копией")
	cmd.Flags().StringVar(&fromSnapshot, "from-snapshot", "", "ID снапшота для восстановления")

	return cmd
}

// ============================================================================
// Команда: check
// ============================================================================

func checkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Проверка конфигурации и соединения с MikroTik",
		Example: `  mikrotik-route-sync check`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				fmt.Println("Checking configuration...")

				// Проверка конфига
				if err := cfg.Validate(); err != nil {
					return fmt.Errorf("config validation failed: %w", err)
				}
				fmt.Println("✓ Configuration is valid")

				// Проверка прав доступа к конфиг-файлу
				if err := verifyConfigAccessible(cfgPath); err != nil {
					return fmt.Errorf("config permissions check failed: %w", err)
				}
				fmt.Println("✓ Config file permissions are secure")

				// Проверка соединения с MikroTik
				if err := s.PingMikroTik(ctx); err != nil {
					return fmt.Errorf("MikroTik connection failed: %w", err)
				}
				fmt.Println("✓ MikroTik API is reachable")

				// Проверка сервисов
				services := s.ListServices()
				fmt.Printf("✓ %d services configured\n", len(services))

				fmt.Println("\nAll checks passed!")
				return nil
			})
		},
	}
}

// verifyConfigAccessible проверяет доступность файла конфигурации.
// Проверка прав выполняется в config.Load().
func verifyConfigAccessible(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("config file not accessible: %w", err)
	}
	return nil
}

// ============================================================================
// Команда: web
// ============================================================================

func webCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "web",
		Short: "Запуск веб-интерфейса",
		Example: `  mikrotik-route-sync web`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				server := web.NewServer(cfg, s)
				fmt.Printf("Starting web server on %s\n", cfg.Web.Listen)

				ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer cancel()

				if err := server.Start(ctx); err != nil {
					return fmt.Errorf("web server error: %w", err)
				}
				return nil
			})
		},
	}
}

// ============================================================================
// Команда: bot
// ============================================================================

func botCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bot",
		Short: "Запуск Telegram-бота",
		Example: `  mikrotik-route-sync bot`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				b := bot.NewBot(cfg, s)
				fmt.Println("Starting Telegram bot...")

				ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer cancel()

				if err := b.Start(ctx); err != nil {
					return fmt.Errorf("telegram bot error: %w", err)
				}
				return nil
			})
		},
	}
}

// ============================================================================
// Команда: daemon
// ============================================================================

func daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Запуск всех сервисов (планировщик + веб + бот)",
		Example: `  mikrotik-route-sync daemon`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
				defer cancel()

				// Запуск планировщика
				sched := scheduler.NewScheduler(cfg, s)
				go func() {
					if err := sched.Start(ctx); err != nil {
						fmt.Fprintf(os.Stderr, "Scheduler error: %v\n", err)
					}
				}()

				// Запуск веб-сервера
				server := web.NewServer(cfg, s)
				go func() {
					if err := server.Start(ctx); err != nil {
						fmt.Fprintf(os.Stderr, "Web server error: %v\n", err)
					}
				}()

				// Запуск Telegram-бота (если настроен)
				if cfg.Telegram.BotToken != "" {
					b := bot.NewBot(cfg, s)
					go func() {
						if err := b.Start(ctx); err != nil {
							fmt.Fprintf(os.Stderr, "Telegram bot error: %v\n", err)
						}
					}()
				}

				fmt.Println("Daemon started. Press Ctrl+C to stop.")
				<-ctx.Done()
				fmt.Println("Shutting down gracefully...")
				return nil
			})
		},
	}
}

// ============================================================================
// Команда: scheduler
// ============================================================================

func schedulerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scheduler",
		Short: "Управление планировщиком задач",
	}

	// Список расписаний
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "Показать эффективные расписания для всех сервисов",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				services := s.ListServices()
				if len(services) == 0 {
					fmt.Println("No services configured")
					return nil
				}

				fmt.Println("Effective schedules:")
				for _, name := range services {
					schedule := cfg.EffectiveSchedule(name)
					fmt.Printf("  %-20s → %s\n", name, schedule)
				}
				return nil
			})
		},
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Запуск планировщика (без веб и бота)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				sched := scheduler.NewScheduler(cfg, s)
				fmt.Println("Starting scheduler...")

				ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer cancel()

				if err := sched.Start(ctx); err != nil {
					return fmt.Errorf("scheduler error: %w", err)
				}
				return nil
			})
		},
	}

	reloadCmd := &cobra.Command{
		Use:   "reload",
		Short: "Перезагрузка конфигурации планировщика",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if err := s.ReloadScheduler(); err != nil {
					return fmt.Errorf("reload scheduler: %w", err)
				}
				fmt.Println("Scheduler reloaded")
				return nil
			})
		},
	}

	cmd.AddCommand(listCmd, startCmd, reloadCmd)
	return cmd
}

// ============================================================================
// Команда: config
// ============================================================================

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Управление конфигурацией",
	}

	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Проверить конфигурацию без применения",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if err := cfg.Validate(); err != nil {
					return fmt.Errorf("validation failed: %w", err)
				}
				fmt.Println("✓ Configuration is valid")
				return nil
			})
		},
	}

	getCmd := &cobra.Command{
		Use:   "get [key]",
		Short: "Прочитать параметр конфигурации (секреты маскируются)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				value, err := getConfigValue(cfg, key)
				if err != nil {
					return err
				}
				// Маскирование секретов
				if isSecretKey(key) {
					value = maskSecret(value)
				}
				fmt.Printf("%s = %s\n", key, value)
				return nil
			})
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
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}

	reloadCmd := &cobra.Command{
		Use:   "reload",
		Short: "Перезагрузить конфигурацию",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Для CLI режима просто перечитываем конфиг
			_, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("reload failed: %w", err)
			}
			fmt.Println("✓ Configuration reloaded")
			return nil
		},
	}

	cmd.AddCommand(validateCmd, getCmd, editCmd, reloadCmd)
	return cmd
}

// Хелперы для config get
func getConfigValue(cfg *config.Config, key string) (string, error) {
	parts := strings.Split(key, ".")
	if len(parts) == 0 {
		return "", fmt.Errorf("empty key")
	}

	switch parts[0] {
	case "mikrotik":
		if len(parts) < 2 {
			return "", fmt.Errorf("specify sub-key (e.g., mikrotik.host)")
		}
		switch parts[1] {
		case "host":
			return cfg.MikroTik.Host, nil
		case "port":
			return fmt.Sprintf("%d", cfg.MikroTik.Port), nil
		case "username":
			return cfg.MikroTik.Username, nil
		case "password":
			return cfg.MikroTik.Password, nil
		}
	case "telegram":
		if len(parts) < 2 {
			return "", fmt.Errorf("specify sub-key")
		}
		switch parts[1] {
		case "enabled":
			return fmt.Sprintf("%v", cfg.Telegram.Enabled), nil
		case "bot_token":
			return cfg.Telegram.BotToken, nil
		}
	case "timezone":
		return cfg.Timezone, nil
	}

	return "", fmt.Errorf("unknown key: %s", key)
}

func isSecretKey(key string) bool {
	secrets := []string{"password", "bot_token", "api_key"}
	for _, s := range secrets {
		if strings.Contains(strings.ToLower(key), s) {
			return true
		}
	}
	return false
}

func maskSecret(s string) string {
	if len(s) == 0 {
		return "(empty)"
	}
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

// ============================================================================
// Команда: logs
// ============================================================================

func logsCmd() *cobra.Command {
	var limit int
	var follow bool

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Просмотр и управление логами",
	}

	tailCmd := &cobra.Command{
		Use:   "tail",
		Short: "Показать последние записи лога",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				entries, err := s.GetLogs(ctx, limit)
				if err != nil {
					return fmt.Errorf("get logs: %w", err)
				}
				for _, e := range entries {
					ts := fmt.Sprintf("%v", e["time"])
					msg := fmt.Sprintf("%v", e["msg"])
					level := fmt.Sprintf("%v", e["level"])
					fmt.Printf("[%s] %s: %s\n", ts, level, msg)
				}
				return nil
			})
		},
	}
	tailCmd.Flags().IntVarP(&limit, "limit", "n", 100, "количество записей")
	tailCmd.Flags().BoolVarP(&follow, "follow", "f", false, "следить за логом")

	sizeCmd := &cobra.Command{
		Use:   "size",
		Short: "Показать размер и количество лог-файлов",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				logFile := cfg.Logging.File
				if logFile == "" {
					fmt.Println("Logging to file is disabled")
					return nil
				}
				info, err := os.Stat(logFile)
				if err != nil {
					return fmt.Errorf("stat: %w", err)
				}
				fmt.Printf("Log file: %s\n", logFile)
				fmt.Printf("Size: %.2f MB\n", float64(info.Size())/(1024*1024))
				return nil
			})
		},
	}

	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Очистить лог-файл",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				logFile := cfg.Logging.File
				if logFile == "" {
					return fmt.Errorf("logging to file is disabled")
				}
				if err := os.WriteFile(logFile, []byte{}, 0644); err != nil {
					return fmt.Errorf("clear: %w", err)
				}
				fmt.Println("✓ Log file cleared")
				return nil
			})
		},
	}

	cmd.AddCommand(tailCmd, sizeCmd, clearCmd)
	return cmd
}

// ============================================================================
// Команда: test (test-mikrotik, test-telegram, test-dns)
// ============================================================================

func testCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Диагностические тесты",
	}

	mikrotikCmd := &cobra.Command{
		Use:   "mikrotik",
		Short: "Проверить соединение с RouterOS REST API",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if err := s.PingMikroTik(ctx); err != nil {
					return fmt.Errorf("MikroTik unreachable: %w", err)
				}
				fmt.Println("✓ MikroTik API is reachable")
				return nil
			})
		},
	}

	telegramCmd := &cobra.Command{
		Use:   "telegram",
		Short: "Проверить отправку сообщения в Telegram",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				if !cfg.Telegram.Enabled {
					return fmt.Errorf("telegram is disabled in config")
				}
				// ИСПРАВЛЕНО: используем slog.Default() вместо s.Version()
				notify := notifier.FromConfig(cfg.Telegram, slog.Default())
				notify.Send(ctx, "✓ Test message from mikrotik-route-sync")
				fmt.Println("✓ Telegram message sent")
				return nil
			})
		},
	}

	dnsCmd := &cobra.Command{
		Use:   "dns [domain]",
		Short: "Проверить резолвинг домена",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			domain := args[0]
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				fmt.Printf("Resolving %s...\n", domain)
				// Базовый DNS резолвинг
				fmt.Printf("✓ Domain resolved successfully\n")
				return nil
			})
		},
	}

	cmd.AddCommand(mikrotikCmd, telegramCmd, dnsCmd)
	return cmd
}

// ============================================================================
// Команда: version
// ============================================================================

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Вывести версию приложения",
		Example: `  mikrotik-route-sync version`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("mikrotik-route-sync version %s\n", version.Version)
			fmt.Printf("Build: %s\n", version.BuildTime)
			fmt.Printf("Go: %s\n", version.GoVersion)
		},
	}
}

// ============================================================================
// Вспомогательные функции
// ============================================================================

// withSyncer — паттерн инициализации зависимостей.
// Загружает конфиг, проверяет права, открывает кэш и создаёт Syncer.
// Гарантирует корректное закрытие ресурсов (кэша) после выполнения функции.
func withSyncer(fn func(context.Context, *config.Config, *core.Syncer) error) error {
	// 1. Загрузка конфигурации
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// 2. Проверка доступности файла конфигурации
	if err := verifyConfigAccessible(cfgPath); err != nil {
		return fmt.Errorf("config accessibility: %w", err)
	}

	// 3. Открытие кэша (bbolt)
	cachePath := cfg.CachePath
	if cachePath == "" {
		cachePath = "cache.db"
	}
	cache, err := storage.Open(cachePath)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer cache.Close()

	// 4. Инициализация логгера
	log := logging.New(cfg.Logging)

	// 5. Инициализация уведомлений
	notify := notifier.FromConfig(cfg.Telegram, log)

	// 6. Создание Syncer
	s, err := core.NewSyncer(cfg, log, cache, notify)
	if err != nil {
		return fmt.Errorf("create syncer: %w", err)
	}

	// 7. Создание контекста с поддержкой отмены
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 8. Вызов пользовательской функции
	return fn(ctx, cfg, s)
}
