// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func mkCargoFS(t *testing.T, srv *fake.Server) (*rawFS, *cargoship.Archive) {
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
	return raw, arch
}

func readAll(t *testing.T, raw *rawFS, fh uint64, size int64) []byte {
	t.Helper()
	out := make([]byte, 0, size)
	const step = 128 << 10
	for off := int64(0); off < size; off += step {
		n := int64(step)
		if off+n > size {
			n = size - off
		}
		buf := make([]byte, n)
		res, st := raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: uint64(off), Size: uint32(n)}, buf)
		if st != fuse.OK {
			t.Fatalf("read at %d: %v", off, st)
		}
		b, _ := res.Bytes(buf)
		out = append(out, b...)
	}
	return out
}

func TestCargoMountReadsFilesByteExact(t *testing.T) {
	srv := fake.New()
	raw, arch := mkCargoFS(t, srv)

	// Namespace: the sub directory and the nested file resolve (derived from the
	// virtual tree, no S3 LIST).
	var eo fuse.EntryOut
	if st := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "sub", &eo); st != fuse.OK {
		t.Fatalf("lookup sub: %v", st)
	}
	if eo.Mode&fuse.S_IFDIR == 0 {
		t.Fatalf("sub is not a directory (mode %o)", eo.Mode)
	}
	subNode := eo.NodeId
	if st := raw.Lookup(nil, &fuse.InHeader{NodeId: subNode}, "beta.txt", &eo); st != fuse.OK {
		t.Fatalf("lookup sub/beta.txt: %v", st)
	}

	// Read the two root-level files byte-exact (checked against the manifest sha).
	for _, name := range []string{"alpha.txt", "readme.txt"} {
		var vf cargoship.VFile
		for _, f := range arch.Files {
			if f.Path == name {
				vf = f
			}
		}
		h, fh := openHandle(t, raw, name)
		if h.cargo == nil {
			t.Fatalf("%s: not a cargo handle", name)
		}
		data := readAll(t, raw, fh, vf.Size)
		if int64(len(data)) != vf.Size {
			t.Fatalf("%s: read %d bytes, want %d", name, len(data), vf.Size)
		}
		if hex.EncodeToString(sha256Sum(data)) != vf.Sum {
			t.Fatalf("%s: checksum mismatch through the mount", name)
		}
	}
}

func sha256Sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }
