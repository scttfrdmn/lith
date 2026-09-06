// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// erofs is the status every mutating operation returns: lith has no write path.
const erofs = fuse.Status(syscall.EROFS)

func (f *rawFS) Create(cancel <-chan struct{}, input *fuse.CreateIn, name string, out *fuse.CreateOut) fuse.Status {
	return erofs
}

func (f *rawFS) Mkdir(cancel <-chan struct{}, input *fuse.MkdirIn, name string, out *fuse.EntryOut) fuse.Status {
	return erofs
}

func (f *rawFS) Mknod(cancel <-chan struct{}, input *fuse.MknodIn, name string, out *fuse.EntryOut) fuse.Status {
	return erofs
}

func (f *rawFS) Unlink(cancel <-chan struct{}, header *fuse.InHeader, name string) fuse.Status {
	return erofs
}

func (f *rawFS) Rmdir(cancel <-chan struct{}, header *fuse.InHeader, name string) fuse.Status {
	return erofs
}

func (f *rawFS) Rename(cancel <-chan struct{}, input *fuse.RenameIn, oldName, newName string) fuse.Status {
	return erofs
}

func (f *rawFS) Link(cancel <-chan struct{}, input *fuse.LinkIn, name string, out *fuse.EntryOut) fuse.Status {
	return erofs
}

func (f *rawFS) Symlink(cancel <-chan struct{}, header *fuse.InHeader, pointedTo, linkName string, out *fuse.EntryOut) fuse.Status {
	return erofs
}

func (f *rawFS) SetAttr(cancel <-chan struct{}, input *fuse.SetAttrIn, out *fuse.AttrOut) fuse.Status {
	return erofs
}

func (f *rawFS) Write(cancel <-chan struct{}, input *fuse.WriteIn, data []byte) (uint32, fuse.Status) {
	return 0, erofs
}

func (f *rawFS) Fallocate(cancel <-chan struct{}, input *fuse.FallocateIn) fuse.Status {
	return erofs
}

func (f *rawFS) SetXAttr(cancel <-chan struct{}, input *fuse.SetXAttrIn, attr string, data []byte) fuse.Status {
	return erofs
}

func (f *rawFS) RemoveXAttr(cancel <-chan struct{}, header *fuse.InHeader, attr string) fuse.Status {
	return erofs
}
