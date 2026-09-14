package tcs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SandboxPath is the PATH every job runs with — the system directories the
// bwrap sandbox binds, and nothing else. It lives here, in the package both
// the daemon and tcs-verify import, so "the PATH a tool's interpreter is
// resolved against" is defined in exactly one place: the desk executes with
// it, preflight resolves against it, and tcs-verify checks interpreter pins
// against it, with no way for the three to disagree.
const SandboxPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Executable reports whether p is a regular (or symlinked-to-regular)
// executable file. Stat, not Lstat: /usr/bin/python3 is normally a symlink.
func Executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// LookPathIn is exec.LookPath against a supplied PATH instead of the process's
// own. A name containing a slash is used as given, exactly as execve treats it.
func LookPathIn(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		if Executable(name) {
			return name, nil
		}
		return "", fmt.Errorf("%s: not an executable file", name)
	}
	for _, dir := range strings.Split(path, ":") {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if Executable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found in PATH", name)
}

// ShebangInterpreter returns the program the kernel will actually exec for a
// script. "#!/usr/bin/env python3" resolves to python3 rather than env, since
// env is never the thing that's missing. A file with no shebang (a compiled
// binary) reports ("", false): it has no interpreter to depend on or pin.
func ShebangInterpreter(runPath string) (string, bool) {
	f, err := os.Open(runPath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "#!") {
		return "", false
	}
	fields := strings.Fields(strings.TrimSpace(line[2:]))
	if len(fields) == 0 {
		return "", false
	}
	interp := fields[0]
	if filepath.Base(interp) == "env" {
		// Skip env's own options ("-S python3 -u") to the first real name.
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "-") && !strings.Contains(f, "=") {
				return f, true
			}
		}
		return "", false
	}
	return interp, true
}

// ResolveInterpreter returns the interpreter a run script will actually be
// executed by, resolved the way a job will find it: the shebang's program,
// looked up against path (normally SandboxPath). A compiled binary has no
// shebang and reports ok=false — interpreter pinning does not apply to it.
// name is the interpreter as written even when it can't be resolved, so a
// caller can report which one was missing.
func ResolveInterpreter(runPath, path string) (name, resolved string, ok bool) {
	interp, has := ShebangInterpreter(runPath)
	if !has {
		return "", "", false
	}
	p, err := LookPathIn(interp, path)
	if err != nil {
		return interp, "", false
	}
	return interp, p, true
}
