package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kfaraon/mikrotik-route-sync/internal/bot"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
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
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry", false, "пробный запуск без применения изменений")
	rootCmd.PersistentFlags().BoolVar(&forceSync, "force", false, "обход защиты от массового удаления")
	rootCmd.PersistentFlags().StringVarP(&service, "service", "s", "", "конкретный сервис для операции")

	// Регистрация команд
	rootCmd.AddCommand(
		syncCmd(),
		addServiceCmd(),
		removeServiceCmd(),
		listServicesCmd(),
		snapshotCmd(),
		backupCmd(),
		restoreCmd(),
		checkCmd(),
		webCmd(),
		botCmd(),
		daemonCmd(),
		schedulerCmd(),
		logsCmd(),
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
	return &cobra.Command{
		Use:   "sync",
		Short: "Синхронизировать маршруты сервисов",
		Example: `  # Синхронизировать все сервисы
  mikrotik-route-sync sync

  # Пробный запуск для конкретного сервиса
  mikrotik-route-sync sync --service instagram --dry

  # Принудительная синхронизация (обход защиты от массового удаления)
  mikrotik-route-sync sync --service youtube --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				services := cfg.Services
				if service != "" {
					services = []string{service}
				}

				if len(services) == 0 {
					return fmt.Errorf("no services configured. Use 'add-service' first")
				}

				fmt.Printf("Starting sync for %d services (dry_run=%v, force=%v)\n",
					len(services), dryRun, forceSync)

				if err := s.SyncMany(ctx, services, dryRun); err != nil {
					return fmt.Errorf("sync failed: %w", err)
				}

				fmt.Println("Sync completed successfully")
				return nil
			})
		},
	}
}

// ============================================================================
// Команда: add-service
// ============================================================================

func addServiceCmd() *cobra.Command {
	var staticURL string
	var method string
	var exclude []string

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
					Method:    method,
					StaticURL: staticURL,
					Exclude:   exclude,
				}
				if err := s.AddService(ctx, name); err != nil {
					return fmt.Errorf("add service: %w", err)
				}
				fmt.Printf("Service '%s' added successfully\n", name)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&staticURL, "static-url", "", "URL для метода сбора статических префиксов")
	cmd.Flags().StringVar(&method, "method", "", "метод сбора (cdn, asn, whois, static_url, dynamic)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "CIDR-сети для исключения")

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
					fmt.Printf("  - %s\n", name)
				}
				return nil
			})
		},
	}
}

// ============================================================================
// Команда: snapshot
// ============================================================================

func snapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Управление снапшотами (для ручного восстановления)",
	}

	// Создать снапшот
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

				prefixes := make([]string, 0, len(routes))
				for _, r := range routes {
					prefixes = append(prefixes, r.DstAddress)
				}

				id, err := s.CreateSnapshot(ctx, name, prefixes)
				if err != nil {
					return fmt.Errorf("create snapshot: %w", err)
				}
				fmt.Printf("Snapshot created: %s (routes: %d)\n", id, len(routes))
				return nil
			})
		},
	}

	// Список снапшотов
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

	// Удалить снапшот
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

	// Очистка старых снапшотов
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

	cmd := &cobra.Command{
		Use:   "restore [service]",
		Short: "Импорт маршрутов сервиса из JSON",
		Args:  cobra.ExactArgs(1),
		Example: `  mikrotik-route-sync restore instagram --input backup.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				name := args[0]

				if inputFile == "" {
					return fmt.Errorf("--input flag is required")
				}

				data, err := os.ReadFile(inputFile)
				if err != nil {
					return fmt.Errorf("read backup file: %w", err)
				}

				var routes []core.RestoreRoute
				if err := json.Unmarshal(data, &routes); err != nil {
					return fmt.Errorf("parse backup: %w", err)
				}

				if err := s.Restore(ctx, name, routes); err != nil {
					return fmt.Errorf("restore: %w", err)
				}

				fmt.Printf("Restore completed (%d routes processed)\n", len(routes))
				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&inputFile, "input", "i", "", "файл с резервной копией")

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
				if err := checkConfigPermissions(cfgPath); err != nil {
					return fmt.Errorf("config permissions check failed: %w", err)
				}
				fmt.Println("✓ Config file permissions are secure (600)")

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
				ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
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

	// Запуск планировщика
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

	// Перезагрузка конфига
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

	cmd.AddCommand(startCmd, reloadCmd)
	return cmd
}

// ============================================================================
// Команда: logs
// ============================================================================

func logsCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Просмотр последних записей лог-файла",
		Example: `  mikrotik-route-sync logs --limit 50`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, cfg *config.Config, s *core.Syncer) error {
				entries, err := s.GetLogs(ctx, limit)
				if err != nil {
					return fmt.Errorf("get logs: %w", err)
				}

				if len(entries) == 0 {
					fmt.Println("No log entries found")
					return nil
				}

				for _, entry := range entries {
					ts := entry["time"]
					msg := entry["msg"]
					level := entry["level"]
					fmt.Printf("[%s] %s: %s\n", ts, level, msg)
				}
				return nil
			})
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 100, "количество записей для вывода")

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

	// 2. Проверка прав доступа к конфиг-файлу (требование безопасности)
	if err := checkConfigPermissions(cfgPath); err != nil {
		return fmt.Errorf("config permissions: %w", err)
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

// checkConfigPermissions проверяет, что права доступа к конфиг-файлу не более 600.
// Это требование безопасности из промпта (минимизация прав).
func checkConfigPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config file: %w", err)
	}

	// Проверяем, что файл не доступен другим пользователям
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		return fmt.Errorf(
			"config file %s has insecure permissions %o (expected 600 or stricter). Run: chmod 600 %s",
			path, perm, path,
		)
	}

	return nil
}
