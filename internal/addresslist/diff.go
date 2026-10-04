package addresslist

import (
	"fmt"
	"sort"
)

// Diff — результат сравнения желаемого набора IPv4-префиксов с управляемыми
// записями address-list сервиса. add/remove/unchanged/update — нормализованные
// CIDR (PROMPT IV.3).
//
// update — оставленные, но выключенные (disabled=true) управляемые записи.
// Сравнение обязано учитывать disabled (PROMPT II.7), поэтому такая запись
// никогда не попадает в unchanged: нужен re-enable (PROMPT I: disabled=false
// для управляемых записей, если не задано иное в конфигурации).
type Diff struct {
	Service   string   `json:"service"`
	List      string   `json:"list"`
	Comment   string   `json:"comment"`
	Add       []string `json:"add"`
	Remove    []string `json:"remove"`
	Unchanged []string `json:"unchanged"`
	Update    []string `json:"update"`
}

// ComputeDiff вычисляет разницу.
//
//   - desired: нормализованные IPv4-префиксы (после агрегации);
//   - managed: существующие управляемые записи (list+comment+dynamic=false);
//   - list/comment: контекст для отчёта.
//
// Сравнение по нормализованному address + disabled. Дубли одного address
// самоочищаются: остаётся один экземпляр (предпочтительно включённый),
// лишние уходят в remove.
func ComputeDiff(service, list, comment string, desired []string, managed []Entry) (*Diff, error) {
	normDesired := make(map[string]struct{}, len(desired))
	for _, d := range desired {
		n, err := NormalizeAddress(d)
		if err != nil {
			return nil, fmt.Errorf("invalid desired address: %w", err)
		}
		normDesired[n] = struct{}{}
	}

	keep, remove, update := classify(normDesired, managed)

	diff := &Diff{
		Service:   service,
		List:      list,
		Comment:   comment,
		Add:       make([]string, 0, len(normDesired)),
		Remove:    make([]string, 0, len(remove)),
		Unchanged: make([]string, 0, len(keep)),
		Update:    make([]string, 0, len(update)),
	}

	kept := make(map[string]struct{}, len(keep)+len(update))
	for _, e := range keep {
		n, err := NormalizeAddress(e.Address)
		if err != nil {
			continue
		}
		kept[n] = struct{}{}
		diff.Unchanged = append(diff.Unchanged, n)
	}
	// Существующие, но выключенные записи покрывают желаемый адрес: нужен
	// re-enable, а не второй create — иначе на роутере появится дубль.
	for _, e := range update {
		if n, err := NormalizeAddress(e.Address); err == nil {
			kept[n] = struct{}{}
		}
	}
	for n := range normDesired {
		if _, ok := kept[n]; !ok {
			diff.Add = append(diff.Add, n)
		}
	}
	for _, e := range update {
		if n, err := NormalizeAddress(e.Address); err == nil {
			diff.Update = append(diff.Update, n)
		}
	}
	for _, e := range remove {
		if n, err := NormalizeAddress(e.Address); err == nil {
			diff.Remove = append(diff.Remove, n)
		}
	}

	sort.Strings(diff.Add)
	sort.Strings(diff.Remove)
	sort.Strings(diff.Unchanged)
	sort.Strings(diff.Update)
	return diff, nil
}

// classify разбивает управляемые записи относительно набора желаемых адресов:
//
//   - keep: по одному экземпляру на каждый желаемый адрес — предпочтительно
//     включённый (enabled), чтобы не плодить лишние PATCH-запросы;
//   - remove: дубли (включая выключенные при наличии включённого дубля)
//     и адреса, отсутствующие в desired;
//   - update: оставленные, но выключенные записи — кандидаты на re-enable.
//
// Записи с некорректным address пропускаются (fail-closed: не трогаем).
// Сравнение учитывает disabled согласно PROMPT II.7.
func classify(desired map[string]struct{}, managed []Entry) (keep, remove, update []Entry) {
	enabledAvailable := make(map[string]bool, len(managed))
	for _, e := range managed {
		n, err := NormalizeAddress(e.Address)
		if err != nil {
			continue
		}
		if _, ok := desired[n]; !ok {
			continue
		}
		if !e.IsDisabled() {
			enabledAvailable[n] = true
		}
	}

	kept := make(map[string]bool, len(managed))
	for _, e := range managed {
		n, err := NormalizeAddress(e.Address)
		if err != nil {
			continue
		}
		if _, ok := desired[n]; !ok {
			remove = append(remove, e)
			continue
		}
		// Выключенный экземпляр при наличии включённого дубля: удаляем его,
		// оставляем включённый — без PATCH.
		if e.IsDisabled() && enabledAvailable[n] {
			remove = append(remove, e)
			continue
		}
		if kept[n] {
			remove = append(remove, e)
			continue
		}
		kept[n] = true
		if e.IsDisabled() {
			update = append(update, e)
		} else {
			keep = append(keep, e)
		}
	}
	return keep, remove, update
}

// desiredSet нормализует желаемые адреса в множество.
// Некорректный адрес пропускается (как в RemovalPlan/UpdatePlan).
func desiredSet(desired []string) map[string]struct{} {
	set := make(map[string]struct{}, len(desired))
	for _, d := range desired {
		if n, err := NormalizeAddress(d); err == nil {
			set[n] = struct{}{}
		}
	}
	return set
}

// RemovalPlan — какие управляемые записи реально удалить (по .id).
// Для каждого нормализованного address: если он есть в desired — оставляем
// один экземпляр (предпочтительно включённый), лишние дубли удаляем;
// если нет — удаляем все экземпляры.
func RemovalPlan(desired []string, managed []Entry) []Entry {
	_, remove, _ := classify(desiredSet(desired), managed)
	return remove
}

// UpdatePlan — оставленные, но выключенные управляемые записи (по .id):
// кандидаты на re-enable при firewall.manage_disabled=true.
func UpdatePlan(desired []string, managed []Entry) []Entry {
	_, _, update := classify(desiredSet(desired), managed)
	return update
}
