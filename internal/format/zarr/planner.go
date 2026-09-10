// SPDX-License-Identifier: Apache-2.0

package zarr

// DefaultK is the number of recent opens used to classify axes.
const DefaultK = 3

// DefaultMinPlane is the smallest plane (in chunks) worth prefetching as a
// selection; below it the caller uses tier-1 (next-along-the-walk) behavior.
const DefaultMinPlane = 4

// genCap bounds how many walk-order chunks a single Observe emits, so a huge
// plane (e.g. a whole time axis) costs O(genCap), not O(plane), per open — the
// budget in the FUSE layer further limits what is dispatched, and the rest is
// re-offered as the walk advances.
const genCap = 4096

// Planner tracks the chunk opens in one array directory and produces the plane
// (kept implicit) to prefetch. Not safe for concurrent use; the caller
// serializes per directory.
type Planner struct {
	dims     []int
	k        int
	minPlane int

	recent [][]int         // up to k most-recent opens, oldest first
	opened map[string]bool // every coord opened (excluded from the prefetch list)

	// Current plane, held implicitly: varying[a] marks an axis with every
	// coordinate selected; fixed axes are pinned to fixedVal[a]; primary is the
	// varying axis walked most recently (the fastest-varying / soonest-needed).
	hasPlane bool
	varying  []bool
	fixedVal []int
	primary  int
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
	return &Planner{dims: append([]int(nil), dims...), k: k, minPlane: minPlane, opened: map[string]bool{}}
}

// Observe records an open at coords and returns the ordered list of chunks to
// prefetch (the plane, minus already-opened chunks, in walk order, capped) and
// whether this open triggered a (re)plan. Returns (nil,false) when there are not
// yet k opens or the classified plane is below minPlane — the caller then falls
// back to tier-1 readahead.
func (p *Planner) Observe(coords []int) (plan [][]int, replanned bool) {
	if len(coords) != len(p.dims) {
		return nil, false
	}
	p.opened[coordKey(coords)] = true
	p.recent = append(p.recent, append([]int(nil), coords...))
	if len(p.recent) > p.k {
		p.recent = p.recent[len(p.recent)-p.k:]
	}

	if p.hasPlane && p.inPlane(coords) {
		return p.generate(coords), false // top up as chunks are consumed; not a re-plan
	}
	if len(p.recent) < p.k {
		return nil, false
	}
	varying, fixedVal, primary, size := classify(p.recent, p.dims)
	if size < p.minPlane {
		p.hasPlane = false
		return nil, false
	}
	replanned = p.hasPlane // a non-nil old plane we've fallen out of = a re-plan
	p.hasPlane, p.varying, p.fixedVal, p.primary = true, varying, fixedVal, primary
	return p.generate(coords), replanned
}

// inPlane reports whether coords lies in the current plane (its fixed axes match
// the pinned values). Varying axes are unconstrained.
func (p *Planner) inPlane(coords []int) bool {
	for a := range coords {
		if !p.varying[a] && coords[a] != p.fixedVal[a] {
			return false
		}
	}
	return true
}

// generate emits up to genCap plane chunks in walk order starting just after
// from — primary axis fastest, other varying axes slower — skipping already-
// opened chunks. Fixed axes stay pinned to from's (== fixedVal) coordinate.
func (p *Planner) generate(from []int) [][]int {
	if !p.hasPlane {
		return nil
	}
	// Odometer over the varying axes, primary least-significant (incremented
	// first). Fixed axes never change.
	order := []int{p.primary}
	for a := range p.dims {
		if p.varying[a] && a != p.primary {
			order = append(order, a)
		}
	}
	cur := append([]int(nil), from...)
	out := make([][]int, 0, 64)
	for len(out) < genCap {
		if !odometerInc(cur, order, p.dims) {
			break // exhausted the plane ahead of from
		}
		k := coordKey(cur)
		if p.opened[k] {
			continue
		}
		out = append(out, append([]int(nil), cur...))
	}
	return out
}

// odometerInc advances cur one step over the varying axes in order (index 0 is
// least-significant). Returns false when the most-significant axis carries out
// (the walk has run off the end of the plane).
func odometerInc(cur, order, dims []int) bool {
	for _, a := range order {
		cur[a]++
		if cur[a] < dims[a] {
			return true
		}
		cur[a] = 0
	}
	return false
}

// Plane returns the full current plane (all chunks in the selection), for tests
// and metrics. Bounded to a sane cap; nil when no plane is classified.
func (p *Planner) Plane() [][]int {
	if !p.hasPlane {
		return nil
	}
	var out [][]int
	cur := append([]int(nil), p.fixedVal...)
	var rec func(a int)
	rec = func(a int) {
		if len(out) > (1 << 20) {
			return
		}
		if a == len(p.dims) {
			out = append(out, append([]int(nil), cur...))
			return
		}
		if !p.varying[a] {
			cur[a] = p.fixedVal[a]
			rec(a + 1)
			return
		}
		for v := 0; v < p.dims[a]; v++ {
			cur[a] = v
			rec(a + 1)
		}
	}
	rec(0)
	return out
}

// classify computes, from the recent opens, which axes vary, the pinned value
// of each fixed axis, the primary varying axis (changed on the most recent
// step), and the plane size (product of the varying axes' dims). An implausibly
// large product returns size 0 so the caller skips it.
func classify(recent [][]int, dims []int) (varying []bool, fixedVal []int, primary, size int) {
	n := len(dims)
	varying = make([]bool, n)
	fixedVal = make([]int, n)
	for a := 0; a < n; a++ {
		fixedVal[a] = recent[0][a]
		for _, r := range recent {
			if r[a] != recent[0][a] {
				varying[a] = true
				break
			}
		}
	}
	size = 1
	for a := 0; a < n; a++ {
		if varying[a] {
			size *= dims[a]
			if size <= 0 {
				return varying, fixedVal, 0, 0
			}
		}
	}
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
	return varying, fixedVal, primary, size
}

func coordKey(c []int) string { return FormatCoords(c) }
