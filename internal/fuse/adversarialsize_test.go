// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/cargoship"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// mkLyingCargoFS builds the real fixture mount, then inflates ONE file's declared size
// without touching its read mapping — the shape a manifest produces when its `size` and
// `length` disagree (M17-B case 3, #217). Everything else is the genuine archive.
func mkLyingCargoFS(t *testing.T, srv *fake.Server, name string, extra int64) (*rawFS, cargoship.VFile) {
	t.Helper()
	dir := filepath.Join("..", "cargoship", "testdata", "fixture")
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := cargoship.Parse(mb)
	if err != nil {
		t.Fatal(err)
	}
	arch, err := m.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	chunkBytes, err := os.ReadFile(filepath.Join(dir, "chunk-0.tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Put(arch.Chunks[0].Key, chunkBytes, time.Unix(1_700_000_000, 0))
	o, err := srv.HeadObject(context.Background(), arch.Chunks[0].Key)
	if err != nil {
		t.Fatal(err)
	}

	var target cargoship.VFile
	found := false
	for i := range arch.Files {
		if arch.Files[i].Path == name {
			target = arch.Files[i]      // the TRUE size and parts, for comparison
			arch.Files[i].Size += extra // the lie: size only, mapping untouched
			found = true
		}
	}
	if !found {
		t.Fatalf("fixture: %q not in the archive", name)
	}

	ix, err := index.BuildFromCargoship(arch, []uint64{index.HashETag(o.ETag)}, [32]byte{}, "u", "2.1", "frames", index.Options{Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "b", BlockSize: 1 << 20, MemCache: 256 << 20, MaxRange: 16 << 20})
	raw := NewRawFileSystem(Config{
		Index: ix, Store: bs, Metrics: metrics.New(),
		SmallFile: 4 << 10, PartsMax: 4 << 10,
		Limits: prefetch.NewPolicy(128<<20, prefetch.DeviceLimits{}, nil),
	}).(*rawFS)
	return raw, target
}

// M17-B case 3 (#217), the bytes: a declared size its parts cannot cover makes the mount
// report N bytes and serve fewer, with no error.
//
// `readCargo` walks the handle's parts and returns only the bytes they cover. The handle's
// size comes from the index, which takes it from the manifest's `size` field; the parts come
// from `length`/`archive_offset`. For a non-split file nothing compared the two (see
// cargoship.TestResolveRejectsASizeItsPartsCannotCover), so a read of the declared range
// returns a SHORT buffer and `fuse.OK`.
//
// That is a wrong answer, not a failure: a short read at an offset the kernel believes is
// inside the file is what a reader sees at EOF, so the file appears truncated — and `stat`
// agrees with the manifest, so nothing looks wrong. The gate is therefore at resolve, where
// the two records are both in hand.
func TestCargoSizeExceedingItsPartsIsNotSilentlyTruncated(t *testing.T) {
	srv := fake.New()
	const extra = 4096
	raw, vf := mkLyingCargoFS(t, srv, "alpha.txt", extra)

	// Honest baseline: the true bytes, to compare against.
	h, fh := openHandle(t, raw, "alpha.txt")
	if h.cargo == nil {
		t.Fatal("not a cargo handle")
	}
	if h.size != vf.Size+extra {
		t.Fatalf("fixture: handle size %d, want the inflated %d — the lie did not take",
			h.size, vf.Size+extra)
	}

	// go-fuse sizes the read from len(buf) (the kernel's buffer), not from in.Size, so
	// every read below passes a buffer of exactly the length it means to request.
	read := func(off int64, n int) ([]byte, fuse.Status) {
		b := make([]byte, n)
		res, st := raw.Read(nil, &fuse.ReadIn{Fh: fh, Offset: uint64(off), Size: uint32(n)}, b)
		if st != fuse.OK {
			return nil, st
		}
		got, _ := res.Bytes(b)
		return got, st
	}

	// Read across the point where the parts stop, the way any reader that trusts stat()
	// would. The declared size says these bytes are inside the file.
	//
	// MUST FAIL CLOSED. Before the guard in readCargo this returned 16 bytes and fuse.OK:
	//
	//   verdict SERVES WRONG: a read of 4096 bytes starting 16 before the true end
	//   returned 16 bytes and fuse.OK, while stat reports 394096 bytes of file
	//
	// A short read at an offset the kernel believes is inside the file is what a reader
	// sees at EOF, so the file appeared truncated while stat() agreed with the manifest --
	// nothing looked wrong. Asserted as EIO rather than merely "not serves-wrong", so
	// removing the guard fails this test instead of logging a different verdict.
	if got, st := read(vf.Size-16, extra); st != fuse.EIO {
		t.Fatalf("verdict SERVES WRONG: a read of %d bytes starting 16 before the true end "+
			"returned %d bytes and %v, while stat reports %d bytes of file",
			extra, len(got), st, h.size)
	}

	// Reads the mapping DOES cover must still work -- a fail-closed guard that refuses the
	// whole file would be a different bug, and the 390000 honest bytes are still honest.
	got, st := read(0, 4096)
	if st != fuse.OK {
		t.Fatalf("a read well inside the mapping returned %v; the guard is refusing "+
			"covered ranges too", st)
	}
	if len(got) != 4096 {
		t.Errorf("a covered read returned %d bytes, want 4096", len(got))
	}
	// Not zero-padded: that is the worse variant of the same bug, since padding produces a
	// full-length read of plausible bytes and a checksum fails with no indication of where.
	if bytes.Equal(got, make([]byte, 4096)) {
		t.Error("a covered read came back as zeros; the fixture is not reading real bytes")
	}

	// The LAST fully covered read must work, so the guard is not off by one at the exact
	// offset where the parts end.
	if got, st := read(vf.Size-16, 16); st != fuse.OK {
		t.Errorf("the final 16 covered bytes returned %v; the guard is off by one", st)
	} else if len(got) != 16 {
		t.Errorf("the final covered read returned %d bytes, want 16", len(got))
	}

	// And one byte past the mapping, still inside the declared size, must fail rather than
	// return empty-and-OK -- the smallest version of the wrong answer.
	if got, st := read(vf.Size, 16); st != fuse.EIO {
		t.Errorf("a 16-byte read starting exactly where the mapping ends returned %d bytes "+
			"and %v, want EIO", len(got), st)
	}
}
