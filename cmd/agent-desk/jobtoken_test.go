package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJobTokenMACIsBoundToEveryField(t *testing.T) {
	key := []byte("0123456789abcdef-secret")
	base := jobTokenMAC(key, "greet-sbx", "job-1", 1, 1000)
	if base != jobTokenMAC(key, "greet-sbx", "job-1", 1, 1000) {
		t.Fatal("MAC is not deterministic")
	}
	for name, other := range map[string]string{
		"tool":    jobTokenMAC(key, "rsync-sbx", "job-1", 1, 1000),
		"job":     jobTokenMAC(key, "greet-sbx", "job-2", 1, 1000),
		"attempt": jobTokenMAC(key, "greet-sbx", "job-1", 2, 1000),
		"time":    jobTokenMAC(key, "greet-sbx", "job-1", 1, 1001),
		"key":     jobTokenMAC([]byte("another-key-0123456789"), "greet-sbx", "job-1", 1, 1000),
	} {
		if other == base {
			t.Errorf("changing the %s did not change the MAC", name)
		}
	}
}

// The receiver (Python) must build the same string. This pins the format and one
// vector so a change on either side fails a test on that side.
func TestJobTokenKnownVector(t *testing.T) {
	if got := jobTokenMessage("greet-sbx", "job-1", 1, 1000); got != "greet-sbx\njob-1\n1\n1000" {
		t.Fatalf("message format changed: %q", got)
	}
	// Computed independently with Python's hmac; harness/broker/test_broker_queue.py
	// asserts the same value, so the two implementations cannot drift apart.
	const want = "c9cc4c00384942a57393589f4d4b0e1af71045787e690e648ed8357aee7d5ff1"
	got := jobTokenMAC([]byte("0123456789abcdef-secret"), "greet-sbx", "job-1", 1, 1000)
	if got != want {
		t.Fatalf("MAC = %s, want %s", got, want)
	}
}

func TestJobTokenEnvReadsTheKeyPerJobAndRefusesABadOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.key")
	if _, err := jobTokenEnv(path, "greet-sbx", "j", 1, time.Unix(1000, 0)); err == nil {
		t.Fatal("a missing key file was accepted")
	}
	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := jobTokenEnv(path, "greet-sbx", "j", 1, time.Unix(1000, 0)); err == nil {
		t.Fatal("a too-short key was accepted")
	}
	if err := os.WriteFile(path, []byte("0123456789abcdef-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := jobTokenEnv(path, "greet-sbx", "j", 1, time.Unix(1000, 0))
	if err != nil || len(env) != 2 || env[0] != "DESKBOX_JOB_TS=1000" || !strings.HasPrefix(env[1], "DESKBOX_JOB_MAC=") {
		t.Fatalf("env = %v, err = %v", env, err)
	}
	// A key rewritten by the receiver is picked up on the next job.
	if err := os.WriteFile(path, []byte("a-different-key-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env2, _ := jobTokenEnv(path, "greet-sbx", "j", 1, time.Unix(1000, 0))
	if env2[1] == env[1] {
		t.Fatal("the key was cached across jobs")
	}
}
