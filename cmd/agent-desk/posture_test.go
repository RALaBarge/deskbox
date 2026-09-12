package main

import (
	"os"
	"strings"
	"testing"
)

// TestListensBeyondLoopback pins the judgement that decides whether the
// desk warns (and whether -strict refuses). Getting ":8080" wrong is the
// expensive case: it reads like "nothing specified" and means "every
// interface", which with auth off is an open remote-execution port.
func TestListensBeyondLoopback(t *testing.T) {
	reachable := []string{
		":8080",          // every interface — the old default
		"0.0.0.0:8080",   // the same thing, said out loud
		"[::]:8080",      // and again, v6
		"192.168.1.5:80", // a LAN address
		"10.0.0.4:8080",
		"deskbox.internal:8080", // a name we can't judge without DNS
	}
	loopbackOnly := []string{
		"127.0.0.1:8080",
		"localhost:8080",
		"[::1]:8080",
		"127.0.0.2:8080", // all of 127/8 is loopback, not just .1
	}
	for _, a := range reachable {
		if !listensBeyondLoopback(a) {
			t.Errorf("%q is reachable from off-box and must be treated as such", a)
		}
	}
	for _, a := range loopbackOnly {
		if listensBeyondLoopback(a) {
			t.Errorf("%q is loopback-only and must not trip the warning", a)
		}
	}
}

// TestRootDetectionMatchesProcess is deliberately not a mock: the whole
// value of the check is that it reports the real process, and a test that
// stubbed the uid would assert nothing about that.
func TestRootDetectionMatchesProcess(t *testing.T) {
	if got, want := runningAsRoot(), os.Getuid() == 0 || os.Geteuid() == 0; got != want {
		t.Errorf("runningAsRoot() = %v, but this process is uid %d/euid %d",
			got, os.Getuid(), os.Geteuid())
	}
	if d := describeUser(); d == "" || !strings.Contains(d, "uid") {
		t.Errorf("describeUser() should name the account and its uid, got %q", d)
	}
}

// TestEnforcementReportsPosture checks that the account tools run as shows
// up in the block an operator (or an agent) reads to decide how much to
// trust a result — and that root counts as degraded, since a sandbox whose
// contents are all reached as uid 0 is worth much less than it looks.
func TestEnforcementReportsPosture(t *testing.T) {
	d := NewDesk(nil, NewQueue(1, nil), t.TempDir(),
		&Settings{AuthEnabled: true}, true, true)
	d.sandboxOK = true
	e := d.enforcement()

	if _, ok := e["user"].(string); !ok {
		t.Errorf("enforcement must name the account tools run as: %#v", e)
	}
	root, ok := e["root"].(bool)
	if !ok {
		t.Fatalf("enforcement must report root-ness: %#v", e)
	}
	if root != runningAsRoot() {
		t.Errorf("enforcement root=%v disagrees with the process", root)
	}
	// With every mechanism present, "degraded" should track root alone —
	// which is the assertion that root is treated as a downgrade at all.
	if e["degraded"] != root {
		t.Errorf("with sandbox+limits+auth all present, degraded should equal root (%v), got %v",
			root, e["degraded"])
	}
}

// TestSandboxArgsHardening guards the flags whose absence is a security
// hole rather than a missing feature, so a future edit to bakeSandbox has
// to break a test to remove one.
func TestSandboxArgsHardening(t *testing.T) {
	tool := &Tool{Name: "t"}
	args, err := sandboxArgs("/usr/bin/bwrap", tool, t.TempDir(), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--unshare-net",     // network:false is false by default, so this must be here
		"--unshare-pid",     // no view of host processes
		"--unshare-ipc",     // no shared memory with the host
		"--new-session",     // no controlling terminal: blocks TIOCSTI injection
		"--die-with-parent", // no orphan surviving the desk
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("bakeSandbox dropped %s:\n%s", want, joined)
		}
	}
}
