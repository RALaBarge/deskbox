package tcs

import (
	"fmt"
	"path"
	"strings"
)

// BindMount is one host path mounted into a job's sandbox.
//
// It exists for the one thing sandbox.in and sandbox.out cannot express: a
// tool that has to reach something which is not a data file — a control
// socket on the host, typically — without being given the host filesystem or
// the network. Both of those stay shut; only the named path is mounted, and
// only read-only unless the contract asks otherwise.
//
// It lives here, next to the schema validator, because two programs have to
// agree on it: the desk (which refuses to load or run a tool whose bind it
// cannot honor) and tcs-verify (which has to reject the same contract offline,
// before it ever reaches a desk).
type BindMount struct {
	Src string `yaml:"src" json:"src"`
	Dst string `yaml:"dst" json:"dst"`
	RW  bool   `yaml:"rw,omitempty" json:"rw,omitempty"` // default false
}

// BindDstRoot is the only tree a contract may bind into. A bind onto /usr or
// /etc would let one tool's contract overwrite the read-only base every other
// tool's sandbox is built on, which is the one thing this field must not be
// able to do.
const BindDstRoot = "/deskbox"

// ValidateBinds rejects the declarations an executor cannot honor. It uses
// path rather than filepath: these are Linux paths inside a Linux sandbox,
// and the rule has to be the one the kernel will see, not the one the machine
// running the linter happens to use.
func ValidateBinds(binds []BindMount) error {
	seen := make(map[string]bool, len(binds))
	for i, b := range binds {
		if !path.IsAbs(b.Src) || path.Clean(b.Src) != b.Src || strings.Contains(b.Src, "..") {
			return fmt.Errorf("sandbox.bind[%d]: src %q must be an absolute, clean path with no ..", i, b.Src)
		}
		if !path.IsAbs(b.Dst) || path.Clean(b.Dst) != b.Dst || strings.Contains(b.Dst, "..") {
			return fmt.Errorf("sandbox.bind[%d]: dst %q must be an absolute, clean path with no ..", i, b.Dst)
		}
		if b.Dst != BindDstRoot && !strings.HasPrefix(b.Dst, BindDstRoot+"/") {
			return fmt.Errorf("sandbox.bind[%d]: dst %q must live under %s", i, b.Dst, BindDstRoot)
		}
		if seen[b.Dst] {
			return fmt.Errorf("sandbox.bind[%d]: another bind already targets %q", i, b.Dst)
		}
		seen[b.Dst] = true
	}
	return nil
}
