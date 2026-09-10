// SPDX-License-Identifier: Apache-2.0

package zarr

import "sort"

// DefaultK is the number of recent opens used to classify axes.
const DefaultK = 3

// DefaultMinPlane is the smallest plane (in chunks) worth prefetching as a
// selection; below it the caller uses tier-1 (next-along-the-walk) behavior.
const DefaultMinPlane = 4

// Planner tracks the chunk opens in one array directory and produces the plane
// to prefetch. It is not safe for concurrent use; the caller serializes per
// directory (the FUSE layer holds a per-dir lock).
type Planner struct {
	dims     []int
	k        int
	minPlane int

	recent  [][]int         // up to k most-recent opens, oldest first
	opened  map[string]bool // every coord opened (excluded from the prefetch list)
	plane   map[string]bool // current plane membership; nil until classified
	primary int             // varying axis that changed on the most recent step (ordering)
}

// NewPlanner returns a Planner for an array of the given chunk-grid dims. k<=0
// and minPlane<=0 use the defaults.
func NewPlanner(dims []int, k, minPlane int) *Planner {
	if k <= 0 {
		k = DefaultK
	}
	if minPlane <= 0 {
		minPlane = DefaultMinPlane
	}
	d := append([]int(nil), dims...)
	return &Planner{dims: d, k: k, minPlane: minPlane, opened: map[string]bool{}}
}

// Observe records an open at coords and returns the ordered list of chunks to
// prefetch (the current plane, minus chunks already opened, in walk order) and
// whether this open triggered a (re)plan. It returns (nil,false) when there are
// not yet k opens, or the classified plane is smaller than minPlane — in both
// cases the caller falls back to tier-1 readahead.
func (p *Planner) Observe(coords []int) (plan [][]int, replanned bool) {
	if len(coords) != len(p.dims) {
		return nil, false
	}
	p.opened[coordKey(coords)] = true
	p.recent = append(p.recent, append([]int(nil), coords...))
	if len(p.recent) > p.k {
		p.recent = p.recent[len(p.recent)-p.k:]
	}

	// In-plane open: keep the plane, just re-offer the remaining (un-opened)
	// chunks in walk order from here so the caller can top up the budget as
	// chunks are consumed. Not a re-plan.
	if p.plane != nil && p.plane[coordKey(coords)] {
		return p.remaining(coords), false
	}

	if len(p.recent) < p.k {
		return nil, false // not enough opens to classify yet
	}

	plane, primary, size := classify(p.recent, p.dims)
	if size < p.minPlane {
		p.plane = nil // too small to treat as a selection; tier-1 territory
		return nil, false
	}
	// Reaching here with a non-nil old plane means this open fell outside it —
	// a genuine re-plan. A nil old plane is the first plan (not counted).
	replanned = p.plane != nil
	p.plane = plane
	p.primary = primary
	return p.remaining(coords), replanned
}

// Plane returns the full current plane (all chunks in the selection, including
// already-opened ones), unordered. Nil when no plane is classified. For tests
// and metrics.
func (p *Planner) Plane() [][]int {
	if p.plane == nil {
		return nil
	}
	out := make([][]int, 0, len(p.plane))
	for k := range p.plane {
		out = append(out, parseKey(k))
	}
	return out
}

// remaining returns the plane's chunks that have not been opened, ordered so
// the soonest-needed come first: nearest forward along the primary (most
// recently walked) axis, then successive lines on the other varying axes.
func (p *Planner) remaining(from []int) [][]int {
	var rem [][]int
	for k := range p.plane {
		if p.opened[k] {
			continue
		}
		rem = append(rem, parseKey(k))
	}
	sort.Slice(rem, func(i, j int) bool {
		ki := p.orderKey(from, rem[i])
		kj := p.orderKey(from, rem[j])
		if ki[0] != kj[0] {
			return ki[0] < kj[0] // secondary (other varying) distance
		}
		return ki[1] < kj[1] // primary-axis forward distance
	})
	return rem
}

// orderKey returns (secondaryDistance, primaryDistance) for c relative to the
// current position from. primaryDistance is the forward (wrapping) distance
// along the primary axis; secondaryDistance is the summed forward distance on
// the other axes (dominates the sort so the current primary line finishes
// first).
func (p *Planner) orderKey(from, c []int) [2]int {
	var primary, secondary int
	for a := range c {
		if c[a] == from[a] {
			continue
		}
		d := forwardDist(from[a], c[a], p.dims[a])
		if a == p.primary {
			primary = d
		} else {
			secondary += d
		}
	}
	return [2]int{secondary, primary}
}

// classify computes, from the recent opens, the plane (chunks with fixed axes
// pinned to their shared coordinate and every coordinate on the varying axes),
// the primary varying axis (the one that changed on the most recent step), and
// the plane size (product of the varying axes' dims). A plane too large to
// enumerate safely returns an empty set with size 0.
func classify(recent [][]int, dims []int) (plane map[string]bool, primary, size int) {
	n := len(dims)
	fixedVal := make([]int, n)
	varying := make([]bool, n)
	for a := 0; a < n; a++ {
		fixedVal[a] = recent[0][a]
		for _, r := range recent {
			if r[a] != recent[0][a] {
				varying[a] = true
				break
			}
		}
	}
	// Plane size = product of varying dims; bail if it would overflow a sane cap.
	size = 1
	for a := 0; a < n; a++ {
		if varying[a] {
			size *= dims[a]
			if size <= 0 || size > (1<<20) {
				return map[string]bool{}, 0, 0 // implausibly large selection; skip
			}
		}
	}
	// Primary axis = the varying axis that changed on the most recent step.
	primary = -1
	if len(recent) >= 2 {
		last, prev := recent[len(recent)-1], recent[len(recent)-2]
		for a := 0; a < n; a++ {
			if varying[a] && last[a] != prev[a] {
				primary = a
			}
		}
	}
	if primary == -1 {
		for a := 0; a < n; a++ {
			if varying[a] {
				primary = a
				break
			}
		}
	}
	// Enumerate the plane (row-major over varying axes).
	plane = map[string]bool{}
	cur := append([]int(nil), fixedVal...)
	var rec func(a int)
	rec = func(a int) {
		if a == n {
			plane[coordKey(cur)] = true
			return
		}
		if !varying[a] {
			rec(a + 1)
			return
		}
		for v := 0; v < dims[a]; v++ {
			cur[a] = v
			rec(a + 1)
		}
	}
	rec(0)
	return plane, primary, size
}

// forwardDist is the forward (wrapping) distance from a to b in [0,dim).
func forwardDist(a, b, dim int) int {
	if dim <= 0 {
		return 0
	}
	d := (b - a) % dim
	if d < 0 {
		d += dim
	}
	return d
}

func coordKey(c []int) string { return FormatCoords(c) }
func parseKey(k string) []int { c, _ := ParseCoords(k); return c }
