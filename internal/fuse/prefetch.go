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

func (w *pfWrapper) observe(block int64) []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pf.Observe(block)
}
