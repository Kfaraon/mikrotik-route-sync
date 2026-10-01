package mikrotik

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

// TransactionState — состояние транзакции применения изменений.
type TransactionState string

const (
	StatePending    TransactionState = "pending"
	StateApplied    TransactionState = "applied"
	StateRolledBack TransactionState = "rolled_back"
	StateDegraded   TransactionState = "degraded"
)

// Transaction — транзакция применения изменений в Firewall Address List.
//
// Порядок применения:
//  1. add
//  2. remove
//
// При ошибке выполняется best-effort rollback:
//   - удалённые созданные записи;
//   - повторно добавленные已成功 удалённые записи.
//
// Если rollback прошёл частично, транзакция переходит в StateDegraded.
type Transaction struct {
	client  *Client
	cfg     *config.Config
	cache   *storage.Cache
	service string
	log     *slog.Logger

	snapshotID string
	state      TransactionState

	created []addresslist.Entry
	deleted []addresslist.Entry
}

// NewTransaction создаёт транзакцию для одного сервиса.
func NewTransaction(
	client *Client,
	cfg *config.Config,
	cache *storage.Cache,
	service string,
	log *slog.Logger,
) *Transaction {
	if log == nil {
		log = slog.Default()
	}

	return &Transaction{
		client:  client,
		cfg:     cfg,
		cache:   cache,
		service: service,
		log:     log,
		state:   StatePending,
	}
}

// SetSnapshotID сохраняет ID снапшота для диагностики/уведомлений.
func (t *Transaction) SetSnapshotID(id string) {
	t.snapshotID = id
}

// State возвращает текущее состояние транзакции.
func (t *Transaction) State() TransactionState {
	return t.state
}

// Apply применяет add/remove к RouterOS.
func (t *Transaction) Apply(
	ctx context.Context,
	add []addresslist.Entry,
	remove []addresslist.Entry,
) error {
	list := t.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(t.cfg.Firewall.CommentPrefix, t.service)

	addEntries, err := t.prepareAdd(add, list, comment)
	if err != nil {
		return err
	}

	removeEntries, err := t.prepareRemove(ctx, remove, list, comment)
	if err != nil {
		return err
	}

	// 1. Сначала добавляем новые записи.
	for _, e := range addEntries {
		created, addErr := t.client.AddEntry(ctx, e)
		if addErr != nil {
			t.log.Error("transaction add failed",
				"service", t.service,
				"list", e.List,
				"comment", e.Comment,
				"address", e.Address,
				"snapshot_id", t.snapshotID,
				"err", addErr,
			)

			if rbErr := t.rollback(); rbErr != nil {
				t.state = StateDegraded
				return fmt.Errorf("add %s: %w; rollback failed: %v", e.Address, addErr, rbErr)
			}

			t.state = StateRolledBack
			return fmt.Errorf("add %s: %w", e.Address, addErr)
		}

		t.created = append(t.created, created)
	}

	// 2. Затем удаляем лишние записи.
	for _, e := range removeEntries {
		delErr := t.client.DeleteEntry(ctx, e.ID)
		if delErr != nil {
			t.log.Error("transaction delete failed",
				"service", t.service,
				"list", e.List,
				"comment", e.Comment,
				"id", e.ID,
				"address", e.Address,
				"snapshot_id", t.snapshotID,
				"err", delErr,
			)

			if rbErr := t.rollback(); rbErr != nil {
				t.state = StateDegraded
				return fmt.Errorf("delete %s: %w; rollback failed: %v", e.ID, delErr, rbErr)
			}

			t.state = StateRolledBack
			return fmt.Errorf("delete %s: %w", e.ID, delErr)
		}

		t.deleted = append(t.deleted, e)
	}

	t.state = StateApplied
	return nil
}

// prepareAdd нормализует и заполняет поля записей для добавления.
func (t *Transaction) prepareAdd(
	in []addresslist.Entry,
	list string,
	comment string,
) ([]addresslist.Entry, error) {
	out := make([]addresslist.Entry, 0, len(in))
	seen := make(map[string]struct{}, len(in))

	for _, e := range in {
		addr := strings.TrimSpace(e.Address)
		if addr == "" {
			continue
		}

		norm, err := addresslist.NormalizeAddress(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid add address %q: %w", addr, err)
		}

		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}

		ne := e
		ne.ID = ""
		ne.Address = norm
		ne.List = list
		ne.Comment = comment
		ne.Dynamic = ""

		if ne.Disabled == "" {
			ne.Disabled = "false"
		}

		out = append(out, ne)
	}

	return out, nil
}

// prepareRemove проверяет записей на удаление и при необходимости находит .id по адресу.
func (t *Transaction) prepareRemove(
	ctx context.Context,
	in []addresslist.Entry,
	list string,
	comment string,
) ([]addresslist.Entry, error) {
	out := make([]addresslist.Entry, 0, len(in))
	seenID := make(map[string]struct{}, len(in))

	var idByAddress map[string]string

	for _, e := range in {
		re := e

		if re.List == "" {
			re.List = list
		}
		if re.Comment == "" {
			re.Comment = comment
		}

		// Если ID не передан, пытаемся найти его на роутере по нормализованному адресу.
		if re.ID == "" {
			addr := strings.TrimSpace(re.Address)
			if addr == "" {
				return nil, fmt.Errorf("remove entry has empty id and address")
			}

			norm, err := addresslist.NormalizeAddress(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid remove address %q: %w", addr, err)
			}

			if idByAddress == nil {
				entries, err := t.client.ListServiceEntries(
					ctx,
					list,
					t.cfg.Firewall.CommentPrefix,
					t.service,
				)
				if err != nil {
					return nil, fmt.Errorf("resolve remove ids: %w", err)
				}

				idByAddress = make(map[string]string, len(entries))
				for _, ex := range entries {
					if ex.ID == "" {
						continue
					}

					exNorm, err := addresslist.NormalizeAddress(ex.Address)
					if err != nil {
						continue
					}

					if _, exists := idByAddress[exNorm]; !exists {
						idByAddress[exNorm] = ex.ID
					}
				}
			}

			id, ok := idByAddress[norm]
			if !ok {
				return nil, fmt.Errorf("remove entry not found on router: %s", norm)
			}

			re.ID = id
			re.Address = norm
		}

		if strings.ContainsAny(re.ID, "/?#") {
			return nil, fmt.Errorf("invalid entry id %q", re.ID)
		}

		if _, ok := seenID[re.ID]; ok {
			continue
		}
		seenID[re.ID] = struct{}{}

		out = append(out, re)
	}

	return out, nil
}

// rollback выполняет best-effort откат транзакции.
func (t *Transaction) rollback() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var errs []string

	// Удаляем то, что успели создать.
	for i := len(t.created) - 1; i >= 0; i-- {
		e := t.created[i]
		if e.ID == "" {
			continue
		}

		if err := t.client.DeleteEntry(ctx, e.ID); err != nil {
			errs = append(errs, fmt.Sprintf(
				"rollback delete created id=%s address=%s: %v",
				e.ID,
				e.Address,
				err,
			))
		}
	}

	// Возвращаем то, что успели удалить.
	for i := len(t.deleted) - 1; i >= 0; i-- {
		e := t.deleted[i]
		e.ID = ""

		if e.Disabled == "" {
			e.Disabled = "false"
		}

		if _, err := t.client.AddEntry(ctx, e); err != nil {
			errs = append(errs, fmt.Sprintf(
				"rollback re-add address=%s comment=%s: %v",
				e.Address,
				e.Comment,
				err,
			))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}

	t.created = nil
	t.deleted = nil

	return nil
}
