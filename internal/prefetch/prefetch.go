// SPDX-License-Identifier: Apache-2.0

// Package prefetch implements lith's per-open-file-handle access-pattern
// detector and readahead planner. The planner keeps a dispatch frontier that
// leads the demand cursor: blocks are dispatched on open and on every window
// advance, never reactively on the miss that reveals a gap. See the pinned
// Design issue, §4.3, and issues #38 and #49.
//
// The detector is tolerant of out-of-order reads: the kernel issues a single
// file handle's readahead concurrently, so reads can arrive at the FUSE layer
// out of order even for a strictly sequential file. A read within the reorder
// band [cursor-window, frontier+window] is treated as sequential progress; only
// a genuine out-of-band jump is a seek, which halves the window (floor 2)
// rather than collapsing it — the full re-ramp was the #49 lone-survivor tail.
package prefetch

// State is the detected access pattern for a file handle.
type State int

const (
	// Cold is the initial state, before a pattern is established.
	Cold State = iota
	// Sequential means consecutive (or in-band reordered) blocks; readahead
	// grows geometrically.
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

// initialWindow is the number of blocks dispatched at open and after a seek.
const initialWindow = 2

// Prefetcher tracks one file handle's read pattern and returns the block
// indices that should be prefetched. It maintains a dispatch frontier (the next
// block index not yet dispatched) so the readahead window stays ahead of the
// demand cursor.
//
//	cold -> sequential (unit step or in-band reorder)
//	     -> strided    (constant non-unit delta observed twice)
//	     -> random     (two out-of-band seeks with no progress between)
//
// Sequential grows its window geometrically from 2 up to maxReadahead; a single
// out-of-band seek halves the window and re-anchors; strided dispatches the
// single next predicted block.
type Prefetcher struct {
	maxReadahead int64

	haveLast  bool
	lastBlock int64
	lastDelta int64
	state     State
	window    int64
	cursor    int64 // highest block demanded
	frontier  int64 // next block index not yet dispatched
	pending   bool  // an unexplained jump is outstanding (one more seeks -> random)

	// Diagnostics (#49). Single-threaded via pfWrapper.
	halvings    int64 // window halvings on a seek from an established pattern
	resetRandom int64 // collapses to Random (a second seek with no progress)
	peakWindow  int64 // largest window ever reached
}

// Halvings reports how many times a seek halved the window.
func (p *Prefetcher) Halvings() int64 { return p.halvings }

// Resets reports how many times the detector collapsed to Random.
func (p *Prefetcher) Resets() int64 { return p.resetRandom }

// PeakWindow reports the largest readahead window this handle ever reached.
func (p *Prefetcher) PeakWindow() int64 { return p.peakWindow }

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

// SetMax updates the readahead window cap (in blocks). The FUSE layer lowers it
// when many handles are open so their windows share the prefetch budget (#55).
// The current window is clamped down to the new cap; the floor is initialWindow.
func (p *Prefetcher) SetMax(n int64) {
	if n < initialWindow {
		n = initialWindow
	}
	p.maxReadahead = n
	if p.window > n {
		p.window = n
	}
}

// Open dispatches the initial readahead window (blocks [0, initialWindow)),
// assuming reads begin near the start of the file. It must be called once,
// before the first Observe, for files worth prefetching.
func (p *Prefetcher) Open() []int64 {
	p.haveLast = true
	p.lastBlock = -1 // so the first read at block 0 registers as sequential (d=1)
	p.state = Cold
	p.window = initialWindow
	p.cursor = -1
	p.frontier = initialWindow
	return blockRange(0, initialWindow)
}

// Observe records a demand read at blockIdx and returns the block indices to
// dispatch now (the frontier advance), empty when the frontier already leads
// far enough.
func (p *Prefetcher) Observe(blockIdx int64) []int64 {
	if !p.haveLast {
		p.haveLast = true
		p.lastBlock = blockIdx
		p.cursor = blockIdx
		return nil
	}
	d := blockIdx - p.lastBlock
	p.lastBlock = blockIdx

	// Re-read of the same block: no state change, no dispatch.
	if d == 0 {
		return nil
	}

	// Strided detection (before the seek rule): a constant non-unit delta seen
	// twice. Not evaluated once a sequential stream is established, so an
	// in-band reorder whose deltas happen to repeat is not misread as a stride.
	if d != 1 && d == p.lastDelta && p.state != Sequential {
		p.state = Strided
		p.pending = false
		p.lastDelta = d
		p.cursor = blockIdx
		pred := blockIdx + d
		if pred >= p.frontier {
			p.frontier = pred + 1
			return []int64{pred}
		}
		return nil
	}

	// Sequential progress: a unit step, or a read within the reorder band.
	if d == 1 || p.inBand(blockIdx) {
		if p.state == Sequential {
			p.window = min(p.window*2, p.maxReadahead)
		} else {
			p.state = Sequential
			p.window = initialWindow
		}
		p.pending = false
		p.lastDelta = 1
		if blockIdx > p.cursor {
			p.cursor = blockIdx
		}
		if p.window > p.peakWindow {
			p.peakWindow = p.window
		}
		if p.frontier < p.cursor+1 {
			p.frontier = p.cursor + 1
		}
		return p.advance(p.cursor + 1 + p.window)
	}

	// Out of band: a seek.
	p.lastDelta = d
	if p.pending {
		// A second unexplained jump with no sequential progress between: stop
		// prefetching until a sequential run resumes.
		p.state = Random
		p.pending = false
		p.window = 0
		p.cursor = blockIdx
		p.frontier = blockIdx
		p.resetRandom++
		return nil
	}
	if p.state == Sequential || p.state == Strided {
		// One jump out of an established pattern: halve the window (floor 2) —
		// not a full reset to zero, which is the #49 lone-survivor re-ramp — and
		// re-anchor at b with the initial 2-block dispatch.
		p.window = max(initialWindow, p.window/2)
		p.halvings++
		if p.window > p.peakWindow {
			p.peakWindow = p.window
		}
		p.state = Sequential
		p.pending = true
		p.cursor = blockIdx
		p.frontier = blockIdx
		return p.advance(blockIdx + initialWindow)
	}
	// From Cold/Random with no pending jump: tentative — wait to see if this is
	// a stride or a seek before committing.
	p.pending = true
	return nil
}

// inBand reports whether b is within the reorder band around the current
// cursor/frontier: [cursor-window, frontier+window], with at least the initial
// window of slack so re-entry from a zero window still recognises progress.
func (p *Prefetcher) inBand(b int64) bool {
	w := p.window
	if w < initialWindow {
		w = initialWindow
	}
	return b >= p.cursor-w && b <= p.frontier+w
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
