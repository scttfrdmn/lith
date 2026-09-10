// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
	"time"
)

// TestWriteMountRecordMode verifies the record file is written 0600 and that
// writing works with XDG_RUNTIME_DIR set to a fresh temp dir.
func TestWriteMountRecordMode(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rec := mountRecord{PID: 99, Mountpoint: "/mnt/y", Bucket: "b", Start: time.Now()}
	p, err := writeMountRecord(rec)
	if err != nil {
		t.Fatalf("writeMountRecord: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %v, want 0600", fi.Mode().Perm())
	}
	// A second write (record rewritten on each mount) must still succeed.
	if _, err := writeMountRecord(rec); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
}
