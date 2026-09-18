package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Save сохраняет текущее состояние Config в файл атомарно.
func (c *Config) Save() error {
	return AtomicWrite(c.path, c)
}

// Set устанавливает значение параметра по точечному пути.
// Поддерживает пути вида "mikrotik.host", "safety.max_delete_ratio".
// После установки требуется вызов Save() для сохранения на диск.
func (c *Config) Set(path string, value any) error {
	if path == "" {
		return fmt.Errorf("empty config path")
	}
	parts := strings.Split(path, ".")
	if len(parts) == 0 {
		return fmt.Errorf("invalid config path: %s", path)
	}
	return setNestedValue(reflect.ValueOf(c).Elem(), parts, value)
}

// setNestedValue рекурсивно устанавливает значение по пути в структуре Config.
func setNestedValue(v reflect.Value, parts []string, value any) error {
	if len(parts) == 0 {
		return fmt.Errorf("empty path")
	}

	current := v
	for i, part := range parts[:len(parts)-1] {
		if current.Kind() == reflect.Ptr {
			current = current.Elem()
		}

		// Обработка map (например, Overrides, Groups, Services в Schedules)
		if current.Kind() == reflect.Map {
			if current.IsNil() {
				current.Set(reflect.MakeMap(current.Type()))
			}
			key := reflect.ValueOf(part)
			elem := current.MapIndex(key)
			if !elem.IsValid() {
				elem = reflect.New(current.Type().Elem()).Elem()
			}
			tmp := reflect.New(elem.Type()).Elem()
			tmp.Set(elem)
			if err := setNestedValue(tmp, parts[i+1:], value); err != nil {
				return err
			}
			current.SetMapIndex(key, tmp)
			return nil
		}

		if current.Kind() != reflect.Struct {
			return fmt.Errorf("path %s: %s is not a struct or map", strings.Join(parts, "."), part)
		}

		field := current.FieldByNameFunc(func(name string) bool {
			return strings.EqualFold(name, part)
		})
		if !field.IsValid() {
			return fmt.Errorf("field %s not found in %s", part, current.Type().Name())
		}
		current = field
	}

	lastPart := parts[len(parts)-1]

	// Обработка map как последнего элемента
	if current.Kind() == reflect.Map {
		if current.IsNil() {
			current.Set(reflect.MakeMap(current.Type()))
		}
		val, err := convertValue(value, current.Type().Elem())
		if err != nil {
			return fmt.Errorf("set %s: %w", lastPart, err)
		}
		current.SetMapIndex(reflect.ValueOf(lastPart), val)
		return nil
	}

	if current.Kind() == reflect.Ptr {
		current = current.Elem()
	}
	if current.Kind() != reflect.Struct {
		return fmt.Errorf("cannot navigate into non-struct: %s", lastPart)
	}

	field := current.FieldByNameFunc(func(name string) bool {
		return strings.EqualFold(name, lastPart)
	})
	if !field.IsValid() {
		return fmt.Errorf("field %s not found", lastPart)
	}
	if !field.CanSet() {
		return fmt.Errorf("field %s is not settable", lastPart)
	}

	val, err := convertValue(value, field.Type())
	if err != nil {
		return fmt.Errorf("set %s: %w", lastPart, err)
	}
	field.Set(val)
	return nil
}

// convertValue приводит значение к целевому типу.
func convertValue(value any, targetType reflect.Type) (reflect.Value, error) {
	if value == nil {
		return reflect.Zero(targetType), nil
	}
	v := reflect.ValueOf(value)

	// Тип совпадает
	if v.Type().AssignableTo(targetType) {
		return v, nil
	}

	// Конвертация в Duration
	if targetType == reflect.TypeOf(Duration(0)) {
		switch val := value.(type) {
		case string:
			dur, err := time.ParseDuration(val)
			if err != nil {
				return reflect.Value{}, err
			}
			return reflect.ValueOf(Duration(dur)), nil
		case time.Duration:
			return reflect.ValueOf(Duration(val)), nil
		}
	}

	// Конвертация в []string
	if targetType == reflect.TypeOf([]string{}) {
		switch val := value.(type) {
		case []string:
			return reflect.ValueOf(val), nil
		case []any:
			out := make([]string, 0, len(val))
			for _, item := range val {
				out = append(out, fmt.Sprint(item))
			}
			return reflect.ValueOf(out), nil
		case string:
			return reflect.ValueOf([]string{val}), nil
		}
	}

	// Примитивные типы
	switch targetType.Kind() {
	case reflect.String:
		return reflect.ValueOf(fmt.Sprint(value)), nil
	case reflect.Int:
		n, err := toInt(value)
		return reflect.ValueOf(int(n)), err
	case reflect.Int64:
		n, err := toInt(value)
		return reflect.ValueOf(int64(n)), err
	case reflect.Float64:
		f, err := toFloat(value)
		return reflect.ValueOf(f), err
	case reflect.Bool:
		b, err := toBool(value)
		return reflect.ValueOf(b), err
	}

	if v.Type().ConvertibleTo(targetType) {
		return v.Convert(targetType), nil
	}
	return reflect.Value{}, fmt.Errorf("cannot convert %T to %s", value, targetType)
}

// toInt преобразует любое значение в int.
func toInt(v any) (int, error) {
	switch val := v.(type) {
	case int:
		return val, nil
	case int64:
		return int(val), nil
	case float64:
		return int(val), nil
	case string:
		n, err := strconv.Atoi(val)
		return n, err
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}

// toFloat преобразует любое значение в float64.
func toFloat(v any) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case string:
		return strconv.ParseFloat(val, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}

// toBool преобразует любое значение в bool.
func toBool(v any) (bool, error) {
	switch val := v.(type) {
	case bool:
		return val, nil
	case string:
		return strconv.ParseBool(val)
	case int:
		return val != 0, nil
	default:
		return false, fmt.Errorf("cannot convert %T to bool", v)
	}
}

// AtomicWrite выполняет атомарную запись конфигурации в файл.
// Шаги:
//  1. Читает оригинальный файл и парсит в yaml.Node (сохранение комментариев)
//  2. Обновляет узел "services" актуальным списком
//  3. Записывает во временный файл (.tmp)
//  4. Выполняет fsync для гарантии записи на диск
//  5. Переименовывает .tmp -> оригинальный путь (атомарная POSIX-операция)
func AtomicWrite(path string, c *Config) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	originalData, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read original config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(originalData, &doc); err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}

	// Обновляем узел services
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		rootNode := doc.Content[0]
		if rootNode.Kind == yaml.MappingNode {
			if err := updateServicesNode(rootNode, c.Services); err != nil {
				return fmt.Errorf("update services: %w", err)
			}
		}
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return fmt.Errorf("encode yaml: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("close encoder: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}

	// Fsync перед rename
	if f, err := os.Open(tmpPath); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	// Гарантируем права 0600
	_ = os.Chmod(path, 0o600)
	return nil
}

// updateServicesNode находит или создаёт узел "services" в корне YAML-документа.
func updateServicesNode(rootNode *yaml.Node, services []string) error {
	if rootNode == nil || rootNode.Kind != yaml.MappingNode {
		return fmt.Errorf("invalid root node")
	}

	var servicesValueNode *yaml.Node
	var servicesKeyNode *yaml.Node

	// Поиск существующего узла "services"
	for i := 0; i < len(rootNode.Content); i += 2 {
		if i+1 >= len(rootNode.Content) {
			break
		}
		keyNode := rootNode.Content[i]
		if keyNode.Kind == yaml.ScalarNode && keyNode.Value == "services" {
			servicesKeyNode = keyNode
			servicesValueNode = rootNode.Content[i+1]
			break
		}
	}

	// Создание нового узла при отсутствии
	if servicesKeyNode == nil {
		servicesKeyNode = &yaml.Node{Kind: yaml.ScalarNode, Value: "services", Tag: "!!str"}
		servicesValueNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		rootNode.Content = append(rootNode.Content, servicesKeyNode, servicesValueNode)
	}

	// Перезаписываем содержимое
	servicesValueNode.Content = make([]*yaml.Node, 0, len(services))
	for _, service := range services {
		node := &yaml.Node{Kind: yaml.ScalarNode, Value: service, Tag: "!!str"}
		servicesValueNode.Content = append(servicesValueNode.Content, node)
	}
	return nil
}
