package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the .m8.yaml configuration file.
type Config struct {
	Database    string `yaml:"database"`
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	User        string `yaml:"user"`
	Password    string `yaml:"password"`
	SSLMode     string `yaml:"sslmode"`
	DatabaseURL string `yaml:"database_url"`
	ShadowURL   string `yaml:"shadow_url"`
	// RequireShadow makes m8 refuse to run rather than fall back to creating
	// schema-diff temp databases on the target. Set it in the .m8.yaml of any
	// repository whose target is a production primary.
	RequireShadow bool `yaml:"require_shadow"`
	// FailOnUnvalidated makes m8 refuse a schema diff whose plan could not be
	// validated, rather than warning and carrying on. require_shadow implies it:
	// an unvalidated plan is the same degrade one layer in. Also settable as
	// M8_FAIL_ON_UNVALIDATED.
	FailOnUnvalidated bool   `yaml:"fail_on_unvalidated"`
	MigrationsDir     string `yaml:"migrations_dir"`
	Strict            bool   `yaml:"strict"`
	// LockTimeout and StatementTimeout override the per-statement timeouts
	// pg-schema-diff derives from each generated statement's hazards. Empty
	// keeps the derived value. Any duration Go's time.ParseDuration accepts,
	// e.g. "5s", "10min" is not valid -- use "10m".
	LockTimeout      string `yaml:"lock_timeout"`
	StatementTimeout string `yaml:"statement_timeout"`
}

// Load reads .m8.yaml from the current directory. A missing file yields an
// empty config and no error; a file that exists but cannot be read or parsed is
// an error, and callers must treat it as fatal -- silently falling back to an
// empty config turns every safety setting in the file off.
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
