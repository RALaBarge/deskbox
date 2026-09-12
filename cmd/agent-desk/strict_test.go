package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// strictTool builds a minimal, working tool on disk so these tests fail for
// the reason under test rather than because there was nothing to run.
func strictTool(t *testing.T) (*Tool, string) {
	t.Helper()
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "tools", "echo")
	if err := os.MkdirAll(toolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(toolDir, "tcs.yaml"), `
name: echo
input: {type: object}
output: {schema: {type: object}}
allowed_side_effects: {files: [], network: false}
execution: {mode: queued, max_retries: 0, timeout_ms: 5000}
`)
	writeFile(t, filepath.Join(toolDir, "run.sh"), "#!/bin/sh\necho '{\"ok\":true}'\n")
	if err := os.Chmod(filepath.Join(toolDir, "run.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	tools, err := LoadTools(filepath.Join(dir, "tools"))
	if err != nil {
		t.Fatal(err)
	}
	tool := tools["echo"]
	if tool == nil {
		t.Fatal("tool did not load")
	}
	return tool, filepath.Join(dir, "data")
}

func strictDesk(t *testing.T, dataDir string, strict, resourceLimitsOK bool) *Desk {
	t.Helper()
	return NewDesk(nil, NewQueue(1, nil), dataDir,
		&Settings{Strict: strict, JobMemoryMax: "512M", JobTasksMax: 64},
		false, resourceLimitsOK)
}

// TestStrictRefusesWhenResourceLimitsGone is the half a startup-only check
// cannot cover: resourceLimitsOK starts true and flips false mid-life when
// the user D-Bus session goes away, so a desk that only checked at boot
// would pass -strict and then run jobs uncapped anyway.
func TestStrictRefusesWhenResourceLimitsGone(t *testing.T) {
	tool, dataDir := strictTool(t)
	d := strictDesk(t, dataDir, true, true)
	d.resourceLimitsOK.Store(false) // the mid-run flip

	job := &Job{ID: "job-strict-limits", Tool: tool.Name}
	_, err := d.Execute(context.Background(), tool, job, map[string]any{})
	if !errors.Is(err, ErrEnforcement) {
		t.Fatalf("strict must refuse the job when per-job caps are gone, got %v", err)
	}
}

// TestStrictRefusesWithoutSandbox covers the other mechanism. bwrap is
// absent in most CI containers, which is exactly the condition being
// asserted; when it is present the refusal cannot be provoked this way, so
// the test says so rather than passing vacuously.
func TestStrictRefusesWithoutSandbox(t *testing.T) {
	if hasBwrap() {
		t.Skip("bwrap is installed here, so bakeSandbox succeeds and there is nothing to refuse")
	}
	tool, dataDir := strictTool(t)
	d := strictDesk(t, dataDir, true, true)

	job := &Job{ID: "job-strict-sandbox", Tool: tool.Name}
	_, err := d.Execute(context.Background(), tool, job, map[string]any{})
	if !errors.Is(err, ErrEnforcement) {
		t.Fatalf("strict must refuse to run a tool unsandboxed, got %v", err)
	}
}

// TestNonStrictStillRunsDegraded guards the other direction: without
// -strict the desk is explicitly a fail-open system, and a bug that made
// degraded enforcement fatal everywhere would break every host without
// bubblewrap or a user D-Bus session.
func TestNonStrictStillRunsDegraded(t *testing.T) {
	tool, dataDir := strictTool(t)
	d := strictDesk(t, dataDir, false, false)

	job := &Job{ID: "job-degraded", Tool: tool.Name}
	result, err := d.Execute(context.Background(), tool, job, map[string]any{})
	if err != nil {
		t.Fatalf("without -strict a degraded desk must still run jobs, got %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok || m["ok"] != true {
		t.Fatalf("unexpected result %#v", result)
	}
}

// TestEnforcementStatus checks the shape GET / reports, since that block is
// how an operator (or an agent deciding whether to trust the desk) learns
// the guarantees are downgraded.
func TestEnforcementStatus(t *testing.T) {
	d := strictDesk(t, t.TempDir(), false, false)
	e := d.enforcement()
	if e["degraded"] != true {
		t.Errorf("a desk with neither mechanism must report degraded: %#v", e)
	}
	warnings, ok := e["warnings"].([]string)
	if !ok || len(warnings) < 2 {
		t.Errorf("expected a warning per missing mechanism, got %#v", e["warnings"])
	}

	d = strictDesk(t, t.TempDir(), false, true)
	d.sandboxOK = true
	d.settings.AuthEnabled = true
	e = d.enforcement()
	// Running as root is itself a downgrade — the mechanisms are all
	// present but everything inside the sandbox is reached as uid 0 — so
	// on a root test runner the fully-enforcing case is the one warning
	// that remains, not zero.
	wantDegraded := runningAsRoot()
	if e["degraded"] != wantDegraded {
		t.Errorf("with every mechanism present, degraded should track root (%v): %#v", wantDegraded, e)
	}
	w, _ := e["warnings"].([]string)
	if wantDegraded && len(w) != 1 {
		t.Errorf("expected exactly the root warning, got %#v", w)
	}
	if !wantDegraded && len(w) != 0 {
		t.Errorf("expected no warnings, got %#v", w)
	}
}
