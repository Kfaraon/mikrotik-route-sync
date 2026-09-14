package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Kfaraon/mikrotik-route-sync/internal/bot"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	"github.com/spf13/cobra"
)

func main() {
	var configFile string
	rootCmd := &cobra.Command{
		Use:   "app",
		Short: "MikroTik Route Sync",
	}
	rootCmd.PersistentFlags().StringVar(&configFile, "config", "config.yaml", "config file path")

	loadConfig := func() (*config.Config, error) {
		return config.Load(configFile)
	}

	openCache := func() (*storage.Cache, error) {
		return storage.Open("cache.db")
	}

	rootCmd.AddCommand(syncCmd(loadConfig, openCache))
	rootCmd.AddCommand(addCmd(loadConfig, openCache))
	rootCmd.AddCommand(removeCmd(loadConfig, openCache))
	rootCmd.AddCommand(infoCmd(loadConfig, openCache))
	rootCmd.AddCommand(scheduleCmd(loadConfig))
	rootCmd.AddCommand(botCmd(loadConfig, openCache))
	rootCmd.AddCommand(testTelegramCmd(loadConfig))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func syncCmd(loadConfig func() (*config.Config, error), openCache func() (*storage.Cache, error)) *cobra.Command {
	var service, group string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync routes for services",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cache, err := openCache()
			if err != nil {
				return fmt.Errorf("open cache: %w", err)
			}
			defer cache.Close()

			log := logging.New(cfg.Logging)
			n := notifier.FromConfig(cfg.Telegram, log)
			syncer := core.NewSyncer(cfg, log, cache, n)

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			var services []string
			switch {
			case service != "":
				services = []string{service}
			case group != "":
				services = cfg.ServicesInGroup(group)
			default:
				services = cfg.Services
			}
			if len(services) == 0 {
				return fmt.Errorf("no services to sync")
			}
			return syncer.SyncMany(ctx, services, dryRun)
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "sync one service")
	cmd.Flags().StringVar(&group, "group", "", "sync all services in a group")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show plan without applying")
	return cmd
}

func addCmd(loadConfig func() (*config.Config, error), openCache func() (*storage.Cache, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "add-service <name>",
		Short: "Add a new service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cache, err := openCache()
			if err != nil {
				return fmt.Errorf("open cache: %w", err)
			}
			defer cache.Close()

			log := logging.New(cfg.Logging)
			n := notifier.FromConfig(cfg.Telegram, log)
			return core.NewSyncer(cfg, log, cache, n).AddService(context.Background(), args[0])
		},
	}
}

func removeCmd(loadConfig func() (*config.Config, error), openCache func() (*storage.Cache, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-service <name>",
		Short: "Remove a service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cache, err := openCache()
			if err != nil {
				return fmt.Errorf("open cache: %w", err)
			}
			defer cache.Close()

			log := logging.New(cfg.Logging)
			n := notifier.FromConfig(cfg.Telegram, log)
			return core.NewSyncer(cfg, log, cache, n).RemoveService(context.Background(), args[0])
		},
	}
}

func infoCmd(loadConfig func() (*config.Config, error), openCache func() (*storage.Cache, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "info <service>",
		Short: "Show info about a service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cache, err := openCache()
			if err != nil {
				return fmt.Errorf("open cache: %w", err)
			}
			defer cache.Close()

			log := logging.New(cfg.Logging)
			n := notifier.FromConfig(cfg.Telegram, log)
			return core.NewSyncer(cfg, log, cache, n).Info(context.Background(), args[0], cmd.OutOrStdout())
		},
	}
}

func scheduleCmd(loadConfig func() (*config.Config, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "schedule", Short: "Schedule management"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Show all schedules",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			for _, s := range cfg.Services {
				cmd.Printf("%-15s %s\n", s, cfg.ScheduleFor(s))
			}
			return nil
		},
	})
	return cmd
}

func botCmd(loadConfig func() (*config.Config, error), openCache func() (*storage.Cache, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "bot",
		Short: "Run Telegram bot",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cache, err := openCache()
			if err != nil {
				return fmt.Errorf("open cache: %w", err)
			}
			defer cache.Close()

			log := logging.New(cfg.Logging)
			n := notifier.FromConfig(cfg.Telegram, log)
			syncer := core.NewSyncer(cfg, log, cache, n)
			sched := scheduler.New(cfg, syncer, n, log)
			sched.Start()

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			err = bot.Run(ctx, cfg, syncer, log)
			sched.Stop(context.Background())
			return err
		},
	}
}

func testTelegramCmd(loadConfig func() (*config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "test-telegram",
		Short: "Send test message",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			log := logging.New(cfg.Logging)
			return notifier.FromConfig(cfg.Telegram, log).Test(context.Background())
		},
	}
}
