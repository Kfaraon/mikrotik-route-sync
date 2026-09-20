package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Save сохраняет текущее состояние Config в файл атомарно.
func (c *Config) Save() error {
	return AtomicWrite(c.path, c)
}

// fieldNameMatches сопоставляет сегмент пути конфига (snake_case из YAML,
// напр. "use_ssl", "max_delete_ratio") с именем Go-поля ("UseSSL",
// "MaxDeleteRatio"). Сравнение регистронезависимое и без подчёркиваний.
// Используется и для Set, и для GetPath, чтобы API совпадал с YAML-ключами.
func fieldNameMatches(goName, pathPart string) bool {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r == '_' || r == '-' {
				continue
			}
			b.WriteRune(r)
		}
		return strings.ToLower(b.String())
	}
	return norm(goName) == norm(pathPart)
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
			return fieldNameMatches(name, part)
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
		return fieldNameMatches(name, lastPart)
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

// GetPath читает значение параметра по точечному пути ("mikrotik.host").
func (c *Config) GetPath(path string) (any, error) {
	if path == "" {
		return nil, fmt.Errorf("empty config path")
	}
	parts := strings.Split(path, ".")
	v := reflect.ValueOf(c).Elem()

	for i, part := range parts {
		for v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Map:
			mv := v.MapIndex(reflect.ValueOf(part))
			if !mv.IsValid() {
				return nil, fmt.Errorf("key %s not found in map", part)
			}
			v = mv
			_ = i
		case reflect.Struct:
			f := v.FieldByNameFunc(func(name string) bool {
				return fieldNameMatches(name, part)
			})
			if !f.IsValid() {
				return nil, fmt.Errorf("field %s not found in %s", part, v.Type().Name())
			}
			v = f
		default:
			return nil, fmt.Errorf("cannot navigate %s: not a struct or map", part)
		}
	}
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil, nil
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil, fmt.Errorf("invalid value at %s", path)
	}
	if v.Type() == reflect.TypeOf(Duration(0)) {
		return time.Duration(v.Int()).String(), nil
	}
	return v.Interface(), nil
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
			// Списки из UI/CLI задаются через запятую или точку с запятой.
			if strings.ContainsAny(val, ",;") {
				parts := strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ';' })
				out := make([]string, 0, len(parts))
				for _, p := range parts {
					if p = strings.TrimSpace(p); p != "" {
						out = append(out, p)
					}
				}
				return reflect.ValueOf(out), nil
			}
			if val == "" {
				return reflect.ValueOf([]string{}), nil
			}
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
//
// Шаги (PROMPT IX):
//  1. Marshal полного состояния Config в YAML
//  2. Запись во временный файл (.tmp) с правами 0600
//  3. fsync для гарантии записи на диск
//  4. os.Rename .tmp -> оригинальный путь (атомарная POSIX-операция)
//
// Полная сериализация гарантирует, что изменения, сделанные через
// веб-интерфейс (настройки, расписания, overrides), реально сохраняются.
func AtomicWrite(path string, c *Config) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("refusing to save invalid config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString("# ВНИМАНИЕ: файл содержит секреты. Права 0600, добавлен в .gitignore.\n")
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(c); err != nil {
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

	// На Windows chmod(0600) делает файл read-only и блокирует последующий
	// rename поверх него — снимаем атрибут перед заменой.
	if _, err := os.Stat(path); err == nil {
		_ = os.Chmod(path, 0o666)
	}

	// renameFunc — точка расширения для тестов.
	var renameErr error
	if renameFunc == nil {
		renameFunc = os.Rename
	}
	if renameErr = renameFunc(tmpPath, path); renameErr != nil {
		// Fallback: rename невозможен поверх bind-mount / между устройствами
		// (Docker: "./config.yaml:/data/config.yaml" -> "device or resource busy",
		// EXDEV и т.п.). Пишем на место: truncate + copy.
		if copyErr := copyFileSync(tmpPath, path); copyErr != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("rename: %v; in-place fallback: %w", renameErr, copyErr)
		}
		os.Remove(tmpPath)
	}

	// Гарантируем права 0600 (POSIX; на Windows пропускаем — см. выше)
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	return nil
}

// renameFunc переопределяется в тестах для эмуляции недоступности rename.
var renameFunc func(oldname, newname string) error

// copyFileSync перезаписывает содержимое dst содержимым src
// (используется, когда атомарный rename недоступен).
func copyFileSync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	defer in.Close()

	fi, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat tmp: %w", err)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return fmt.Errorf("open target: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync: %w", err)
	}
	return out.Close()
}
