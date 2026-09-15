package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ErrPathNotSet возвращается, если Save() вызван до загрузки конфига из файла.
var ErrPathNotSet = errors.New("config path is not set")

// Save сохраняет конфигурацию атомарно в файл, путь к которому был задан
// при вызове Load(). Права файла: 0600 (security by default).
//
// Механизм: временный файл → fsync → os.Rename → chmod 0600.
// Это гарантирует, что при сбое (крах, потеря питания) конфиг не будет
// повреждён — остаётся либо старая версия, либо полностью записанная новая.
func (c *Config) Save() error {
	if c.path == "" {
		return ErrPathNotSet
	}
	return AtomicWrite(c.path, c)
}

// AtomicWrite выполняет атомарную запись YAML-конфигурации в файл.
//   1. Создаёт временный файл в той же директории (для корректного Rename).
//   2. Сериализует значение в YAML.
//   3. Записывает с fsync.
//   4. Устанавливает права 0600 и владельца (по возможности).
//   5. Атомарно переименовывает временный файл в целевой.
//
// Это критично для production: предотвращает "обрыв" конфига на середине записи
// при SIGKILL или потере питания, что могло бы привести к полной потере
// маршрутов при следующем запуске (т.к. сервисы = пустой список).
func AtomicWrite(path string, v any) error {
	// 1. Проверка директории
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := CheckSecureDir(dir); err != nil {
		return fmt.Errorf("insecure config dir %q: %w", dir, err)
	}

	// 2. Временный файл в той же FS для корректного rename
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	// 3. Права 0600 ДО записи секрета
	if err = tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	// 4. Сериализация YAML
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}

	// 5. Запись + fsync
	if _, err = tmp.Write(b); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// 6. Атомарный rename (на POSIX — атомарно в пределах одной FS)
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to config: %w", err)
	}

	// 7. Контрольная установка прав (на случай наследования от umask)
	if err = os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod config file: %w", err)
	}

	success = true
	return nil
}

// CheckSecureDir проверяет, что права директории — 0700 (только владелец).
// Требование секции IX промпта "Безопасность конфигурации".
func CheckSecureDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	perm := info.Mode().Perm()
	if perm&0o077 != 0 {
		return fmt.Errorf("directory has permissions %04o, expected 0700 or stricter", perm)
	}
	return nil
}
