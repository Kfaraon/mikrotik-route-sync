package notifier

import (
    "context"

    "github.com/Kfaraon/mikrotik-route-sync/internal/config"
    "github.com/Kfaraon/mikrotik-route-sync/internal/logging"
)

type SyncResult struct {
    Service   string
    Added     int
    Removed   int
    Errors    int
    Duration  string
    DryRun    bool
}

type Notifier interface {
    SyncStart(ctx context.Context, services []string, trigger string, schedule string)
    SyncDone(ctx context.Context, results []SyncResult, totalDuration string, dryRun bool)
    Error(ctx context.Context, service string, err error)
    Test(ctx context.Context) error
    Report(ctx context.Context, cfg *config.Config, text string) error
}

// Nop is a no-op notifier.
type Nop struct{}

func (Nop) SyncStart(context.Context, []string, string, string) {}
func (Nop) SyncDone(context.Context, []SyncResult, string, bool) {}
func (Nop) Error(context.Context, string, error)                {}
func (Nop) Test(context.Context) error                          { return nil }
func (Nop) Report(context.Context, *config.Config, string) error { return nil }

// FromConfig returns a Telegram notifier if enabled, otherwise Nop.
func FromConfig(cfg config.TelegramConfig, log *logging.NoopLogger) Notifier {
    _ = log
    if !cfg.Enabled {
        return Nop{}
    }
    return NewTelegram(cfg)
}
