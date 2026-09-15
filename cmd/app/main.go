package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	webui "github.com/Kfaraon/mikrotik-route-sync/internal/web"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if e := newRoot().Execute(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	var cfgPath string
	root := &cobra.Command{Use: "app", Short: "Secure MikroTik RouterOS v7 route synchronizer"}
	root.PersistentFlags().StringVar(&cfgPath, "config", "config.yaml", "config path")

	load := func() (*config.Config, error) {
		c, e := config.Load(cfgPath)
		if e != nil {
			return nil, e
		}
		return c, nil
	}

	withSyncer := func(fn func(context.Context, *config.Config, *core.Syncer) error) error {
		c, e := load()
		if e != nil {
			return e
		}
		cache, e := storage.Open("cache.db")
		if e != nil {
			return e
		}
		defer cache.Close()
		log := logging.New(c.Logging)
		s := core.NewSyncer(c, log, cache, notifier.FromConfig(c.Telegram, log))
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return fn(ctx, c, s)
	}

	var service, group string
	var dry, force bool

	syncCmd := &cobra.Command{Use: "sync", RunE: func(cmd *cobra.Command, args []string) error {
		return withSyncer(func(ctx context.Context, c *config.Config, s *core.Syncer) error {
			var names []string
			if service != "" {
				names = []string{service}
			} else if group != "" {
				names = c.ServicesInGroup(group)
			} else {
				names = c.Services
			}
			if force && len(names) != 1 {
				return fmt.Errorf("--force requires exactly one service")
			}
			if force {
				r, e := s.SyncService(ctx, names[0], dry, true)
				printJSON(r)
				return e
			}
			return s.SyncMany(ctx, names, dry)
		})
	}}
	syncCmd.Flags().StringVar(&service, "service", "", "service")
	syncCmd.Flags().StringVar(&group, "group", "", "group")
	syncCmd.Flags().BoolVar(&dry, "dry-run", false, "calculate only")
	syncCmd.Flags().BoolVar(&force, "force", false, "override safe-delete ratio")
	root.AddCommand(syncCmd)

	root.AddCommand(&cobra.Command{Use: "diff <service>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withSyncer(func(ctx context.Context, c *config.Config, s *core.Syncer) error {
			r, e := s.SyncService(ctx, args[0], true, false)
			printJSON(r)
			return e
		})
	}})

	root.AddCommand(&cobra.Command{Use: "list", RunE: func(cmd *cobra.Command, args []string) error {
		c, e := load()
		if e != nil {
			return e
		}
		for _, s := range c.Services {
			fmt.Printf("%s\t%s\n", s, c.EffectiveSchedule(s))
		}
		return nil
	}})

	root.AddCommand(&cobra.Command{Use: "info <service>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, e := load()
		if e != nil {
			return e
		}
		fmt.Printf("service: %s\nschedule: %s\noverride: %+v\n", args[0], c.EffectiveSchedule(args[0]), c.Overrides[args[0]])
		return nil
	}})

	root.AddCommand(&cobra.Command{Use: "test-mikrotik", RunE: func(cmd *cobra.Command, args []string) error {
		c, e := load()
		if e != nil {
			return e
		}
		return mikrotik.New(c.MikroTik).Ping(cmd.Context())
	}})

	root.AddCommand(&cobra.Command{Use: "config-validate", RunE: func(cmd *cobra.Command, args []string) error {
		c, e := load()
		if e != nil {
			return e
		}
		return c.Validate()
	}})

	root.AddCommand(&cobra.Command{Use: "web", RunE: func(cmd *cobra.Command, args []string) error {
		return withSyncer(func(ctx context.Context, c *config.Config, s *core.Syncer) error {
			return webui.New(c, s, logging.New(c.Logging)).Run(ctx)
		})
	}})

	root.AddCommand(&cobra.Command{Use: "daemon", RunE: func(cmd *cobra.Command, args []string) error {
		return withSyncer(func(ctx context.Context, c *config.Config, s *core.Syncer) error {
			return webui.New(c, s, logging.New(c.Logging)).Run(ctx)
		})
	}})

	root.AddCommand(&cobra.Command{Use: "version", Run: func(cmd *cobra.Command, args []string) { fmt.Println(version) }})

	root.AddCommand(&cobra.Command{Use: "add-service <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, e := load()
		if e != nil {
			return e
		}
		cache, e := storage.Open("cache.db")
		if e != nil {
			return e
		}
		defer cache.Close()
		log := logging.New(c.Logging)
		sy := core.NewSyncer(c, log, cache, notifier.FromConfig(c.Telegram, log))
		if e = sy.AddService(cmd.Context(), args[0]); e != nil {
			return e
		}
		return config.AtomicWrite(cfgPath, c)
	}})

	var removeForce bool
	removeCmd := &cobra.Command{Use: "remove-service <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
			if e := sy.RemoveService(ctx, args[0], removeForce); e != nil {
				return e
			}
			out := c.Services[:0]
			for _, x := range c.Services {
				if x != args[0] {
					out = append(out, x)
				}
			}
			c.Services = out
			return config.AtomicWrite(cfgPath, c)
		})
	}}
	removeCmd.Flags().BoolVar(&removeForce, "force", false, "confirm large removal")
	root.AddCommand(removeCmd)

	root.AddCommand(&cobra.Command{Use: "backup <service>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
			r, e := sy.Backup(ctx, args[0])
			if e != nil {
				return e
			}
			printJSON(r)
			return nil
		})
	}})

	root.AddCommand(&cobra.Command{Use: "test-telegram", RunE: func(cmd *cobra.Command, args []string) error {
		c, e := load()
		if e != nil {
			return e
		}
		log := logging.New(c.Logging)
		return notifier.FromConfig(c.Telegram, log).Send(cmd.Context(), "✅ mikrotik-route-sync: Telegram test")
	}})

	// ИСПРАВЛЕНИЕ: Команды ДО return, а не после
	root.AddCommand(newBackupCmd())
	root.AddCommand(newRestoreCmd())
	root.AddCommand(newSnapshotsCmd())

	return root
}

func printJSON(v any) { b, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(b)) }
