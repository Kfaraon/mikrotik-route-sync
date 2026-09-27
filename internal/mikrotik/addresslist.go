package mikrotik

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

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

// AddEntry создаёт запись address-list (PUT) и возвращает .id.
func (c *Client) AddEntry(ctx context.Context, e addresslist.Entry) (string, error) {
	return c.putID(ctx, addressListPath, e)
}

// DeleteEntry удаляет запись по .id.
func (c *Client) DeleteEntry(ctx context.Context, id string) error {
	return c.deleteByID(ctx, addressListPath, id)
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
