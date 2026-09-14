package config

import (
	"os"
	"gopkg.in/yaml.v3"
)

func (c *Config) Save() error {
	raw, err := yaml.Marshal(c)
	if err != nil { return err }
	return os.WriteFile(c.path, raw, 0o600)
}