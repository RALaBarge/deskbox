package tcs

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
			got, ok := ShebangInterpreter(p)
			if ok != tc.ok || got != tc.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestLookPathInIgnoresHostPath is the point of having a custom lookup at all:
// resolving against the operator's PATH would "find" a runtime in ~/bin that
// no job can ever reach, and report a box as ready when it is not.
func TestLookPathInIgnoresHostPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "only-here")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir) // on the process's PATH, but not the desk's

	if _, err := LookPathIn("only-here", SandboxPath); err == nil {
		t.Error("a binary outside the desk's PATH must not resolve")
	}
	got, err := LookPathIn("only-here", dir)
	if err != nil || got != bin {
		t.Errorf("got (%q, %v), want (%q, nil)", got, err, bin)
	}

	// Non-executable files are not commands, however well-named.
	plain := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LookPathIn("not-executable", dir); err == nil {
		t.Error("a non-executable file must not resolve as a command")
	}
}

// TestResolveInterpreter ties the two together: a script's shebang resolved
// against a PATH, which is what the desk and tcs-verify both pin against.
func TestResolveInterpreter(t *testing.T) {
	dir := t.TempDir()
	interp := filepath.Join(dir, "myterp")
	if err := os.WriteFile(interp, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "run")
	if err := os.WriteFile(script, []byte("#!"+interp+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	name, path, ok := ResolveInterpreter(script, dir)
	if !ok || path != interp {
		t.Errorf("resolve got (%q, %q, %v), want (…, %q, true)", name, path, ok, interp)
	}

	// A compiled binary (no shebang) has no interpreter to resolve or pin.
	bin := filepath.Join(dir, "compiled")
	if err := os.WriteFile(bin, []byte("\x7fELF"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ResolveInterpreter(bin, dir); ok {
		t.Error("a binary with no shebang must report no interpreter")
	}
}
