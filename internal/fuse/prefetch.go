// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"sync"
	"sync/atomic"

	"github.com/scttfrdmn/lith/internal/prefetch"
)

// pfWrapper makes a prefetch.Prefetcher safe for the concurrent reads the
// kernel may issue against a single file handle.
type pfWrapper struct {
	mu sync.Mutex
	pf *prefetch.Prefetcher
	// lastReadEnd is the byte offset just past the previous read the prefetcher was
	// driven with. It lives here, under the same mutex that serializes the handle's
	// decisions, because the byte gap is an INPUT to Observe and has to be computed and
	// updated atomically with it (#278). It was previously an atomic.Int64 on the handle,
	// loaded before the lock and stored after: two concurrent reads on one handle both
	// saw the same stale endpoint, and whichever store landed last could move the
	// endpoint BACKWARDS, yielding a gap matching no serialization of the reads. That gap
	// gates the sequential branch (absInt64(byteGap) <= seqGapMax at
	// internal/prefetch/prefetch.go:425), so a genuinely sequential handle read
	// concurrently enough could synthesize a gap over the threshold, be classified as
	// seeking, and lose the readahead it had earned.
	lastReadEnd int64
}

// coverageWindow / coverageMin are the #221 coverage gate parameters, chosen
// from the characterization (streams >= 0.89 at W=16, every scattered walk
// <= 0.07): a wide window is required so field-internal / tiled reads cannot tile
// locally and look sequential, and 0.5 sits in the middle of that margin.
const (
	coverageWindow = 16
	coverageMin    = 0.5
)

func newPFWrapper(maxReadahead, blockSize int64, evidenceRatio float64, rtBytes int64) *pfWrapper {
	pf := prefetch.New(maxReadahead)
	// A read landing more than one block past the previous is a seek, not
	// sequential progress, however in-band it looks (#210/M16 1b).
	pf.SetGapMax(blockSize)
	// A punctate handle (low coverage over a trailing window) is not sequential
	// however its block deltas look; force it Random so the seek path stops
	// re-anchoring and prefetching across a scattered walk (#221).
	pf.SetCoverage(coverageWindow, coverageMin)
	// Bound a committed window by the bytes the handle has actually read, so
	// ~384 KiB of contiguous evidence cannot buy a NIC-sized bet (#256). Off by
	// default; ratio <= 0 is a no-op.
	pf.SetEvidence(evidenceRatio, blockSize)
	// Floor the evidence cap at one first-byte round trip of reading rather than the
	// constant 2 blocks (#256). A window under a round trip cannot keep a reader fed,
	// and cross-region that turned a flat cost into a bimodal 4 s / 20 s race.
	if rtBytes > 0 && blockSize > 0 {
		pf.SetEvidenceFloor((rtBytes + blockSize - 1) / blockSize)
	}
	return &pfWrapper{pf: pf}
}

// observation is everything a trace row needs about one decision, captured while
// the handle's lock is still held (#267). Reading `after`/`window`/`peak` after
// observe() returned meant that on a handle with concurrent reads those three
// columns could describe a *different* read's transition, and appending the row
// outside the lock meant rows could be written out of decision order — measured at
// 1.9-2.0% of window rows on a 48-rank capture, some logically impossible
// (recorded gap 0 for an offset the handle had already read past).
//
// `seq` is a mount-wide monotonic decision number, allocated inside the lock. A
// timestamp taken in tracePF could not fix this: it would stamp the *append*, so it
// would record the wrong order faithfully. Per handle this is exact, because one
// handle's decisions are serialized by w.mu; across handles any two decisions that
// are genuinely ordered (one finishes before the other starts) get ordered seqs,
// which is the serialization a shared-cache replay needs.
type observation struct {
	dispatch []int64
	seq      int64
	after    prefetch.State
	window   int64
	peak     int64
	// gap is the byte gap the detector actually saw, computed inside the lock and
	// returned so the trace records the input to the decision rather than a re-derivation
	// of it (#278).
	gap int64
}

// observe drives the detector for one read and returns everything the trace needs, all of
// it captured under the handle's lock. The read's end offset is taken rather than its
// length so the byte gap and the new endpoint are both derived here, atomically with the
// Observe they feed (#278).
func (w *pfWrapper) observe(block, off, end, maxWindow int64, seq *atomic.Int64) observation {
	w.mu.Lock()
	defer w.mu.Unlock()
	gap := off - w.lastReadEnd
	w.lastReadEnd = end
	w.pf.SetMax(maxWindow)
	d := w.pf.Observe(block, off, end-off, gap)
	return observation{
		dispatch: d,
		seq:      seq.Add(1),
		after:    w.pf.State(),
		window:   w.pf.Window(),
		peak:     w.pf.PeakWindow(),
		gap:      gap,
	}
}

// observeContiguous drives the detector with a byte gap of ZERO regardless of the handle's
// last endpoint. The CargoShip path reads a decoded frame stream rather than scattered
// object metadata, so neither the #210/M16-1b byte-gap gate nor the #221 coverage gate
// should fire; it still advances the endpoint so a later windowed read on the same handle
// measures from the right place.
func (w *pfWrapper) observeContiguous(block, off, end, maxWindow int64, seq *atomic.Int64) observation {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastReadEnd = end
	w.pf.SetMax(maxWindow)
	d := w.pf.Observe(block, off, end-off, 0)
	return observation{
		dispatch: d, seq: seq.Add(1), after: w.pf.State(),
		window: w.pf.Window(), peak: w.pf.PeakWindow(), gap: 0,
	}
}

// gapFrom reports the byte gap a read at off would see, without advancing the endpoint.
// Only the trace uses it, for reads the prefetcher is deliberately not driven with (a
// whole-file parts fetch, or a footer handle's byte-exact plan): those rows keep today's
// semantics of a gap measured against the last read the detector DID see.
func (w *pfWrapper) gapFrom(off int64) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return off - w.lastReadEnd
}

// isEstablished reports whether the handle's access pattern is known to tile —
// the #229 gate the open-time parts-fetch consults before committing a broad
// whole-file fetch. Serialized with the concurrent reads.
func (w *pfWrapper) isEstablished() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.Established()
}

func (w *pfWrapper) open(maxWindow int64) []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pf.SetMax(maxWindow)
	return w.pf.Open()
}

// state reports the handle's detected access pattern (for the demand-path
// byte-exact decision, #210/M16). Serialized with the concurrent reads.
func (w *pfWrapper) state() prefetch.State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.State()
}

func (w *pfWrapper) resets() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.Resets()
}

func (w *pfWrapper) halvings() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.Halvings()
}

func (w *pfWrapper) deEstablished() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.DeEstablished()
}

func (w *pfWrapper) evidence() (held, withheld int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.EvidenceHeld(), w.pf.EvidenceWithheld()
}

func (w *pfWrapper) window() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.Window()
}

func (w *pfWrapper) peakWindow() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.PeakWindow()
}
