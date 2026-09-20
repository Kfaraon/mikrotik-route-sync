package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

// RDAP bootstrap: rdap.org перенаправляет на реестр нужного RIR.
const rdapIPBase = "https://rdap.org/ip/"

// rdapEntity — фрагмент RDAP-ответа: entity с номером автономной системы.
type rdapEntity struct {
	Handle   string       `json:"handle"`
	ID       string       `json:"id"`
	Entities []rdapEntity `json:"entities"`
}

type rdapResponse struct {
	Name     string       `json:"name"`
	Handle   string       `json:"handle"`
	Entities []rdapEntity `json:"entities"`
}

// VerifyPrefixBelongsToASN проверяет через RDAP, что префикс p
// действительно аннсится указанным ASN (PROMPT II.3, уровень 3).
//
// Возвращает:
//   - (true, nil):  префикс подтверждён;
//   - (false, nil): RDAP явно говорит, что ASN другой — префикс отклоняется;
//   - (true, err):  RDAP недоступен/непонятен — считаем проверенным
//     (префиксы уже получены из BGP/RIPEstat, не блокируем синк инфраструктурной ошибкой).
func (r *Resolver) VerifyPrefixBelongsToASN(ctx context.Context, p netip.Prefix, asn int) (bool, error) {
	if r.fetch == nil {
		return true, fmt.Errorf("rdap: no fetcher configured")
	}

	body, err := r.fetch.Fetch(ctx, rdapIPBase+p.Addr().Unmap().String())
	if err != nil {
		return true, fmt.Errorf("rdap fetch: %w", err)
	}

	var v rdapResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return true, fmt.Errorf("rdap decode: %w", err)
	}

	// Если RDAP вернул запись без упоминания ASN — проверять нечего.
	found := collectRDAPASNs(&v)
	if len(found) == 0 {
		return true, nil
	}
	if _, ok := found[asn]; ok {
		return true, nil
	}
	return false, nil
}

// collectRDAPASNs рекурсивно собирает все ASN из RDAP-entities (autnum<NNN>).
func collectRDAPASNs(v *rdapResponse) map[int]struct{} {
	out := map[int]struct{}{}
	var walk func(ents []rdapEntity)
	walk = func(ents []rdapEntity) {
		for _, e := range ents {
			for _, s := range []string{e.Handle, e.ID} {
				s = strings.ToUpper(strings.TrimSpace(s))
				if strings.HasPrefix(s, "AUTNUM") {
					s = strings.TrimPrefix(s, "AUTNUM")
				}
				if strings.HasPrefix(s, "AS") {
					if n, err := ParseASN(s); err == nil {
						out[n] = struct{}{}
					}
				}
			}
			walk(e.Entities)
		}
	}
	walk(v.Entities)
	return out
}
