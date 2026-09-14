package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Kfaraon/mikrotik-route-sync/internal/bot"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	"github.com/Kfaraon/mikrotik-route-sync/internal/web"
)

var (
	configPath string
	cachePath  string
)

func main() {
	root := &cobra.Command{Use: "app", Short: "MikroTik route sync"}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "config.yaml", "path to config")
	root.PersistentFlags().StringVar(&cachePath, "cache", "/var/lib/mikrotik-route-sync/cache.db", "path to cache db")

	root.AddCommand(syncCmd(), addCmd(), removeCmd(), listCmd(), infoCmd())
	root.AddCommand(scheduleCmd(), botCmd(), webCmd(), testTelegramCmd(), configCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadConfig() (*config.Config, error) { return config.Load(configPath) }
func openCache() (*storage.Cache, error)   { return storage.Open(cachePath) }

func syncCmd() *cobra.Command {
	var service, group string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync routes for services",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil { return err }
			cache, err := openCache()
			if err != nil { return err }
			defer cache.Close()

			log := logging.New(cfg.Logging)
			n := notifier.FromConfig(cfg.Telegram, log)
			syncer := core.NewSyncer(cfg, log, cache, n)

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			var services []string
			switch {
			case service != "": services = []string{service}
			case group != "": services = cfg.ServicesInGroup(group)
			default: services = cfg.Services
			}
			if len(services) == 0 { return fmt.Errorf("no services to sync") }
			return syncer.SyncMany(ctx, services, dryRun)
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "sync one service")
	cmd.Flags().StringVar(&group, "group", "", "sync all services in a group")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show plan without applying")
	return cmd
}

func addCmd() *cobra.Command {
	return &cobra.Command{Use: "add-service <name>", Short: "Add a new service", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig(); cache, _ := openCache(); defer cache.Close()
			log := logging.New(cfg.Logging); n := notifier.FromConfig(cfg.Telegram, log)
			return core.NewSyncer(cfg, log, cache, n).AddService(context.Background(), args[0])
		},
	}
}

func removeCmd() *cobra.Command {
	return &cobra.Command{Use: "remove-service <name>", Short: "Remove a service", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig(); cache, _ := openCache(); defer cache.Close()
			log := logging.New(cfg.Logging); n := notifier.FromConfig(cfg.Telegram, log)
			return core.NewSyncer(cfg, log, cache, n).RemoveService(context.Background(), args[0])
		},
	}
}

func listCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List services",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig()
			for _, s := range cfg.Services { cmd.Printf("- %-15s schedule=%s\n", s, cfg.ScheduleFor(s)) }
			return nil
		},
	}
}

func infoCmd() *cobra.Command {
	return &cobra.Command{Use: "info <service>", Short: "Show info about a service", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig(); cache, _ := openCache(); defer cache.Close()
			log := logging.New(cfg.Logging); n := notifier.FromConfig(cfg.Telegram, log)
			return core.NewSyncer(cfg, log, cache, n).Info(context.Background(), args[0], cmd.OutOrStdout())
		},
	}
}

func scheduleCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "schedule", Short: "Schedule management"}
	cmd.AddCommand(&cobra.Command{Use: "list", Short: "Show all schedules",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig()
			for _, s := range cfg.Services { cmd.Printf("%-15s %s\n", s, cfg.ScheduleFor(s)) }
			return nil
		},
	})
	return cmd
}

func botCmd() *cobra.Command {
	return &cobra.Command{Use: "bot", Short: "Run Telegram bot",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig(); cache, _ := openCache(); defer cache.Close()
			log := logging.New(cfg.Logging); n := notifier.FromConfig(cfg.Telegram, log)
			syncer := core.NewSyncer(cfg, log, cache, n)
			sched := scheduler.New(cfg, syncer, n, log)
			sched.Start()
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			err := bot.Run(ctx, cfg, syncer, log)
			sched.Stop(context.Background())
			return err
		},
	}
}

func webCmd() *cobra.Command {
	return &cobra.Command{Use: "web", Short: "Run web interface",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig(); cache, _ := openCache(); defer cache.Close()
			log := logging.New(cfg.Logging); n := notifier.FromConfig(cfg.Telegram, log)
			syncer := core.NewSyncer(cfg, log, cache, n)
			sched := scheduler.New(cfg, syncer, n, log)
			sched.Start()
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			err := web.Run(ctx, cfg, syncer, sched, log, configPath)
			sched.Stop(context.Background())
			return err
		},
	}
}

func testTelegramCmd() *cobra.Command {
	return &cobra.Command{Use: "test-telegram", Short: "Send test message",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig()
			log := logging.New(cfg.Logging)
			return notifier.FromConfig(cfg.Telegram, log).Test(context.Background())
		},
	}
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Config management"}
	cmd.AddCommand(&cobra.Command{Use: "get <key>", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig()
			v, err := cfg.Get(args[0])
			if err != nil { return err }
			if config.IsSecret(args[0]) { cmd.Println("••••••••"); return nil }
			cmd.Println(v); return nil
		},
	})
	cmd.AddCommand(&cobra.Command{Use: "set <key> <value>", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := loadConfig()
			if err := cfg.Set(args[0], args[1]); err != nil { return err }
			return cfg.Save()
		},
	})
	return cmd
}
