package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"github.com/spf13/cobra"
)

func newBackupCmd() *cobra.Command {
	var outputFile string
	var fromSnapshot string

	cmd := &cobra.Command{
		Use:   "backup <service>",
		Short: "Экспорт маршрутов сервиса в JSON файл",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			return withSyncer(func(ctx context.Context, c *config.Config, sy *core.Syncer) error {
				var routes []mikrotik.Route
				var err error

				if fromSnapshot != "" {
					// Загружаем из snapshot
					err = sy.GetSnapshot(ctx, service, fromSnapshot, &routes)
					if err != nil {
						return fmt.Errorf("snapshot not found: %w", err)
					}
					fmt.Fprintf(os.Stderr, "✅ Восстановлено из snapshot %s\n", fromSnapshot)
				} else {
					// Получаем текущие маршруты
					routes, err = sy.Backup(ctx, service)
					if err != nil {
						return fmt.Errorf("backup failed: %w", err)
					}
					fmt.Fprintf(os.Stderr, "✅ Экспортировано %d маршрутов\n", len(routes))
				}

				// Создаём структуру экспорта
				export := struct {
					Service   string           `json:"service"`
					Timestamp time.Time        `json:"timestamp"`
					Version   string           `json:"version"`
					Routes    []mikrotik.Route `json:"routes"`
					Count     int              `json:"count"`
				}{
					Service:   service,
					Timestamp: time.Now(),
					Version:   version,
					Routes:    routes,
					Count:     len(routes),
				}

				data, err := json.MarshalIndent(export, "", "  ")
				if err != nil {
					return fmt.Errorf("marshal failed: %w", err)
				}

				if outputFile != "" {
					// Записываем в файл
					err = os.WriteFile(outputFile, data, 0644)
					if err != nil {
						return fmt.Errorf("write file failed: %w", err)
					}
					fmt.Fprintf(os.Stderr, "✅ Сохранено в %s\n", outputFile)
				} else {
					// Выводим в stdout
					fmt.Println(string(data))
				}

				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "выходной файл")
	cmd.Flags().StringVar(&fromSnapshot, "from-snapshot", "", "ID snapshot для экспорта")

	return cmd
}
