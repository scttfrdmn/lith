// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"github.com/hanwen/go-fuse/v2/fuse"
)

// MountOptions are the lith-level knobs for a mount.
type MountOptions struct {
	AllowOther bool
	FsName     string // shown in df/mount output (typically the bucket)
}

// Mount builds the read-only FUSE server, mounts it at mountpoint, and starts
// serving in the background. The caller waits on the returned server and calls
// Unmount to tear it down.
func Mount(mountpoint string, cfg Config, mo MountOptions) (*fuse.Server, IndexSwapper, error) {
	raw := NewRawFileSystem(cfg)

	// THE FUSE TRANSPORT. Three kernel-negotiated limits bear on throughput here, and only
	// the first has been measured. They are listed together because the next person to
	// wonder about one of them should not have to re-derive the other two.
	//
	// MaxWrite: left at the go-fuse default of 128 KiB, DELIBERATELY. go-fuse sets max_read
	// equal to MaxWrite, so this is also the largest read the kernel will issue -- a 1 MiB
	// chunk therefore reaches the application in eight FUSE round trips, seven of which are
	// cache hits returning a sub-slice. Raising it to 1 MiB was tried: it cut FUSE syscalls
	// and made each reply exceed go-fuse's splice pipe, forcing a copy, which was a net loss
	// for the CPU-bound multi-reader path. Each 128 KiB read still lands within one chunk and
	// takes the zero-copy single-chunk path in Read. Revisiting it is not a lith-side flag:
	// the pipe ceiling is /proc/sys/fs/pipe-max-size (1 MiB by default) but the grow policy
	// is go-fuse's (splice/copy.go grows to 256 KiB), so the copy would have to be addressed
	// there first.
	//
	// MaxBackground and CongestionThreshold: NOT SET, and not measured. go-fuse defaults them
	// to 12 and 3/4 x 12 = 9, so the kernel will keep at most twelve ASYNC requests
	// outstanding on this mount and marks the backing device congested at nine. That bounds
	// kernel-initiated readahead to ~1.1 MB in flight mount-wide, shared across every reader.
	// It does NOT bound application reads -- go-fuse's own doc is explicit that synchronous
	// I/O is unlimited -- so a 48-rank job calling read() is unaffected, and lith's prefetch
	// is unaffected too because it runs as goroutines against S3 rather than as FUSE requests.
	// The shape it could bind on is one that leans on the kernel's readahead instead of
	// lith's: mmap (#232), where the fault is synchronous but the readahead around it is not.
	// Unmeasured, so unchanged; the log line below makes it visible rather than guessed at.
	//
	// MaxReadAhead: not set, and lith can only ever LOWER it. go-fuse echoes the kernel's
	// request and applies opts.MaxReadAhead only if it is smaller, and Linux caps the value
	// at 128 KiB regardless. So "ask the kernel to read ahead more" is not available.
	opts := &fuse.MountOptions{
		AllowOther: mo.AllowOther,
		FsName:     mo.FsName,
		Name:       "lith",
		// Read-only mount; the kernel enforces it and lith returns EROFS anyway.
		Options: []string{"ro"},
	}

	srv, err := fuse.NewServer(raw, mountpoint, opts)
	if err != nil {
		return nil, nil, err
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		return nil, nil, err
	}
	swapper, _ := raw.(IndexSwapper)
	return srv, swapper, nil
}

// NegotiatedMaxWrite and NegotiatedMaxBackground report the go-fuse defaults lith relies on,
// so the mount can log what its transport is actually doing (#232).
//
// They are constants rather than reads of the live server because go-fuse exposes only the
// kernel's InitIn, not the InitOut it replied with -- MaxWrite and MaxBackground are ours and
// are never read back. Keeping them here, next to the options they describe, is what stops the
// log line and the configuration drifting apart; if either option is ever set explicitly,
// these move with it.
func NegotiatedMaxWrite() int64      { return 128 << 10 }
func NegotiatedMaxBackground() int64 { return 12 }
