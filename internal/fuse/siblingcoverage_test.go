// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// newSiblingFS builds a mount over one object, with the #316 hole discount on or off.
func newSiblingFS(t *testing.T, objBytes int64, sibling bool) (*rawFS, *fake.Server) {
	t.Helper()
	srv := fake.New()
	srv.Put("big.bin", bytes.Repeat([]byte("A"), int(objBytes)), time.Unix(1_700_000_000, 0))
	ix, err := index.BuildFromList(context.Background(), srv,
		index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: 8 << 20, MemCache: 512 << 20, MaxRange: 64 << 20,
	})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	t.Cleanup(bs.Close)
	raw := NewRawFileSystem(Config{
		Index: ix, Store: bs, Metrics: metrics.New(), UID: 1000, GID: 1000,
		Limits: prefetch.NewPolicy(256<<20,
			prefetch.DeviceLimits{NICBytesPerSec: 100 << 20, TTFB: 10 * time.Millisecond}, nil),
		MaxReadahead: 32, SiblingCoverage: sibling,
	}).(*rawFS)
	return raw, srv
}

// #316 end to end: a handle whose holes were already demanded by a sibling must establish,
// and the same handle shape on an untouched object must not.
//
// This is the wiring test rather than the arithmetic one — internal/prefetch covers the
// coverage computation itself. What can go wrong here is the predicate closing over the
// wrong key, size or handle, or not being installed at all, none of which the unit test on
// the other side of the boundary can see.
func TestSiblingCoverageEstablishesOnlyWhenTheHolesWereDemanded(t *testing.T) {
	const objBytes = int64(64) << 20
	const readLen = int64(128) << 10

	// A reader that sees every 4th 128 KiB range — what one of four concurrent readers of
	// one object is left with once the shared page cache absorbs its siblings' reads.
	punctate := func(raw *rawFS, fh uint64) {
		buf := make([]byte, readLen)
		for i := int64(0); i < 24; i++ {
			off := i * 4 * readLen
			if _, st := raw.Read(nil, &fuse.ReadIn{Fh: fh,
				Offset: uint64(off), Size: uint32(readLen)}, buf); st != fuse.OK {
				t.Fatalf("read at %d: %v", off, st)
			}
		}
	}

	// ARM 1: the holes were never demanded, which is a scattered walk. Must stay held —
	// #232's constraint, and the arm that makes this a discrimination rather than a
	// blanket loosening.
	rawCold, _ := newSiblingFS(t, objBytes, true)
	hCold, fhCold := openHandle(t, rawCold, "big.bin")
	punctate(rawCold, fhCold)
	coldStreaming := hCold.pf.isStreaming()

	// ARM 2: a sibling has already read the whole object through lith, so every hole holds
	// demanded bytes. Same offsets, same flag, same fixture.
	rawWarm, _ := newSiblingFS(t, objBytes, true)
	hSib, fhSib := openHandle(t, rawWarm, "big.bin")
	sbuf := make([]byte, 1<<20)
	for off := int64(0); off < 16<<20; off += 1 << 20 {
		if _, st := rawWarm.Read(nil, &fuse.ReadIn{Fh: fhSib,
			Offset: uint64(off), Size: uint32(len(sbuf))}, sbuf); st != fuse.OK {
			t.Fatalf("sibling read at %d: %v", off, st)
		}
	}
	_ = hSib
	hWarm, fhWarm := openHandle(t, rawWarm, "big.bin")
	punctate(rawWarm, fhWarm)
	warmStreaming := hWarm.pf.isStreaming()

	t.Logf("punctate handle streaming: holes undemanded=%v, holes demanded by a sibling=%v",
		coldStreaming, warmStreaming)

	if coldStreaming {
		t.Error("a punctate handle over an untouched object established: a scattered walk " +
			"would start prefetching, which #232 says must not happen")
	}
	if !warmStreaming {
		t.Error("a punctate handle whose holes were already demanded did NOT establish: " +
			"the #316 shape is still held, so the predicate is not reaching the gate")
	}
}

// With the flag off, the demanded-holes arm must behave exactly as before — held. This is
// what every mount gets by default, so it is the property that makes shipping it off
// meaningful rather than nominal.
func TestSiblingCoverageIsInertWhenDisabled(t *testing.T) {
	const objBytes = int64(64) << 20
	const readLen = int64(128) << 10

	raw, _ := newSiblingFS(t, objBytes, false)
	_, fhSib := openHandle(t, raw, "big.bin")
	sbuf := make([]byte, 1<<20)
	for off := int64(0); off < 16<<20; off += 1 << 20 {
		if _, st := raw.Read(nil, &fuse.ReadIn{Fh: fhSib,
			Offset: uint64(off), Size: uint32(len(sbuf))}, sbuf); st != fuse.OK {
			t.Fatalf("sibling read: %v", st)
		}
	}
	h, fh := openHandle(t, raw, "big.bin")
	buf := make([]byte, readLen)
	for i := int64(0); i < 24; i++ {
		off := i * 4 * readLen
		if _, st := raw.Read(nil, &fuse.ReadIn{Fh: fh,
			Offset: uint64(off), Size: uint32(readLen)}, buf); st != fuse.OK {
			t.Fatalf("read at %d: %v", off, st)
		}
	}
	if h.pf.isStreaming() {
		t.Error("a punctate handle established with --prefetch-sibling-coverage off; the " +
			"default is not the pre-#316 behaviour")
	}
}

// Both conditions must earn their place, so this is the arm where the SIBLING condition is
// satisfied and the DEMANDED one is not.
//
// THE HOLE SIZE HERE IS THE WHOLE POINT, and a first version of this test was vacuous for
// getting it wrong. The two conditions cover different bands:
//
//   - holes SMALLER than a 1 MiB chunk: the reader's own demand fetches cover them, so the
//     demanded condition is always satisfied and the SIBLING condition is the only guard
//     (that is what keeps #222's lone strided reader out);
//   - holes LARGER than the 8 MiB seqGapMax: the byte-gap gate forces Random before coverage
//     is ever consulted, so neither condition matters. A 16 MiB-stride fixture therefore
//     tests nothing about this code — it was passing on the gap gate.
//   - holes BETWEEN a chunk and a block: only the demanded condition can hold these, and
//     this is that band.
//
// 128 KiB reads 4 MiB apart: each hole is ~3.9 MiB, too wide for the reader's own chunk
// fetches to cover and narrow enough that the gap gate does not fire first.
func TestSiblingCoverageStillHoldsUndemandedHolesWiderThanAChunk(t *testing.T) {
	const objBytes = int64(256) << 20
	const readLen = int64(128) << 10
	const stride = int64(4) << 20

	raw, _ := newSiblingFS(t, objBytes, true)
	// Two descriptors open, so the sibling condition is satisfied. The second reads
	// nothing — a process that has opened the file and not started yet.
	_, _ = openHandle(t, raw, "big.bin")
	h, fh := openHandle(t, raw, "big.bin")

	buf := make([]byte, readLen)
	for i := int64(0); i < 20; i++ {
		off := i * stride
		if _, st := raw.Read(nil, &fuse.ReadIn{Fh: fh,
			Offset: uint64(off), Size: uint32(readLen)}, buf); st != fuse.OK {
			t.Fatalf("read at %d: %v", off, st)
		}
	}

	// FIXTURE PRECONDITIONS, both of which a first version of this test failed.
	if !raw.siblingsOpen("big.bin") {
		t.Fatal("fixture: no sibling descriptor open, so the sibling condition was never " +
			"satisfied and this arm tests nothing")
	}
	if stride-readLen <= blockstore.ChunkSize {
		t.Fatal("fixture: the holes fit inside a chunk, so the reader's own fetches satisfy " +
			"the demanded condition and this arm tests the sibling condition again")
	}
	if stride >= 8<<20 {
		t.Fatal("fixture: the stride is at or past seqGapMax, so the byte-gap gate holds " +
			"the handle before coverage is consulted and this arm passes on the wrong gate")
	}

	if h.pf.isStreaming() {
		t.Error("a handle whose holes are wider than a chunk and were never demanded " +
			"established: the demanded condition is not holding the band only it can hold")
	}
}
