package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// installSignalHandler ставит обработку SIGHUP (reload) и SIGINT/SIGTERM (shutdown).
func installSignalHandler(cancel context.CancelFunc, reload func() error) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range ch {
			switch sig {
			case syscall.SIGHUP:
				slog.Info("received SIGHUP, reloading config")
				if err := reload(); err != nil {
					slog.Error("reload failed", "err", err)
				}
			default:
				slog.Info("shutting down", "sig", sig.String())
				cancel()
				return
			}
		}
	}()
}
