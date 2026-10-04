package mikrotik

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
)

// addressListPath — endpoint Firewall Address List в RouterOS REST API.
const addressListPath = "/ip/firewall/address-list"

// addressListProps — ограничиваем ответ нужными полями. ВНИМАНИЕ: пагинация
// .skip/.limit на этом эндпоинте в RouterOS v7 возвращает пустой список
// (проверено на живом роутере), поэтому её нет: адресные списки малы
// (тысячи записей), ответ читается целиком.
const addressListProps = ".proplist=.id,address,list,comment,disabled,dynamic"

// ListEntries возвращает все записи указанного address-list.
//
// Server-side фильтр `?list=` работает в RouterOS v7, но результат ВСЕГДА
// перепроверяется на клиенте точным сравнением list (изоляция guaranteed).
// При ошибке/пустом ответе фильтра читаем список целиком и фильтруем локально
// (PROMPT II.2).
func (c *Client) ListEntries(ctx context.Context, list string) ([]addresslist.Entry, error) {
	if list == "" {
		return nil, fmt.Errorf("address list name is empty")
	}

	var entries []addresslist.Entry
	primaryErr := c.do(ctx, http.MethodGet,
		addressListPath+"?list="+url.QueryEscape(list)+"&"+addressListProps, nil, &entries)
	if primaryErr == nil && len(entries) > 0 {
		return verifyList(entries, list), nil
	}

	var all []addresslist.Entry
	if err := c.do(ctx, http.MethodGet, addressListPath+"?"+addressListProps, nil, &all); err != nil {
		if primaryErr != nil {
			return nil, primaryErr
		}
		return nil, err
	}
	return verifyList(all, list), nil
}

func verifyList(entries []addresslist.Entry, list string) []addresslist.Entry {
	out := make([]addresslist.Entry, 0, len(entries))
	for _, e := range entries {
		if e.List == list {
			out = append(out, e)
		}
	}
	return out
}

// AddEntry создаёт запись address-list (PUT) и возвращает запись с заполненным .id.
//
// Ранее возвращался только string-ID, из-за чего в transaction.go возникала ошибка:
// cannot use created (variable of type string) as addresslist.Entry value.
// Теперь возвращаем полноценный Entry, пригодный для rollback.
func (c *Client) AddEntry(ctx context.Context, e addresslist.Entry) (addresslist.Entry, error) {
	in := e
	in.ID = ""

	id, err := c.putID(ctx, addressListPath, in)
	if err != nil {
		return addresslist.Entry{}, err
	}

	out := e
	out.ID = id
	return out, nil
}

// DeleteEntry удаляет запись по .id.
func (c *Client) DeleteEntry(ctx context.Context, id string) error {
	return c.deleteByID(ctx, addressListPath, id)
}

// UpdateEntry изменяет поле disabled существующей записи через
// PATCH {path}/{.id} (PROMPT II.7 — сравнение учитывает disabled;
// re-enable выключенных управляемых записей).
//
// Проверено на живом RouterOS v7 (192.168.88.1):
//   - PATCH /rest/ip/firewall/address-list/*XX {"disabled":"false"} → 200,
//     в ответе возвращается обновлённый объект;
//   - PATCH по коллекции с ".id" в теле → 400 "missing or invalid resource
//     identifier" — поэтому путь обязательно содержит .id;
//   - остальные поля (address/list/comment) управляются через add/remove:
//     изменение адреса = новая запись + удаление старой (PROMPT IV.4).
func (c *Client) UpdateEntry(ctx context.Context, e addresslist.Entry) error {
	if e.ID == "" || strings.ContainsAny(e.ID, "/?#") {
		return fmt.Errorf("invalid entry id %q", e.ID)
	}
	if e.Disabled == "" {
		return fmt.Errorf("update entry %s: nothing to update (disabled is empty)", e.ID)
	}
	_, err := c.doRaw(ctx, http.MethodPatch,
		addressListPath+"/"+e.ID, map[string]string{"disabled": e.Disabled})
	return err
}

// ListServiceEntries — управляемые записи сервиса:
// list = глобальный, comment = AUTO:<service>, dynamic = false.
func (c *Client) ListServiceEntries(ctx context.Context, list, commentPrefix, service string) ([]addresslist.Entry, error) {
	entries, err := c.ListEntries(ctx, list)
	if err != nil {
		return nil, err
	}
	return addresslist.FilterManaged(entries, list, addresslist.CommentFor(commentPrefix, service)), nil
}
