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

// prefetchBudget bounds the bytes held for prefetch that a demand read has not
// yet consumed (in-flight prefetch fills plus fetched-but-unread chunks), so
// aggregate readahead cannot exceed a fraction of the memory tier and thrash it
// (#55). A reservation blocks until the budget has room, so a prefetch goroutine
// simply waits (staying ahead of demand) rather than skipping a block — skipping
// would leave the per-handle prefetch frontier advanced past an un-fetched
// block, turning it into an uncovered demand miss (the #55 small-RAM collapse).
// A stopped store (Close) wakes all waiters.
type prefetchBudget struct {
	mu      sync.Mutex
	cond    *sync.Cond
	avail   int64
	cap     int64
	stopped bool
}

func newPrefetchBudget(capacity int64) *prefetchBudget {
	if capacity <= 0 {
		return nil
	}
	b := &prefetchBudget{avail: capacity, cap: capacity}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// reserve blocks until n bytes are available (n clamped to the cap), then holds
// them. It returns false only if the budget was stopped while waiting.
func (p *prefetchBudget) reserve(n int64) bool {
	if p == nil {
		return true // budget disabled: never gate
	}
	if n > p.cap {
		n = p.cap
	}
	if n <= 0 {
		return true
	}
	p.mu.Lock()
	for p.avail < n && !p.stopped {
		p.cond.Wait()
	}
	if p.stopped {
		p.mu.Unlock()
		return false
	}
	p.avail -= n
	p.mu.Unlock()
	return true
}

// release returns n bytes to the budget and wakes a waiter.
func (p *prefetchBudget) release(n int64) {
	if p == nil || n <= 0 {
		return
	}
	p.mu.Lock()
	p.avail += n
	if p.avail > p.cap {
		p.avail = p.cap
	}
	p.mu.Unlock()
	p.cond.Signal()
}

// used reports the reserved bytes (for tests/metrics).
func (p *prefetchBudget) usedBytes() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cap - p.avail
}

// stop wakes every blocked reserver so shutdown does not hang.
func (p *prefetchBudget) stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.cond.Broadcast()
}
