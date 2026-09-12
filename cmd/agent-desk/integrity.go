package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrIntegrity marks a tool whose run script is not the one that was
// vetted. Permanent, never retried: the file on disk will not change
// between attempts, and retrying a tool you no longer recognise is the
// opposite of what this check is for.
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
// It is opt-in per tool. A pin that the desk wrote for itself would be
// worthless (it would just re-pin whatever it found), so the operator adds
// it deliberately after reading the script, and `-pin` only prints the
// block to paste.

// IntegritySpec is the optional `integrity:` block in tcs.yaml.
type IntegritySpec struct {
	// RunSHA256 is the hex SHA-256 of the tool's run script as vetted.
	RunSHA256 string `yaml:"run_sha256,omitempty" json:"run_sha256,omitempty"`
}

func (i IntegritySpec) pinned() bool { return strings.TrimSpace(i.RunSHA256) != "" }

// hashFile returns the hex SHA-256 of a file, streaming rather than
// reading it whole: a tool may legitimately vendor a large static binary
// as its run target, and the hash should not be the thing that needs it
// all in memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyIntegrity checks a tool's run script against its pin. An unpinned
// tool passes: pinning is opt-in, and refusing to run everything that
// hasn't adopted it yet would just get the check turned off.
func verifyIntegrity(t *Tool) error {
	if !t.Integrity.pinned() {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(t.Integrity.RunSHA256))
	got, err := hashFile(t.runPath)
	if err != nil {
		return fmt.Errorf("%w: cannot hash %s: %v", ErrIntegrity, t.runPath, err)
	}
	if got != want {
		return fmt.Errorf("%w: %s has changed since it was vetted\n  pinned:  %s\n  on disk: %s\n"+
			"  Read the script. If the change is yours and you have reviewed it, update "+
			"integrity.run_sha256 in tcs.yaml (agent-desk -pin prints the new value).",
			ErrIntegrity, t.runPath, want, got)
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
		sum, err := hashFile(t.runPath)
		if err != nil {
			fmt.Fprintf(&b, "# %s: cannot hash %s: %v\n\n", name, t.runPath, err)
			continue
		}
		state := "currently unpinned"
		switch {
		case t.Integrity.pinned() && strings.EqualFold(t.Integrity.RunSHA256, sum):
			state = "already pinned to this value"
		case t.Integrity.pinned():
			state = "PINNED TO A DIFFERENT VALUE — read the script before pasting this"
		}
		fmt.Fprintf(&b, "# %s (%s) — %s\nintegrity:\n  run_sha256: %q\n\n",
			name, t.runPath, state, sum)
	}
	return b.String()
}
