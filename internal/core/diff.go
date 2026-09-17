package core

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
)

// ============================================================================
// Типы данных
// ============================================================================

// Diff описывает результат сравнения двух наборов маршрутов.
// Используется для инкрементальной синхронизации (добавить/удалить только то, что изменилось).
type Diff struct {
	// Add — маршруты, которые есть в desired, но отсутствуют в existing.
	Add []RouteKey

	// Remove — маршруты из existing, которых нет в desired (будут удалены из MikroTik).
	// Содержат полные объекты mikrotik.Route с .ID для точечного удаления.
	Remove []mikrotik.Route

	// Unchanged — количество маршрутов, которые совпадают в обоих наборах
	// (не требуют никаких действий).
	Unchanged int
}

// routeKeyString — уникальный ключ маршрута для сравнения.
// Включает нормализованный CIDR + gateway + routing table + distance.
type routeKeyString struct {
	cidr     string
	gateway  string
	table    string
	distance int
}

// ============================================================================
// Основная логика
// ============================================================================

// ComputeDiff вычисляет разницу между желаемым и текущим наборами маршрутов.
//
// Параметры:
//   - desired: []RouteKey — желаемый набор (результат сбора + валидации + агрегации).
//   - existing: []mikrotik.Route — текущие маршруты из MikroTik (отфильтрованы по comment=AUTO:<service>).
//
// Возвращает:
//   - *Diff с полями Add, Remove, Unchanged.
//   - error только при критических ошибках (например, невалидный CIDR в desired).
//
// Особенности:
//   - CIDR нормализуются (хостовые биты обнуляются).
//   - Distance парсится из строки (MikroTik API возвращает строку).
//   - Пустые значения Gateway/Table трактуются как пустые строки.
//   - Битые записи в existing (невалидный CIDR) пропускаются и не удаляются
//     (fail-closed: не трогаем то, что не понимаем).
func ComputeDiff(desired []RouteKey, existing []mikrotik.Route) (*Diff, error) {
	diff := &Diff{
		Add:    make([]RouteKey, 0),
		Remove: make([]mikrotik.Route, 0),
	}

	// Шаг 1: индексируем existing по нормализованному ключу.
	// Используем map[routeKeyString]mikrotik.Route для O(1) поиска.
	existingMap := make(map[routeKeyString]mikrotik.Route, len(existing))
	seenKeys := make(map[routeKeyString]bool, len(desired))

	for _, r := range existing {
		key, err := mikrotikRouteToKey(r)
		if err != nil {
			// Fail-closed: пропускаем битую запись, не удаляем её.
			// Она останется в MikroTik и будет видна в следующем запуске.
			continue
		}
		existingMap[key] = r
	}

	// Шаг 2: проходим по desired и формируем Add / Unchanged.
	for _, d := range desired {
		key, err := routeKeyToKeyString(d)
		if err != nil {
			// Критическая ошибка: desired должен быть уже валидирован.
			return nil, fmt.Errorf("invalid desired route: %w", err)
		}

		seenKeys[key] = true

		if _, exists := existingMap[key]; exists {
			diff.Unchanged++
		} else {
			diff.Add = append(diff.Add, d)
		}
	}

	// Шаг 3: проходим по existing и формируем Remove (всё, что не было "seen").
	for key, r := range existingMap {
		if !seenKeys[key] {
			diff.Remove = append(diff.Remove, r)
		}
	}

	return diff, nil
}

// ============================================================================
// Вспомогательные функции
// ============================================================================

// mikrotikRouteToKey конвертирует mikrotik.Route в routeKeyString.
// Парсит CIDR (с нормализацией) и Distance (из строки в int).
func mikrotikRouteToKey(r mikrotik.Route) (routeKeyString, error) {
	normalizedCIDR, err := normalizeCIDR(r.DstAddress)
	if err != nil {
		return routeKeyString{}, fmt.Errorf("parse dst-address %q: %w", r.DstAddress, err)
	}

	distance := 1 // default distance in RouterOS
	if r.Distance != "" {
		if d, err := strconv.Atoi(r.Distance); err == nil {
			distance = d
		}
	}

	return routeKeyString{
		cidr:     normalizedCIDR,
		gateway:  r.Gateway,
		table:    r.RoutingTable,
		distance: distance,
	}, nil
}

// routeKeyToKeyString конвертирует RouteKey в routeKeyString.
// Нормализует CIDR и валидирует его.
func routeKeyToKeyString(r RouteKey) (routeKeyString, error) {
	normalizedCIDR, err := normalizeCIDR(r.CIDR)
	if err != nil {
		return routeKeyString{}, fmt.Errorf("parse CIDR %q: %w", r.CIDR, err)
	}

	return routeKeyString{
		cidr:     normalizedCIDR,
		gateway:  r.Gateway,
		table:    r.Table,
		distance: r.Distance,
	}, nil
}

// normalizeCIDR приводит CIDR к каноническому виду:
//   - Обнуляет хостовые биты (8.8.8.1/24 → 8.8.8.0/24).
//   - Поддерживает IPv4 и IPv6.
//   - Возвращает ошибку при невалидном формате.
//
// Это обеспечивает идемпотентность: повторный запуск с теми же данными
// не создаст дубликатов в MikroTik.
func normalizeCIDR(cidr string) (string, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", err
	}
	// Masked() обнуляет хостовые биты — это ключ к идемпотентности.
	return p.Masked().String(), nil
}
