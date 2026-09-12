package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the desk's structural, non-secret settings — meant to be edited
// by hand and checked into version control, the same role tcs.yaml plays
// for a tool's contract. Secrets (a Postgres DSN with a password, an auth
// token) stay in .env, which is gitignored; this file isn't.
//
// Every field has a built-in default, so a missing or partially-filled
// deskbox.yaml is fine — LoadConfig returns a zero Config rather than an
// error, and callers only use the fields they need.
type Config struct {
	Store struct {
		Kind   string `yaml:"kind"` // sqlite (default) | postgres | memory
		SQLite struct {
			Path string `yaml:"path"`
		} `yaml:"sqlite"`
	} `yaml:"store"`
}

// LoadConfig reads path as YAML. A missing file is not an error — it
// changes nothing, same as loadDotenv's handling of a missing .env.
func LoadConfig(path string) (*Config, error) {
	var cfg Config
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
