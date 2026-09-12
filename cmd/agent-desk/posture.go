package main

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// Deployment posture: the two things about *how the desk is run* that
// decide how much the sandbox is actually worth.
//
// Everything else the desk enforces is a mechanism it can probe for —
// bubblewrap is installed or it isn't. These two are choices the operator
// makes, and both of them can quietly erase the guarantees the rest of the
// project is built on. So they get checked and reported rather than assumed.

// runningAsRoot reports whether tools will run with uid 0.
//
// Nothing in the executor sets a credential, so a tool runs as exactly the
// user the desk runs as — that is what makes "tools can't touch anything
// this user can't" true, and it is also why running the desk as root throws
// that away. The sandbox still limits which paths exist inside it, but
// everything reachable there (the job's out/ dir, /dev, whatever a
// network:true tool talks to) is reached as root, and files the tool leaves
// behind are root-owned on the host.
func runningAsRoot() bool { return os.Getuid() == 0 || os.Geteuid() == 0 }

// describeUser names the account tools will run as, for the startup log and
// the enforcement block. A desk is only as contained as this account is.
func describeUser() string {
	uid := os.Getuid()
	name := strconv.Itoa(uid)
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = fmt.Sprintf("%s (uid %d)", u.Username, uid)
	} else {
		name = "uid " + name
	}
	if euid := os.Geteuid(); euid != uid {
		name += fmt.Sprintf(", effective uid %d", euid)
	}
	return name
}

// isUnixSocket reports whether addr names a filesystem path rather than a
// host:port. Anything containing a separator is a path — a TCP address
// never is.
func isUnixSocket(addr string) bool { return strings.Contains(addr, "/") }

// listen opens the desk's socket. A unix socket is created 0600, which is
// the only binding that is actually scoped to one OS user: loopback stops
// the network, but every local account can still reach 127.0.0.1, and a
// job submitted by any of them runs as the user the desk runs as. With a
// socket, the kernel checks the file mode on connect and there is nothing
// to authenticate against.
func listen(addr string) (net.Listener, error) {
	if !isUnixSocket(addr) {
		return net.Listen("tcp", addr)
	}
	// A leftover socket from a desk that was killed hard would make bind
	// fail with EADDRINUSE forever. Remove it — but only once Lstat
	// confirms it is a socket, so a mistyped path can never delete a real
	// file someone cares about.
	if fi, err := os.Lstat(addr); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to replace it", addr)
		}
		if err := os.Remove(addr); err != nil {
			return nil, fmt.Errorf("removing stale socket %s: %w", addr, err)
		}
	}
	// Create it unreachable and widen to 0600, rather than creating it
	// world-accessible and narrowing: between Listen and Chmod the socket
	// is live, and anything that connects in that window is already inside.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", addr)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	// Umask alone is not guaranteed to be honoured for sockets on every
	// kernel/filesystem combination, so state the mode outright too.
	if err := os.Chmod(addr, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("securing socket %s: %w", addr, err)
	}
	return ln, nil
}

// listensBeyondLoopback reports whether addr is reachable from another
// machine. An empty host (":8080") means every interface, which is the
// case worth catching: combined with auth off — the default — it means
// anyone who can route to the box can submit jobs to a daemon whose entire
// purpose is running programs.
func listensBeyondLoopback(addr string) bool {
	if isUnixSocket(addr) {
		return false // a 0600 socket is reachable by exactly one account
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // not host:port; let the caller's Listen report the real error
	}
	switch host {
	case "":
		return true // ":8080" — all interfaces
	case "localhost":
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname that isn't "localhost": can't resolve it to a
		// judgement here without a DNS lookup whose answer can change, so
		// say yes. Over-warning about a name is better than staying quiet
		// about an interface that turns out to be public.
		return true
	}
	if ip.IsUnspecified() {
		return true // 0.0.0.0 / ::
	}
	return !ip.IsLoopback()
}
