package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/bot"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	webui "github.com/Kfaraon/mikrotik-route-sync/internal/web"
	"github.com/spf13/cobra"
)

var (
	version = "dev"
	cfgPath string
)

func main() {
	if e := newRoot().Execute(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

// loadConfig загружает конфигурацию и проверяет права доступа к файлу.
func loadConfig() (*config.Config, error) {
	// Проверка прав доступа к файлу конфигурации (должны быть 0600)
	// Пропускаем проверку на Windows, где используется ACL
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(cfgPath); err == nil {
			if fi.Mode().Perm() != 0600 {
				return nil, fmt.Errorf("config file %s must have 0600 permissions, got %o", cfgPath, fi.Mode().Perm())
			}
		} else {
			return nil, fmt.Errorf("cannot stat config file: %w", err)
		}
	}
	return config.Load(cfgPath)
}

// withSyncer инициализирует зависимости и выполняет функцию с Syncer.
func withSyncer(fn func(context.Context, *config.Config, *core.Syncer) error) error {
	c, e := loadConfig()
	if e != nil {
		return e
	}
	// Путь к кэшу берется из конфигурации (поле CachePath), с fallback на "cache.db".
	cachePath := c.CachePath
	if cachePath == "" {
		cachePath = "cache.db"
	}
	cache, e := storage.Open(cachePath)
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

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "app",
		Short: "Secure MikroTik RouterOS v7 route synchronizer",
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "config.yaml", "config path")

	var service, group string
	var dry, force bool
	syncCmd := &cobra.Command{
		Use: "sync",
		RunE: func(cmd *cobra.Command, args []string) error {
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
		},
	}
	syncCmd.Flags().StringVar(&service, "service", "", "service")
	syncCmd.Flags().StringVar(&group, "group", "", "group")
	syncCmd.Flags().BoolVar(&dry, "dry-run", false, "calculate only")
	syncCmd.Flags().BoolVar(&force, "force", false, "override safe-delete ratio")
	root.AddCommand(syncCmd)

	root.AddCommand(&cobra.Command{
		Use:   "diff <service>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, s *core.Syncer) error {
				r, e := s.SyncService(ctx, args[0], true, false)
				printJSON(r)
				return e
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use: "list",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, e := loadConfig()
			if e != nil {
				return e
			}
			for _, s := range c.Services {
				fmt.Printf("%s\t%s\n", s, c.EffectiveSchedule(s))
			}
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "info <service>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, e := loadConfig()
			if e != nil {
				return e
			}
			svc := args[0]
			info := map[string]interface{}{
				"service":  svc,
				"schedule": c.EffectiveSchedule(svc),
				"override": c.Overrides[svc],
			}
			printJSON(info)
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use: "test-mikrotik",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, e := loadConfig()
			if e != nil {
				return e
			}
			return mikrotik.New(c.MikroTik).Ping(cmd.Context())
		},
	})

	root.AddCommand(&cobra.Command{
		Use: "test-telegram",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, e := loadConfig()
			if e != nil {
				return e
			}
			log := logging.New(c.Logging)
			return notifier.FromConfig(c.Telegram, log).Send(cmd.Context(), "✅ mikrotik-route-sync: Telegram test")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "test-dns <domain>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			// Используем настроенный resolver вместо системного
			resolver := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					d := net.Dialer{Timeout: 5 * time.Second}
					return d.DialContext(ctx, "udp", c.External.Resolver)
				},
			}
			ips, err := resolver.LookupIP(cmd.Context(), "ip", args[0])
			if err != nil {
				return err
			}
			for _, ip := range ips {
				fmt.Println(ip.String())
			}
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use: "web",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, s *core.Syncer) error {
				return webui.New(c, s, logging.New(c.Logging), cfgPath).Run(ctx)
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use: "bot",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, s *core.Syncer) error {
				b, err := bot.New(c.Telegram, s, c.Services, logging.New(c.Logging))
				if err != nil {
					return err
				}
				return b.Run(ctx)
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use: "daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, s *core.Syncer) error {
				log := logging.New(c.Logging)
				n := notifier.FromConfig(c.Telegram, log)
				sch := scheduler.New(c, s, n, log)
				sch.Start()
				defer sch.Stop(ctx)

				b, err := bot.New(c.Telegram, s, c.Services, log)
				if err != nil {
					return err
				}
				w := webui.New(c, s, log, cfgPath)

				ctx, cancel := context.WithCancel(ctx)
				defer cancel()

				var wg sync.WaitGroup
				errCh := make(chan error, 2)

				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := b.Run(ctx); err != nil && ctx.Err() == nil {
						errCh <- fmt.Errorf("bot: %w", err)
						cancel()
					}
				}()

				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := w.Run(ctx); err != nil && ctx.Err() == nil {
						errCh <- fmt.Errorf("web: %w", err)
						cancel()
					}
				}()

				sigHup := make(chan os.Signal, 1)
				signal.Notify(sigHup, syscall.SIGHUP)
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case <-sigHup:
							log.Info("received SIGHUP, reloading")
							if err := sch.Reload(); err != nil {
								log.Error("scheduler reload failed", "err", err)
							}
						}
					}
				}()

				<-ctx.Done()
				wg.Wait()
				select {
				case err := <-errCh:
					return err
				default:
					return nil
				}
			})
		},
	})

	root.AddCommand(&cobra.Command{
		Use: "version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "add-service <service>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				if e := sy.AddService(ctx, args[0]); e != nil {
					return e
				}
				c.Services = append(c.Services, args[0])
				return c.Save()
			})
		},
	})

	var removeForce bool
	removeCmd := &cobra.Command{
		Use:   "remove-service <service>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
				return c.Save()
			})
		},
	}
	removeCmd.Flags().BoolVar(&removeForce, "force", false, "confirm large removal")
	root.AddCommand(removeCmd)

	var backupOut, backupSnap string
	backupCmd := &cobra.Command{
		Use:   "backup <service>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				var routes []mikrotik.Route
				var err error
				if backupSnap != "" {
					err = sy.GetSnapshot(ctx, args[0], backupSnap, &routes)
				} else {
					routes, err = sy.Backup(ctx, args[0])
				}
				if err != nil {
					return err
				}
				data, _ := json.MarshalIndent(map[string]any{"service": args[0], "routes": routes}, "", "  ")
				if backupOut != "" {
					return os.WriteFile(backupOut, data, 0644)
				}
				fmt.Println(string(data))
				return nil
			})
		},
	}
	backupCmd.Flags().StringVarP(&backupOut, "output", "o", "", "file")
	backupCmd.Flags().StringVar(&backupSnap, "from-snapshot", "", "snapshot ID")
	root.AddCommand(backupCmd)

	var restoreFile, restoreSnap string
	var restoreForce bool
	restoreCmd := &cobra.Command{
		Use:   "restore <service>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				var routes []mikrotik.Route
				if restoreFile != "" {
					data, err := os.ReadFile(restoreFile)
					if err != nil {
						return err
					}
					var exp struct{ Routes []mikrotik.Route }
					if err := json.Unmarshal(data, &exp); err != nil {
						return err
					}
					routes = exp.Routes
				} else if restoreSnap != "" {
					if err := sy.GetSnapshot(ctx, args[0], restoreSnap, &routes); err != nil {
						return err
					}
				} else {
					return fmt.Errorf("specify --from-file or --from-snapshot")
				}
				if !restoreForce {
					return fmt.Errorf("use --force to confirm restore")
				}
				return sy.Restore(ctx, args[0], routes)
			})
		},
	}
	restoreCmd.Flags().StringVar(&restoreFile, "from-file", "", "JSON file")
	restoreCmd.Flags().StringVar(&restoreSnap, "from-snapshot", "", "snapshot ID")
	restoreCmd.Flags().BoolVar(&restoreForce, "force", false, "confirm")
	root.AddCommand(restoreCmd)

	snapshotsCmd := &cobra.Command{Use: "snapshots", Short: "Управление snapshots"}
	snapshotsCmd.AddCommand(&cobra.Command{
		Use:   "list <service>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				snaps, err := sy.ListSnapshots(ctx, args[0])
				if err != nil {
					return err
				}
				for _, s := range snaps {
					fmt.Printf("%s\t%d\n", s.ID, s.Count)
				}
				return nil
			})
		},
	})
	snapshotsCmd.AddCommand(&cobra.Command{
		Use:   "delete <service> <id>",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				return sy.DeleteSnapshot(ctx, args[0], args[1])
			})
		},
	})
	var ttlHours int
	cleanupCmd := &cobra.Command{
		Use:   "cleanup <service>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				n, err := sy.CleanupSnapshots(ctx, args[0], time.Duration(ttlHours)*time.Hour)
				if err == nil {
					fmt.Printf("Deleted %d snapshots\n", n)
				}
				return err
			})
		},
	}
	cleanupCmd.Flags().IntVar(&ttlHours, "ttl", 168, "TTL в часах")
	snapshotsCmd.AddCommand(cleanupCmd)
	root.AddCommand(snapshotsCmd)

	logsCmd := &cobra.Command{Use: "logs", Short: "Управление логами"}
	var follow bool
	var lines int
	tailCmd := &cobra.Command{
		Use: "tail",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			return tailLog(c.Logging.File, lines, follow)
		},
	}
	tailCmd.Flags().BoolVar(&follow, "follow", false, "следить в реальном времени")
	tailCmd.Flags().IntVarP(&lines, "lines", "n", 100, "количество строк")
	logsCmd.AddCommand(tailCmd)
	logsCmd.AddCommand(&cobra.Command{
		Use: "clear",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			return os.Truncate(c.Logging.File, 0)
		},
	})
	logsCmd.AddCommand(&cobra.Command{
		Use: "size",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			dir := filepath.Dir(c.Logging.File)
			entries, _ := os.ReadDir(dir)
			var total int64
			var n int
			for _, e := range entries {
				info, _ := e.Info()
				total += info.Size()
				n++
			}
			fmt.Printf("files=%d total=%d bytes\n", n, total)
			return nil
		},
	})
	root.AddCommand(logsCmd)

	scheduleCmd := &cobra.Command{Use: "schedule", Short: "Управление расписаниями"}
	scheduleCmd.AddCommand(&cobra.Command{
		Use: "list",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			for _, s := range c.Services {
				fmt.Printf("%-20s %s\n", s, c.EffectiveSchedule(s))
			}
			return nil
		},
	})
	scheduleCmd.AddCommand(&cobra.Command{
		Use: "reload",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			fmt.Println("Config reloaded. Services:", len(c.Services))
			return nil
		},
	})
	root.AddCommand(scheduleCmd)

	configCmd := &cobra.Command{Use: "config", Short: "Управление конфигурацией"}
	configCmd.AddCommand(&cobra.Command{
		Use: "validate",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			return c.Validate()
		},
	})
	configCmd.AddCommand(&cobra.Command{
		Use: "reload",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := os.FindProcess(os.Getpid())
			if err == nil {
				p.Signal(syscall.SIGHUP)
			}
			fmt.Println("Reload signal sent.")
			return nil
		},
	})
	configCmd.AddCommand(&cobra.Command{
		Use: "edit",
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
	})
	configCmd.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			key := args[0]
			value, err := getConfigValue(c, key)
			if err != nil {
				return err
			}
			// Маскируем чувствительные значения
			value = redactSensitive(key, value)
			fmt.Printf("%s: %v\n", key, value)
			return nil
		},
	})
	root.AddCommand(configCmd)

	return root
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

// getConfigValue получает значение конфигурации по пути (например, "mikrotik.host")
func getConfigValue(c *config.Config, path string) (any, error) {
	parts := strings.Split(path, ".")
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty path")
	}

	v := reflect.ValueOf(c)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	for i, part := range parts {
		if !v.IsValid() || v.Kind() != reflect.Struct {
			return nil, fmt.Errorf("invalid path: %s", path)
		}

		field := v.FieldByNameFunc(func(name string) bool {
			return strings.EqualFold(name, part)
		})

		if !field.IsValid() {
			return nil, fmt.Errorf("field %q not found in path %s", part, path)
		}

		v = field
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}

		// Если это последний элемент, возвращаем значение
		if i == len(parts)-1 {
			return v.Interface(), nil
		}
	}

	return nil, fmt.Errorf("invalid path: %s", path)
}

// redactSensitive маскирует чувствительные значения
func redactSensitive(key string, value any) any {
	sensitivePatterns := []string{
		"password",
		"token",
		"secret",
		"key",
	}

	keyLower := strings.ToLower(key)
	for _, pattern := range sensitivePatterns {
		if strings.Contains(keyLower, pattern) {
			return "[REDACTED]"
		}
	}

	return value
}

// tailLog читает лог-файл с поддержкой ротации (собственная реализация без внешних зависимостей)
func tailLog(path string, n int, follow bool) error {
	if !follow {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		sc := bufio.NewScanner(f)
		buf := make([]string, 0, n)
		for sc.Scan() {
			if len(buf) == n {
				buf = buf[1:]
			}
			buf = append(buf, sc.Text())
		}
		for _, l := range buf {
			fmt.Println(l)
		}
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, _ := f.Stat()
	_, _ = f.Seek(fi.Size(), io.SeekStart)

	scanner := bufio.NewScanner(f)
	for {
		if scanner.Scan() {
			fmt.Println(scanner.Text())
		} else {
			if err := scanner.Err(); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
			fiNew, err := os.Stat(path)
			if err == nil {
				if fiNew.Size() < fi.Size() || !os.SameFile(fiNew, fi) {
					f.Close()
					f, err = os.Open(path)
					if err != nil {
						return err
					}
					fi = fiNew
					scanner = bufio.NewScanner(f)
				} else {
					fi = fiNew
				}
			}
		}
	}
}
