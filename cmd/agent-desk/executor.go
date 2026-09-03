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

// Execute runs a tool's run script and enforces its contract:
//
//   - input is passed via stdin as a JSON document
//   - the tool runs in a fresh scratch dir under os.TempDir(); file writes are
//     audited against allowed_side_effects.files afterwards
//   - when network:false, the tool runs inside bubblewrap with --unshare-net
//     (advisory only if bwrap is not installed)
//   - stdout must be a single JSON document; if output.schema is declared it
//     is validated against that schema
//   - non-zero exit or timeout => retryable failure
func (d *Desk) Execute(tool *Tool, job *Job, input map[string]any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tool.Execution.Timeout())
	defer cancel()

	scratch, err := os.MkdirTemp("", "deskbox-*")
	if err != nil {
		return nil, fmt.Errorf("create scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)

	inJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode input: %w", err)
	}

	cmd := exec.CommandContext(ctx, tool.runPath)
	cmd.Dir = scratch
	cmd.Stdin = bytes.NewReader(inJSON)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.Env = append(os.Environ(),
		"DESKBOX_JOB_ID="+job.ID,
		"DESKBOX_TOOL="+tool.Name,
		fmt.Sprintf("DESKBOX_ATTEMPT=%d", job.Attempt),
	)

	if !tool.AllowedSideEffects.Network {
		wrapNetIsolation(cmd)
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

	if viol := auditSideEffects(scratch, tool.AllowedSideEffects.Files); len(viol) > 0 {
		return nil, fmt.Errorf("%w: unexpected file write(s) not allowed by contract: %s",
			ErrContract, strings.Join(viol, ", "))
	}

	out := strings.TrimSpace(outBuf.String())
	if out == "" {
		return nil, fmt.Errorf("%w: tool produced no output on stdout (contract requires a JSON document)", ErrContract)
	}
	var result any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("%w: output is not valid JSON: %v", ErrContract, err)
	}

	if viol := Validate(tool.Output.Schema, result); len(viol) > 0 {
		return nil, fmt.Errorf("%w: output schema violation: %s", ErrContract, strings.Join(viol, "; "))
	}

	return result, nil
}

// auditSideEffects verifies that everything the tool wrote in its scratch dir
// is on the contract's allowed list. An empty allowed list means: no writes.
func auditSideEffects(scratch string, allowed []string) []string {
	allowedSet := map[string]bool{}
	for _, a := range allowed {
		allowedSet[a] = true
		allowedSet[filepath.Base(a)] = true
	}
	var viol []string
	_ = filepath.WalkDir(scratch, func(p string, de os.DirEntry, err error) error {
		if err != nil || p == scratch || de.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(scratch, p)
		if !allowedSet[rel] {
			viol = append(viol, rel)
		}
		return nil
	})
	sort.Strings(viol)
	return viol
}

// wrapNetIsolation rewrites cmd to run under bubblewrap with no network when
// the contract forbids egress. Everything is mounted read-only except the
// scratch dir, /tmp, /dev and /proc, so the tool physically cannot reach the
// network or modify the host.
func wrapNetIsolation(cmd *exec.Cmd) {
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		log.Printf("WARN: contract says network:false but bubblewrap (bwrap) is not installed; running unsandboxed (advisory)")
		return
	}
	origProg := cmd.Path
	scriptArgs := append([]string{}, cmd.Args[1:]...)
	scratch := cmd.Dir
	args := []string{
		"bwrap",
		"--die-with-parent",
		"--unshare-pid", "--unshare-uts", "--unshare-ipc",
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--bind", scratch, scratch,
		"--dev", "/dev",
		"--proc", "/proc",
		"--unshare-net",
		"--chdir", scratch,
		origProg,
	}
	args = append(args, scriptArgs...)
	cmd.Path = bw
	cmd.Args = args
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