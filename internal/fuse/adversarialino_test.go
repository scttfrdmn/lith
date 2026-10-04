// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// M17-B (#217), case 1: an index whose inode assignment REUSES an inode for a different path
// across a refresh. Verdict wanted: detected, fail-closed, or serves wrong.
//
// Inodes are unique WITHIN one index -- assignIno probes forward on collision against a `used`
// set -- so there is no intra-index collision to find. The adversarial case is across a swap,
// and it is reachable without a hash collision at all: assignment order decides who gets the
// base hash and who gets probed forward, so a refresh that adds or removes a colliding key can
// hand inode X from path A to path B while both paths still exist.
//
// The mechanism under test is in the FUSE node registry, not the index:
//
//	register()  writes f.nodes[ino] only `if _, ok := f.nodes[ino]; !ok` -- FIRST WRITER WINS
//	resolve()   reads f.nodes[id] and reads go through the PATH it finds
//	SwapIndex() replaces the index but never updates f.nodes
//
// So if the kernel learns B -> X from a post-swap Lookup while lith still holds X -> A, an
// Open of X serves A. The content check is what decides it: both files exist and are valid, so
// nothing else distinguishes "served B" from "served A".
type renumberedIndex struct {
	index.Reader
	ino map[string]uint64 // "/path" -> the inode this version hands out
}

func (r renumberedIndex) Stat(path string) (index.FileInfo, error) {
	fi, err := r.Reader.Stat(path)
	if err != nil {
		return fi, err
	}
	if n, ok := r.ino[path]; ok {
		fi.Ino = n
	}
	return fi, nil
}

func (r renumberedIndex) Lookup(path string) (index.FileInfo, error) {
	fi, err := r.Reader.Lookup(path)
	if err != nil {
		return fi, err
	}
	if n, ok := r.ino[path]; ok {
		fi.Ino = n
	}
	return fi, nil
}

func TestAdversarialInodeReuseAcrossRefresh(t *testing.T) {
	// Two files with distinguishable content and distinguishable lengths.
	base := buildVersionIndex(t, map[string]string{
		"alpha.txt": "AAAAAAAA",
		"bravo.txt": "BBBBBBBBBBBBBBBB",
	})
	srv := fake.New()
	srv.PutString("alpha.txt", "AAAAAAAA", time.Unix(1_700_000_000, 0))
	srv.PutString("bravo.txt", "BBBBBBBBBBBBBBBB", time.Unix(1_700_000_000, 0))
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: 4 << 20, MemCache: 1 << 20, MaxRange: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bs.Close)

	const shared = uint64(0x5EED)
	// v1 hands the shared inode to alpha; v2 hands the SAME inode to bravo. Both files exist
	// and are valid in both versions -- only the numbering moved, which is what a probe-order
	// change does.
	v1 := renumberedIndex{Reader: base, ino: map[string]uint64{"/alpha.txt": shared}}
	v2 := renumberedIndex{Reader: base, ino: map[string]uint64{"/bravo.txt": shared}}

	raw := NewRawFileSystem(Config{
		Index: v1, Store: bs, Metrics: metrics.New(), UID: 1000, GID: 1000,
	}).(*rawFS)

	// The kernel learns alpha -> shared.
	var eo1 fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "alpha.txt", &eo1); s != fuse.OK {
		t.Fatalf("lookup alpha: %v", s)
	}
	if eo1.NodeId != shared {
		t.Fatalf("fixture: alpha got NodeId %d, want %d", eo1.NodeId, shared)
	}

	// Refresh. v2 numbers bravo with the inode alpha used to hold.
	raw.SwapIndex(v2)

	// The kernel now learns bravo -> shared, which is what a post-refresh lookup returns.
	var eo2 fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "bravo.txt", &eo2); s != fuse.OK {
		t.Fatalf("lookup bravo: %v", s)
	}
	t.Logf("after refresh, bravo resolves to NodeId %d (alpha held %d before the swap)",
		eo2.NodeId, shared)

	// THE VERDICT. Open and read what the kernel believes is bravo.
	var oo fuse.OpenOut
	if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: eo2.NodeId}}, &oo); s != fuse.OK {
		t.Logf("VERDICT: fail-closed — Open of the reused inode returned %v", s)
		return
	}
	buf := make([]byte, 64)
	res, s := raw.Read(nil, &fuse.ReadIn{
		InHeader: fuse.InHeader{NodeId: eo2.NodeId}, Fh: oo.Fh, Offset: 0, Size: 64,
	}, buf)
	if s != fuse.OK {
		t.Logf("VERDICT: fail-closed — Read of the reused inode returned %v", s)
		return
	}
	got, _ := res.Bytes(buf)
	t.Logf("read %d bytes: %q", len(got), string(got))

	for _, b := range got {
		if b == 'A' {
			t.Errorf("VERDICT: SERVES WRONG — the kernel asked for bravo.txt and got "+
				"alpha.txt's bytes (%q). register() is first-writer-wins and SwapIndex "+
				"never updates f.nodes, so inode %d still resolves to the path the "+
				"PREVIOUS index gave it.", string(got), shared)
			return
		}
	}
	t.Logf("VERDICT: correct — served bravo's own bytes despite the inode reuse")
}
