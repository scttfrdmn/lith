// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestClassifyDiskCache(t *testing.T) {
	cases := []struct {
		name     string
		fsType   int64
		onRoot   bool
		wantWarn bool
		contains string
	}{
		{"tmpfs (/dev/shm)", magicTmpfs, false, false, ""},
		{"tmpfs even on root dev", magicTmpfs, true, false, ""},
		{"local NVMe (ext4, non-root mount)", 0xEF53, false, false, ""},
		{"root filesystem (EBS)", 0xEF53, true, true, "root filesystem"},
		{"NFS", magicNFS, false, true, "network filesystem"},
		{"CIFS", magicCIFS, false, true, "network filesystem"},
		{"FUSE", magicFUSE, true, true, "network filesystem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDiskCache(tc.fsType, tc.onRoot)
			if tc.wantWarn && got == "" {
				t.Fatalf("expected a warning, got none")
			}
			if !tc.wantWarn && got != "" {
				t.Fatalf("expected no warning, got %q", got)
			}
			if tc.contains != "" && !strings.Contains(got, tc.contains) {
				t.Errorf("warning %q should contain %q", got, tc.contains)
			}
		})
	}
}

func TestDefaultMemCacheBytes(t *testing.T) {
	// Should be positive on any platform (real /proc/meminfo on Linux, or the
	// fallback elsewhere).
	if defaultMemCacheBytes() <= 0 {
		t.Fatal("default mem cache must be positive")
	}
}
