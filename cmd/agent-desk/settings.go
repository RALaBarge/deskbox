package main

import (
	"bufio"
	"fmt"
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
	// Strict refuses to start, and refuses to run jobs, when an
	// enforcement mechanism is unavailable — rather than logging a warning
	// and continuing with the guarantee quietly downgraded to advisory.
	Strict      bool
	StoreKind   string // "sqlite" (default) | "postgres" | "memory"
	SQLitePath  string // empty = <data-dir>/deskbox.db
	PostgresDSN string

	// Per-job cgroup limits (via systemd-run --scope). bwrap's namespaces
	// isolate what a tool can see; they don't cap what it can consume, so
	// without these one runaway or malicious tool degrades the host for
	// every other job running at the same time.
	JobMemoryMax string // systemd MemoryMax, e.g. "512M"
	JobTasksMax  int    // systemd TasksMax: caps forked processes/threads, stops fork bombs
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

func LoadSettings() (*Settings, error) {
	authEnabled := false
	if v := os.Getenv("DESKBOX_AUTH_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("DESKBOX_AUTH_ENABLED=%q is not a valid boolean", v)
		}
		authEnabled = b
	}

	strict := false
	if v := os.Getenv("DESKBOX_STRICT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("DESKBOX_STRICT=%q is not a valid boolean", v)
		}
		strict = b
	}

	memMax := os.Getenv("DESKBOX_JOB_MEMORY_MAX")
	if memMax == "" {
		memMax = "512M"
	}

	tasksMax := 64
	if v := os.Getenv("DESKBOX_JOB_TASKS_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("DESKBOX_JOB_TASKS_MAX=%q is not a positive integer", v)
		}
		tasksMax = n
	}

	return &Settings{
		AuthEnabled: authEnabled,
		AuthToken:   os.Getenv("DESKBOX_AUTH_TOKEN"),
		Strict:      strict,
		// StoreKind/SQLitePath are left "" when unset — main.go falls back
		// to deskbox.yaml and then the hardcoded default, in that order, so
		// an env var must actually be set to win at this layer.
		StoreKind:    os.Getenv("DESKBOX_STORE"),
		SQLitePath:   os.Getenv("DESKBOX_SQLITE_PATH"),
		PostgresDSN:  os.Getenv("DESKBOX_POSTGRES_DSN"),
		JobMemoryMax: memMax,
		JobTasksMax:  tasksMax,
	}, nil
}
