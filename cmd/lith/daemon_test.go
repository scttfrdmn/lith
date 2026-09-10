// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenOwnedLog verifies the normal case: a fresh log is created 0600 and
// accepted because it is a regular file owned by the current euid.
func TestOpenOwnedLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lith-mount.log")
	f, err := openOwnedLog(p)
	if err != nil {
		t.Fatalf("openOwnedLog: %v", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestOpenOwnedLogRejectsNonRegular verifies a pre-created non-regular file at
// the log path is refused rather than appended to.
func TestOpenOwnedLogRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lith-mount.log")
	// Pre-plant a directory at the log path: openOwnedLog must refuse it (either
	// the O_WRONLY open fails with EISDIR, or the fstat regular-file check does).
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if f, err := openOwnedLog(p); err == nil {
		_ = f.Close()
		t.Fatalf("openOwnedLog accepted a non-regular file")
	}
}
