package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the .m8.yaml configuration file.
type Config struct {
	Database      string `yaml:"database"`
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	User          string `yaml:"user"`
	Password      string `yaml:"password"`
	SSLMode       string `yaml:"sslmode"`
	DatabaseURL   string `yaml:"database_url"`
	MigrationsDir string `yaml:"migrations_dir"`
	Strict        bool   `yaml:"strict"`
}

// Load reads .m8.yaml from the current directory. Returns an empty config
// if the file doesn't exist (not an error).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
