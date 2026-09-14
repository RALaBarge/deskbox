package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"deskbox/agent-desk/internal/tcs"
	"gopkg.in/yaml.v3"
)

// Preflight answers the one question that decides whether a desk someone
// just downloaded will work on their box: does this machine actually have
// the things the tools need, and can the sandbox see them?
//
// Both halves matter, and the second is the one that surprises people. The
// sandbox binds /usr, /bin, /sbin, /lib and /lib64 and nothing else, so a
// runtime installed under /opt, /nix or a home directory is plainly present
// on the host and simply does not exist inside the sandbox. Without this
// check that shows up as a confusing ENOENT on the first job, long after
// install; with it, it is one line at startup.
//
// Everything here is read from declared data — a script's shebang, a
// shim.yaml's exec — never by interpreting the body of a shell script.
// Guessing at shell semantics would produce confident wrong answers, which
// is worse than no check at all.

// Severity of a preflight finding.
const (
	PreflightOK      = "ok"
	PreflightMissing = "missing"         // not on the box at all: the tool cannot run
	PreflightOutside = "outside_sandbox" // present, but not on a path the sandbox binds
	PreflightUnknown = "unknown"         // templated/dynamic: nothing static to resolve
)

// sandboxRoots are the prefixes bakeSandbox binds into the sandbox. A
// dependency resolving outside all of them is invisible to a sandboxed tool.
var sandboxRoots = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"}

// Dependency is one thing a tool needs, and whether this box has it.
type Dependency struct {
	Tool   string `json:"tool"`
	Kind   string `json:"kind"`             // interpreter | command | shim
	Name   string `json:"name"`             // as written: "python3", "jq", "tcs-shim"
	Path   string `json:"path,omitempty"`   // where it resolved, if it did
	Status string `json:"status"`           // ok | missing | outside_sandbox | unknown
	Detail string `json:"detail,omitempty"` // what to do about it
}

// Fatal reports whether this finding means the tool cannot run as configured.
// A dependency outside the sandbox roots only breaks things once bubblewrap
// is actually in use — on a host without it the tool runs unsandboxed and
// the path resolves fine — so the same finding is an error there and a
// portability warning here, and saying so beats picking one and being wrong
// half the time.
func (d Dependency) Fatal(sandboxed bool) bool {
	switch d.Status {
	case PreflightMissing:
		return true
	case PreflightOutside:
		return sandboxed
	}
	return false
}

// Preflight resolves every declared dependency of every loaded tool.
func Preflight(tools map[string]*Tool) []Dependency {
	var out []Dependency
	for _, n := range sortedToolNames(tools) {
		out = append(out, toolDependencies(tools[n])...)
	}
	return out
}

// sortedToolNames keeps every report — preflight, pins — in a stable order
// so the same box prints the same thing twice.
func sortedToolNames(tools map[string]*Tool) []string {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func toolDependencies(t *Tool) []Dependency {
	var deps []Dependency

	// The run script's shebang. A compiled binary has none, and needs
	// nothing — that is the whole argument for vendoring a static binary.
	if interp, ok := tcs.ShebangInterpreter(t.runPath); ok {
		deps = append(deps, resolveDep(t.Name, "interpreter", interp))
	}

	// A shim.yaml means two more: the adapter itself, and the command it
	// wraps. Both are declared, so neither is a guess.
	shimPath := filepath.Join(t.dir, "shim.yaml")
	if _, err := os.Stat(shimPath); err == nil {
		deps = append(deps, resolveDep(t.Name, "shim", "tcs-shim"))
		if cmd, ok := shimCommand(shimPath); ok {
			deps = append(deps, resolveDep(t.Name, "command", cmd))
		} else {
			deps = append(deps, Dependency{
				Tool: t.Name, Kind: "command", Name: "(templated)",
				Status: PreflightUnknown,
				Detail: "shim.yaml's exec[0] is rendered from the input document, so " +
					"there is no fixed command to check for here",
			})
		}
	}
	return deps
}

// resolveDep looks a dependency up the way the tool will actually find it:
// against sandboxPath, the PATH the desk hands every job, rather than the
// PATH of whatever shell started the desk. Checking the operator's PATH
// would happily "find" something in ~/bin that no job can ever reach.
func resolveDep(tool, kind, name string) Dependency {
	d := Dependency{Tool: tool, Kind: kind, Name: name}
	path, err := tcs.LookPathIn(name, sandboxPath)
	if err != nil {
		d.Status = PreflightMissing
		d.Detail = fmt.Sprintf("not found on the desk's PATH (%s)", sandboxPath)
		if kind == "shim" {
			d.Detail += "; build it with: go build -o /usr/local/bin/tcs-shim ./cmd/tcs-shim"
		}
		return d
	}
	d.Path = path
	if !insideSandboxRoots(path) {
		d.Status = PreflightOutside
		d.Detail = "installed outside the paths the sandbox binds (" +
			strings.Join(sandboxRoots, ", ") + "), so it does not exist inside " +
			"the sandbox even though it is on this box — see the path trap in " +
			"examples/tools/README.md"
		return d
	}
	d.Status = PreflightOK
	return d
}

func insideSandboxRoots(p string) bool {
	for _, root := range sandboxRoots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

// shimCommand reads exec[0] out of a shim.yaml. Only a plain string literal
// counts: a templated first element is chosen by the caller at run time and
// there is nothing static to check.
func shimCommand(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var cfg struct {
		Exec []any `yaml:"exec"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil || len(cfg.Exec) == 0 {
		return "", false
	}
	s, ok := cfg.Exec[0].(string)
	if !ok || s == "" || strings.Contains(s, "{{") {
		return "", false
	}
	return s, true
}

// FormatPreflight renders the findings for a terminal — the output of
// -check, and what gets logged at startup when something is wrong.
func FormatPreflight(deps []Dependency, sandboxed bool) string {
	// Column widths come from the data: an interpreter path like
	// /usr/bin/perl is longer than any fixed guess, and a ragged table is
	// harder to scan than a wide one.
	nameW := 0
	for _, d := range deps {
		if len(d.Name) > nameW {
			nameW = len(d.Name)
		}
	}
	var b strings.Builder
	tool := ""
	for _, d := range deps {
		if d.Tool != tool {
			fmt.Fprintf(&b, "%s\n", d.Tool)
			tool = d.Tool
		}
		where := d.Path
		if where == "" {
			where = "NOT FOUND"
		}
		mark := "ok"
		if d.Fatal(sandboxed) {
			mark = "FAIL"
		} else if d.Status != PreflightOK {
			mark = "warn"
		}
		fmt.Fprintf(&b, "  %-4s %-11s %-*s  %s\n", mark, d.Kind, nameW, d.Name, where)
		if d.Detail != "" {
			fmt.Fprintf(&b, "       %s\n", wrapDetail(d.Detail, 68, "       "))
		}
	}
	return b.String()
}

func wrapDetail(s string, width int, indent string) string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		if line == "" {
			line = w
		} else {
			line += " " + w
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"+indent)
}

// PreflightProblems returns every finding that isn't a clean resolve.
func PreflightProblems(deps []Dependency) []Dependency {
	var out []Dependency
	for _, d := range deps {
		if d.Status != PreflightOK {
			out = append(out, d)
		}
	}
	return out
}
