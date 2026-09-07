// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fuse

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// setReadAheadKB raises the kernel readahead window for the FUSE mount's
// backing device. go-fuse can only lower MaxReadAhead to the kernel's proposal
// (typically 128 KiB), so even with max_pages=256 the kernel issues 128 KiB
// reads; bumping the bdi's read_ahead_kb makes it issue reads up to that size
// (1 MiB = one chunk). Best-effort: requires privilege to write /sys.
func setReadAheadKB(mountpoint string, kb int) error {
	var st syscall.Stat_t
	if err := syscall.Stat(mountpoint, &st); err != nil {
		return err
	}
	dev := uint64(st.Dev)
	p := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", unix.Major(dev), unix.Minor(dev))
	return os.WriteFile(p, []byte(strconv.Itoa(kb)), 0o644)
}
