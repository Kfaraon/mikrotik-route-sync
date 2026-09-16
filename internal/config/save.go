package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

var ErrPathNotSet = errors.New("config path is not set")

func (c *Config) Save() error {
	if c.path == "" {
		return ErrPathNotSet
	}
	return AtomicWrite(c.path, c)
}

func CheckSecurePermissions(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("insecure file permissions: %o (expected 0600)", fi.Mode().Perm())
	}
	return nil
}

func AtomicWrite(path string, v any) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
	}

	if fi, err := os.Stat(path); err == nil {
		if fi.Mode().Perm() != 0o600 {
			return fmt.Errorf("insecure config file permissions %q: %o (expected 0600)", path, fi.Mode().Perm())
		}
	}

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

	if err = tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}

	if _, err = tmp.Write(b); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to config: %w", err)
	}

	if err = os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod config file: %w", err)
	}

	success = true
	return nil
}
