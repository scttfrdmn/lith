// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scttfrdmn/lith/internal/prefetch"
)

// The established-stream count is maintained by DELTAS rather than recomputed, because
// perHandleWindow runs on every read and the reporting workload has 6229 handles — an
// O(handles) scan taking each handle's lock per read would cost more than the
// misallocation it fixes (#301).
//
// Deltas can drift; a scan cannot. So the arithmetic is what needs testing: every
// transition reported exactly once, and the count conserved across establish, collapse,
// and close.
func TestStreamDeltaIsReportedOncePerTransition(t *testing.T) {
	const blockSize = int64(1) << 20
	w := newPFWrapper(223, blockSize, 0, coverageMin)
	var seq atomic.Int64
	var count int64

	// A sequential walk. The handle establishes once, and only once, however many
	// sequential reads follow — a delta per read would inflate the divisor without bound
	// and drive every reader to the floor.
	establishes := 0
	for i := int64(0); i < 32; i++ {
		off := i * blockSize
		obs := w.observe(i, off, off+blockSize, 223, 0, &seq)
		count += obs.streamDelta
		if obs.streamDelta == 1 {
			establishes++
		}
		if obs.streamDelta != 0 && obs.streamDelta != 1 && obs.streamDelta != -1 {
			t.Fatalf("read %d: streamDelta = %d, want -1, 0 or +1", i, obs.streamDelta)
		}
	}
	if establishes != 1 {
		t.Errorf("a 32-read sequential walk reported %d establishments, want exactly 1", establishes)
	}
	if count != 1 {
		t.Errorf("count after a sequential walk = %d, want 1", count)
	}
	if w.state() != prefetch.Sequential {
		t.Fatalf("fixture: the handle is not Sequential after 32 contiguous reads (state %v)", w.state())
	}

	// Closing gives the share back, and is idempotent: a double Release must not drive the
	// count negative, which would hand the next reader a window computed from a divisor
	// smaller than the number of streams actually running.
	count += w.releaseStreaming()
	if count != 0 {
		t.Errorf("count after close = %d, want 0", count)
	}
	if d := w.releaseStreaming(); d != 0 {
		t.Errorf("second releaseStreaming returned %d, want 0 (must be idempotent)", d)
	}
}

// A handle the detector never establishes must never be counted. This is the case the old
// divisor got wrong by construction: a descriptor opened and never read was a full divisor.
func TestNeverEstablishedHandleIsNeverCounted(t *testing.T) {
	w := newPFWrapper(223, 1<<20, 0, coverageMin)
	// Opened, never read.
	if d := w.releaseStreaming(); d != 0 {
		t.Errorf("closing a never-read handle returned %d, want 0", d)
	}
	if w.streaming.Load() {
		t.Error("a never-read handle reports itself as streaming")
	}
}

// A handle that collapses to Random gives its share back without being closed, so a reader
// that stops being sequential stops charging for depth it is not using.
func TestCollapseToRandomReleasesTheShare(t *testing.T) {
	const blockSize = int64(1) << 20
	w := newPFWrapper(223, blockSize, 0, coverageMin)
	var seq atomic.Int64
	var count int64
	for i := int64(0); i < 32; i++ {
		off := i * blockSize
		count += w.observe(i, off, off+blockSize, 223, 0, &seq).streamDelta
	}
	if count != 1 {
		t.Fatalf("fixture: count = %d after a sequential walk, want 1", count)
	}

	// Scattered reads far apart, each a seek. The detector collapses to Random, and when it
	// does the count must come back down.
	for i := int64(0); i < 64 && count != 0; i++ {
		off := (1000 + i*997) * blockSize
		count += w.observe(off/blockSize, off, off+4096, 223, 0, &seq).streamDelta
	}
	if w.state() == prefetch.Sequential {
		t.Skipf("the detector stayed Sequential through 64 scattered reads; this fixture no "+
			"longer produces a collapse (state %v)", w.state())
	}
	if count != 0 {
		t.Errorf("the handle collapsed to %v but the count is %d, want 0: a handle that is no "+
			"longer prefetched for is still charging for a share", w.state(), count)
	}
}

// The count must survive concurrent reads on ONE handle. The kernel may issue several reads
// against a single file handle at once, which is the whole reason pfWrapper exists, and a
// transition double-reported under that concurrency would permanently skew the divisor.
func TestStreamDeltaUnderConcurrentReadsOnOneHandle(t *testing.T) {
	const blockSize = int64(1) << 20
	w := newPFWrapper(223, blockSize, 0, coverageMin)
	var seq atomic.Int64
	var count atomic.Int64

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := int64(0); i < 64; i++ {
				blk := int64(g)*64 + i
				off := blk * blockSize
				count.Add(w.observe(blk, off, off+blockSize, 223, 0, &seq).streamDelta)
			}
		}(g)
	}
	wg.Wait()

	// Whatever state the interleaving produced, the count must AGREE with it: 1 if the
	// handle is streaming, 0 if not. Any other value is a lost or duplicated delta.
	want := int64(0)
	if w.streaming.Load() {
		want = 1
	}
	if got := count.Load(); got != want {
		t.Errorf("after 512 concurrent reads on one handle: count = %d, want %d "+
			"(streaming=%v) — a delta was lost or double-reported",
			got, want, w.streaming.Load())
	}
	count.Add(w.releaseStreaming())
	if got := count.Load(); got != 0 {
		t.Errorf("count after close = %d, want 0", got)
	}
}
