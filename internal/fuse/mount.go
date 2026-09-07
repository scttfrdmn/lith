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
	raw := NewRawFileSystem(cfg)

	// Ask the kernel for 1 MiB requests (max_pages = 256): go-fuse derives
	// max_pages from MaxWrite, and the kernel caps read/readahead at it. One
	// chunk per FUSE read means the read stays on the zero-copy single-chunk
	// path (see Read) instead of being split into many small requests.
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
	// Raise the kernel readahead so it issues 1 MiB reads (one chunk).
	// Best-effort: needs privilege to write /sys.
	_ = setReadAheadKB(mountpoint, 1024)
	return srv, nil
}
