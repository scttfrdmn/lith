// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Filesystem magic numbers (Linux statfs f_type) used to classify a disk-cache
// path. tmpfs is ideal; network filesystems and the root volume draw a warning.
const (
	magicTmpfs = 0x01021994
	magicNFS   = 0x6969
	magicCIFS  = 0xFF534D42
	magicSMB2  = 0xFE534D42
	magicFUSE  = 0x65735546
	magicSMB   = 0x517B
)

// classifyDiskCache returns a warning string (empty = fine) for a disk-cache
// path given its filesystem type magic and whether it resolves onto the root
// filesystem. Split out from the syscall so it can be unit-tested with a fake
// statfs.
func classifyDiskCache(fsType int64, onRootFS bool) string {
	switch fsType {
	case magicTmpfs:
		return "" // tmpfs (e.g. /dev/shm) is a good place for the cache
	case magicNFS, magicCIFS, magicSMB2, magicSMB, magicFUSE:
		return "disk-cache path is on a network filesystem; caching there can be slower than re-fetching from S3 in-region. Prefer local NVMe or /dev/shm."
	}
	if onRootFS {
		return "disk-cache path is on the root filesystem (often EBS on cloud instances); prefer a local NVMe mount or /dev/shm, or disable the disk cache and rely on the memory tier + page cache."
	}
	return ""
}

// totalMemBytes reports total system memory from /proc/meminfo, or 0 when that cannot be
// read (non-Linux dev machines, or a container without /proc). Split out from
// defaultMemCacheBytes because the figure is needed on its own: the tier's default is a
// fraction of it, and memTierHeadroomWarning compares the tier against it (#314).
func totalMemBytes() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line) // "MemTotal: 12345 kB"
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// defaultMemCacheBytes returns 25% of total system memory, or a 1 GiB fallback when that is
// unavailable (non-Linux dev machines).
//
// WHY 25% AND NOT MORE, which was not written down until #314 made it load-bearing: the tier
// is ordinary Go heap (every chunk is a `make([]byte, cl)` in cloneChunk), so it is LIVE heap
// that the garbage collector sizes itself around. At the default GOGC=100 the collector lets
// the heap grow to about twice the live set, so a tier of T targets a heap of ~2T. 25% keeps
// that target near half of RAM, which leaves room for the application, the page cache, and
// the transient garbage a burst of fills produces. Raising it past 50% cannot work without a
// memory limit — see memTierHeadroomWarning.
func defaultMemCacheBytes() int64 { return memCacheDefaultFor(totalMemBytes()) }

// memCacheDefaultFor is the decision, separated from the /proc lookup so the fraction is
// testable where there is no /proc/meminfo.
//
// NOT a cosmetic split. With the arithmetic inlined behind the lookup, an assertion on "the
// default is 25%" was UNREACHABLE on a non-Linux dev machine — totalMemBytes returns 0, the
// test took the fallback branch and returned early, and raising the default to 50% of RAM
// passed the whole file. Found by reverting (the fourth of five reverts was the one that got
// through), and it is the third time in this project a test has proved nothing because its
// fixture never reached the code under test.
func memCacheDefaultFor(total int64) int64 {
	const fallback = 1 << 30
	if total <= 0 {
		return fallback
	}
	return total / 4 // 25%
}

// memTierHeadroomWarning returns a warning when the configured memory tier cannot fit on the
// box once the garbage collector's headroom is counted, and "" when it can or when total
// memory is unknown (#314).
//
// THE MECHANISM, and it is not the one the issue was filed with. #314 reported RSS peaking at
// 30.56 GB for a 20 GB `--mem-cache` on a 33.02 GB box, and modelled it as the tier PLUS
// outstanding prefetch commitment (13.078 GB), which sums to MemTotal almost exactly. That
// addition does not hold: `lith_prefetch_committed_bytes` is charged at DISPATCH and released
// only on consume/evict/fail, so it keeps counting a chunk after that chunk has landed IN THE
// TIER -- it double-counts the resident half -- and a dispatched chunk that has not yet been
// admitted holds no chunk buffer at all, because cloneChunk runs after fetchReader returns and
// after the bytes-in-flight budget is acquired. blockstore.go says so at the field itself: "it
// is NOT a memory figure".
//
// What does explain it is the collector. The tier is live Go heap, lith sets neither GOGC nor
// GOMEMLIMIT, and at the default GOGC=100 the heap target is about twice the live set. So a
// 20 GB tier targets ~40 GB on a 33 GB box and the process dies on the way there -- which also
// explains why the kill came at 30.56 GB rather than at the 33.08 GB the additive model
// predicted. The ceiling is therefore ~50% of RAM, and that is arithmetic from GOGC's own
// definition rather than a constant anyone picked.
//
// 2*tier is a LOWER BOUND on the target, because the tier is not the only live heap, so this
// under-warns rather than over-warns. Deliberate: a warning that fires on a configuration that
// would have survived teaches operators to ignore it.
//
// A warning and not a clamp or a refusal, consistent with --prefetch-pressure-max: an operator
// who typed a number keeps it, and a tier is filled lazily, so a short-lived mount over a small
// dataset may never reach the bound. The fix is named in the message, including GOMEMLIMIT,
// which makes the collector work harder instead of letting the kernel kill the process.
func memTierHeadroomWarning(tier, total int64) string {
	if tier <= 0 || total <= 0 || 2*tier < total {
		return ""
	}
	return fmt.Sprintf("--mem-cache %d bytes is %.0f%% of this box's %d bytes of RAM, and the "+
		"memory tier is live Go heap: at the default GOGC=100 the collector grows the heap to "+
		"about TWICE the live set, so expect this process to reach ~%d bytes on a %d-byte box "+
		"and to be OOM-killed before the tier is even full. --mem-cache bounds the TIER, not "+
		"the process. MEASURED, three arms differing 3x in outstanding prefetch: next_gc came "+
		"in at 2.03x the tier every time, with RSS within 1%% of next_gc -- so the doubling is "+
		"not an estimate. Either keep --mem-cache under 50%% of RAM (the default is 25%%), or "+
		"set GOMEMLIMIT to the footprint you can actually afford on this box: it makes the "+
		"collector work harder instead of letting the kernel kill the process, and it must be "+
		"comfortably ABOVE %d bytes (the tier itself) or the collector will thrash trying to "+
		"reclaim a live set it cannot shrink (#314)",
		tier, 100*float64(tier)/float64(total), total, 2*tier, total, tier)
}
