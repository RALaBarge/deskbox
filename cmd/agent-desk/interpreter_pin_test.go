package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deskbox/agent-desk/internal/tcs"
)

// interpPinnedTool writes a shell tool whose tcs.yaml pins interpreter_sha256
// to the given value. The shebang is /bin/sh so the interpreter resolves to a
// real, stable file on any Linux — the test controls the hash by choosing to
// pin the true value or a wrong one, touching no system files.
func interpPinnedTool(t *testing.T, interpHash string) (*Tool, string) {
	t.Helper()
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "tools", "echo")
	if err := os.MkdirAll(toolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(toolDir, "run.sh"), "#!/bin/sh\necho '{\"ok\":true}'\n")
	if err := os.Chmod(filepath.Join(toolDir, "run.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(toolDir, "tcs.yaml"), `
name: echo
input: {type: object}
output: {schema: {type: object}}
allowed_side_effects: {files: [], network: false}
execution: {mode: queued, max_retries: 0, timeout_ms: 5000}
integrity:
  interpreter_sha256: "`+interpHash+`"
`)
	tools, err := LoadTools(filepath.Join(dir, "tools"))
	return firstTool(t, tools, err), filepath.Join(dir, "data")
}

func firstTool(t *testing.T, tools map[string]*Tool, err error) *Tool {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return tools["echo"]
}

// shHash is the true hash of the interpreter /bin/sh resolves to on this box,
// or a skip if /bin/sh isn't resolvable on the sandbox PATH (it always is on a
// normal Linux, but the test says so rather than asserting it).
func shHash(t *testing.T) string {
	t.Helper()
	path, err := tcs.LookPathIn("/bin/sh", sandboxPath)
	if err != nil {
		t.Skipf("/bin/sh not resolvable on sandbox PATH: %v", err)
	}
	sum, err := tcs.HashFile(path)
	if err != nil {
		t.Fatalf("hash /bin/sh: %v", err)
	}
	return sum
}

// TestInterpreterPinMatchesLoads: a tool pinned to the interpreter actually on
// disk loads and runs — the check is invisible when nothing is wrong.
func TestInterpreterPinMatchesLoads(t *testing.T) {
	tool, dataDir := interpPinnedTool(t, shHash(t))
	if tool == nil {
		t.Fatal("a correctly interpreter-pinned tool must load")
	}
	d := NewDesk(map[string]*Tool{tool.Name: tool}, NewQueue(1, nil), dataDir, &Settings{}, false, false)
	if _, err := d.Execute(context.Background(), tool, &Job{ID: "job-interp-ok"}, map[string]any{}); err != nil {
		t.Fatalf("an unchanged interpreter-pinned tool must run: %v", err)
	}
}

// TestInterpreterPinMismatchRefusesToLoad: a tool pinned to a different
// interpreter than the one on disk must not load — the same "only advertise
// tools we can stand behind" rule the run-script pin follows.
func TestInterpreterPinMismatchRefusesToLoad(t *testing.T) {
	wrong := strings.Repeat("00", 32) // a valid-shaped hash that is not /bin/sh's
	tool, _ := interpPinnedTool(t, wrong)
	if tool != nil {
		t.Fatal("a tool pinned to the wrong interpreter hash must be refused at load")
	}
}

// TestInterpreterPinMismatchRefusedMidFlight: the load-time check has a hole a
// long-running desk must also close per job — verifyIntegrity is what Execute
// calls, so exercise it directly with a mismatch and confirm the actionable
// refusal.
func TestInterpreterPinMismatchRefusedMidFlight(t *testing.T) {
	// Build a valid tool, then swap in a wrong interpreter pin on the loaded
	// struct so we're testing verifyIntegrity, not LoadTools' skip.
	tool, _ := interpPinnedTool(t, shHash(t))
	if tool == nil {
		t.Fatal("precondition: correctly pinned tool must load")
	}
	tool.Integrity.InterpreterSHA256 = strings.Repeat("ab", 32)
	err := verifyIntegrity(tool)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("a changed interpreter must be refused, got %v", err)
	}
	for _, want := range []string{"interpreter", "pinned:", "on disk:", "-pin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q:\n%v", want, err)
		}
	}
}
