// SPDX-License-Identifier: Apache-2.0

package prefetch

import (
	"sync/atomic"
	"time"
)

// Limits is the single policy object the prefetch/fill path queries for "how
// much may I have outstanding, and what should I fetch next". It replaces the
// four limits that accreted across sessions 5–12 as separate patches — the
// prefetch-concurrency sub-limit (#48), disk write-behind backpressure (#40),
// the memory-tier prefetch budget / fair-share window (#55), and the BDP
// readahead window (#56) — with one queryable interface, so the next tuning
// change is one edit rather than four (the A4 refactor; #64, Design #1 §4.2).
//
// The three concerns:
//
//   - Budget/Reserve/Release: a mount-wide byte budget for prefetch not yet
//     demanded. Per-handle sequential readahead sizes its window from Budget();
//     sibling readahead (#63) and small-file parallel parts (#69) Reserve
//     against the same budget so no single mechanism can starve the others.
//   - Neighborhood: the next keys in Index order under a file's directory, so a
//     directory being walked in key order can be read ahead across siblings.
//   - Device: the physical limits (NIC bandwidth-delay product, memory-tier
//     size, disk write rate) the sizing decisions derive from.
type Limits interface {
	// Budget returns the mount-wide prefetch budget and how much of it is
	// currently reserved, both in bytes.
	Budget() (total, used int64)
	// Reserve tries to reserve n bytes of prefetch budget, returning false if
	// that would exceed the total (the caller then skips the prefetch). A
	// non-positive n always succeeds and reserves nothing.
	Reserve(n int64) bool
	// Release returns n bytes previously reserved.
	Release(n int64)
	// Neighborhood returns up to n siblings that follow key in Index order under
	// the same directory (direct children only), with their sizes and ETag
	// hashes. It is O(log N + n) against the Index's sorted arena.
	Neighborhood(key string, n int) []Sibling
	// Device reports the physical limits sizing decisions derive from.
	Device() DeviceLimits
}

// Sibling is one neighbor returned by Limits.Neighborhood: an Index-relative
// key (the mount prefix stripped), its size, and its recorded ETag hash — the
// three things needed to fetch it without a second Index lookup.
type Sibling struct {
	Key      string
	Size     int64
	ETagHash uint64
}

// DeviceLimits are the physical limits the policy exposes to consumers. Zero
// fields mean "unknown / not applicable" (e.g. DiskWriteBPS is 0 with no disk
// tier).
type DeviceLimits struct {
	NICBDPBytes   int64 // bytes in flight to fill the NIC (bandwidth-delay product)
	MemCacheBytes int64 // memory-tier capacity
	DiskWriteBPS  int64 // disk-tier sustained write bytes/s, 0 if no disk tier
	// NICBytesPerSec is the NIC baseline bandwidth and TTFB the measured first-byte
	// latency; their product (clamped) is the device-derived coalesce gap
	// (#124/session 30). Zero means unknown.
	NICBytesPerSec int64
	TTFB           time.Duration
}

// Policy is the concrete Limits. The budget is a lock-free reserved-byte
// counter against a fixed cap; the neighborhood is served by an injected
// function (the FUSE layer closes it over the Index, so this package does not
// import index); device limits are fixed at construction.
type Policy struct {
	budgetCap int64
	reserved  atomic.Int64
	dev       DeviceLimits
	neigh     func(key string, n int) []Sibling
}

// NewPolicy builds a Policy. budgetCap <= 0 disables reservations (Reserve
// always fails for positive n, so budget-gated prefetch is off). neigh may be
// nil (Neighborhood then returns nil).
func NewPolicy(budgetCap int64, dev DeviceLimits, neigh func(key string, n int) []Sibling) *Policy {
	return &Policy{budgetCap: budgetCap, dev: dev, neigh: neigh}
}

// Budget returns the total budget and the bytes currently reserved.
func (p *Policy) Budget() (total, used int64) {
	return p.budgetCap, p.reserved.Load()
}

// Reserve reserves n bytes if they fit within the remaining budget.
func (p *Policy) Reserve(n int64) bool {
	if n <= 0 {
		return true
	}
	for {
		cur := p.reserved.Load()
		if cur+n > p.budgetCap {
			return false
		}
		if p.reserved.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// Release returns n bytes to the budget. Over-release is clamped at zero so a
// double Release cannot drive the counter negative.
func (p *Policy) Release(n int64) {
	if n <= 0 {
		return
	}
	for {
		cur := p.reserved.Load()
		next := cur - n
		if next < 0 {
			next = 0
		}
		if p.reserved.CompareAndSwap(cur, next) {
			return
		}
	}
}

// Neighborhood returns up to n siblings following key in Index order.
func (p *Policy) Neighborhood(key string, n int) []Sibling {
	if p.neigh == nil {
		return nil
	}
	return p.neigh(key, n)
}

// Device reports the physical limits.
func (p *Policy) Device() DeviceLimits { return p.dev }
