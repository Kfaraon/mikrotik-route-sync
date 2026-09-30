package mikrotik

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
)

// ============================================================================
// Константы
// ============================================================================

// rollbackTimeout — независимый таймаут на компенсационный откат.
// Выбирается так, чтобы RouterOS успел обработать компенсацию даже при
// медленной работе (до ~200 записей × ~500ms на запись с учётом rate limiter).
// НЕ наследуется от родительского ctx — это главное отличие от Apply.
const rollbackTimeout = 2 * time.Minute

// ============================================================================
// Состояния транзакции
// ============================================================================

// TxState описывает этап жизненного цикла транзакции.
type TxState string

const (
	TxStatePending     TxState = "pending"      // создана, но не начата
	TxStateApplying    TxState = "applying"     // идёт применение
	TxStateApplied     TxState = "applied"      // успешно применена
	TxStateFailed      TxState = "failed"       // ошибка в Apply (до rollback)
	TxStateRolledBack  TxState = "rolled_back"  // Apply упал, rollback успешен
	TxStateDegraded    TxState = "degraded"     // rollback не удался, нужна ручная правка
)

// ============================================================================
// Ошибки
// ============================================================================

// ErrRollbackTimeout — компенсация не уложилась в rollbackTimeout.
// Сигнализирует, что RouterOS был доступен, но не успел откатиться.
var ErrRollbackTimeout = errors.New("rollback timeout")

// ErrRollbackFailed — компенсация упала из-за ошибки RouterOS/сети.
// Сервис переходит в DEGRADED.
var ErrRollbackFailed = errors.New("rollback failed")

// ============================================================================
// Транзакция
// ============================================================================

// Transaction описывает одну синхронизацию address-list одного сервиса.
// Гарантирует add-before-remove (PROMPT IV.4) и компенсационный rollback.
type Transaction struct {
	client   *Client
	service  string
	list     string
	comment  string
	logger   *slog.Logger

	// Состояние
	state    TxState
	added    []addresslist.Entry // записи, которые реально добавлены в RouterOS
	removed  []addresslist.Entry // записи, которые реально удалены из RouterOS
	err      error               // последняя ошибка Apply или rollback
}

// NewTransaction создаёт транзакцию для заданного сервиса.
// Начальное состояние — pending.
func NewTransaction(c *Client, service, list, comment string, logger *slog.Logger) *Transaction {
	if logger == nil {
		logger = slog.Default()
	}
	return &Transaction{
		client:  c,
		service: service,
		list:    list,
		comment: comment,
		logger:  logger.With("service", service, "list", list),
		state:   TxStatePending,
	}
}

// ============================================================================
// Публичный API
// ============================================================================

// Apply применяет транзакцию:
//  1. Добавляет desired записи (add-before-remove: сначала Add, потом Remove).
//  2. При любой ошибке — запускает rollback с собственным контекстом.
//
// Возвращаемые состояния:
//   - applied      — всё применено, ошибок нет.
//   - rolled_back  — Apply упал, но rollback компенсировал все изменения.
//   - degraded     — Apply упал И rollback не смог полностью откатить (ручная правка).
//
// ВАЖНО: даже если родительский ctx отменён (таймаут, SIGINT, отмена из UI),
// rollback всё равно будет пытаться выполниться на собственном контексте
// с таймаутом rollbackTimeout. Это гарантирует, что мы не оставим RouterOS
// в частично изменённом состоянии из-за простой отмены запроса.
func (t *Transaction) Apply(ctx context.Context, desired, existing []addresslist.Entry) error {
	t.state = TxStateApplying
	t.logger.Info("transaction: applying",
		"desired", len(desired),
		"existing", len(existing),
	)

	// Diff: что добавить (есть в desired, нет в existing).
	toAdd := addresslist.Subtract(desired, existing)
	// Diff: что удалить (есть в existing, нет в desired).
	toRemove := addresslist.Subtract(existing, desired)

	t.logger.Info("transaction: diff computed",
		"to_add", len(toAdd),
		"to_remove", len(toRemove),
	)

	// Шаг 1: Add-before-Remove (PROMPT IV.4).
	// Сначала добавляем новое покрытие, потом снимаем старое — это
	// минимизирует окно, когда адрес не покрыт ни одной записью.
	for _, e := range toAdd {
		if err := ctx.Err(); err != nil {
			t.state = TxStateFailed
			t.err = fmt.Errorf("apply: context cancelled before add: %w", err)
			t.logger.Warn("transaction: apply interrupted, rolling back",
				"phase", "add", "err", t.err.Error())
			return t.rollbackWithDetachedContext()
		}

		e.List = t.list
		e.Comment = t.comment
		e.ID = "" // RouterOS выдаст новый ID

		created, err := t.client.AddEntry(ctx, e)
		if err != nil {
			t.state = TxStateFailed
			t.err = fmt.Errorf("apply: add %s: %w", e.Address, err)
			t.logger.Warn("transaction: apply failed, rolling back",
				"phase", "add", "address", e.Address, "err", t.err.Error())
			return t.rollbackWithDetachedContext()
		}

		// Запоминаем реально добавленные записи (с их ID) — на случай rollback.
		t.added = append(t.added, created)
	}

	// Шаг 2: Удаляем лишнее.
	for _, e := range toRemove {
		if err := ctx.Err(); err != nil {
			t.state = TxStateFailed
			t.err = fmt.Errorf("apply: context cancelled before remove: %w", err)
			t.logger.Warn("transaction: apply interrupted, rolling back",
				"phase", "remove", "err", t.err.Error())
			return t.rollbackWithDetachedContext()
		}

		if err := t.client.DeleteEntry(ctx, e.ID); err != nil {
			t.state = TxStateFailed
			t.err = fmt.Errorf("apply: remove %s (.id=%s): %w", e.Address, e.ID, err)
			t.logger.Warn("transaction: apply failed, rolling back",
				"phase", "remove", "address", e.Address, "id", e.ID, "err", t.err.Error())
			return t.rollbackWithDetachedContext()
		}

		// Запоминаем реально удалённые записи — для компенсации.
		t.removed = append(t.removed, e)
	}

	t.state = TxStateApplied
	t.logger.Info("transaction: applied successfully",
		"added", len(t.added),
		"removed", len(t.removed),
	)
	return nil
}

// State возвращает текущее состояние транзакции.
func (t *Transaction) State() TxState { return t.state }

// Err возвращает последнюю ошибку (из Apply или rollback).
func (t *Transaction) Err() error { return t.err }

// Added возвращает реально добавленные записи (с ID от RouterOS).
func (t *Transaction) Added() []addresslist.Entry { return t.added }

// Removed возвращает реально удалённые записи.
func (t *Transaction) Removed() []addresslist.Entry { return t.removed }

// ============================================================================
// Rollback (компенсация)
// ============================================================================

// rollbackWithDetachedContext выполняет компенсацию на НЕЗАВИСИМОМ контексте.
//
// Ключевая идея (фикс C1): если Apply был прерван отменой родительского ctx
// (таймаут API-запроса, SIGINT, отмена из UI), то обычный rollback с тем же
// ctx упал бы с "context cancelled" на первой же операции — и сервис
// перешёл бы в DEGRADED с пометкой "manual intervention required", хотя
// RouterOS физически способен откатиться.
//
// Решение: создаём контекст от context.Background() с собственным таймаутом
// rollbackTimeout (2 мин). Это гарантирует, что компенсация выполнится
// независимо от причины отмены родителя.
//
// Возвращаемые ошибки:
//   - nil                — компенсация прошла полностью, состояние → rolled_back
//   - ErrRollbackTimeout — компенсация не уложилась в 2 минуты → degraded
//   - ErrRollbackFailed  — RouterOS не отвечает/ошибка сети → degraded
func (t *Transaction) rollbackWithDetachedContext() error {
	// Отдельный контекст с таймаутом, НЕ наследуется от родителя.
	rbCtx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()

	t.logger.Info("transaction: rollback started on detached context",
		"timeout", rollbackTimeout.String(),
		"added_to_revert", len(t.added),
		"removed_to_restore", len(t.removed),
	)

	var rbErrs []error

	// Шаг A: восстановить удалённые записи (re-add).
	// Адреса, которые были удалены в Apply, возвращаем обратно.
	for _, e := range t.removed {
		if err := rbCtx.Err(); err != nil {
			rbErrs = append(rbErrs, fmt.Errorf(
				"restore %s: %w (%w)", e.Address, ErrRollbackTimeout, err))
			break
		}

		// Сбрасываем ID: RouterOS выдаст новый.
		entryCopy := e
		entryCopy.ID = ""

		_, err := t.client.AddEntry(rbCtx, entryCopy)
		if err != nil {
			rbErrs = append(rbErrs, fmt.Errorf("restore %s: %w", e.Address, err))
			t.logger.Error("transaction: rollback: failed to restore entry",
				"address", e.Address, "err", err.Error())
		}
	}

	// Шаг B: удалить добавленные записи (re-delete).
	// У нас есть их ID из ответа AddEntry — удаляем точечно.
	for _, e := range t.added {
		if err := rbCtx.Err(); err != nil {
			rbErrs = append(rbErrs, fmt.Errorf(
				"re-delete %s (.id=%s): %w (%w)",
				e.Address, e.ID, ErrRollbackTimeout, err))
			break
		}

		if err := t.client.DeleteEntry(rbCtx, e.ID); err != nil {
			rbErrs = append(rbErrs, fmt.Errorf(
				"re-delete %s (.id=%s): %w", e.Address, e.ID, err))
			t.logger.Error("transaction: rollback: failed to re-delete entry",
				"address", e.Address, "id", e.ID, "err", err.Error())
		}
	}

	// Итог rollback.
	if len(rbErrs) > 0 {
		t.state = TxStateDegraded
		t.err = errors.Join(
			t.err, // сохраняем исходную ошибку Apply
			fmt.Errorf("%w: %v", ErrRollbackFailed, rbErrs),
		)
		t.logger.Error("transaction: rollback INCOMPLETE — DEGRADED",
			"partial_failures", len(rbErrs),
			"manual_intervention_required", true,
		)
		return t.err
	}

	// Rollback полностью успешен — RouterOS вернулся к исходному состоянию.
	t.state = TxStateRolledBack
	t.logger.Warn("transaction: rolled back successfully (original apply was interrupted)",
		"restored", len(t.removed),
		"re_deleted", len(t.added),
	)
	return t.err // возвращаем исходную ошибку Apply (не nil) — она произошла
}

// ============================================================================
// Служебное
// ============================================================================

// String возвращает краткое описание транзакции (для логов).
func (t *Transaction) String() string {
	return fmt.Sprintf(
		"tx{service=%s list=%s state=%s added=%d removed=%d}",
		t.service, t.list, t.state, len(t.added), len(t.removed),
	)
}
