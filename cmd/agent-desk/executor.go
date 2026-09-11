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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrContract marks a failure caused by violating the Tool Contract Spec
// (side-effect violation, non-JSON output, output schema mismatch). These are
// permanent — the desk does not retry them.
var ErrContract = errors.New("contract violation")

// ErrEnforcement marks a job refused because an enforcement mechanism the
// desk promises under -strict is not available. It is permanent, not
// retryable: the condition is process-wide (bubblewrap missing, the user
// D-Bus session gone), so a second attempt would hit exactly the same wall
// while burning the tool's retry budget and delaying the operator's answer.
var ErrEnforcement = errors.New("enforcement unavailable")

const (
	// maxToolOutput bounds what a single job can hand back — stdout, or a
	// declared out file. The result becomes a JSON document the desk holds
	// in memory, stores, and serves, so "however much the tool felt like
	// printing" is not a size the desk can accept.
	maxToolOutput = 32 << 20 // 32 MiB
	// maxToolStderr bounds the diagnostic text that becomes job.Error.
	maxToolStderr = 64 << 10
)

// cappedBuffer accumulates up to max bytes, then drops the rest while still
// reporting success to the writer — a tool that overruns gets a clean
// contract violation rather than an EPIPE mid-write, and the desk's heap
// stays bounded regardless of what the tool does.
type cappedBuffer struct {
	buf      bytes.Buffer
	max      int64
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if !b.overflow {
		if remaining := b.max - int64(b.buf.Len()); int64(len(p)) > remaining {
			b.buf.Write(p[:remaining])
			b.overflow = true
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

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
//
// parent is the caller's cancellation source — Queue.process derives a
// per-job context from it and cancels that context when an operator DELETEs
// the job, which kills the running process (exec.CommandContext) instead of
// letting it run to completion after the desk stopped caring about it.
func (d *Desk) Execute(parent context.Context, tool *Tool, job *Job, input map[string]any) (any, error) {
	// Strict has to hold at run time, not only at startup: resourceLimitsOK
	// starts true and can flip false mid-life when the user D-Bus session
	// goes away (see the "Failed to connect to bus" handling below). A desk
	// that passed its -strict startup check and then quietly began running
	// jobs uncapped would be making exactly the promise -strict exists to
	// stop it making.
	if d.settings.Strict && !d.resourceLimitsOK.Load() {
		return nil, fmt.Errorf("%w: per-job memory and task caps are unavailable "+
			"(systemd-run --user --scope), and -strict refuses to run a job without them",
			ErrEnforcement)
	}

	ctx, cancel := context.WithTimeout(parent, tool.Execution.Timeout())
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
	// If the desk process itself dies hard (OOM-killed, kill -9, a crash —
	// not the clean-shutdown case), nothing else tells this child its parent
	// is gone: it would otherwise be reparented to init and keep running
	// detached, writing into the same job dir Resume() is about to reuse for
	// a second, duplicate run of the same job. Pdeathsig closes that no
	// matter how violently the desk dies, without needing graceful shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	// Capped, not a plain Buffer: the desk runs outside the per-job cgroup
	// scope, so a tool firehosing stdout would grow the *desk's* heap, not
	// its own capped one — and on hosts where systemd-run isn't usable
	// (the desk fails open there by design) there is no cap at all.
	outBuf := &cappedBuffer{max: maxToolOutput}
	errBuf := &cappedBuffer{max: maxToolStderr}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
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
	scoped := false
	if args, err := bakeSandbox(tool, inDir, outDir, toolDir); err == nil {
		script := filepath.Join("/deskbox/tool", filepath.Base(tool.runPath))
		args = append(args, script)
		wrapped := wrapWithResourceLimits(args, d.settings, d.resourceLimitsOK.Load())
		scoped = len(wrapped) > len(args)
		cmd.Path = wrapped[0]
		cmd.Args = wrapped
		// Inside the sandbox the tool's own folder is always bound here.
		cmd.Env = append(cmd.Env, "DESKBOX_TOOL_DIR=/deskbox/tool")
	} else if errors.Is(err, ErrContract) {
		// The contract itself is malformed (a sandbox.in path escaping the
		// workspace). That is the tool author's bug in either mode — running
		// it unsandboxed would be the worst possible response.
		return nil, err
	} else if d.settings.Strict {
		return nil, fmt.Errorf("%w: the sandbox could not be built (%v), and -strict "+
			"refuses to run a tool unsandboxed", ErrEnforcement, err)
	} else {
		log.Printf("WARN: sandbox unavailable (%v); running unsandboxed (advisory only)", err)
		// Unsandboxed, /deskbox/tool doesn't exist — a tool that reads its
		// own folder (vendored data, a shim's config) needs the real path
		// or it silently breaks in exactly the advisory mode meant to be a
		// degraded-but-working fallback.
		cmd.Env = append(cmd.Env, "DESKBOX_TOOL_DIR="+toolDir)
	}

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	// Canceled (an operator's DELETE /jobs/{id}) is checked ahead of
	// DeadlineExceeded so the returned error is accurate; Queue.process
	// decides the job's final status from job.Status, not from this
	// message, so this mainly matters for logging/debugging clarity.
	if ctx.Err() == context.Canceled {
		return nil, fmt.Errorf("canceled after %s", elapsed.Round(time.Millisecond))
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("timeout after %s", elapsed.Round(time.Millisecond))
	}
	if runErr != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = runErr.Error()
		}
		// systemd-run --user --scope needs a live user D-Bus session. The
		// boot-time probe (canScopeJobs) can pass and then stop being true
		// later — an SSH-launched process losing its session, a container
		// without full session infrastructure, distro-specific quirks — so
		// this is a real failure mode seen in practice, not hypothetical.
		// Fail open: disable the wrapper for the rest of this process's
		// life instead of breaking every subsequent job forever. This job
		// still fails and goes through the tool's normal retry policy,
		// exactly like any other transient infrastructure hiccup.
		if scoped && strings.Contains(msg, "Failed to connect to bus") && d.resourceLimitsOK.CompareAndSwap(true, false) {
			if d.settings.Strict {
				// Under -strict the flag is not a fail-open switch: it is what
				// the check at the top of Execute reads, so every subsequent
				// job is refused rather than run uncapped.
				log.Printf("ERROR: systemd-run --user --scope stopped working mid-run (%s); "+
					"-strict: refusing every further job until the desk is restarted "+
					"with a working user D-Bus session", truncate(msg, 200))
			} else {
				log.Printf("WARN: systemd-run --user --scope stopped working mid-run (%s); "+
					"disabling job memory/task-count limits for the rest of this process's life", truncate(msg, 200))
			}
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
			p := filepath.Join(outDir, filepath.Clean(f))
			// out/ is bind-mounted read-write into the sandbox, so the tool
			// could write a symlink instead of a real file (e.g. pointing at
			// /etc/passwd or the host's .env) and have the desk read the
			// target on its behalf once the sandbox is gone. Refuse anything
			// that isn't a plain regular file.
			fi, statErr := os.Lstat(p)
			if statErr != nil {
				continue
			}
			if !fi.Mode().IsRegular() {
				return nil, fmt.Errorf("%w: declared output %q is not a regular file", ErrContract, f)
			}
			// Size-checked before reading: a declared out file is written
			// inside the sandbox and can be arbitrarily large, and
			// os.ReadFile would pull all of it into the desk's heap.
			if fi.Size() > maxToolOutput {
				return nil, fmt.Errorf("%w: declared output %q is %d bytes, over the %d-byte limit",
					ErrContract, f, fi.Size(), maxToolOutput)
			}
			b, err := os.ReadFile(p)
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
		if outBuf.overflow {
			return nil, fmt.Errorf("%w: tool wrote more than %d bytes to stdout",
				ErrContract, maxToolOutput)
		}
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
//
// A symlink is always a violation, regardless of its name: out/ is bound
// read-write into the sandbox, so a symlink is how a tool would try to make
// the desk read (or later serve) an arbitrary host path once the sandbox
// that wrote it is gone.
func auditOutDir(outDir string, declared []string) []string {
	allowed := map[string]bool{}
	for _, d := range declared {
		allowed[filepath.Clean(d)] = true
	}
	var viol []string
	_ = filepath.WalkDir(outDir, func(p string, de os.DirEntry, err error) error {
		if err != nil || p == outDir {
			return nil
		}
		rel, _ := filepath.Rel(outDir, p)
		if de.Type()&os.ModeSymlink != 0 {
			viol = append(viol, rel+" (symlink, not allowed)")
			return nil
		}
		if de.IsDir() {
			return nil
		}
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

// canScopeJobs checks, once at startup, whether `systemd-run --user --scope`
// actually works here — not just whether the binary exists. It needs a
// working user D-Bus session (XDG_RUNTIME_DIR + a running systemd --user
// instance), which a plain non-interactive process — the desk started over
// bare SSH, or as a systemd service without a linger'd/logind session — may
// not have even though the binary is on PATH. A real probe, done once,
// avoids silently failing cmd.Run() for every single job the first time one
// actually executes.
func canScopeJobs() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	// Bounded: this probe runs in main() before the HTTP server starts
	// listening, so an unbounded Run() here means a wedged D-Bus session
	// (which can hang rather than fail fast) stops the desk from ever
	// serving, with no signal to the operator beyond a silent hang. A
	// probe that should take milliseconds gets five seconds; past that,
	// treat it as unusable and fail open, exactly like a probe that
	// returned an error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run", "--user", "--scope", "--quiet",
		"-p", "MemoryMax=16M", "--", "/bin/true")
	return cmd.Run() == nil
}

// wrapWithResourceLimits prefixes argv with systemd-run so the job's process
// tree runs in its own cgroup scope, capped independently of every other
// concurrent job. bwrap's namespaces isolate identity and filesystem
// visibility; they do not cap memory or process count, so without this a
// single runaway or malicious tool (leak, fork bomb, infinite loop) can
// degrade the host for every other job running at the same time — a real
// risk once real, less-trusted tool traffic is running 10-wide.
//
// systemd-run --scope attaches the cgroup and then execs directly into the
// target (no supervisor process stays in between), so this changes nothing
// about how the caller's context-timeout kill or bwrap's --die-with-parent
// behave: the PID Go spawned is still the PID that ends up running.
func wrapWithResourceLimits(args []string, s *Settings, available bool) []string {
	if !available {
		return args
	}
	// cmd.Path is set directly from args[0] below (same reasoning as
	// bakeSandbox's bw): Go only resolves PATH for exec.Command, not for a
	// manually-assigned cmd.Path/cmd.Args, so this needs the absolute path.
	sr, err := exec.LookPath("systemd-run")
	if err != nil {
		return args
	}
	prefix := []string{
		sr, "--user", "--scope", "--quiet",
		"-p", "MemoryMax=" + s.JobMemoryMax,
		"-p", "MemorySwapMax=0",
		"-p", "TasksMax=" + strconv.Itoa(s.JobTasksMax),
	}
	return append(prefix, args...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
