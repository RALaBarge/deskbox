package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"golang.org/x/net/bpf"
)

// Seccomp: filtering what a tool can *ask* the kernel for, not just what it
// can *reach*.
//
// bwrap's namespaces limit what a job can see (no host fs, no host net, no
// host pids). They do not limit which syscalls it can make — a tool inside
// the sandbox can still call ptrace, load a kernel module, or reboot the box
// if its uid is allowed to. A seccomp filter closes that: the desk assembles
// one BPF program and hands it to bwrap via --seccomp, which installs it on
// the sandboxed process before exec.
//
// It is a denylist, not an allowlist, on purpose. An allowlist across every
// language runtime someone might drop into tools/ (python, node, perl, a Go
// binary, a shell) is unmaintainable — miss one syscall a runtime needs and
// the tool dies with a cryptic SIGSYS. A denylist of the syscalls no ordinary
// tool needs and every one of which widens the blast radius of a sandbox
// escape is stable across runtimes and cheap to reason about. Blocked calls
// return EPERM (not SIGSYS), so a tool that probes for a capability sees a
// normal "not permitted" rather than being killed.
//
// The program is built pure-Go (elastic/go-seccomp-bpf assembles the
// arch-correct instructions; golang.org/x/net/bpf turns them into the raw
// sock_filter form) so the CGO_ENABLED=0 static-binary story is preserved —
// no libseccomp, no C.

// deniedSyscalls is the denylist. Every entry is a call an ordinary tool has
// no reason to make and that meaningfully widens what a sandbox escape could
// do: process inspection/injection, kernel programs and modules, the keyring,
// mounting, swap, clock/host identity, reboot/kexec. Names not present on the
// running architecture are filtered out before assembly, so this one list
// works on both amd64 and arm64.
var deniedSyscalls = []string{
	// process inspection / injection
	"ptrace", "process_vm_readv", "process_vm_writev", "kcmp",
	// kernel programs, tracing, modules
	"bpf", "perf_event_open", "init_module", "finit_module", "delete_module",
	// kernel keyring
	"keyctl", "add_key", "request_key",
	// mounting the filesystem out from under the sandbox
	"mount", "umount", "umount2", "pivot_root", "chroot",
	"move_mount", "open_tree", "fsopen", "fsconfig", "fsmount", "mount_setattr",
	// swap
	"swapon", "swapoff",
	// taking the machine down
	"reboot", "kexec_load", "kexec_file_load",
	// clock and host identity
	"clock_settime", "clock_settime64", "clock_adjtime", "clock_adjtime64",
	"settimeofday", "adjtimex", "sethostname", "setdomainname",
	// misc privileged administration
	"acct", "quotactl", "_sysctl", "setns",
}

// buildSeccompProgram assembles the denylist into the raw sock_filter bytes
// bwrap expects on its --seccomp fd. Returns an error only if the arch is
// unknown or the assembler rejects the policy — both of which the caller
// treats as "seccomp unavailable here" (fail open, or refuse under -strict),
// never as a reason to run the tool with a half-built filter.
func buildSeccompProgram() ([]byte, error) {
	info, err := arch.GetInfo("") // "" = the architecture we are running on
	if err != nil {
		return nil, fmt.Errorf("seccomp: unknown architecture: %w", err)
	}
	// Keep only the denied names this kernel/arch actually knows. A name the
	// arch lacks would make Assemble fail the whole program; dropping it costs
	// nothing (the syscall doesn't exist to be called here anyway).
	present := make([]string, 0, len(deniedSyscalls))
	for _, name := range deniedSyscalls {
		if _, ok := info.SyscallNames[name]; ok {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return nil, fmt.Errorf("seccomp: none of the denied syscalls exist on %s", info.ID)
	}

	policy := seccomp.Policy{
		DefaultAction: seccomp.ActionAllow, // denylist: everything not named is allowed
		Syscalls: []seccomp.SyscallGroup{{
			Names:  present,
			Action: seccomp.ActionErrno, // blocked calls return EPERM, not SIGSYS
		}},
	}
	insts, err := policy.Assemble()
	if err != nil {
		return nil, fmt.Errorf("seccomp: assemble policy: %w", err)
	}
	raw, err := bpf.Assemble(insts)
	if err != nil {
		return nil, fmt.Errorf("seccomp: assemble bpf: %w", err)
	}
	return serializeFilter(raw), nil
}

// serializeFilter packs assembled instructions into the kernel's struct
// sock_filter array: {u16 code; u8 jt; u8 jf; u32 k}, native byte order,
// which is the exact wire format bwrap reads from the --seccomp fd.
func serializeFilter(raw []bpf.RawInstruction) []byte {
	buf := make([]byte, 0, len(raw)*8)
	var tmp [8]byte
	for _, ins := range raw {
		binary.NativeEndian.PutUint16(tmp[0:2], ins.Op)
		tmp[2] = ins.Jt
		tmp[3] = ins.Jf
		binary.NativeEndian.PutUint32(tmp[4:8], ins.K)
		buf = append(buf, tmp[:]...)
	}
	return buf
}

// seccompPipe returns the read end of a pipe preloaded with the seccomp
// program, ready to hand to a child as an inherited fd for bwrap's --seccomp.
// The program is a few hundred bytes — far under the pipe buffer — so it is
// written and the write end closed synchronously here: bwrap reads the whole
// program and gets a clean EOF, with no writer goroutine to leak or block.
// The caller owns the returned file and must Close it after the child exits.
func seccompPipe(program []byte) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(program); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// probeSeccomp confirms the filter can actually be applied on this box, once,
// at startup — the same discipline as canScopeJobs. It runs /bin/true through
// the exact real invocation (systemd-run scope when resource limits are on,
// then bwrap with the seccomp fd), so whatever this kernel + bwrap + systemd
// combination does with the filter and the inherited fd is what the probe
// reflects. A box where the kernel rejects the filter, or the fd doesn't
// survive the wrapping, reports seccomp unavailable rather than silently
// running every job unfiltered.
func probeSeccomp(program []byte, settings *Settings, resourceLimitsOK bool) bool {
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return false
	}
	inDir, err := os.MkdirTemp("", "deskbox-seccomp-probe-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(inDir)
	outDir, err := os.MkdirTemp("", "deskbox-seccomp-probe-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(outDir)
	// sandboxArgs binds inDir/input.json read-only; it must exist or bwrap
	// fails for that reason rather than the one under test.
	if err := os.WriteFile(inDir+"/input.json", []byte("{}"), 0o600); err != nil {
		return false
	}

	args, err := sandboxArgs(bw, &Tool{}, inDir, outDir, inDir, nil)
	if err != nil {
		return false
	}
	args = withSeccompFlag(args, 3) // first inherited fd is 3
	args = append(args, "/bin/true")
	args = wrapWithResourceLimits(args, settings, resourceLimitsOK)

	r, err := seccompPipe(program)
	if err != nil {
		return false
	}
	defer r.Close()

	cmd := exec.Command(args[0], args[1:]...)
	cmd.ExtraFiles = []*os.File{r} // -> fd 3 in the child
	// systemd-run --user --scope needs the desk's real env to reach the user
	// bus; bwrap's own --clearenv still isolates the tool. Same reasoning as
	// Execute's real run path.
	cmd.Env = os.Environ()
	return cmd.Run() == nil
}

// withSeccompFlag inserts `--seccomp <fd>` right after the bwrap binary
// (args[0]) so it lands among bwrap's own options regardless of any
// systemd-run prefix added afterward. Kept separate so the daemon's real run
// path and the startup probe wire the flag in identically.
func withSeccompFlag(args []string, fd int) []string {
	out := make([]string, 0, len(args)+2)
	out = append(out, args[0], "--seccomp", fmt.Sprintf("%d", fd))
	out = append(out, args[1:]...)
	return out
}
