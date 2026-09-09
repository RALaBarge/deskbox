package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Tool is a loaded tool: its contract (tcs.yaml) plus where it lives on disk.
// The contract, the implementation, and the skill-like notes for agents all
// live in the same folder — "a space the agent can access".
type Tool struct {
	Name               string        `yaml:"name"`
	Summary            string        `yaml:"summary,omitempty"`
	Input              *Schema       `yaml:"input,omitempty"` // what agents may send
	Output             OutputSpec    `yaml:"output"`          // what agents get back
	AllowedSideEffects SideEffects   `yaml:"allowed_side_effects"`
	Sandbox            SandboxSpec   `yaml:"sandbox,omitempty"`
	Execution          ExecutionSpec `yaml:"execution"`

	dir     string // on-disk location
	runPath string // resolved executable
	raw     []byte // original tcs.yaml bytes (served at GET /tools/{name})
}

type OutputSpec struct {
	Schema Schema `yaml:"schema,omitempty"`
}

type SideEffects struct {
	Files   []string `yaml:"files" json:"files"`     // empty = no file writes allowed
	Network bool     `yaml:"network" json:"network"` // true = egress allowed
}

// SandboxSpec declares the per-job workspace surface. Inside the bwrap layer
// the tool sees ONLY the files in `in` (read-only, one by one) and the `out`
// dir (writable, host-tailable). If empty, the desk materializes input.json
// in in/ and accepts stdout as the result channel.
type SandboxSpec struct {
	In  []string `yaml:"in"  json:"in"`  // exact files the pull needs (ro)
	Out []string `yaml:"out" json:"out"` // files the tool may write (rw, tail-able)
}

type ExecutionSpec struct {
	Mode       string `yaml:"mode"` // "queued" (default) | "direct"
	MaxRetries int    `yaml:"max_retries"`
	TimeoutMS  int    `yaml:"timeout_ms"`
}

func (e ExecutionSpec) IsQueued() bool { return e.Mode == "" || e.Mode == "queued" }

func (e ExecutionSpec) Timeout() time.Duration {
	if e.TimeoutMS <= 0 {
		return 30 * time.Second
	}
	return time.Duration(e.TimeoutMS) * time.Millisecond
}

// LoadTools scans dir for tool folders. A folder is a tool iff it contains a
// tcs.yaml and an executable run script.
//
// A folder that has a tcs.yaml but fails to load (bad YAML, no executable
// run script yet, a duplicate tool name) is logged and skipped rather than
// failing the whole load: one WIP tool folder — a contract added before
// run.sh is chmod +x, a YAML typo — must not make every other tool
// unreachable, especially across a restart meant to pick up new tools.
func LoadTools(dir string) (map[string]*Tool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("abs %s: %w", dir, err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	tools := map[string]*Tool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		tdir := filepath.Join(abs, name)
		raw, err := os.ReadFile(filepath.Join(tdir, "tcs.yaml"))
		if err != nil {
			continue // folder without a contract is not a tool
		}
		var t Tool
		if err := yaml.Unmarshal(raw, &t); err != nil {
			log.Printf("WARN: skipping tool folder %q: bad tcs.yaml: %v", name, err)
			continue
		}
		t.dir = tdir
		t.raw = raw
		if t.Name == "" {
			t.Name = name
		}
		run, err := resolveRunScript(tdir)
		if err != nil {
			log.Printf("WARN: skipping tool folder %q: %v", name, err)
			continue
		}
		t.runPath = run
		if _, dup := tools[t.Name]; dup {
			log.Printf("WARN: skipping tool folder %q: name %q already loaded from another folder", name, t.Name)
			continue
		}
		tools[t.Name] = &t
	}
	return tools, nil
}

// AsJSON re-decodes the tool's raw tcs.yaml generically (not through the Tool
// struct, so no field the struct doesn't know about is silently dropped) for
// serving over the API. Authoring stays YAML (tcs.yaml on disk); everything
// an agent reads or sends over the wire — this included — is JSON, no
// exceptions to remember.
func (t *Tool) AsJSON() (map[string]any, error) {
	var generic map[string]any
	if err := yaml.Unmarshal(t.raw, &generic); err != nil {
		return nil, err
	}
	return generic, nil
}

func resolveRunScript(dir string) (string, error) {
	for _, cand := range []string{"run.sh", "run.py", "run"} {
		p := filepath.Join(dir, cand)
		fi, err := os.Stat(p)
		if err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("no executable run script (run.sh/run.py/run) found")
}
