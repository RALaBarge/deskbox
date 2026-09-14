package main

import (
	"errors"
	"fmt"
	"strings"

	"deskbox/agent-desk/internal/tcs"
)

// ErrIntegrity marks a tool whose run script — or the interpreter that runs
// it — is not the one that was vetted. Permanent, never retried: the file on
// disk will not change between attempts, and retrying a tool you no longer
// recognise is the opposite of what this check is for.
var ErrIntegrity = errors.New("integrity check failed")

// Tool integrity: making "I verified this tool" survive the moment you
// verified it.
//
// The desk already refuses to load a run script that other accounts can
// write, but that says nothing about the file's *contents* — a tool read
// last month is whatever is on disk today, whether it changed by an edit,
// a bad merge, a sync, or something that got in. Pinning the hash turns
// vetting from a memory into a check.
//
// Two things can be pinned, both opt-in per tool:
//
//   - run_sha256: the run script itself. Author-controlled; changes only
//     when the tool does, so a mismatch means the tool changed.
//   - interpreter_sha256: the interpreter the script's shebang names
//     (python3, /bin/sh, …), resolved against the sandbox PATH. NOT
//     author-controlled — a `dnf upgrade python3` changes it — so it goes
//     stale on a routine system update, by design: the whole point is that
//     "what actually executes my tool" includes the interpreter, and a
//     changed interpreter is worth re-vetting even though the script didn't
//     move. `-pin` reprints the current value to paste after such an update.
//
// A pin the desk wrote for itself would be worthless (it would just re-pin
// whatever it found), so the operator adds it deliberately after reading,
// and `-pin` only prints the block.

// IntegritySpec is the optional `integrity:` block in tcs.yaml.
type IntegritySpec struct {
	// RunSHA256 is the hex SHA-256 of the tool's run script as vetted.
	RunSHA256 string `yaml:"run_sha256,omitempty" json:"run_sha256,omitempty"`
	// InterpreterSHA256 is the hex SHA-256 of the interpreter the run
	// script's shebang names, resolved against the sandbox PATH.
	InterpreterSHA256 string `yaml:"interpreter_sha256,omitempty" json:"interpreter_sha256,omitempty"`
}

func (i IntegritySpec) runPinned() bool    { return strings.TrimSpace(i.RunSHA256) != "" }
func (i IntegritySpec) interpPinned() bool { return strings.TrimSpace(i.InterpreterSHA256) != "" }

// resolveInterpreter returns the interpreter a tool's run script will actually
// be executed by, resolved the same way a job will find it and the same way
// tcs-verify checks it: shared in internal/tcs so the daemon and the offline
// linter can't resolve it differently.
func resolveInterpreter(runPath string) (name, path string, ok bool) {
	return tcs.ResolveInterpreter(runPath, sandboxPath)
}

// verifyIntegrity checks a tool's run script, and the interpreter that runs
// it, against their pins. An unpinned tool passes: pinning is opt-in, and
// refusing to run everything that hasn't adopted it yet would just get the
// check turned off.
func verifyIntegrity(t *Tool) error {
	if t.Integrity.runPinned() {
		want := strings.ToLower(strings.TrimSpace(t.Integrity.RunSHA256))
		got, err := tcs.HashFile(t.runPath)
		if err != nil {
			return fmt.Errorf("%w: cannot hash %s: %v", ErrIntegrity, t.runPath, err)
		}
		if got != want {
			return fmt.Errorf("%w: %s has changed since it was vetted\n  pinned:  %s\n  on disk: %s\n"+
				"  Read the script. If the change is yours and you have reviewed it, update "+
				"integrity.run_sha256 in tcs.yaml (agent-desk -pin prints the new value).",
				ErrIntegrity, t.runPath, want, got)
		}
	}

	if t.Integrity.interpPinned() {
		want := strings.ToLower(strings.TrimSpace(t.Integrity.InterpreterSHA256))
		name, path, ok := resolveInterpreter(t.runPath)
		if !ok {
			return fmt.Errorf("%w: %s pins an interpreter hash, but the interpreter (%s) could not be "+
				"resolved on the sandbox PATH to hash it", ErrIntegrity, t.runPath, orDefault(name, "none — no shebang"))
		}
		got, err := tcs.HashFile(path)
		if err != nil {
			return fmt.Errorf("%w: cannot hash interpreter %s: %v", ErrIntegrity, path, err)
		}
		if got != want {
			return fmt.Errorf("%w: the interpreter %s (%s) has changed since %s was vetted\n  pinned:  %s\n  on disk: %s\n"+
				"  Often this is a routine package upgrade. If you trust the new interpreter, update "+
				"integrity.interpreter_sha256 in tcs.yaml (agent-desk -pin prints the new value).",
				ErrIntegrity, name, path, t.runPath, want, got)
		}
	}
	return nil
}

// FormatPins prints a paste-ready integrity block per tool, so adopting a
// pin costs one copy. Deliberately print-only: a flag that edited tcs.yaml
// would re-pin whatever happens to be on disk, which is exactly the thing
// the pin exists to catch.
func FormatPins(tools map[string]*Tool) string {
	var b strings.Builder
	for _, name := range sortedToolNames(tools) {
		t := tools[name]
		sum, err := tcs.HashFile(t.runPath)
		if err != nil {
			fmt.Fprintf(&b, "# %s: cannot hash %s: %v\n\n", name, t.runPath, err)
			continue
		}
		state := "currently unpinned"
		switch {
		case t.Integrity.runPinned() && strings.EqualFold(t.Integrity.RunSHA256, sum):
			state = "already pinned to this value"
		case t.Integrity.runPinned():
			state = "PINNED TO A DIFFERENT VALUE — read the script before pasting this"
		}
		fmt.Fprintf(&b, "# %s (%s) — %s\nintegrity:\n  run_sha256: %q\n", name, t.runPath, state, sum)

		// The interpreter line only makes sense for a script with a shebang
		// whose interpreter resolves on the sandbox PATH; a compiled binary
		// vendored as run has none to pin.
		if iname, ipath, ok := resolveInterpreter(t.runPath); ok {
			if isum, err := tcs.HashFile(ipath); err == nil {
				fmt.Fprintf(&b, "  interpreter_sha256: %q  # %s -> %s\n", isum, iname, ipath)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
