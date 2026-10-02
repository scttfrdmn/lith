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
	// streaming is whether this handle is an ESTABLISHED sequential stream -- the
	// detector is in prefetch.Sequential, so it is actually being prefetched for. This is
	// the divisor's input (#301), replacing the open-descriptor count.
	//
	// Kept here, as a bool under the handle's own mutex, rather than recomputed by walking
	// every handle: perHandleWindow runs on every read, and the reporting workload has 6229
	// handles, so an O(handles) scan taking each handle's lock per read would cost more
	// than the misallocation it fixes. Transitions are reported as a delta to a mount-wide
	// atomic instead.
	streaming bool
}

// coverageWindow / coverageMin are the #221 coverage gate parameters, chosen
// from the characterization (streams >= 0.89 at W=16, every scattered walk
// <= 0.07): a wide window is required so field-internal / tiled reads cannot tile
// locally and look sequential, and 0.5 sits in the middle of that margin.
const (
	coverageWindow = 16
	coverageMin    = 0.5
)

func newPFWrapper(maxReadahead, blockSize int64, evidenceRatio float64) *pfWrapper {
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
	// streamDelta is the change this read made to the mount-wide established-stream count:
	// +1 when the handle just became a sequential stream, -1 when it stopped being one, 0
	// otherwise. Computed under the handle's lock with the transition it describes, for the
	// same reason `after` is (#267): read afterwards, it could describe a different read.
	streamDelta int64
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
		dispatch:    d,
		seq:         seq.Add(1),
		after:       w.pf.State(),
		window:      w.pf.Window(),
		peak:        w.pf.PeakWindow(),
		gap:         gap,
		streamDelta: w.updateStreaming(),
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
		streamDelta: w.updateStreaming(),
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

// updateStreaming recomputes whether this handle is an established sequential stream and
// returns the change to the mount-wide count. Called under w.mu, with the Observe whose
// transition it reports.
//
// prefetch.Sequential is exactly the condition "this handle is being prefetched for": the
// window grows geometrically from 2 in that state and nothing is prefetched in Random. So a
// descriptor that is open but never read, or one the detector has given up on, contributes
// nothing to the divisor -- which is the whole correction.
func (w *pfWrapper) updateStreaming() int64 {
	now := w.pf.State() == prefetch.Sequential
	if now == w.streaming {
		return 0
	}
	w.streaming = now
	if now {
		return 1
	}
	return -1
}

// releaseStreaming gives up this handle's share, for the close path. Returns the delta to
// apply to the mount-wide count, and is idempotent so a double Release cannot drive the
// count negative.
//
// This is the half the reverted admission attempt did not have: rawFS.Release deleted the
// handle without releasing its prefetch commitment, so under a hard byte cap one closed
// handle could pin the whole budget forever if the working set fit the memory tier. A
// divisor cannot leak that way -- it is recomputed from a live count rather than
// accumulated -- but the count itself still has to be decremented here or it only ever
// grows.
func (w *pfWrapper) releaseStreaming() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.streaming {
		return 0
	}
	w.streaming = false
	return -1
}
