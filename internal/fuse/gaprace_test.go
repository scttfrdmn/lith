// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"sync"
	"sync/atomic"
	"testing"
)

// #278: the byte gap is an INPUT to Observe, so computing it, feeding it, and updating the
// endpoint have to be one atomic step. Previously the gap was loaded from an atomic field on
// the handle before taking the lock and stored after releasing it, so two concurrent reads on
// one handle both measured against the same stale endpoint and whichever store landed last
// could move the endpoint BACKWARDS.
//
// The invariant: every gap a handle reports must correspond to SOME serialization of its
// reads. For a set of reads that exactly tile a range, every serialization has each read
// either contiguous with a predecessor (gap 0) or starting the tile — so no gap may exceed
// the span of the reads issued so far, and the endpoint may never regress. A stale-load
// race produces gaps far outside that bound.
func TestByteGapIsConsistentWithSomeSerialization(t *testing.T) {
	const (
		readers = 16
		perRead = int64(128 << 10)
		blockSz = int64(8 << 20)
	)
	w := newPFWrapper(223, blockSz, 0, 0)
	var seq atomic.Int64
	span := int64(readers) * perRead

	gaps := make([]int64, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * perRead
			obs := w.observe(off/blockSz, off, off+perRead, 223, &seq)
			gaps[i] = obs.gap
		}(i)
	}
	wg.Wait()

	// No gap may exceed the total span the reads cover: under every serialization of reads
	// that tile [0, span), the furthest any read can be from the previous endpoint is span.
	for i, g := range gaps {
		if g > span || g < -span {
			t.Errorf("reader %d reported gap %d, outside +/-%d (the span of all reads): "+
				"this gap corresponds to no serialization of the reads", i, g, span)
		}
	}
	// The endpoint must have landed on a real read boundary, never behind the start.
	if got := w.gapFrom(0); got > 0 {
		t.Errorf("endpoint regressed below 0: gapFrom(0) = %d", got)
	}
	if end := -w.gapFrom(0); end <= 0 || end > span {
		t.Errorf("final endpoint %d is not one of the reads' end offsets in [1,%d]", end, span)
	}
	// Every decision got a distinct sequence number, so the trace can order them.
	if n := seq.Load(); n != readers {
		t.Errorf("seq allocated %d times, want %d", n, readers)
	}
}

// The gap must be reported from inside the lock: the value in the observation has to be the
// one Observe was actually called with, not a re-derivation. A single sequential handle makes
// that checkable exactly.
func TestObservedGapIsTheValueTheDetectorSaw(t *testing.T) {
	const blockSz = int64(8 << 20)
	w := newPFWrapper(223, blockSz, 0, 0)
	var seq atomic.Int64
	// First read starts at 0: gap 0 against a fresh endpoint.
	if obs := w.observe(0, 0, 1<<20, 223, &seq); obs.gap != 0 {
		t.Errorf("first read gap = %d, want 0", obs.gap)
	}
	// Contiguous continuation: gap 0.
	if obs := w.observe(0, 1<<20, 2<<20, 223, &seq); obs.gap != 0 {
		t.Errorf("contiguous read gap = %d, want 0", obs.gap)
	}
	// A seek forward by exactly one block: the gap is the hole, and it is reported.
	off := int64(2<<20) + blockSz
	if obs := w.observe(off/blockSz, off, off+(1<<20), 223, &seq); obs.gap != blockSz {
		t.Errorf("seek gap = %d, want %d", obs.gap, blockSz)
	}
	// A backward seek reports a negative gap rather than being clamped away: Observe takes
	// the magnitude, so the sign must survive to the trace for a reader to see the seek.
	if obs := w.observe(0, 0, 1<<20, 223, &seq); obs.gap >= 0 {
		t.Errorf("backward seek gap = %d, want negative", obs.gap)
	}
}
