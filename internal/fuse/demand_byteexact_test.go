// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// newByteExactFS builds a rawFS over one large object with a device-bearing
// Limits policy, so byteExactThreshold() is non-zero and the demand-path
// byte-exact decision (#210/M16 step 1) can fire. The device (100 MB/s × 10 ms
// = 1 MiB) clamps the threshold to ChunkSize, so any sub-chunk read on a
// confirmed-random handle is a candidate — exactly the measurement-box (fat NIC)
// regime.
func newByteExactFS(t *testing.T, objBytes int64) (*rawFS, *fake.Server) {
	t.Helper()
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.Put("big.bin", bytes.Repeat([]byte("A"), int(objBytes)), now)
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	bs, err := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 1 << 20, MemCache: 512 << 20, MaxRange: 64 << 20})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	lim := prefetch.NewPolicy(256<<20, prefetch.DeviceLimits{NICBytesPerSec: 100 << 20, TTFB: 10 * time.Millisecond}, nil)
	raw := NewRawFileSystem(Config{Index: ix, Store: bs, Metrics: metrics.New(), UID: 1000, GID: 1000, Limits: lim}).(*rawFS)
	return raw, srv
}

func (f *rawFS) handleOf(fh uint64) *fileHandle {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.handles[fh]
}

func openBig(t *testing.T, raw *rawFS) (node, fh uint64) {
	t.Helper()
	var eo fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "big.bin", &eo); s != fuse.OK {
		t.Fatalf("lookup: %v", s)
	}
	var oo fuse.OpenOut
	if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: eo.NodeId}}, &oo); s != fuse.OK {
		t.Fatalf("open: %v", s)
	}
	return eo.NodeId, oo.Fh
}

// quiesce waits until the fake server's byte counter stops moving, i.e. all
// background readahead spawned by earlier reads/opens has completed — so a
// before/after GetByteCount() delta reflects only the read under measurement.
func quiesce(srv *fake.Server) int64 {
	for {
		a := srv.GetByteCount()
		time.Sleep(30 * time.Millisecond)
		if srv.GetByteCount() == a {
			return a
		}
	}
}

func readAt(t *testing.T, raw *rawFS, node, fh uint64, off, length int64) {
	t.Helper()
	buf := make([]byte, length)
	_, s := raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: node}, Fh: fh, Offset: uint64(off), Size: uint32(length)}, buf)
	if s != fuse.OK {
		t.Fatalf("read @%d: %v", off, s)
	}
}

const chunk = blockstore.ChunkSize // 1 MiB

// driveRandom issues the read sequence that walks the prefetcher into Random
// (cold → pending → random via two out-of-band jumps with no progress between),
// leaving the handle confirmed non-sequential. Returns after the state is Random.
func driveRandom(t *testing.T, raw *rawFS, node, fh uint64) {
	t.Helper()
	readAt(t, raw, node, fh, 0, 4<<10)         // cold
	readAt(t, raw, node, fh, 30*chunk, 4<<10)  // out-of-band jump → pending
	readAt(t, raw, node, fh, 5*chunk, 4<<10)   // second jump, no progress → Random
	h := raw.handleOf(fh)
	if got := h.pf.state(); got != prefetch.Random {
		t.Fatalf("expected Random posture after the jumps, got %v", got)
	}
}

// TestDemandByteExactOnRandomHandle: a small read on a confirmed-random handle
// fetches one 64 KiB extent, not the whole 1 MiB chunk.
func TestDemandByteExactOnRandomHandle(t *testing.T) {
	raw, srv := newByteExactFS(t, 64<<20)
	node, fh := openBig(t, raw)
	driveRandom(t, raw, node, fh)

	before := quiesce(srv)
	readAt(t, raw, node, fh, 50*chunk+1234, 1<<10) // 1 KiB read in a cold chunk
	got := quiesce(srv) - before
	if got == 0 || got >= chunk {
		t.Fatalf("random small read fetched %d bytes; want one extent (<%d, not a whole chunk)", got, chunk)
	}
	if got > blockstore.ExtentSize*2 {
		t.Errorf("random small read fetched %d bytes; expected ~one 64 KiB extent", got)
	}
}

// TestDemandWholeChunkOnSequentialHandle: the same small read on a sequential
// handle fetches the whole chunk (byte-exact must not leak into streaming).
func TestDemandWholeChunkOnSequentialHandle(t *testing.T) {
	raw, srv := newByteExactFS(t, 64<<20)
	node, fh := openBig(t, raw)
	// Establish Sequential posture.
	readAt(t, raw, node, fh, 0, 4<<10)
	readAt(t, raw, node, fh, 1*chunk, 4<<10)
	readAt(t, raw, node, fh, 2*chunk, 4<<10)
	if got := raw.handleOf(fh).pf.state(); got != prefetch.Sequential {
		t.Fatalf("expected Sequential posture, got %v", got)
	}
	before := quiesce(srv)
	readAt(t, raw, node, fh, 40*chunk+1234, 1<<10) // small read, far cold chunk
	got := quiesce(srv) - before
	if got < chunk {
		t.Fatalf("sequential handle small read fetched %d bytes; want a whole chunk (>=%d)", got, chunk)
	}
}

// TestByteExactCoalescesWithinChunk: scattered small reads within one chunk on a
// random handle fetch that chunk's extents without re-fetching the whole chunk.
func TestByteExactCoalescesWithinChunk(t *testing.T) {
	raw, srv := newByteExactFS(t, 64<<20)
	node, fh := openBig(t, raw)
	driveRandom(t, raw, node, fh)

	before := quiesce(srv)
	// Ten scattered 1 KiB reads all inside chunk 50.
	base := int64(50 * chunk)
	for i := int64(0); i < 10; i++ {
		readAt(t, raw, node, fh, base+i*100<<10, 1<<10)
	}
	got := quiesce(srv) - before
	// Each distinct 64 KiB extent is fetched at most once (cached thereafter); the
	// whole chunk is never pulled. Ten reads at 100 KiB spacing touch <=10 extents.
	if got >= chunk {
		t.Fatalf("ten scattered small reads fetched %d bytes (>= a whole chunk); byte-exact not applied", got)
	}
}

// TestDetectorStillGovernsPosture: a random small read (byte-exact) followed by a
// sequential run transitions the handle back to whole-chunk fills — the detector,
// not the fix, governs posture.
func TestDetectorStillGovernsPosture(t *testing.T) {
	raw, srv := newByteExactFS(t, 64<<20)
	node, fh := openBig(t, raw)
	driveRandom(t, raw, node, fh)
	readAt(t, raw, node, fh, 50*chunk+10, 1<<10) // byte-exact (Random)

	// Now read sequentially to re-establish Sequential.
	readAt(t, raw, node, fh, 55*chunk, 4<<10)
	readAt(t, raw, node, fh, 56*chunk, 4<<10)
	readAt(t, raw, node, fh, 57*chunk, 4<<10)
	if got := raw.handleOf(fh).pf.state(); got != prefetch.Sequential {
		t.Fatalf("expected Sequential after the run, got %v", got)
	}
	before := quiesce(srv)
	readAt(t, raw, node, fh, 62*chunk+10, 1<<10) // small read, now on a Sequential handle
	if got := quiesce(srv) - before; got < chunk {
		t.Fatalf("post-transition read fetched %d bytes; want whole chunk (>=%d)", got, chunk)
	}
}

// TestStreamingByteIdentical: a strictly sequential read run never reaches Random,
// so no read goes byte-exact — every S3 fetch is chunk-aligned (whole-chunk
// fills only), identical to the pre-change behavior.
func TestStreamingByteIdentical(t *testing.T) {
	raw, srv := newByteExactFS(t, 32<<20) // exact multiple of ChunkSize
	node, fh := openBig(t, raw)
	for i := int64(0); i < 32; i++ {
		readAt(t, raw, node, fh, i*chunk, 64<<10) // sequential 64 KiB reads
		if st := raw.handleOf(fh).pf.state(); st == prefetch.Random {
			t.Fatalf("sequential run reached Random at chunk %d", i)
		}
	}
	// Every fetch was a whole chunk: total S3 bytes are chunk-aligned (no 64 KiB
	// partial fetch leaked in). A byte-exact fetch would leave a non-chunk residue.
	if gb := quiesce(srv); gb%chunk != 0 {
		t.Fatalf("streaming fetched %d bytes, not chunk-aligned — a byte-exact fill leaked into the sequential path", gb)
	}
}

// TestConcurrentExtentAndWholeFill: concurrent byte-exact and whole-chunk fills of
// the same chunk from two handles must be race-free (run under -race).
func TestConcurrentExtentAndWholeFill(t *testing.T) {
	raw, _ := newByteExactFS(t, 64<<20)
	nodeR, fhR := openBig(t, raw)
	nodeS, fhS := openBig(t, raw)
	driveRandom(t, raw, nodeR, fhR)
	// fhS sequential
	readAt(t, raw, nodeS, fhS, 0, 4<<10)
	readAt(t, raw, nodeS, fhS, 1*chunk, 4<<10)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); readAt(t, raw, nodeR, fhR, 45*chunk+7, 1<<10) }()  // byte-exact of chunk 45
	go func() { defer wg.Done(); readAt(t, raw, nodeS, fhS, 45*chunk+300<<10, 4<<10) }() // whole chunk 45
	wg.Wait()
}
