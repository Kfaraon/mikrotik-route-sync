package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/spf13/cobra"
)

func newSnapshotsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshots",
		Short: "Управление snapshots",
	}

	// snapshots list <service>
	listCmd := &cobra.Command{
		Use:   "list <service>",
		Short: "Показать список snapshots для сервиса",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				snapshots, err := sy.ListSnapshots(ctx, service)
				if err != nil {
					return fmt.Errorf("list snapshots failed: %w", err)
				}

				if len(snapshots) == 0 {
					fmt.Println("Snapshots не найдены")
					return nil
				}

				fmt.Printf("%-20s %-25s %-10s\n", "ID", "Создан", "Маршрутов")
				fmt.Println("--------------------------------------------------------------------------------")

				for _, snap := range snapshots {
					var ts int64
					fmt.Sscanf(snap.ID, "%d", &ts)
					createdAt := time.Unix(0, ts)

					fmt.Printf("%-20s %-25s %-10d\n",
						snap.ID,
						createdAt.Format("2006-01-02 15:04:05"),
						snap.Count)
				}

				return nil
			})
		},
	}

	// snapshots delete <service> <snapshot-id>
	deleteCmd := &cobra.Command{
		Use:   "delete <service> <snapshot-id>",
		Short: "Удалить snapshot",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			snapshotID := args[1]

			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				err := sy.DeleteSnapshot(ctx, service, snapshotID)
				if err != nil {
					return fmt.Errorf("delete snapshot failed: %w", err)
				}

				fmt.Printf("✅ Snapshot %s удалён\n", snapshotID)
				return nil
			})
		},
	}

	// snapshots cleanup <service>
	var ttlHours int
	cleanupCmd := &cobra.Command{
		Use:   "cleanup <service>",
		Short: "Удалить старые snapshots",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			ttl := time.Duration(ttlHours) * time.Hour

			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				deleted, err := sy.CleanupSnapshots(ctx, service, ttl)
				if err != nil {
					return fmt.Errorf("cleanup failed: %w", err)
				}

				fmt.Printf("✅ Удалено snapshots: %d (старше %v)\n", deleted, ttl)
				return nil
			})
		},
	}
	cleanupCmd.Flags().IntVar(&ttlHours, "ttl", 168, "TTL в часах (по умолчанию 168 = 7 дней)")

	cmd.AddCommand(listCmd, deleteCmd, cleanupCmd)
	return cmd
}
