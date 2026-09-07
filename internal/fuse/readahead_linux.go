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
// backing device so the kernel issues reads up to that size (1 MiB = one
// chunk). Best-effort: needs privilege to write /sys.
func setReadAheadKB(mountpoint string, kb int) error {
	var st syscall.Stat_t
	if err := syscall.Stat(mountpoint, &st); err != nil {
		return err
	}
	dev := uint64(st.Dev)
	p := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", unix.Major(dev), unix.Minor(dev))
	return os.WriteFile(p, []byte(strconv.Itoa(kb)), 0o644)
}

// raisePipeMaxSize best-effort raises the system pipe-max-size so go-fuse can
// grow its splice pipe to hold a 1 MiB read reply (data + header). Without
// this, 1 MiB reads exceed the 1 MiB default pipe and go-fuse falls back to a
// copy. Needs privilege to write /proc/sys.
func raisePipeMaxSize(bytes int) error {
	const p = "/proc/sys/fs/pipe-max-size"
	cur, err := os.ReadFile(p)
	if err == nil {
		if n, e := strconv.Atoi(string(trimNL(cur))); e == nil && n >= bytes {
			return nil
		}
	}
	return os.WriteFile(p, []byte(strconv.Itoa(bytes)), 0o644)
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
