// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime/debug"
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
func defaultMemCacheBytes() int64 { return memCacheDefaultFor(memBudgetBytes()) }

// memBudgetBytes is the memory this process can actually use: the smaller of the box's
// MemTotal and the cgroup limit it runs under, or 0 when neither can be read.
//
// ★ MemTotal ALONE IS THE HOST'S RAM INSIDE A CONTAINER, and that was the default until #314's
// follow-up. /proc/meminfo is not namespaced by the memory controller, so a container limited to
// 4 GiB on a 256 GiB host read 256 GiB here -- and the default tier, 25% of that, was 64 GiB,
// SIXTEEN TIMES the container's whole allowance. The headroom warning compared against the same
// figure and so stayed silent. This is the documented deployment for the gateway (the image's
// entrypoint is `lith serve nfs`), and a Kubernetes pod with a memory limit is the ordinary case,
// so the shipped default was an OOM-kill on the ordinary case.
func memBudgetBytes() int64 {
	return memBudgetFrom(totalMemBytes(),
		readTrimmed("/sys/fs/cgroup/memory.max"),                   // cgroup v2
		readTrimmed("/sys/fs/cgroup/memory/memory.limit_in_bytes")) // cgroup v1
}

// readTrimmed returns a file's contents with surrounding whitespace removed, or "" if it cannot
// be read. Absence is the normal case off Linux and outside a memory cgroup.
func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// memBudgetFrom is the decision, separated from the lookups for the same reason
// memCacheDefaultFor is: so it is testable where there is no /proc and no cgroup, which is
// every machine this is developed on.
//
// "Unlimited" has two spellings and both must be recognised rather than parsed as a number:
// cgroup v2 writes the literal "max", and cgroup v1 writes a page-rounded near-MaxInt64
// (9223372036854771712 on x86-64). Any limit at or above MemTotal is treated as no limit, which
// handles the v1 value and also a limit set above physical RAM, where MemTotal is the real bound.
func memBudgetFrom(memTotal int64, cgroupV2, cgroupV1 string) int64 {
	budget := memTotal
	for _, raw := range []string{cgroupV2, cgroupV1} {
		if raw == "" || raw == "max" {
			continue
		}
		limit, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || limit <= 0 {
			continue
		}
		if budget > 0 && limit >= budget {
			continue // no tighter than what we already have
		}
		if budget <= 0 && limit >= 1<<62 {
			continue // v1's "unlimited", with no MemTotal to compare against
		}
		budget = limit
	}
	return budget
}

// memLimitHeadroom is the fraction of the memory budget an automatic soft limit is set to.
//
// NOT MEASURED HERE, and said so. It follows the Go team's own guidance for setting a memory
// limit in a container -- leave 5-10% of headroom for memory the runtime does not account for
// (stacks, the runtime's own structures, cgo, and here the kernel's FUSE buffers) -- taking the
// conservative end. And its precise value matters less than it looks, for a reason that is
// structural: the automatic limit only engages when the configuration is already predicted to
// exceed the budget (see memoryLimitFor), so the comparison is not "0.90 versus 0.85" but "a
// limit somewhere under the budget versus an OOM-kill". Any value in that band beats the kill.
const memLimitHeadroom = 0.90

// memoryLimitFor returns the soft memory limit to set, in bytes, or 0 to leave the runtime alone.
//
//   - "off" or "0"       -> 0
//   - an explicit size   -> that size; an operator who typed a number keeps it
//   - "auto" (default)   -> 0 if GOMEMLIMIT is already set, since the runtime has honoured it
//     before main ran; otherwise a limit at memLimitHeadroom of the budget, BUT ONLY WHEN the
//     tier is already predicted not to fit (2 x tier >= budget, the same condition
//     memTierHeadroomWarning fires on).
//
// ★ THE AUTO CASE CHANGES NOTHING FOR A CONFIGURATION THAT WAS FINE. At the default 25% tier the
// collector's target is ~50% of the budget, far under any limit, so a mount that has never been
// at risk behaves exactly as before. The limit engages only where the alternative is the kernel
// killing the process -- and the trade there is not subtle: with a limit the collector runs
// harder (the Go runtime caps the extra GC CPU at about half the process), without it the
// process dies. That is why this can default on where #314's first round recommended GOMEMLIMIT
// without setting it: that ruling was about imposing a limit on every mount, and the objection
// to that -- the right value depends on what else is on the box -- does not survive the case
// where the box is already predicted to OOM.
func memoryLimitFor(flag string, envSet bool, tier, budget int64) (int64, error) {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "off", "0":
		return 0, nil
	case "", "auto":
		if envSet || budget <= 0 || tier <= 0 || 2*tier < budget {
			return 0, nil
		}
		return int64(memLimitHeadroom * float64(budget)), nil
	}
	n, err := parseSize(flag)
	if err != nil {
		return 0, fmt.Errorf("--memory-limit: %w (want a size, \"auto\" or \"off\")", err)
	}
	return n, nil
}

// memoryLimitWarning returns a warning when a soft limit is set at or below the tier itself, ""
// otherwise. The tier is live heap the collector cannot reclaim, so a limit under it makes the
// collector run continuously against a target it can never meet. tier is a LOWER BOUND on the
// live set (bytes in flight are live too), so this under-warns, for the same reason
// memTierHeadroomWarning does.
func memoryLimitWarning(limit, tier int64) string {
	if limit <= 0 || tier <= 0 || tier < limit {
		return ""
	}
	return fmt.Sprintf("the memory limit (%d bytes) is at or below --mem-cache (%d bytes). The "+
		"tier is live heap the garbage collector cannot reclaim, so the collector will run "+
		"continuously trying to meet a target it cannot reach. Lower --mem-cache or raise "+
		"--memory-limit / GOMEMLIMIT; the limit must sit comfortably above the tier (#314)",
		limit, tier)
}

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
func memTierHeadroomWarning(tier, total, limit int64) string {
	if tier <= 0 || total <= 0 || 2*tier < total {
		return ""
	}
	if limit > 0 {
		return fmt.Sprintf("--mem-cache %d bytes is %.0f%% of this process's %d-byte memory "+
			"budget, and the memory tier is live Go heap: at the default GOGC=100 the "+
			"collector would grow the heap to about twice the tier (~%d bytes) and the process "+
			"would be OOM-killed. A soft memory limit of %d bytes is in force instead, so the "+
			"collector works harder rather than the kernel killing the process -- expect more "+
			"GC CPU than a smaller tier would cost. To avoid the trade, keep --mem-cache under "+
			"50%% of the budget (the default is 25%%) (#314)",
			tier, 100*float64(tier)/float64(total), total, 2*tier, limit)
	}
	return fmt.Sprintf("--mem-cache %d bytes is %.0f%% of this process's %d-byte memory budget, and the "+
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

// applyMemoryLimit resolves --memory-limit, sets it on the runtime when it resolves to a limit,
// logs what is in force, and returns the EFFECTIVE limit (0 if none) for the warnings to read.
//
// ONE FUNCTION FOR BOTH COMMANDS, on purpose. The mount and the gateway each derive their memory
// tier, and this project has seven recorded instances of the two wiring the same mechanism
// differently -- a flag present on both and derived on one, a metric exported by one. A soft
// memory limit applied by the mount and forgotten by the gateway would be the eighth, and the
// gateway is where an OOM takes every client's mount down with it.
//
// The effective limit is read back from the runtime rather than inferred, because GOMEMLIMIT may
// have been set in the environment, in which case the runtime applied it before main ran and
// memoryLimitFor deliberately returns 0 -- the warnings must still know a limit is in force.
func applyMemoryLimit(log *slog.Logger, flag string, tier, budget int64) (int64, error) {
	envSet := os.Getenv("GOMEMLIMIT") != ""
	want, err := memoryLimitFor(flag, envSet, tier, budget)
	if err != nil {
		return 0, err
	}
	source := "auto"
	if f := strings.ToLower(strings.TrimSpace(flag)); f != "" && f != "auto" {
		source = "--memory-limit"
	}
	if want > 0 {
		debug.SetMemoryLimit(want)
	} else if envSet {
		source = "GOMEMLIMIT"
	}
	effective := debug.SetMemoryLimit(-1) // reads without changing
	if effective == math.MaxInt64 {
		effective = 0 // the runtime's "no limit"
	}
	if effective > 0 {
		log.Info("memory limit", "bytes", effective, "budget", budget, "tier", tier, "source", source)
	}
	return effective, nil
}
