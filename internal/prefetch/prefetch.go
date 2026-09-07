// SPDX-License-Identifier: Apache-2.0

// Package prefetch implements lith's per-open-file-handle access-pattern
// detector and readahead planner. The planner keeps a dispatch frontier that
// leads the demand cursor: blocks are dispatched on open and on every window
// advance, never reactively on the miss that reveals a gap. See the pinned
// Design issue, §4.3, and issue #38.
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

// initialWindow is the number of blocks dispatched at open.
const initialWindow = 2

// Prefetcher tracks one file handle's read pattern and returns the block
// indices that should be prefetched. It maintains a dispatch frontier (the
// next block not yet dispatched) so the readahead window stays ahead of the
// demand cursor.
//
//	cold -> sequential (consecutive blocks)
//	     -> strided    (constant non-unit delta observed twice)
//	     -> random     (anything else)
//
// Sequential grows its window geometrically from 2 up to maxReadahead and
// advances the frontier so frontier-cursor >= window; strided dispatches the
// single next predicted block; random and cold dispatch nothing.
type Prefetcher struct {
	maxReadahead int64

	haveLast  bool
	lastBlock int64
	lastDelta int64
	state     State
	window    int64
	frontier  int64 // next block index not yet dispatched
}

// New returns a Prefetcher with the given max readahead window in blocks
// (values < 2 are raised to 2).
func New(maxReadahead int64) *Prefetcher {
	if maxReadahead < initialWindow {
		maxReadahead = initialWindow
	}
	return &Prefetcher{maxReadahead: maxReadahead, state: Cold}
}

// State returns the current detected pattern.
func (p *Prefetcher) State() State { return p.state }

// Open dispatches the initial readahead window (blocks [0, initialWindow)),
// assuming reads begin near the start of the file. It must be called once,
// before the first Observe, for files worth prefetching.
func (p *Prefetcher) Open() []int64 {
	p.haveLast = true
	p.lastBlock = -1 // so the first read at block 0 registers as sequential (d=1)
	p.state = Cold
	p.window = initialWindow
	p.frontier = initialWindow
	return blockRange(0, initialWindow)
}

// Observe records a demand read at blockIdx and returns the block indices to
// dispatch now (the frontier advance), which is empty when the frontier
// already leads far enough.
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
		// Re-read of the same block: no state change, no dispatch.
		return nil

	case d == 1:
		if p.state == Sequential {
			p.window = min(p.window*2, p.maxReadahead)
		} else {
			p.state = Sequential
			if p.window < initialWindow {
				p.window = initialWindow
			}
		}
		p.lastDelta = 1
		// Never dispatch behind the cursor.
		if p.frontier < blockIdx+1 {
			p.frontier = blockIdx + 1
		}
		return p.advance(blockIdx + 1 + p.window)

	case d == p.lastDelta:
		// Constant non-unit delta seen twice: strided. Dispatch the next
		// predicted block (a single jump, not a contiguous window).
		p.state = Strided
		pred := blockIdx + d
		if pred >= p.frontier {
			p.frontier = pred + 1
			return []int64{pred}
		}
		return nil

	default:
		// New or broken delta: not (yet) a pattern.
		p.lastDelta = d
		p.state = Random
		p.window = 0
		return nil
	}
}

// advance dispatches [frontier, target) and moves the frontier to target.
func (p *Prefetcher) advance(target int64) []int64 {
	if target <= p.frontier {
		return nil
	}
	lo := p.frontier
	p.frontier = target
	return blockRange(lo, target-lo)
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
