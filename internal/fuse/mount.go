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
	blockSize := int(cfg.Store.BlockSize())

	opts := &fuse.MountOptions{
		AllowOther:   mo.AllowOther,
		FsName:       mo.FsName,
		Name:         "lith",
		MaxReadAhead: blockSize,
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
	return srv, nil
}
