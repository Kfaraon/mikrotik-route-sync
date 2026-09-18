package mikrotik

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

// TransactionState описывает жизненный цикл транзакции.
type TransactionState string

const (
	StatePending    TransactionState = "pending"
	StateApplying   TransactionState = "applying"
	StateApplied    TransactionState = "applied"
	StateFailed     TransactionState = "failed"
	StateRolledBack TransactionState = "rolled_back"
	StateDegraded   TransactionState = "degraded"
)

type Transaction struct {
	client     *Client
	cfg        *config.Config
	cache      *storage.Cache
	service    string
	log        *slog.Logger
	snapshotID string
	state      TransactionState
	startTime  time.Time
}

func NewTransaction(
	client *Client,
	cfg *config.Config,
	cache *storage.Cache,
	service string,
	log *slog.Logger,
) *Transaction {
	return &Transaction{
		client:    client,
		cfg:       cfg,
		cache:     cache,
		service:   service,
		log:       log.With("service", service, "component", "transaction"),
		state:     StatePending,
		startTime: time.Now(),
	}
}

func (t *Transaction) SetSnapshotID(id string) {
	t.snapshotID = id
	t.log.Debug("snapshot attached to transaction", "snapshot_id", id)
}

// Apply выполняет применение изменений маршрутов.
// 
// КРИТИЧЕСКИ ВАЖНО: Порядок применения - сначала POST (добавление), затем DELETE (удаление).
// Это обеспечивает zero-downtime: новые маршруты создаются до удаления старых,
// избегая разрывов связи во время обновления.
func (t *Transaction) Apply(ctx context.Context, toAdd []Route, toRemove []Route) error {
	t.state = StateApplying
	t.log.Info("starting transaction",
		"add_count", len(toAdd),
		"remove_count", len(toRemove),
		"snapshot_id", t.snapshotID,
	)

	var addedIDs []string
	var removedRoutes []Route
	var applyErr error

	// ФАЗА 1: Добавление новых маршрутов (ПЕРЕД удалением старых для zero-downtime)
	for _, r := range toAdd {
		if err := ctx.Err(); err != nil {
			applyErr = fmt.Errorf("context cancelled during add phase: %w", err)
			break
		}

		id, err := t.client.AddRoute(ctx, r)
		if err != nil {
			applyErr = fmt.Errorf("add route %s: %w", r.DstAddress, err)
			t.log.Error("failed to add route during apply",
				"dst", r.DstAddress,
				"gateway", r.Gateway,
				"err", err,
			)
			break
		}
		addedIDs = append(addedIDs, id)
	}

	// ФАЗА 2: Удаление старых маршрутов (ТОЛЬКО если добавление прошло успешно)
	if applyErr == nil {
		for _, r := range toRemove {
			if err := ctx.Err(); err != nil {
				applyErr = fmt.Errorf("context cancelled during remove phase: %w", err)
				break
			}

			if err := t.client.DeleteRoute(ctx, r.ID); err != nil {
				applyErr = fmt.Errorf("delete route %s (%s): %w", r.ID, r.DstAddress, err)
				t.log.Error("failed to delete route during apply",
					"route_id", r.ID,
					"dst", r.DstAddress,
					"err", err,
				)
				break
			}
			removedRoutes = append(removedRoutes, r)
		}
	}

	// Проверяем результат применения
	if applyErr != nil {
		t.state = StateFailed
		t.log.Error("transaction failed, initiating rollback",
			"apply_error", applyErr,
			"removed_before_failure", len(removedRoutes),
			"added_before_failure", len(addedIDs),
		)

		rollbackErr := t.rollback(ctx, addedIDs, removedRoutes)

		if rollbackErr != nil {
			t.state = StateDegraded
			t.log.Error("rollback failed, service is DEGRADED",
				"apply_error", applyErr,
				"rollback_error", rollbackErr,
				"manual_intervention_required", true,
			)
			return fmt.Errorf(
				"transaction failed (%v) AND rollback failed (%v): service %s is degraded, manual intervention required (snapshot_id: %s)",
				applyErr, rollbackErr, t.service, t.snapshotID,
			)
		}

		t.state = StateRolledBack
		t.log.Info("rollback completed successfully",
			"rolled_back_deletes", len(removedRoutes),
			"rolled_back_adds", len(addedIDs),
			"final_state", t.state,
		)
		return fmt.Errorf("transaction failed and was rolled back: %w", applyErr)
	}

	t.state = StateApplied
	elapsed := time.Since(t.startTime)
	t.log.Info("transaction applied successfully",
		"added", len(addedIDs),
		"removed", len(removedRoutes),
		"duration", elapsed,
		"final_state", t.state,
	)

	return nil
}

func (t *Transaction) State() TransactionState {
	return t.state
}

func (t *Transaction) SnapshotID() string {
	return t.snapshotID
}

// rollback выполняет компенсирующий откат изменений.
// ВАЖНО: Порядок отката обратный применению - сначала POST (восстановление), потом DELETE.
func (t *Transaction) rollback(ctx context.Context, addedIDs []string, removedRoutes []Route) error {
	var rollbackErrors []error

	t.log.Info("rollback started",
		"to_delete", len(addedIDs),
		"to_restore", len(removedRoutes),
	)

	// ШАГ 1: Восстанавливаем маршруты, которые были удалены (ПЕРЕД удалением новых)
	for _, r := range removedRoutes {
		if err := ctx.Err(); err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("context cancelled during rollback restore: %w", err))
			continue
		}

		r.ID = ""
		r.Comment = fmt.Sprintf("%s:%s", t.cfg.MikroTik.CommentPrefix, t.service)

		_, err := t.client.AddRoute(ctx, r)
		if err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("rollback restore route %s: %w", r.DstAddress, err))
			t.log.Error("rollback: failed to restore removed route",
				"dst", r.DstAddress,
				"gateway", r.Gateway,
				"err", err,
			)
		}
	}

	// ШАГ 2: Удаляем маршруты, которые были добавлены (ТОЛЬКО после восстановления)
	for _, id := range addedIDs {
		if err := ctx.Err(); err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("context cancelled during rollback delete: %w", err))
			continue
		}

		if err := t.client.DeleteRoute(ctx, id); err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("rollback delete route %s: %w", id, err))
			t.log.Error("rollback: failed to delete added route",
				"route_id", id,
				"err", err,
			)
		}
	}

	if len(rollbackErrors) > 0 {
		t.log.Error("rollback completed with errors",
			"error_count", len(rollbackErrors),
			"first_error", rollbackErrors[0],
		)
		return fmt.Errorf(
			"%d rollback operations failed: %w",
			len(rollbackErrors), rollbackErrors[0],
		)
	}

	t.log.Info("rollback completed successfully",
		"restored_removed_routes", len(removedRoutes),
		"deleted_added_routes", len(addedIDs),
	)
	return nil
}
