// Package config reads the JSON configuration file that can stand in for command-line flags.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds the values a configuration file can set. Each JSON key is named after the flag it stands in for.
type Config struct {
	Org      string   `json:"org"`
	Dir      string   `json:"dir"`
	Repos    []string `json:"repos"`
	Identity string   `json:"identity"`
	Parallel int      `json:"parallel"`
	Shallow  bool     `json:"shallow"`
}

// Load reads the configuration file at path. Unknown keys are an error, to catch typos.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer f.Close()

	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	return &cfg, nil
}

// Flags returns the values that are set, keyed by flag name and formatted the way the flag would be on the command
// line.
func (c *Config) Flags() map[string]string {
	flags := make(map[string]string)

	if c.Org != "" {
		flags["org"] = c.Org
	}

	if c.Dir != "" {
		flags["dir"] = c.Dir
	}

	if len(c.Repos) > 0 {
		flags["repos"] = strings.Join(c.Repos, ",")
	}

	if c.Identity != "" {
		flags["identity"] = c.Identity
	}

	if c.Parallel > 0 {
		flags["parallel"] = strconv.Itoa(c.Parallel)
	}

	if c.Shallow {
		flags["shallow"] = "true"
	}

	return flags
}
