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
func Mount(mountpoint string, cfg Config, mo MountOptions) (*fuse.Server, error) {
	// Raise fs.pipe-max-size BEFORE go-fuse reads (and caches) it, so its splice
	// pipe can grow to hold a 1 MiB reply. Best-effort (needs privilege).
	_ = raisePipeMaxSize(2 << 20)

	raw := NewRawFileSystem(cfg)

	// [agent/fuse-splice] Negotiate 1 MiB requests (max_pages=256 via MaxWrite)
	// so the kernel issues one read per chunk (~8x fewer FUSE requests). This
	// only stays zero-copy if go-fuse can grow its splice pipe past 1 MiB to
	// hold the reply (data+header) — see raisePipeMaxSize below.
	const oneMiB = 1 << 20
	opts := &fuse.MountOptions{
		AllowOther:   mo.AllowOther,
		FsName:       mo.FsName,
		Name:         "lith",
		MaxWrite:     oneMiB,
		MaxReadAhead: oneMiB,
		// Read-only mount; the kernel enforces it and lith returns EROFS anyway.
		Options: []string{"ro"},
	}

	srv, err := fuse.NewServer(raw, mountpoint, opts)
	if err != nil {
		return nil, err
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		return nil, err
	}
	// [agent/fuse-splice] Enable 1 MiB reads that still splice zero-copy:
	// raise the readahead so the kernel issues 1 MiB reads, and raise
	// fs.pipe-max-size so go-fuse can grow its splice pipe to fit the reply.
	// Best-effort (needs privilege); a failure just falls back to 128 KiB reads.
	_ = setReadAheadKB(mountpoint, 1024)
	return srv, nil
}
