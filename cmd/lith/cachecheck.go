// SPDX-License-Identifier: Apache-2.0

package main

import (
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

// defaultMemCacheBytes returns 25% of total system memory (from /proc/meminfo),
// or a 1 GiB fallback when that is unavailable (non-Linux dev machines).
func defaultMemCacheBytes() int64 {
	const fallback = 1 << 30
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return fallback
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line) // "MemTotal: 12345 kB"
		if len(fields) < 2 {
			return fallback
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return fallback
		}
		return kb * 1024 / 4 // 25%
	}
	return fallback
}
