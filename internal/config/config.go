// Package config loads .seed/config.toml — the checked-in, human-owned
// coordination config (control surface, D4.1).
package config

import (
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Coordination struct {
		Backend     string `toml:"backend"`
		Remote      string `toml:"remote"`
		StateBranch string `toml:"state_branch"`
	} `toml:"coordination"`
	Claim struct {
		DefaultLease string `toml:"default_lease"`
	} `toml:"claim"`
	Operators struct {
		Actors []string `toml:"actors"`
	} `toml:"operators"`
}

func Load(seedDir string) (*Config, error) {
	var c Config
	// Defaults hold for a repo with no config file yet.
	c.Coordination.Backend = "filecards"
	c.Coordination.Remote = "origin"
	c.Coordination.StateBranch = "seed-state"
	c.Claim.DefaultLease = "60m"

	path := filepath.Join(seedDir, "config.toml")
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &c); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

func (c *Config) DefaultLease() time.Duration {
	d, err := time.ParseDuration(c.Claim.DefaultLease)
	if err != nil || d <= 0 {
		return 60 * time.Minute
	}
	return d
}

// IsOperator reports roster membership (§10 Q5). Local identity is asserted,
// not authenticated — the real enforcement is push access + server-attributed
// gates (R10).
func (c *Config) IsOperator(actor string) bool {
	return slices.Contains(c.Operators.Actors, actor)
}

// FindRoot walks upward from dir to the repo root containing .seed/.
func FindRoot(dir string) (string, bool) {
	for {
		if st, err := os.Stat(filepath.Join(dir, ".seed")); err == nil && st.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
