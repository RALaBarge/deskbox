package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// A job token says "this desk started this job, for this tool, just now". The
// desk is the only party that creates a job, so it is the only one that can
// vouch for who is calling when the job later talks to something outside the
// sandbox (the sbx-broker). The token is an HMAC over the fields the receiver
// needs to bind it to, keyed by a file shared with that receiver and never
// bound into any job, so a job can present its own token but cannot make one
// for another tool, another job or another time.
//
// There is deliberately no registration step: the receiver recomputes the MAC
// from the fields it is given, checks the age, and remembers (job, attempt) to
// refuse a second use.

// jobTokenMessage is the exact byte string that is MACed. The receiver
// (harness/broker/sbx_broker.py) builds the same string; keep them identical.
func jobTokenMessage(tool, jobID string, attempt int, ts int64) string {
	return fmt.Sprintf("%s\n%s\n%d\n%d", tool, jobID, attempt, ts)
}

func jobTokenMAC(key []byte, tool, jobID string, attempt int, ts int64) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(jobTokenMessage(tool, jobID, attempt, ts)))
	return hex.EncodeToString(m.Sum(nil))
}

// minJobKeyBytes refuses a key too short to be a secret.
const minJobKeyBytes = 16

func readJobKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("job key: %w", err)
	}
	key := []byte(strings.TrimSpace(string(b)))
	if len(key) < minJobKeyBytes {
		return nil, fmt.Errorf("job key %s is shorter than %d bytes", path, minJobKeyBytes)
	}
	return key, nil
}

// jobTokenEnv returns the two variables a job needs to present its token, in
// addition to the DESKBOX_TOOL, DESKBOX_JOB_ID and DESKBOX_ATTEMPT it already
// gets. The key is read per job: the receiver may regenerate it when it
// restarts, and a desk that cached the old one would fail every job until it
// was restarted too.
func jobTokenEnv(keyPath, tool, jobID string, attempt int, now time.Time) ([]string, error) {
	key, err := readJobKey(keyPath)
	if err != nil {
		return nil, err
	}
	ts := now.Unix()
	return []string{
		fmt.Sprintf("DESKBOX_JOB_TS=%d", ts),
		"DESKBOX_JOB_MAC=" + jobTokenMAC(key, tool, jobID, attempt, ts),
	}, nil
}
