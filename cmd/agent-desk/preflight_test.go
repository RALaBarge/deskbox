package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShebangInterpreter(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{"absolute", "#!/usr/bin/perl\nprint 1;\n", "/usr/bin/perl", true},
		{"env", "#!/usr/bin/env python3\nprint(1)\n", "python3", true},
		// env -S is how a script passes options to its interpreter; the
		// dependency is still the interpreter, never env itself.
		{"env with options", "#!/usr/bin/env -S python3 -u\n", "python3", true},
		{"env with assignment", "#!/usr/bin/env FOO=1 ruby\n", "ruby", true},
		{"interpreter with args", "#!/bin/sh -e\n", "/bin/sh", true},
		// A compiled binary has no shebang and needs nothing installed —
		// reporting a bogus dependency for it would be worse than silence.
		{"binary", "\x7fELF\x02\x01\x01\x00", "", false},
		{"empty", "", "", false},
		{"no shebang", "echo hi\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "run")
			if err := os.WriteFile(p, []byte(tc.content), 0o700); err != nil {
				t.Fatal(err)
			}
			got, ok := shebangInterpreter(p)
			if ok != tc.ok || got != tc.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestInsideSandboxRoots guards the prefix trap: a plain strings.HasPrefix
// against "/usr" would accept "/usrlocal/bin/python3" and "/libre/x",
// reporting a path the sandbox cannot see as fine.
func TestInsideSandboxRoots(t *testing.T) {
	in := []string{"/usr/bin/perl", "/usr/local/bin/tcs-shim", "/bin/sh", "/lib64/ld.so", "/usr"}
	out := []string{"/opt/node22/bin/node", "/home/me/bin/x", "/nix/store/x/bin/y",
		"/usrlocal/bin/python3", "/libre/x", "/binary/x", ""}
	for _, p := range in {
		if !insideSandboxRoots(p) {
			t.Errorf("%q should be visible inside the sandbox", p)
		}
	}
	for _, p := range out {
		if insideSandboxRoots(p) {
			t.Errorf("%q is NOT bound into the sandbox but was reported as visible", p)
		}
	}
}

// TestLookPathInIgnoresHostPath is the point of having a custom lookup at
// all: resolving against the operator's PATH would "find" a runtime in
// ~/bin that no job can ever reach, and report a box as ready when it is
// not.
func TestLookPathInIgnoresHostPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "only-here")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir) // on the process's PATH, but not the desk's

	if _, err := lookPathIn("only-here", sandboxPath); err == nil {
		t.Error("a binary outside the desk's PATH must not resolve")
	}
	got, err := lookPathIn("only-here", dir)
	if err != nil || got != bin {
		t.Errorf("got (%q, %v), want (%q, nil)", got, err, bin)
	}

	// Non-executable files are not commands, however well-named.
	plain := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lookPathIn("not-executable", dir); err == nil {
		t.Error("a non-executable file must not resolve as a command")
	}
}

// TestOutsideSandboxSeverityDependsOnSandbox pins the judgement call: the
// same finding is fatal on a host with bubblewrap (the tool genuinely
// cannot see its runtime) and a portability warning on one without (the
// tool runs unsandboxed and the path resolves fine).
func TestOutsideSandboxSeverityDependsOnSandbox(t *testing.T) {
	outside := Dependency{Status: PreflightOutside}
	if !outside.Fatal(true) {
		t.Error("with a sandbox, an unreachable runtime means the tool cannot run")
	}
	if outside.Fatal(false) {
		t.Error("without a sandbox the path resolves normally — warn, don't fail")
	}

	missing := Dependency{Status: PreflightMissing}
	if !missing.Fatal(true) || !missing.Fatal(false) {
		t.Error("a missing dependency is fatal either way")
	}
	for _, s := range []string{PreflightOK, PreflightUnknown} {
		d := Dependency{Status: s}
		if d.Fatal(true) || d.Fatal(false) {
			t.Errorf("%s must not be fatal", s)
		}
	}
}

func TestShimCommand(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if got, ok := shimCommand(write("a.yaml", "exec:\n  - jq\n  - \"{{.filter}}\"\n")); !ok || got != "jq" {
		t.Errorf("got (%q,%v), want (jq,true)", got, ok)
	}
	// A templated command is chosen at run time; there is nothing static to
	// check, and guessing would report a dependency that doesn't exist.
	if _, ok := shimCommand(write("b.yaml", "exec:\n  - \"{{.cmd}}\"\n")); ok {
		t.Error("a templated exec[0] must not be reported as a fixed dependency")
	}
	if _, ok := shimCommand(write("c.yaml", "exec: []\n")); ok {
		t.Error("an empty exec has no command")
	}
	if _, ok := shimCommand(write("d.yaml", "exec:\n  - {spread: paths}\n")); ok {
		t.Error("a spread as exec[0] is not a literal command")
	}
	if _, ok := shimCommand(filepath.Join(dir, "nope.yaml")); ok {
		t.Error("a missing shim.yaml has no command")
	}
}

// TestPreflightFindsToolDependencies drives the whole thing against tools
// on disk, the way -check does.
func TestPreflightFindsToolDependencies(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, run, shim string) {
		d := filepath.Join(dir, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(d, "tcs.yaml"), `
name: `+name+`
input: {type: object}
output: {schema: {type: object}}
allowed_side_effects: {files: [], network: false}
execution: {mode: queued, max_retries: 0, timeout_ms: 5000}
`)
		if err := os.WriteFile(filepath.Join(d, "run.sh"), []byte(run), 0o700); err != nil {
			t.Fatal(err)
		}
		if shim != "" {
			writeFile(t, filepath.Join(d, "shim.yaml"), shim)
		}
	}
	mk("present", "#!/bin/sh\necho '{}'\n", "")
	mk("absent", "#!/usr/bin/env deskbox-no-such-runtime\n", "")
	mk("shimmed", "#!/bin/sh\nexec tcs-shim\n", "exec:\n  - deskbox-no-such-command\noutput:\n  mode: raw\n")

	tools, err := LoadTools(dir)
	if err != nil {
		t.Fatal(err)
	}
	deps := Preflight(tools)

	byTool := map[string][]Dependency{}
	for _, d := range deps {
		byTool[d.Tool] = append(byTool[d.Tool], d)
	}
	if got := byTool["present"]; len(got) != 1 || got[0].Status != PreflightOK {
		t.Errorf("present: %#v", got)
	}
	if got := byTool["absent"]; len(got) != 1 || got[0].Status != PreflightMissing ||
		got[0].Name != "deskbox-no-such-runtime" {
		t.Errorf("absent: %#v", got)
	}
	// A shimmed tool declares three things: its shell, the adapter, and the
	// command being wrapped.
	shimmed := byTool["shimmed"]
	if len(shimmed) != 3 {
		t.Fatalf("shimmed: expected interpreter+shim+command, got %#v", shimmed)
	}
	var sawCommand bool
	for _, d := range shimmed {
		if d.Kind == "command" {
			sawCommand = true
			if d.Status != PreflightMissing {
				t.Errorf("the wrapped command is not installed: %#v", d)
			}
		}
	}
	if !sawCommand {
		t.Error("the wrapped command must be checked, not just the shim")
	}

	// Output is sorted by tool so -check reads the same way twice.
	if deps[0].Tool != "absent" {
		t.Errorf("expected deterministic tool ordering, got %q first", deps[0].Tool)
	}
}

// TestLoadToolsMissingDirIsNotFatal covers the first thing someone does
// with a release: unpack the tarball and run the binary. There is no
// tools/ directory yet, and exiting before printing anything useful is a
// bad first thirty seconds. An unreadable directory is still an error —
// that one is a misconfiguration, not an empty starting point.
func TestLoadToolsMissingDirIsNotFatal(t *testing.T) {
	tools, err := LoadTools(filepath.Join(t.TempDir(), "tools"))
	if err != nil {
		t.Fatalf("a missing tools dir must not be fatal on a fresh install: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected no tools, got %d", len(tools))
	}

	notADir := filepath.Join(t.TempDir(), "tools")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTools(notADir); err == nil {
		t.Error("a tools path that is not a directory is a misconfiguration and must error")
	}
}
