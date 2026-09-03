package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrContract marks a failure caused by violating the Tool Contract Spec
// (side-effect violation, non-JSON output, output schema mismatch). These are
// permanent — the desk does not retry them.
var ErrContract = errors.New("contract violation")

// bakeSandbox builds the bwrap argv for a tool run.
//
// The sandbox layer is minimal by construction. Inside it, the tool sees:
//
//	/deskbox/in/    — ONLY the files declared in sandbox.in (each bound
//	                  read-only individually: the tool cannot even list
//	                  siblings it wasn't given)
//	/deskbox/out/   — the writable output dir (host-tailable: the operator
//	                  can `tail -f` these files live)
//	/deskbox/tool/  — the tool's own folder, read-only (run.sh + static bits)
//	/usr /etc /dev /proc /tmp
//
// The host filesystem (home dirs, ~/.ssh, /var, /etc service secrets) is NOT
// mounted at all — no `--ro-bind / /`. Environment is fully controlled in
// cmd.Env (no OPENROUTER_API_KEY or other host env leaks into the sandbox).
func bakeSandbox(tool *Tool, inDir, outDir, toolDir string) ([]string, error) {
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, err
	}

	dstOut := "/deskbox/out"
	args := []string{
		bw, // absolute path — Go must exec this directly, no PATH lookup
		"--die-with-parent",
		"--unshare-pid", "--unshare-uts", "--unshare-ipc",
	}

	// System read-only base: toolchain + a bare minimum of /etc.
	// /bin /sbin /lib /lib64 are toplevel symlinks to usr/ on Fedora; the ELF
	// interpreter resolves through /lib64/ld-linux*, so they MUST be bound or
	// execvp fails with ENOENT inside the fresh root.
	base := []struct{ src, dst string }{
		{"/usr", "/usr"},
		{"/bin", "/bin"},
		{"/sbin", "/sbin"},
		{"/lib", "/lib"},
		{"/lib64", "/lib64"},
		{"/etc/passwd", "/etc/passwd"},
		{"/etc/group", "/etc/group"},
		{"/etc/hosts", "/etc/hosts"},
		{"/etc/nsswitch.conf", "/etc/nsswitch.conf"},
		{"/etc/ld.so.cache", "/etc/ld.so.cache"},
	}
	for _, b := range base {
		if _, err := os.Stat(b.src); err == nil {
			args = append(args, "--ro-bind", b.src, b.dst)
		}
	}

	if tool.AllowedSideEffects.Network {
		// egress allowed: bind DNS + TLS trust so curl/git actually work
		for _, p := range []string{"/etc/resolv.conf", "/etc/ssl", "/etc/pki", "/etc/crypto-policies", "/run/systemd/resolve"} {
			if _, err := os.Lstat(p); err == nil {
				args = append(args, "--ro-bind", p, p)
			}
		}
	} else {
		args = append(args, "--unshare-net")
	}

	args = append(args, "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp")

	// The work surface: declared input files (ro, one by one), output dir (rw).
	ins := tool.Sandbox.In
	if len(ins) == 0 {
		ins = []string{"input.json"}
	}
	for _, f := range ins {
		if pathTraverses(f) {
			return nil, fmt.Errorf("%w: sandbox read path escapes workspace: %q", ErrContract, f)
		}
		host := filepath.Join(inDir, filepath.Clean(f))
		dst := filepath.Join("/deskbox/in", filepath.Clean(f))
		args = append(args, "--ro-bind", host, dst)
	}
	args = append(args, "--bind", outDir, dstOut)
	args = append(args, "--ro-bind", toolDir, "/deskbox/tool")

	args = append(args, "--chdir", "/deskbox")
	return args, nil
}

// Execute runs a tool's run script and enforces its contract:
//
//   - each job gets its own workspace under <dataDir>/jobs/<id>/ with in/ and
//     out/; the input document is materialized as in/input.json (or the exact
//     files declared in sandbox.in) — "the directory necessary for the pull"
//   - the tool runs inside the minimal bwrap sandbox from bakeSandbox
//   - output is read from the first declared sandbox.out file (or from stdout
//     if the contract declares no sandbox.out) and validated against
//     output.schema
//   - any file written to out/ that is NOT declared in sandbox.out is a
//     contract violation (permanent, no retry)
//   - non-zero exit or timeout => retryable failure
func (d *Desk) Execute(tool *Tool, job *Job, input map[string]any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tool.Execution.Timeout())
	defer cancel()

	// Job workspace is stable on disk (not tmp): the operator can tail -f the
	// out/ files live, and the audit trail survives the process.
	jobDir := filepath.Join(d.dataDir, "jobs", job.ID)
	inDir := filepath.Join(jobDir, "in")
	outDir := filepath.Join(jobDir, "out")
	for _, dir := range []string{inDir, outDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create job workspace: %w", err)
		}
	}

	// Materialize input: exactly the files the contract says this pull needs.
	declaredIn := tool.Sandbox.In
	if len(declaredIn) == 0 {
		declaredIn = []string{"input.json"}
	}
	inData, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode input: %w", err)
	}
	for _, f := range declaredIn {
		if pathTraverses(f) {
			return nil, fmt.Errorf("%w: sandbox read path escapes workspace: %q", ErrContract, f)
		}
		host := filepath.Join(inDir, filepath.Clean(f))
		if dir := filepath.Dir(host); dir != inDir {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(host, inData, 0o644); err != nil {
			return nil, err
		}
	}

	// stdin still receives the input doc (legacy/stdio tools), stdout is
	// captured as the fallback result channel.
	cmd := exec.CommandContext(ctx, tool.runPath)
	cmd.Dir = jobDir
	cmd.Stdin = bytes.NewReader(inData)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// Fully-controlled env: nothing from the host shell leaks into the sandbox.
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/tmp",
		"LANG=C.UTF-8",
		"TERM=dumb",
		"DESKBOX_JOB_ID=" + job.ID,
		"DESKBOX_TOOL=" + tool.Name,
		fmt.Sprintf("DESKBOX_ATTEMPT=%d", job.Attempt),
	}

	toolDir := filepath.Dir(tool.runPath)
	if args, err := bakeSandbox(tool, inDir, outDir, toolDir); err == nil {
		script := filepath.Join("/deskbox/tool", filepath.Base(tool.runPath))
		args = append(args, script)
		cmd.Path = args[0]
		cmd.Args = args
	} else {
		log.Printf("WARN: sandbox unavailable (%v); running unsandboxed (advisory only)", err)
	}

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("timeout after %s", elapsed.Round(time.Millisecond))
	}
	if runErr != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return nil, fmt.Errorf("tool exited with error: %s", truncate(msg, 500))
	}

	// File-write contract: any file in out/ that isn't declared is a violation.
	if viol := auditOutDir(outDir, tool.Sandbox.Out); len(viol) > 0 {
		return nil, fmt.Errorf("%w: unexpected file write(s) not allowed by contract: %s",
			ErrContract, strings.Join(viol, ", "))
	}

	// Result: declared out file wins; stdout is the fallback for stdio tools.
	var result any
	if len(tool.Sandbox.Out) > 0 {
		var raw []byte
		for _, f := range tool.Sandbox.Out {
			if pathTraverses(f) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(outDir, filepath.Clean(f)))
			if err == nil {
				raw = b
				break
			}
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("%w: declared output file(s) not produced: %v",
				ErrContract, tool.Sandbox.Out)
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("%w: output file is not valid JSON: %v", ErrContract, err)
		}
	} else {
		out := strings.TrimSpace(outBuf.String())
		if out == "" {
			return nil, fmt.Errorf("%w: tool produced no output on stdout (contract requires a JSON document)", ErrContract)
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			return nil, fmt.Errorf("%w: output is not valid JSON: %v", ErrContract, err)
		}
	}

	if viol := Validate(tool.Output.Schema, result); len(viol) > 0 {
		return nil, fmt.Errorf("%w: output schema violation: %s", ErrContract, strings.Join(viol, "; "))
	}

	return result, nil
}

// auditOutDir walks out/ and returns every file that is not declared in the
// contract's sandbox.out. Empty declared list means: no writes at all.
func auditOutDir(outDir string, declared []string) []string {
	allowed := map[string]bool{}
	for _, d := range declared {
		allowed[filepath.Clean(d)] = true
	}
	var viol []string
	_ = filepath.WalkDir(outDir, func(p string, de os.DirEntry, err error) error {
		if err != nil || p == outDir || de.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(outDir, p)
		if !allowed[rel] {
			viol = append(viol, rel)
		}
		return nil
	})
	sort.Strings(viol)
	return viol
}

// pathTraverses rejects absolute paths, "..", and empty values so a contract
// can't smuggle a path outside the job workspace.
func pathTraverses(p string) bool {
	return p == "" || filepath.IsAbs(p) || strings.Contains(p, "..")
}

func hasBwrap() bool {
	_, err := exec.LookPath("bwrap")
	return err == nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}