// SPDX-License-Identifier: Apache-2.0

package blockstore

import "sync"

// bytesBudget is a weighted semaphore bounding total bytes in flight, so
// concurrency tracks the network bandwidth-delay product rather than a fixed
// request count. A single acquire larger than the whole budget is clamped to
// the budget (it still proceeds, alone).
type bytesBudget struct {
	mu    sync.Mutex
	cond  *sync.Cond
	avail int64
	cap   int64
}

func newBytesBudget(capacity int64) *bytesBudget {
	if capacity <= 0 {
		return nil
	}
	b := &bytesBudget{avail: capacity, cap: capacity}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire reserves n bytes (clamped to the budget), blocking until available.
func (b *bytesBudget) acquire(n int64) int64 {
	if b == nil {
		return 0
	}
	if n > b.cap {
		n = b.cap
	}
	if n <= 0 {
		return 0
	}
	b.mu.Lock()
	for b.avail < n {
		b.cond.Wait()
	}
	b.avail -= n
	b.mu.Unlock()
	return n
}

// release returns n bytes to the budget.
func (b *bytesBudget) release(n int64) {
	if b == nil || n <= 0 {
		return
	}
	b.mu.Lock()
	b.avail += n
	b.mu.Unlock()
	b.cond.Broadcast()
}
