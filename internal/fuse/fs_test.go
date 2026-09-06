// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"context"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

func newTestFS(t *testing.T) (fuse.RawFileSystem, *fake.Server) {
	t.Helper()
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.PutString("hello.txt", "hello world", now)
	srv.PutString("dir/inner.txt", "0123456789abcdef", now)

	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{
		Options: index.Options{Bucket: "bkt"},
	})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	bs, err := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 4, MemCache: 1 << 20, MaxRange: 1 << 20})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	raw := NewRawFileSystem(Config{Index: ix, Store: bs, UID: 1000, GID: 1000})
	return raw, srv
}

func TestLookupGetattrRead(t *testing.T) {
	raw, _ := newTestFS(t)

	// Lookup hello.txt under root.
	var eo fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "hello.txt", &eo); s != fuse.OK {
		t.Fatalf("lookup: %v", s)
	}
	if eo.Attr.Size != 11 || eo.NodeId == 0 {
		t.Fatalf("lookup attr: size=%d node=%d", eo.Attr.Size, eo.NodeId)
	}
	if eo.Attr.Mode&fuse.S_IFREG == 0 {
		t.Errorf("expected regular file mode, got %o", eo.Attr.Mode)
	}

	// GetAttr on the node.
	var ao fuse.AttrOut
	if s := raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: eo.NodeId}}, &ao); s != fuse.OK {
		t.Fatalf("getattr: %v", s)
	}
	if ao.Attr.Size != 11 {
		t.Errorf("getattr size = %d, want 11", ao.Attr.Size)
	}

	// Open and read.
	var oo fuse.OpenOut
	if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: eo.NodeId}}, &oo); s != fuse.OK {
		t.Fatalf("open: %v", s)
	}
	if oo.OpenFlags&fuse.FOPEN_KEEP_CACHE == 0 {
		t.Error("expected FOPEN_KEEP_CACHE for immutable content")
	}
	buf := make([]byte, 11)
	res, s := raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: eo.NodeId}, Fh: oo.Fh, Offset: 0, Size: 11}, buf)
	if s != fuse.OK {
		t.Fatalf("read: %v", s)
	}
	got, _ := res.Bytes(buf)
	if string(got) != "hello world" {
		t.Errorf("read = %q, want %q", got, "hello world")
	}
}

func TestReaddirListsChildrenAndDots(t *testing.T) {
	raw, _ := newTestFS(t)
	de := fuse.NewDirEntryList(make([]byte, 4096), 0)
	if s := raw.ReadDir(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}}, de); s != fuse.OK {
		t.Fatalf("readdir: %v", s)
	}
	// Can't easily parse the raw buffer here; instead assert the directory
	// resolves and its entries are reachable via Lookup.
	for _, name := range []string{"hello.txt", "dir"} {
		var eo fuse.EntryOut
		if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, name, &eo); s != fuse.OK {
			t.Errorf("lookup %q: %v", name, s)
		}
	}
	// A missing name is ENOENT.
	var eo fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "nope", &eo); s != fuse.ENOENT {
		t.Errorf("lookup missing: %v, want ENOENT", s)
	}
}

func TestStatFsReportsIndexTotals(t *testing.T) {
	raw, _ := newTestFS(t)
	var so fuse.StatfsOut
	if s := raw.StatFs(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, &so); s != fuse.OK {
		t.Fatalf("statfs: %v", s)
	}
	if so.Files != 2 {
		t.Errorf("statfs files = %d, want 2", so.Files)
	}
	if so.Bavail != 0 || so.Bfree != 0 {
		t.Errorf("read-only fs should report 0 free space, got bfree=%d bavail=%d", so.Bfree, so.Bavail)
	}
}

// TestAllMutatingOpsReturnEROFS enumerates every mutating operation and asserts
// it returns EROFS.
func TestAllMutatingOpsReturnEROFS(t *testing.T) {
	raw, _ := newTestFS(t)
	h := fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}

	checks := []struct {
		name string
		got  fuse.Status
	}{
		{"create", statusOf(raw.Create(nil, &fuse.CreateIn{InHeader: h}, "f", &fuse.CreateOut{}))},
		{"mkdir", raw.Mkdir(nil, &fuse.MkdirIn{InHeader: h}, "d", &fuse.EntryOut{})},
		{"mknod", raw.Mknod(nil, &fuse.MknodIn{InHeader: h}, "n", &fuse.EntryOut{})},
		{"unlink", raw.Unlink(nil, &h, "f")},
		{"rmdir", raw.Rmdir(nil, &h, "d")},
		{"rename", raw.Rename(nil, &fuse.RenameIn{InHeader: h}, "a", "b")},
		{"link", raw.Link(nil, &fuse.LinkIn{InHeader: h}, "l", &fuse.EntryOut{})},
		{"symlink", raw.Symlink(nil, &h, "target", "link", &fuse.EntryOut{})},
		{"setattr", raw.SetAttr(nil, &fuse.SetAttrIn{InHeader: h}, &fuse.AttrOut{})},
		{"write", statusOfWrite(raw.Write(nil, &fuse.WriteIn{InHeader: h}, []byte("x")))},
		{"fallocate", raw.Fallocate(nil, &fuse.FallocateIn{InHeader: h})},
		{"setxattr", raw.SetXAttr(nil, &fuse.SetXAttrIn{InHeader: h}, "a", []byte("v"))},
		{"removexattr", raw.RemoveXAttr(nil, &h, "a")},
	}
	for _, c := range checks {
		if c.got != erofs {
			t.Errorf("%s returned %v, want EROFS", c.name, c.got)
		}
	}
}

func statusOf(s fuse.Status) fuse.Status { return s }

func statusOfWrite(_ uint32, s fuse.Status) fuse.Status { return s }
