package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

func newLogsCmd(cfgPath *string) *cobra.Command {
	c := &cobra.Command{Use: "logs", Short: "Управление логами"}

	var follow bool
	var lines int

	tail := &cobra.Command{
		Use:   "tail",
		Short: "Показать последние N строк",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadCfg(*cfgPath)
			if err != nil {
				return err
			}
			return tailLog(cfg.Logging.File, lines, follow)
		},
	}
	tail.Flags().BoolVar(&follow, "follow", false, "следить в реальном времени")
	tail.Flags().IntVarP(&lines, "lines", "n", 100, "количество строк")

	clear := &cobra.Command{
		Use: "clear",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadCfg(*cfgPath)
			if err != nil {
				return err
			}
			return os.Truncate(cfg.Logging.File, 0)
		},
	}

	size := &cobra.Command{
		Use: "size",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadCfg(*cfgPath)
			if err != nil {
				return err
			}
			dir := filepath.Dir(cfg.Logging.File)
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
	}

	c.AddCommand(tail, clear, size)
	return c
}

func tailLog(path string, n int, follow bool) error {
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
	if !follow {
		return nil
	}
	// Простой tail -f
	pos, _ := f.Seek(0, io.SeekEnd)
	for {
		sc := bufio.NewScanner(f)
		f.Seek(pos, io.SeekStart)
		for sc.Scan() {
			fmt.Println(sc.Text())
			pos += int64(len(sc.Text()) + 1)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
