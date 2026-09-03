package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Settings is every runtime toggle read from the environment, gathered in
// one place instead of scattered os.Getenv calls. Populated after
// loadDotenv has merged in .env (if present).
type Settings struct {
	AuthEnabled bool
	AuthToken   string
	PostgresDSN string
}

// loadDotenv reads KEY=VALUE lines from path into the process environment.
// Blank lines and lines starting with '#' are skipped. A missing file is
// not an error, .env is optional. A variable already set in the real
// environment is never overwritten, so systemd/docker/shell env always
// wins over the file.
func loadDotenv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return scanner.Err()
}

func LoadSettings() *Settings {
	authEnabled, _ := strconv.ParseBool(os.Getenv("DESKBOX_AUTH_ENABLED"))
	return &Settings{
		AuthEnabled: authEnabled,
		AuthToken:   os.Getenv("DESKBOX_AUTH_TOKEN"),
		PostgresDSN: os.Getenv("DESKBOX_POSTGRES_DSN"),
	}
}
