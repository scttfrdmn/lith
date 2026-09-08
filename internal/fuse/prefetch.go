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

func newPFWrapper(maxReadahead int64) *pfWrapper {
	return &pfWrapper{pf: prefetch.New(maxReadahead)}
}

func (w *pfWrapper) observe(block, maxWindow int64) []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pf.SetMax(maxWindow)
	return w.pf.Observe(block)
}

func (w *pfWrapper) open(maxWindow int64) []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pf.SetMax(maxWindow)
	return w.pf.Open()
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
