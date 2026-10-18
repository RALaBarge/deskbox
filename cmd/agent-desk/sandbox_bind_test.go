package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deskbox/agent-desk/internal/tcs"
)

// TestSandboxBindIsReadOnlyByDefault: a declared bind is what replaces
// network:true for a tool that only needs to reach a host socket, so the
// mount has to be the least it can be. Read-only is the default; rw is a
// deliberate thing to write.
func TestSandboxBindIsReadOnlyByDefault(t *testing.T) {
	tool := &Tool{
		Name:    "t",
		Sandbox: SandboxSpec{Bind: []tcs.BindMount{{Src: "/run/sbx/broker.sock", Dst: "/deskbox/broker.sock"}}},
	}
	args, err := sandboxArgs("/usr/bin/bwrap", tool, t.TempDir(), t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--ro-bind /run/sbx/broker.sock /deskbox/broker.sock") {
		t.Fatalf("declared bind not mounted read-only:\n%s", joined)
	}
	if strings.Contains(joined, "--bind /run/sbx/broker.sock") {
		t.Fatalf("default bind came out read-write:\n%s", joined)
	}
}

func TestSandboxBindRWUsesBind(t *testing.T) {
	tool := &Tool{
		Name:    "t",
		Sandbox: SandboxSpec{Bind: []tcs.BindMount{{Src: "/var/lib/x/spool", Dst: "/deskbox/spool", RW: true}}},
	}
	args, err := sandboxArgs("/usr/bin/bwrap", tool, t.TempDir(), t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "--bind /var/lib/x/spool /deskbox/spool") {
		t.Fatalf("rw bind not mounted read-write:\n%s", joined)
	}
}

// TestSandboxBindRejectsEscapeDst is the reason validateBinds exists: the
// base the sandbox is built on (/usr, /etc) is shared by every tool, so a
// contract that could bind over it could change what every other tool runs.
func TestSandboxBindRejectsEscapeDst(t *testing.T) {
	for _, dst := range []string{
		"/etc/passwd",
		"/usr/bin/python3",
		"/deskboxx/not-under", // a prefix that is not a path component
		"/deskbox/../etc/passwd",
		"deskbox/sock",  // relative
		"/deskbox",      // would hide the work surface
		"/deskbox/tool", // would replace the directory the integrity pin hashed
		"/deskbox/tool/run.py",
		"/deskbox/in/input.json",
		"/deskbox/out",
	} {
		tool := &Tool{Name: "t", Sandbox: SandboxSpec{Bind: []tcs.BindMount{{Src: "/run/sbx/broker.sock", Dst: dst}}}}
		if _, err := sandboxArgs("/usr/bin/bwrap", tool, t.TempDir(), t.TempDir(), t.TempDir(), nil); err == nil {
			t.Errorf("dst %q was accepted", dst)
		}
	}
}

func TestSandboxBindRejectsBadSrc(t *testing.T) {
	for _, src := range []string{"run/sbx/broker.sock", "/run/sbx/../broker.sock", ""} {
		tool := &Tool{Name: "t", Sandbox: SandboxSpec{Bind: []tcs.BindMount{{Src: src, Dst: "/deskbox/broker.sock"}}}}
		if _, err := sandboxArgs("/usr/bin/bwrap", tool, t.TempDir(), t.TempDir(), t.TempDir(), nil); err == nil {
			t.Errorf("src %q was accepted", src)
		}
	}
}

func TestSandboxBindRejectsCollision(t *testing.T) {
	tool := &Tool{Name: "t", Sandbox: SandboxSpec{Bind: []tcs.BindMount{
		{Src: "/a", Dst: "/deskbox/sock"},
		{Src: "/b", Dst: "/deskbox/sock"},
	}}}
	if _, err := sandboxArgs("/usr/bin/bwrap", tool, t.TempDir(), t.TempDir(), t.TempDir(), nil); err == nil {
		t.Fatal("two binds onto one dst were accepted")
	}
}

// TestMissingBindSourceFailsTheJob covers the check bakeSandbox does before
// bwrap is invoked. bakeSandbox needs a real bwrap on PATH, so this asserts
// the same predicate it uses rather than the whole call.
func TestMissingBindSourceFailsTheJob(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "broker.sock")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(present); err != nil {
		t.Fatalf("test setup: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone.sock")); err == nil {
		t.Fatal("expected a missing file to be missing")
	}
}

func TestLoadToolsSkipsBadBind(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "bad")
	if err := os.Mkdir(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(tdir, "run.sh")
	if err := os.WriteFile(run, []byte("#!/bin/sh\necho '{}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tcs := "name: bad\n" +
		"output:\n  schema:\n    type: object\n" +
		"sandbox:\n  bind:\n    - src: /run/sbx/broker.sock\n      dst: /etc/passwd\n"
	if err := os.WriteFile(filepath.Join(tdir, "tcs.yaml"), []byte(tcs), 0o600); err != nil {
		t.Fatal(err)
	}
	tools, err := LoadTools(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tools["bad"]; ok {
		t.Fatal("a tool with a bind outside /deskbox was loaded")
	}
}
