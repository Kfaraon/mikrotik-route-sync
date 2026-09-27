package addresslist

import (
	"fmt"
	"sort"
)

// Diff — результат сравнения желаемого набора IPv4-префиксов с управляемыми
// записями address-list сервиса. add/remove/unchanged — нормализованные CIDR
// (PROMPT IV.3).
type Diff struct {
	Service   string   `json:"service"`
	List      string   `json:"list"`
	Comment   string   `json:"comment"`
	Add       []string `json:"add"`
	Remove    []string `json:"remove"`
	Unchanged []string `json:"unchanged"`
}

// ComputeDiff вычисляет разницу.
//
//   - desired: нормализованные IPv4-префиксы (после агрегации);
//   - managed: существующие управляемые записи (list+comment+dynamic=false);
//   - list/comment: контекст для отчёта.
//
// Сравнение по нормализованному address. Дубли в managed (один address
// встречается несколько раз) дают лишний адрес в Remove — самоочистка.
func ComputeDiff(service, list, comment string, desired []string, managed []Entry) (*Diff, error) {
	normDesired := make(map[string]struct{}, len(desired))
	order := make([]string, 0, len(desired))
	for _, d := range desired {
		n, err := NormalizeAddress(d)
		if err != nil {
			return nil, fmt.Errorf("invalid desired address: %w", err)
		}
		if _, seen := normDesired[n]; !seen {
			normDesired[n] = struct{}{}
			order = append(order, n)
		}
	}

	// existing: address -> количество записей (для дедуп-удаления).
	existingCount := make(map[string]int, len(managed))
	for _, e := range managed {
		n, err := NormalizeAddress(e.Address)
		if err != nil {
			// Не понимаем запись — не трогаем (fail-closed).
			continue
		}
		existingCount[n]++
	}

	diff := &Diff{
		Service:   service,
		List:      list,
		Comment:   comment,
		Add:       make([]string, 0),
		Remove:    make([]string, 0),
		Unchanged: make([]string, 0),
	}

	for _, n := range order {
		if existingCount[n] > 0 {
			diff.Unchanged = append(diff.Unchanged, n)
			// Лишние дубли того же address → в Remove (остается одна запись).
			for i := 0; i < existingCount[n]-1; i++ {
				diff.Remove = append(diff.Remove, n)
			}
		} else {
			diff.Add = append(diff.Add, n)
		}
	}

	// Существующие адреса, которых нет в desired → удалить ВСЕ их экземпляры.
	for n, cnt := range existingCount {
		if _, ok := normDesired[n]; ok {
			continue
		}
		for i := 0; i < cnt; i++ {
			diff.Remove = append(diff.Remove, n)
		}
	}

	sort.Strings(diff.Add)
	sort.Strings(diff.Remove)
	sort.Strings(diff.Unchanged)
	return diff, nil
}

// RemovalPlan — какие управляемые записи реально удалить (по .id).
// Для каждого нормализованного address: если он есть в desired — оставляем
// первую запись, лишние дубли удаляем; если нет — удаляем все записи.
func RemovalPlan(desired []string, managed []Entry) []Entry {
	desiredSet := make(map[string]struct{}, len(desired))
	for _, d := range desired {
		if n, err := NormalizeAddress(d); err == nil {
			desiredSet[n] = struct{}{}
		}
	}

	seen := make(map[string]bool, len(managed))
	var remove []Entry
	for _, e := range managed {
		n, err := NormalizeAddress(e.Address)
		if err != nil {
			continue
		}
		if _, ok := desiredSet[n]; ok {
			// Первый экземпляр оставляем, дубли — удаляем.
			if seen[n] {
				remove = append(remove, e)
			}
			seen[n] = true
			continue
		}
		remove = append(remove, e)
	}
	return remove
}
