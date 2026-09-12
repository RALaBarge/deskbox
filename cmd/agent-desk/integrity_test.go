package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedTool writes a working tool, optionally with an integrity pin
// matching its script as written.
func pinnedTool(t *testing.T, pin bool) (map[string]*Tool, string, string) {
	t.Helper()
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "tools", "echo")
	if err := os.MkdirAll(toolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(toolDir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '{\"ok\":true}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tcs := `
name: echo
input: {type: object}
output: {schema: {type: object}}
allowed_side_effects: {files: [], network: false}
execution: {mode: queued, max_retries: 0, timeout_ms: 5000}
`
	if pin {
		sum, err := hashFile(script)
		if err != nil {
			t.Fatal(err)
		}
		tcs += "integrity:\n  run_sha256: \"" + sum + "\"\n"
	}
	writeFile(t, filepath.Join(toolDir, "tcs.yaml"), tcs)

	tools, err := LoadTools(filepath.Join(dir, "tools"))
	if err != nil {
		t.Fatal(err)
	}
	return tools, script, filepath.Join(dir, "data")
}

// TestPinnedToolRunsWhenUnchanged: the check must be invisible when
// nothing is wrong, or it would just get removed.
func TestPinnedToolRunsWhenUnchanged(t *testing.T) {
	tools, _, dataDir := pinnedTool(t, true)
	tool := tools["echo"]
	if tool == nil {
		t.Fatal("a correctly-pinned tool must load")
	}
	d := NewDesk(tools, NewQueue(1, nil), dataDir, &Settings{}, false, false)
	if _, err := d.Execute(context.Background(), tool, &Job{ID: "job-pin-ok"}, map[string]any{}); err != nil {
		t.Fatalf("an unchanged pinned tool must run: %v", err)
	}
}

// TestChangedScriptIsRefusedMidFlight is the case a load-time-only check
// misses entirely, and the one that matters: the desk is a long-running
// process, so a script edited an hour after startup would otherwise run
// with the desk still believing it was vetted.
func TestChangedScriptIsRefusedMidFlight(t *testing.T) {
	tools, script, dataDir := pinnedTool(t, true)
	tool := tools["echo"]
	d := NewDesk(tools, NewQueue(1, nil), dataDir, &Settings{}, false, false)

	if _, err := d.Execute(context.Background(), tool, &Job{ID: "job-a"}, map[string]any{}); err != nil {
		t.Fatalf("precondition: the tool should run before it changes: %v", err)
	}

	// Same length, different bytes — a check comparing size or mtime would
	// sail past this.
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '{\"ok\":fals}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := d.Execute(context.Background(), tool, &Job{ID: "job-b"}, map[string]any{})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("a changed script must be refused, got %v", err)
	}
	// The message has to be actionable: both hashes and what to do next.
	for _, want := range []string{"pinned:", "on disk:", "-pin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q:\n%v", want, err)
		}
	}
}

// TestChangedScriptRefusesToLoad covers the other layer: a tool the desk
// cannot stand behind should never appear in GET /tools at all.
func TestChangedScriptRefusesToLoad(t *testing.T) {
	tools, script, _ := pinnedTool(t, true)
	toolsDir := filepath.Dir(filepath.Dir(script))

	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '{\"evil\":true}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadTools(toolsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 0 {
		t.Errorf("a tool whose script changed must not load, got %v", reloaded)
	}
	_ = tools
}

// TestUnpinnedToolStillRuns: pinning is opt-in. Refusing everything that
// has not adopted it would get the whole check switched off.
func TestUnpinnedToolStillRuns(t *testing.T) {
	tools, script, dataDir := pinnedTool(t, false)
	tool := tools["echo"]
	if tool == nil {
		t.Fatal("an unpinned tool must load")
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '{\"ok\":true,\"changed\":1}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := NewDesk(tools, NewQueue(1, nil), dataDir, &Settings{}, false, false)
	if _, err := d.Execute(context.Background(), tool, &Job{ID: "job-unpinned"}, map[string]any{}); err != nil {
		t.Fatalf("an unpinned tool must run regardless of changes: %v", err)
	}
}

// TestFormatPinsNeverEditsAnything: a -pin that rewrote tcs.yaml would
// re-pin whatever happens to be on disk, which is precisely what the pin
// exists to catch.
func TestFormatPinsNeverEditsAnything(t *testing.T) {
	tools, script, _ := pinnedTool(t, false)
	tcsPath := filepath.Join(filepath.Dir(script), "tcs.yaml")
	before, err := os.ReadFile(tcsPath)
	if err != nil {
		t.Fatal(err)
	}

	out := FormatPins(tools)
	if !strings.Contains(out, "run_sha256") || !strings.Contains(out, "unpinned") {
		t.Errorf("expected a paste-ready block marked unpinned, got:\n%s", out)
	}
	after, err := os.ReadFile(tcsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("-pin must only print; it must never edit tcs.yaml")
	}
}
