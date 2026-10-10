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

// THE REPORTED CASE MUST FIRE AND THE SHIPPING DEFAULT MUST NOT. Those two rows are the whole
// point: #314 OOM-killed a daemon at a configuration lith accepted silently, and a warning that
// also fires on the default would be ignored within a day.
func TestMemTierHeadroomWarning(t *testing.T) {
	// #314's box, to the byte: MemTotal 33.02 GB, --mem-cache 20 GB, RSS peak 30.56 GB.
	const total = int64(33_020_000_000)
	const reported = int64(20) << 30

	w := memTierHeadroomWarning(reported, total)
	if w == "" {
		t.Fatal("the configuration that OOM-killed a daemon in #314 draws no warning")
	}
	// The message has to carry the arithmetic, not just a verdict -- the operator's next
	// action is choosing a smaller number, and they need the ceiling to choose it.
	// The message must carry the measured basis and the one constraint on GOMEMLIMIT that is
	// derivable (it has to exceed the tier, or the collector thrashes on a live set it cannot
	// shrink). "2.03" is the external figure, three arms differing 3x in outstanding prefetch.
	for _, want := range []string{"GOGC", "GOMEMLIMIT", "50%", "TIER, not the process",
		"2.03x the tier", "ABOVE"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning omits %q, which is what makes it actionable: %q", want, w)
		}
	}

	// The default must be silent on every box size, or the warning is noise. Asked of
	// memCacheDefaultFor rather than of a hard-coded mt/4, so that if the default fraction
	// ever moves, THIS test moves with it and the boundary assertions below are what decide
	// whether the new default is survivable.
	for _, mt := range []int64{1 << 30, 8 << 30, total, 33 << 30, 256 << 30, 2 << 40} {
		if w := memTierHeadroomWarning(memCacheDefaultFor(mt), mt); w != "" {
			t.Errorf("the shipping default on a %d-byte box warns: %q", mt, w)
		}
	}

	// THE BOUNDARY IS 50% AND IT IS INCLUSIVE, because at exactly 50% the GC target equals
	// MemTotal -- no room for the application, the page cache, or transient garbage.
	if w := memTierHeadroomWarning(total/2, total); w == "" {
		t.Error("a tier at exactly 50% of RAM targets the whole box and draws no warning")
	}
	if w := memTierHeadroomWarning(total/2-1, total); w != "" {
		t.Errorf("a tier one byte under 50%% warns: %q", w)
	}

	// Unknown or absent inputs are silent. totalMemBytes returns 0 off Linux and in some
	// containers, and a warning that fires on "I could not tell" stops being greppable -- the
	// same rule the cross-region check follows (#362).
	for _, tc := range []struct {
		name        string
		tier, total int64
	}{
		{"total unknown (off Linux, or no /proc)", 20 << 30, 0},
		{"tier disabled", 0, total},
		{"both unknown", 0, 0},
		{"negative total", 20 << 30, -1},
	} {
		if w := memTierHeadroomWarning(tc.tier, tc.total); w != "" {
			t.Errorf("%s: warned on an input it cannot judge: %q", tc.name, w)
		}
	}
}

// totalMemBytes and defaultMemCacheBytes must not disagree about the box, since the warning
// compares a default derived from one against the other. Before #314 the total was read and
// immediately divided, so it existed nowhere and could not be compared to anything.
func TestDefaultMemCacheIsAQuarterOfTotal(t *testing.T) {
	// ASSERTED ON THE PURE FUNCTION, so it runs everywhere. The earlier version of this test
	// read totalMemBytes() first and returned early when it was 0, which is every non-Linux
	// machine -- so raising the default to 50% of RAM passed the entire file. Caught by
	// reverting, not by reading.
	for _, mt := range []int64{1 << 30, 8 << 30, 33_020_000_000, 256 << 30, 2 << 40} {
		if got, want := memCacheDefaultFor(mt), mt/4; got != want {
			t.Errorf("memCacheDefaultFor(%d) = %d, want %d (25%%). The 25%% is not a taste:\n"+
				"the tier is live Go heap, so at GOGC=100 its GC target is ~2x, and 25%% puts "+
				"that target at half the box. A larger fraction needs GOMEMLIMIT (#314).",
				mt, got, want)
		}
	}
	// Off Linux (or in a container with no /proc): the documented 1 GiB fallback, and the
	// warning stays silent because the total is UNKNOWN rather than small.
	if got := memCacheDefaultFor(0); got != 1<<30 {
		t.Errorf("memCacheDefaultFor(0) = %d, want the 1 GiB fallback", got)
	}
	if w := memTierHeadroomWarning(memCacheDefaultFor(0), 0); w != "" {
		t.Errorf("fallback default warns when total is unknown: %q", w)
	}
	// And the two entry points agree on this box, whichever box it is.
	if got, want := defaultMemCacheBytes(), memCacheDefaultFor(totalMemBytes()); got != want {
		t.Errorf("defaultMemCacheBytes() = %d but memCacheDefaultFor(totalMemBytes()) = %d",
			got, want)
	}
}
