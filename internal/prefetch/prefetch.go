// SPDX-License-Identifier: Apache-2.0

// Package prefetch implements lith's per-open-file-handle access-pattern
// detector and readahead planner. See the pinned Design issue, §4.3.
package prefetch

// State is the detected access pattern for a file handle.
type State int

const (
	// Cold is the initial state, before a pattern is established.
	Cold State = iota
	// Sequential means consecutive blocks; readahead grows geometrically.
	Sequential
	// Strided means a constant non-unit block delta seen at least twice.
	Strided
	// Random means no detectable pattern; nothing is prefetched.
	Random
)

func (s State) String() string {
	switch s {
	case Cold:
		return "cold"
	case Sequential:
		return "sequential"
	case Strided:
		return "strided"
	default:
		return "random"
	}
}

// Prefetcher tracks one file handle's read pattern and, given each demand
// block, returns the block indices that should be prefetched ahead of it.
//
//	cold -> sequential (two consecutive blocks)
//	     -> strided    (constant delta observed twice)
//	     -> random     (anything else)
//
// Sequential grows its readahead window geometrically from 2 up to
// maxReadahead; strided prefetches only the single next predicted block;
// random and cold prefetch nothing.
type Prefetcher struct {
	maxReadahead int64

	haveLast  bool
	lastBlock int64
	lastDelta int64
	state     State
	window    int64
}

// New returns a Prefetcher with the given max readahead window in blocks
// (values < 2 are raised to 2).
func New(maxReadahead int64) *Prefetcher {
	if maxReadahead < 2 {
		maxReadahead = 2
	}
	return &Prefetcher{maxReadahead: maxReadahead, state: Cold}
}

// State returns the current detected pattern.
func (p *Prefetcher) State() State { return p.state }

// Observe records a demand read at blockIdx and returns the block indices to
// prefetch (possibly empty). Callers issue lower-priority fetches for them.
func (p *Prefetcher) Observe(blockIdx int64) []int64 {
	if !p.haveLast {
		p.haveLast = true
		p.lastBlock = blockIdx
		return nil
	}
	d := blockIdx - p.lastBlock
	p.lastBlock = blockIdx

	switch {
	case d == 0:
		// Re-read of the same block: no state change, no prefetch.
		return nil

	case d == 1:
		if p.state == Sequential {
			p.window = min(p.window*2, p.maxReadahead)
		} else {
			p.state = Sequential
			p.window = 2
		}
		p.lastDelta = 1
		return blockRange(blockIdx+1, p.window)

	case d == p.lastDelta:
		// A constant non-unit delta seen twice: strided. Predict the next.
		p.state = Strided
		return []int64{blockIdx + d}

	default:
		// New or broken delta: not (yet) a pattern. Record the delta so a
		// second identical one promotes to strided.
		p.lastDelta = d
		p.state = Random
		p.window = 0
		return nil
	}
}

// blockRange returns [start, start+count).
func blockRange(start, count int64) []int64 {
	if count <= 0 {
		return nil
	}
	out := make([]int64, count)
	for i := int64(0); i < count; i++ {
		out[i] = start + i
	}
	return out
}
