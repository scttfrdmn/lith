// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"sync"

	"github.com/scttfrdmn/lith/internal/prefetch"
)

// pfWrapper makes a prefetch.Prefetcher safe for the concurrent reads the
// kernel may issue against a single file handle.
type pfWrapper struct {
	mu sync.Mutex
	pf *prefetch.Prefetcher
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

func (w *pfWrapper) observe(block, off, length, byteGap, maxWindow int64) []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pf.SetMax(maxWindow)
	return w.pf.Observe(block, off, length, byteGap)
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

func (w *pfWrapper) peakWindow() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.PeakWindow()
}
