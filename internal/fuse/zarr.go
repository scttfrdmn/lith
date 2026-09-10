// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"path"
	"sync"

	"github.com/scttfrdmn/lith/internal/blockstore"
	fzarr "github.com/scttfrdmn/lith/internal/format/zarr"
)

// Zarr grid-aware readahead (#70). A chunked store (Zarr) is many small objects
// named by their grid coordinates (`<dir>/i.j.k`). Key-order sibling readahead
// (#63) walks only the last axis; when the app walks another axis every step is
// an uncovered miss.
//
// Tier 1 (#87): infer the walked axis from the last two opens and prefetch the
// next N chunks along it. It still stalls the app's bounded worker pool at every
// grid-row boundary (session-21 diagnosis).
//
// Tier 2 (#70, #109): after K opens, classify each axis as fixed or varying and
// prefetch the whole **plane** (the selection) — not the walk. An open outside
// the plane re-plans. The plane is issued in walk order, budget-bounded through
// Limits and via the parts path, and the sibling-unread guardrail applies. The
// grid comes from the store's consolidated `.zmetadata` (one small object) with
// a per-array `.zarray` fallback. Metadata is attacker-controlled and parsed
// under the #101 rule (size cap, bounds-checked, fuzzed) in internal/format/zarr.

const (
	zarrK        = fzarr.DefaultK        // opens before axis classification
	zarrMinPlane = fzarr.DefaultMinPlane // below this, tier-1 line behavior
)

// zarrState caches, per array directory, the parsed grid and the tier-2 planner,
// plus per-store consolidated metadata. Guarded by its own mutex.
type zarrState struct {
	mu      sync.Mutex
	grid    map[string]*fzarr.Grid     // dir -> grid; entry present with nil = "not zarr"
	checked map[string]bool            // dir -> we've attempted grid detection
	last    map[string][]int           // dir -> last chunk coords (tier-1 fallback)
	planner map[string]*fzarr.Planner  // dir -> tier-2 planner
	issued  map[string]map[string]bool // dir -> chunk basenames already plane-dispatched
	// consolidated caches parsed .zmetadata per store root; consChecked marks a
	// store root as already attempted (so a missing .zmetadata is not re-read).
	consChecked map[string]bool
}

func newZarrState() *zarrState {
	return &zarrState{
		grid: map[string]*fzarr.Grid{}, checked: map[string]bool{},
		last: map[string][]int{}, planner: map[string]*fzarr.Planner{},
		issued: map[string]map[string]bool{}, consChecked: map[string]bool{},
	}
}

// gridFor returns the cached chunk grid for an array directory, detecting it on
// first use from the store's consolidated `.zmetadata` (preferred, one object
// for the whole store) or the per-array `.zarray`. A nil grid (cached) means
// "not a parseable Zarr variable". Detection reads at most one small object per
// store (`.zmetadata`) or per dir (`.zarray`).
func (f *rawFS) gridFor(dir string) *fzarr.Grid {
	f.zarr.mu.Lock()
	if f.zarr.checked[dir] {
		g := f.zarr.grid[dir]
		f.zarr.mu.Unlock()
		return g
	}
	f.zarr.mu.Unlock()

	g := f.consolidatedGrid(dir)
	if g == nil {
		g = f.zarrayGrid(dir)
	}
	f.zarr.mu.Lock()
	f.zarr.checked[dir] = true
	f.zarr.grid[dir] = g
	f.zarr.mu.Unlock()
	return g
}

// consolidatedGrid finds the store root for dir (the nearest ancestor with a
// `.zmetadata`), parses it once (size-capped), caches every array's grid keyed
// by its full directory, and returns the grid for dir (or nil).
func (f *rawFS) consolidatedGrid(dir string) *fzarr.Grid {
	// Walk up from dir looking for a `.zmetadata`, bounded.
	root := ""
	found := false
	cur := dir
	for i := 0; i < 16; i++ {
		rel := path.Join(cur, ".zmetadata")
		if fi, err := f.ix.Stat("/" + rel); err == nil && !fi.IsDir && fi.Size > 0 && fi.Size < fzarr.MaxConsolidatedBytes {
			root, found = cur, true
			break
		}
		if cur == "" {
			break
		}
		cur = dirOf(cur)
	}
	if !found {
		return nil
	}

	f.zarr.mu.Lock()
	already := f.zarr.consChecked[root]
	f.zarr.mu.Unlock()
	if already {
		f.zarr.mu.Lock()
		g := f.zarr.grid[dir]
		f.zarr.mu.Unlock()
		return g
	}

	rel := path.Join(root, ".zmetadata")
	fi, err := f.ix.Stat("/" + rel)
	if err != nil {
		return nil
	}
	key := blockstore.Key{Key: f.objectKey(rel), ETagHash: f.ix.ETagHashOf("/" + rel)}
	body, err := f.store.GetRange(f.ctx, key, 0, fi.Size, fi.Size)
	if err != nil {
		return nil
	}
	grids, ok := fzarr.ParseConsolidated(body)
	f.zarr.mu.Lock()
	f.zarr.consChecked[root] = true
	if ok {
		f.met.FormatDetect("zarr")
		for arrayDir, g := range grids {
			full := path.Join(root, arrayDir) // arrayDir is relative to the store root
			f.zarr.checked[full] = true
			f.zarr.grid[full] = g
		}
	}
	g := f.zarr.grid[dir]
	f.zarr.mu.Unlock()
	return g
}

// zarrayGrid reads and parses <dir>/.zarray (the per-array fallback).
func (f *rawFS) zarrayGrid(dir string) *fzarr.Grid {
	rel := path.Join(dir, ".zarray") // "" dir -> ".zarray"
	fi, err := f.ix.Stat("/" + rel)
	if err != nil || fi.IsDir || fi.Size <= 0 || fi.Size >= fzarr.MaxConsolidatedBytes {
		return nil
	}
	f.met.FormatDetect("zarr")
	key := blockstore.Key{Key: f.objectKey(rel), ETagHash: f.ix.ETagHashOf("/" + rel)}
	body, err := f.store.GetRange(f.ctx, key, 0, fi.Size, fi.Size)
	if err != nil {
		return nil
	}
	if g, ok := fzarr.ParseArray(body); ok {
		return g
	}
	return nil
}

// maybeZarrReadahead handles the open of a Zarr chunk. It returns true when the
// directory is a Zarr variable (so the caller must NOT also run key-order
// sibling readahead), and false when the dir is not Zarr (fall back to
// key-order). Tier 2 prefetches the classified plane; before K opens, or for a
// plane below the minimum, it falls back to tier-1 line readahead.
func (f *rawFS) maybeZarrReadahead(relPath string) bool {
	if f.cfg.SiblingReadahead <= 0 || f.cfg.Limits == nil {
		return false
	}
	base := path.Base(relPath)
	coords, ok := fzarr.ParseCoords(base)
	if !ok {
		return false // not a chunk name
	}
	dir := dirOf(relPath)
	grid := f.gridFor(dir)
	if grid == nil || len(grid.Dims) != len(coords) {
		return false // not a parseable Zarr variable -> key-order fallback
	}

	f.sibMu.Lock()
	f.markSiblingUsedLocked(relPath)
	f.sibMu.Unlock()

	// Tier 2: feed the open to the per-dir planner and get the plane to prefetch.
	f.zarr.mu.Lock()
	last := f.zarr.last[dir]
	f.zarr.last[dir] = coords
	pl := f.zarr.planner[dir]
	if pl == nil {
		pl = fzarr.NewPlanner(grid.Dims, zarrK, zarrMinPlane)
		f.zarr.planner[dir] = pl
	}
	plan, replanned := pl.Observe(coords)
	if replanned {
		f.zarr.issued[dir] = map[string]bool{} // new plane -> re-issue fresh
	}
	issuedSet := f.zarr.issued[dir]
	if issuedSet == nil {
		issuedSet = map[string]bool{}
		f.zarr.issued[dir] = issuedSet
	}
	// Snapshot the chunks to dispatch (in plan order) that we haven't issued yet.
	var todo [][]int
	for _, c := range plan {
		if !issuedSet[fzarr.FormatCoords(c)] {
			todo = append(todo, c)
		}
	}
	f.zarr.mu.Unlock()

	if replanned {
		f.met.FormatReplan()
	}

	if plan == nil {
		// Not enough opens yet, or plane below the minimum: tier-1 line behavior.
		return f.zarrTier1(dir, coords, last, grid)
	}

	// Dispatch the plane in walk order, budget-bounded via the parts path.
	var dispatched []string
	issued := 0
	for _, c := range todo {
		rel := path.Join(dir, fzarr.FormatCoords(c))
		fi, err := f.ix.Stat("/" + rel)
		if err != nil || fi.IsDir || fi.Size <= 0 || fi.Size > f.cfg.SmallFile {
			continue
		}
		key := blockstore.Key{Key: f.objectKey(rel), ETagHash: f.ix.ETagHashOf("/" + rel)}
		if !f.prefetchWhole(key, fi.Size) {
			break // budget exhausted: the rest is offered again as chunks are consumed
		}
		f.noteSiblingIssued(rel)
		dispatched = append(dispatched, fzarr.FormatCoords(c))
		issued++
	}
	if issued > 0 {
		f.met.FormatPlaneChunks(int64(issued))
		f.zarr.mu.Lock()
		for _, k := range dispatched {
			f.zarr.issued[dir][k] = true
		}
		f.zarr.mu.Unlock()
	}
	return true
}

// zarrTier1 is the tier-1 fallback: prefetch the next N chunks along the single
// walked axis (or fall back to key-order sibling readahead when the walk is
// ambiguous). Returns whether this open is a Zarr open (true unless ambiguous,
// in which case key-order is used instead).
func (f *rawFS) zarrTier1(dir string, coords, last []int, grid *fzarr.Grid) bool {
	if walkedAxis(coords, last) == -1 {
		return false // ambiguous -> key-order readahead (#63)
	}
	for _, rel := range planZarrChunks(dir, coords, last, grid, f.cfg.SiblingReadahead) {
		fi, err := f.ix.Stat("/" + rel)
		if err != nil || fi.IsDir || fi.Size <= 0 || fi.Size > f.cfg.SmallFile {
			continue
		}
		key := blockstore.Key{Key: f.objectKey(rel), ETagHash: f.ix.ETagHashOf("/" + rel)}
		if !f.prefetchWhole(key, fi.Size) {
			break
		}
		f.noteSiblingIssued(rel)
	}
	return true
}

// walkedAxis returns the single axis whose coordinate advanced from last to
// coords, or -1 if ambiguous (no previous open, a backward step, or >1 axis
// changed).
func walkedAxis(coords, last []int) int {
	if len(last) != len(coords) {
		return -1
	}
	axis := -1
	for i := range coords {
		if coords[i] != last[i] {
			if axis != -1 || coords[i] < last[i] {
				return -1
			}
			axis = i
		}
	}
	return axis
}

// planZarrChunks returns the relative keys of the next n chunks after coords
// along the walked axis, in grid order, clamped to the grid bounds.
func planZarrChunks(dir string, coords, last []int, grid *fzarr.Grid, n int) []string {
	axis := walkedAxis(coords, last)
	if axis == -1 {
		return nil
	}
	var out []string
	for d := 1; d <= n; d++ {
		v := coords[axis] + d
		if v >= grid.Dims[axis] {
			break
		}
		next := append([]int(nil), coords...)
		next[axis] = v
		out = append(out, path.Join(dir, fzarr.FormatCoords(next)))
	}
	return out
}
