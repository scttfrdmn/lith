// SPDX-License-Identifier: Apache-2.0

package fuse

import "testing"

// TestLogMissDedupAndCap: the #240 ENOENT breadcrumb logs each distinct missing
// path once and is bounded so a probe-heavy app cannot flood the log or the map.
func TestLogMissDedupAndCap(t *testing.T) {
	f := &rawFS{missSeen: map[string]struct{}{}}

	// Same path many times → recorded once (dedup).
	for i := 0; i < 5; i++ {
		f.logMiss("MERRA2/2015/01/foo_CN.nc4")
	}
	if got := len(f.missSeen); got != 1 {
		t.Fatalf("dedup: distinct-miss count = %d, want 1", got)
	}

	// Distinct paths accumulate but stop at the cap (bounded memory).
	for i := 0; i < missLogCap*2; i++ {
		f.logMiss("p/" + string(rune('a'+i%26)) + "/" + itoa(i))
	}
	if got := len(f.missSeen); got > missLogCap {
		t.Fatalf("cap: distinct-miss count = %d, want <= %d", got, missLogCap)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
