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

func newPFWrapper(maxReadahead, blockSize int64) *pfWrapper {
	pf := prefetch.New(maxReadahead)
	// A read landing more than one block past the previous is a seek, not
	// sequential progress, however in-band it looks (#210/M16 1b).
	pf.SetGapMax(blockSize)
	return &pfWrapper{pf: pf}
}

func (w *pfWrapper) observe(block, byteGap, maxWindow int64) []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pf.SetMax(maxWindow)
	return w.pf.Observe(block, byteGap)
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
