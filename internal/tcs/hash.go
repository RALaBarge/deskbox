package tcs

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// HashFile returns the hex SHA-256 of a file, streaming rather than reading it
// whole: a tool may legitimately vendor a large static binary as its run
// target, and the hash should not be the thing that needs it all in memory.
// Shared so the desk (per-job integrity check) and tcs-verify (offline check)
// hash a file the exact same way.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
