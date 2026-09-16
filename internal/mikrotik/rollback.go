package mikrotik

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
)

// RollbackManager управляет откатом изменений при сбое.
type RollbackManager struct {
	client *Client
	logger *slog.Logger
}

// NewRollbackManager создаёт новый менеджер отката.
func NewRollbackManager(client *Client, logger *slog.Logger) *RollbackManager {
	return &RollbackManager{
		client: client,
		logger: logger,
	}
}

// Snapshot представляет снимок маршрутов для отката.
type Snapshot struct {
	Service  string          `json:"service"`
	Routes   []RouteSnapshot `json:"routes"`
	Count    int             `json:"count"`
}

// RouteSnapshot представляет снимок одного маршрута.
type RouteSnapshot struct {
	ID            string        `json:"id"`
	DstAddress    netip.Prefix  `json:"dst_address"`
	Gateway       string        `json:"gateway"`
	RoutingTable  string        `json:"routing_table"`
	Distance      int           `json:"distance"`
	Comment       string        `json:"comment"`
}

// DiffPlan представляет план изменений для применения.
type DiffPlan struct {
	ToAdd    []RouteSnapshot `json:"to_add"`
	ToRemove []RouteSnapshot `json:"to_remove"`
	Unchanged []RouteSnapshot `json:"unchanged"`
}

// RollbackResult представляет результат операции отката.
type RollbackResult struct {
	Success         bool     `json:"success"`
	RestoredCount   int      `json:"restored_count"`
	FailedCount     int      `json:"failed_count"`
	Errors          []string `json:"errors,omitempty"`
	PartialRollback bool     `json:"partial_rollback"`
}

// CreateSnapshot создаёт снимок текущих маршрутов сервиса.
func (rm *RollbackManager) CreateSnapshot(ctx context.Context, service string) (*Snapshot, error) {
	comment := fmt.Sprintf("AUTO:%s", service)
	
	routes, err := rm.client.GetRoutesByComment(ctx, comment)
	if err != nil {
		return nil, fmt.Errorf("get routes for snapshot: %w", err)
	}
	
	snapshot := &Snapshot{
		Service: service,
		Count:   len(routes),
	}
	
	for _, route := range routes {
		snapshot.Routes = append(snapshot.Routes, RouteSnapshot{
			ID:           route.ID,
			DstAddress:   route.DstAddress,
			Gateway:      route.Gateway,
			RoutingTable: route.RoutingTable,
			Distance:     route.Distance,
			Comment:      route.Comment,
		})
	}
	
	rm.logger.Debug("created snapshot",
		slog.String("service", service),
		slog.Int("routes_count", len(routes)))
	
	return snapshot, nil
}

// ExecuteRollback выполняет откат к снимку.
func (rm *RollbackManager) ExecuteRollback(ctx context.Context, snapshot *Snapshot, plan *DiffPlan) (*RollbackResult, error) {
	rm.logger.Warn("executing rollback",
		slog.String("service", snapshot.Service),
		slog.Int("snapshot_routes", snapshot.Count),
		slog.Int("to_add", len(plan.ToAdd)),
		slog.Int("to_remove", len(plan.ToRemove)))
	
	result := &RollbackResult{
		Success: true,
	}
	
	// Шаг 1: Удалить добавленные маршруты (которые были в plan.ToAdd)
	for _, route := range plan.ToAdd {
		// Находим ID добавленного маршрута
		// Предполагаем, что маршрут был добавлен с теми же параметрами
		routes, err := rm.client.GetRoutesByComment(ctx, fmt.Sprintf("AUTO:%s", snapshot.Service))
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("find added route: %v", err))
			result.FailedCount++
			continue
		}
		
		// Ищем маршрут по dst_address
		for _, r := range routes {
			if r.DstAddress == route.DstAddress {
				if err := rm.client.DeleteRoute(ctx, r.ID); err != nil {
					result.Errors = append(result.Errors, fmt.Sprintf("delete route %s: %v", r.ID, err))
					result.FailedCount++
				} else {
					result.RestoredCount++
				}
				break
			}
		}
	}
	
	// Шаг 2: Восстановить удалённые маршруты (которые были в plan.ToRemove)
	for _, route := range plan.ToRemove {
		if err := rm.client.AddRoute(ctx, AddRouteRequest{
			DstAddress:   route.DstAddress,
			Gateway:      route.Gateway,
			RoutingTable: route.RoutingTable,
			Distance:     route.Distance,
			Comment:      route.Comment,
		}); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("restore route %s: %v", route.DstAddress, err))
			result.FailedCount++
		} else {
			result.RestoredCount++
		}
	}
	
	// Определяем, был ли откат частичным
	if result.FailedCount > 0 {
		result.PartialRollback = true
		result.Success = false
		rm.logger.Error("partial rollback completed",
			slog.String("service", snapshot.Service),
			slog.Int("restored", result.RestoredCount),
			slog.Int("failed", result.FailedCount))
	} else {
		rm.logger.Info("rollback completed successfully",
			slog.String("service", snapshot.Service),
			slog.Int("restored", result.RestoredCount))
	}
	
	return result, nil
}

// VerifyRollback проверяет, что состояние после отката соответствует снимку.
func (rm *RollbackManager) VerifyRollback(ctx context.Context, snapshot *Snapshot) (bool, error) {
	currentRoutes, err := rm.client.GetRoutesByComment(ctx, fmt.Sprintf("AUTO:%s", snapshot.Service))
	if err != nil {
		return false, fmt.Errorf("get current routes: %w", err)
	}
	
	if len(currentRoutes) != snapshot.Count {
		rm.logger.Warn("rollback verification failed: route count mismatch",
			slog.String("service", snapshot.Service),
			slog.Int("expected", snapshot.Count),
			slog.Int("actual", len(currentRoutes)))
		return false, nil
	}
	
	// Проверяем, что все маршруты из снимка присутствуют
	snapshotMap := make(map[netip.Prefix]bool)
	for _, route := range snapshot.Routes {
		snapshotMap[route.DstAddress] = true
	}
	
	for _, route := range currentRoutes {
		if !snapshotMap[route.DstAddress] {
			rm.logger.Warn("rollback verification failed: unexpected route",
				slog.String("service", snapshot.Service),
				slog.String("dst_address", route.DstAddress.String()))
			return false, nil
		}
	}
	
	rm.logger.Info("rollback verification passed",
		slog.String("service", snapshot.Service),
		slog.Int("routes_count", len(currentRoutes)))
	
	return true, nil
}
