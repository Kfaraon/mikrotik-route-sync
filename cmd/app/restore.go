package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/spf13/cobra"
)

func newRestoreCmd() *cobra.Command {
	var fromFile string
	var fromSnapshot string
	var force bool

	cmd := &cobra.Command{
		Use:   "restore <service>",
		Short: "Восстановление маршрутов сервиса",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			if fromFile == "" && fromSnapshot == "" {
				return fmt.Errorf("укажите --from-file или --from-snapshot")
			}

			if fromFile != "" && fromSnapshot != "" {
				return fmt.Errorf("нельзя использовать одновременно --from-file и --from-snapshot")
			}

			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				var routes []mikrotik.Route
				var err error

				if fromFile != "" {
					// Читаем из файла
					data, err := os.ReadFile(fromFile)
					if err != nil {
						return fmt.Errorf("read file failed: %w", err)
					}

					var export struct {
						Service string           `json:"service"`
						Routes  []mikrotik.Route `json:"routes"`
					}

					err = json.Unmarshal(data, &export)
					if err != nil {
						return fmt.Errorf("parse file failed: %w", err)
					}

					if export.Service != service {
						return fmt.Errorf("service mismatch: file=%s, requested=%s", export.Service, service)
					}

					routes = export.Routes
					fmt.Fprintf(os.Stderr, "✅ Загружено %d маршрутов из %s\n", len(routes), fromFile)
				} else {
					// Загружаем из snapshot
					err = sy.GetSnapshot(ctx, service, fromSnapshot, &routes)
					if err != nil {
						return fmt.Errorf("snapshot not found: %w", err)
					}
					fmt.Fprintf(os.Stderr, "✅ Загружено %d маршрутов из snapshot %s\n", len(routes), fromSnapshot)
				}

				// Получаем текущие маршруты для подтверждения
				current, err := sy.Backup(ctx, service)
				if err != nil {
					return fmt.Errorf("get current routes failed: %w", err)
				}

				if !force {
					fmt.Fprintf(os.Stderr, "\nТекущие маршруты: %d\n", len(current))
					fmt.Fprintf(os.Stderr, "Будет восстановлено: %d\n", len(routes))
					fmt.Fprintf(os.Stderr, "\nВнимание: все текущие маршруты сервиса будут удалены!\n")
					fmt.Fprintf(os.Stderr, "Используйте --force для подтверждения\n")
					return fmt.Errorf("требуется --force для подтверждения")
				}

				// Восстанавливаем
				err = sy.Restore(ctx, service, routes)
				if err != nil {
					return fmt.Errorf("restore failed: %w", err)
				}

				fmt.Fprintf(os.Stderr, "✅ Восстановлено %d маршрутов для сервиса %s\n", len(routes), service)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&fromFile, "from-file", "", "JSON файл для восстановления")
	cmd.Flags().StringVar(&fromSnapshot, "from-snapshot", "", "ID snapshot для восстановления")
	cmd.Flags().BoolVar(&force, "force", false, "подтвердить восстановление")

	return cmd
}
