package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/bot"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/scheduler"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
	webui "github.com/Kfaraon/mikrotik-route-sync/internal/web"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var configPath string
var cachePath string

func main() {
	root := &cobra.Command{Use: "app", Short: "Automatic per-service MikroTik RouterOS v7 route synchronizer"}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "config.yaml", "path to config.yaml")
	root.PersistentFlags().StringVar(&cachePath, "cache", "/var/lib/mikrotik-route-sync/cache.db", "path to bbolt cache")
	root.AddCommand(syncCmd(), addCmd(), removeCmd(), infoCmd(), listCmd(), scheduleCmd(), botCmd(), webCmd(), configCmd(), logsCmd(), testTelegramCmd(), testMikrotikCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func load() (*config.Config, error) { return config.Load(configPath) }
func runtime(cfg *config.Config) (*storage.Cache, *core.Syncer, notifier.Notifier, error) {
	cache, err := storage.Open(cachePath)
	if err != nil {
		return nil, nil, nil, err
	}
	log := logging.New(cfg.Logging)
	n := notifier.FromConfig(cfg.Telegram, log)
	return cache, core.NewSyncer(cfg, log, cache, n), n, nil
}
func ctxSignal() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}
func syncCmd() *cobra.Command {
	var service, group string
	var dry bool
	c := &cobra.Command{Use: "sync", Short: "Synchronize routes", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		cache, s, _, err := runtime(cfg)
		if err != nil {
			return err
		}
		defer cache.Close()
		var list []string
		switch {
		case service != "":
			list = []string{service}
		case group != "":
			list = cfg.ServicesInGroup(group)
		default:
			list = cfg.Services
		}
		if len(list) == 0 {
			return fmt.Errorf("no services selected")
		}
		ctx, cancel := ctxSignal()
		defer cancel()
		return s.SyncMany(ctx, list, dry)
	}}
	c.Flags().StringVar(&service, "service", "", "one service")
	c.Flags().StringVar(&group, "group", "", "service group")
	c.Flags().BoolVar(&dry, "dry-run", false, "calculate changes without applying")
	return c
}
func addCmd() *cobra.Command {
	return &cobra.Command{Use: "add-service <service>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		cache, s, _, err := runtime(cfg)
		if err != nil {
			return err
		}
		defer cache.Close()
		return s.AddService(cmd.Context(), args[0])
	}}
}
func removeCmd() *cobra.Command {
	return &cobra.Command{Use: "remove-service <service>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		cache, s, _, err := runtime(cfg)
		if err != nil {
			return err
		}
		defer cache.Close()
		return s.RemoveService(cmd.Context(), args[0])
	}}
}
func infoCmd() *cobra.Command {
	return &cobra.Command{Use: "info <service>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		cache, s, _, err := runtime(cfg)
		if err != nil {
			return err
		}
		defer cache.Close()
		return s.Info(cmd.Context(), args[0], cmd.OutOrStdout())
	}}
}
func listCmd() *cobra.Command {
	return &cobra.Command{Use: "list", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		for _, v := range cfg.Services {
			cmd.Printf("%-24s %s\n", v, cfg.ScheduleFor(v))
		}
		return nil
	}}
}
func scheduleCmd() *cobra.Command {
	c := &cobra.Command{Use: "schedule"}
	c.AddCommand(&cobra.Command{Use: "list", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		for _, v := range cfg.Services {
			cmd.Printf("%-24s %s\n", v, cfg.ScheduleFor(v))
		}
		return nil
	}}, &cobra.Command{Use: "reload", Run: func(cmd *cobra.Command, args []string) {
		cmd.Println("Config is reloaded automatically on next command; running daemon accepts SIGHUP by restart policy.")
	}})
	return c
}
func botCmd() *cobra.Command {
	return &cobra.Command{Use: "bot", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		cache, s, n, err := runtime(cfg)
		if err != nil {
			return err
		}
		defer cache.Close()
		log := logging.New(cfg.Logging)
		sched := scheduler.New(cfg, s, n, log)
		sched.Start()
		ctx, cancel := ctxSignal()
		defer cancel()
		defer sched.Stop(context.Background())
		return bot.Run(ctx, cfg, s, log)
	}}
}
func webCmd() *cobra.Command {
	return &cobra.Command{Use: "web", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		cache, s, n, err := runtime(cfg)
		if err != nil {
			return err
		}
		defer cache.Close()
		log := logging.New(cfg.Logging)
		sched := scheduler.New(cfg, s, n, log)
		sched.Start()
		ctx, cancel := ctxSignal()
		defer cancel()
		defer sched.Stop(context.Background())
		return webui.Run(ctx, cfg, s, sched, log, configPath)
	}}
}
func testTelegramCmd() *cobra.Command {
	return &cobra.Command{Use: "test-telegram", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		return notifier.FromConfig(cfg.Telegram, logging.New(cfg.Logging)).Test(cmd.Context())
	}}
}
func testMikrotikCmd() *cobra.Command {
	return &cobra.Command{Use: "test-mikrotik", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		cache, s, _, err := runtime(cfg)
		if err != nil {
			return err
		}
		defer cache.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		if err := s.RouterPing(ctx); err != nil {
			return err
		}
		cmd.Println("OK")
		return nil
	}}
}
func configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config"}
	c.AddCommand(&cobra.Command{Use: "get <key>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		v, err := yamlGet(configPath, args[0])
		if err != nil {
			return err
		}
		if config.IsSecret(args[0]) {
			cmd.Println("••••••••")
		} else {
			cmd.Println(v)
		}
		return nil
	}}, &cobra.Command{Use: "set <key> <value>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error { return yamlSet(configPath, args[0], args[1]) }}, &cobra.Command{Use: "edit", RunE: func(cmd *cobra.Command, args []string) error {
		ed := os.Getenv("EDITOR")
		if ed == "" {
			ed = "vi"
		}
		x := exec.Command(ed, configPath)
		x.Stdin = os.Stdin
		x.Stdout = os.Stdout
		x.Stderr = os.Stderr
		return x.Run()
	}}, &cobra.Command{Use: "reload", Run: func(cmd *cobra.Command, args []string) {
		cmd.Println("Reload is effective on the next invocation; daemon deployments should send SIGHUP/restart through supervisor.")
	}})
	return c
}
func yamlGet(path, key string) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err = yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	var cur any = m
	for _, p := range strings.Split(key, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s not found", key)
		}
		cur, ok = mm[p]
		if !ok {
			return nil, fmt.Errorf("%s not found", key)
		}
	}
	return cur, nil
}
func yamlSet(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	if err = yaml.Unmarshal(b, &m); err != nil {
		return err
	}
	parts := strings.Split(key, ".")
	cur := m
	for _, p := range parts[:len(parts)-1] {
		v, ok := cur[p].(map[string]any)
		if !ok {
			v = map[string]any{}
			cur[p] = v
		}
		cur = v
	}
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) != nil {
		decoded = value
	}
	cur[parts[len(parts)-1]] = decoded
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func logsCmd() *cobra.Command {
	c := &cobra.Command{Use: "logs"}
	var n int
	var follow bool
	tail := &cobra.Command{Use: "tail", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		if cfg.Logging.File == "" {
			return fmt.Errorf("logging.file is empty")
		}
		return tailFile(cmd.OutOrStdout(), cfg.Logging.File, n, follow)
	}}
	tail.Flags().IntVarP(&n, "lines", "n", 100, "number of lines")
	tail.Flags().BoolVarP(&follow, "follow", "f", false, "follow file")
	c.AddCommand(tail, &cobra.Command{Use: "clear", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		return os.WriteFile(cfg.Logging.File, nil, 0o600)
	}}, &cobra.Command{Use: "size", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := load()
		if err != nil {
			return err
		}
		matches, _ := filepath.Glob(cfg.Logging.File + "*")
		var total int64
		for _, p := range matches {
			if st, e := os.Stat(p); e == nil {
				total += st.Size()
			}
		}
		cmd.Printf("files=%d bytes=%d\n", len(matches), total)
		return nil
	}})
	return c
}
func tailFile(w io.Writer, path string, n int, follow bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	for _, v := range lines {
		fmt.Fprintln(w, v)
	}
	if !follow {
		return sc.Err()
	}
	for {
		if sc.Scan() {
			fmt.Fprintln(w, sc.Text())
			continue
		}
		if err := sc.Err(); err != nil {
			return err
		}
		time.Sleep(time.Second)
	}
}
