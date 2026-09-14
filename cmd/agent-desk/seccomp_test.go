package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"deskbox/agent-desk/internal/tcs"
)

// TestSeccompProgramWellFormed: the assembled filter must be non-empty and a
// whole number of struct sock_filter entries (8 bytes each), or bwrap will
// reject it. Pure — no kernel, no bwrap — so it runs everywhere.
func TestSeccompProgramWellFormed(t *testing.T) {
	prog, err := buildSeccompProgram()
	if err != nil {
		t.Fatalf("buildSeccompProgram: %v", err)
	}
	if len(prog) == 0 {
		t.Fatal("seccomp program is empty")
	}
	if len(prog)%8 != 0 {
		t.Fatalf("seccomp program is %d bytes, not a whole number of 8-byte sock_filter entries", len(prog))
	}
}

// TestSeccompDeniesPtrace is the end-to-end proof Fable asked for: run a real
// program inside a real sandbox and confirm a denied syscall (ptrace) comes
// back EPERM with the filter on, and succeeds with it off — so the filter,
// not something else, is what blocked it.
//
// It needs bwrap, a working seccomp path on this kernel, and python3 as the
// in-sandbox agent that calls ptrace via libc. Any of those missing is a
// skip, not a failure: this is an integration test of the host, and the
// well-formedness test above always covers the assembly itself.
func TestSeccompDeniesPtrace(t *testing.T) {
	if !hasBwrap() {
		t.Skip("no bwrap: nothing to install a filter on")
	}
	prog, err := buildSeccompProgram()
	if err != nil {
		t.Fatalf("buildSeccompProgram: %v", err)
	}
	if !probeSeccomp(prog, &Settings{}, false) {
		t.Skip("seccomp does not apply on this kernel/host (probe failed) — see known.md")
	}
	if _, err := tcs.LookPathIn("python3", sandboxPath); err != nil {
		t.Skip("python3 not on the sandbox PATH: no in-sandbox agent to call ptrace")
	}

	// ptrace(PTRACE_TRACEME) returns 0 normally; under the denylist it returns
	// -1/EPERM. Print the errno so the test reads one deterministic number.
	py := "import ctypes,json\n" +
		"libc=ctypes.CDLL(None,use_errno=True)\n" +
		"r=libc.ptrace(0,0,0,0)\n" +
		"print(json.dumps({'ret':int(r),'errno':ctypes.get_errno()}))\n"

	withFilter := runSeccompSandbox(t, prog, "python3", "-c", py)
	if !strings.Contains(withFilter, `"errno": 1`) && !strings.Contains(withFilter, `"errno":1`) {
		t.Fatalf("with the filter, ptrace should return EPERM (errno 1); got: %s", withFilter)
	}
	withoutFilter := runSeccompSandbox(t, nil, "python3", "-c", py)
	if !strings.Contains(withoutFilter, `"ret": 0`) && !strings.Contains(withoutFilter, `"ret":0`) {
		t.Fatalf("without the filter, ptrace(TRACEME) should succeed (ret 0); got: %s", withoutFilter)
	}
}

// runSeccompSandbox runs argv inside a real bwrap sandbox built the same way a
// job's is, optionally with the seccomp program attached on fd 3. Returns the
// combined output. program==nil runs the identical sandbox with no filter, so
// a test can compare the two.
func runSeccompSandbox(t *testing.T, program []byte, argv ...string) string {
	t.Helper()
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("no bwrap")
	}
	inDir := t.TempDir()
	outDir := t.TempDir()
	if err := os.WriteFile(inDir+"/input.json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := sandboxArgs(bw, &Tool{}, inDir, outDir, inDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var extra []*os.File
	if program != nil {
		args = withSeccompFlag(args, 3)
		r, err := seccompPipe(program)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		extra = []*os.File{r}
	}
	args = append(args, argv...)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.ExtraFiles = extra
	out, _ := cmd.CombinedOutput() // a denied syscall is reported in output, not exit code
	return string(out)
}
