// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/scttfrdmn/lith/internal/blockstore"
)

// Zarr grid-aware readahead (#70 tier 1) — the smallest useful Format plan.
//
// A chunked store (Zarr) is many small objects named by their grid coordinates,
// e.g. a variable's chunks are `<dir>/i.j.k`. Key-order sibling readahead (#63)
// prefetches the next keys lexically, which walks only the *last* axis; when the
// app walks a different axis (a year over the time axis, say) the next chunk it
// needs is `i+1.j.k`, not the next key `i.j.k+1`, so every axis step is an
// uncovered miss (#63's residual). This shapes readahead to the chunk grid
// instead: detect the variable is Zarr from a `.zarray` sibling (Index-only, no
// S3), read the grid once, infer which axis the app is walking from the last two
// opens, and prefetch the next N chunks along that axis in grid order.
//
// Detect = "a `.zarray` sibling exists"; Plan = "next N chunks along the walked
// axis". Both are cheap and budget-bounded through Limits; the
// lith_sibling_prefetch_unread_total guardrail applies unchanged.

// zarrGrid is the chunk-grid shape of one Zarr variable directory: dims[i] is
// the number of chunks along axis i (ceil(shape[i]/chunks[i])).
type zarrGrid struct {
	dims []int
}

// zarrState caches, per directory, the parsed grid (nil once we've decided the
// dir is not a parseable Zarr variable) and the last chunk coordinates opened
// there. Guarded by its own mutex.
type zarrState struct {
	mu      sync.Mutex
	grid    map[string]*zarrGrid // dir -> grid; entry present with nil = "not zarr"
	last    map[string][]int     // dir -> last chunk coords seen
	checked map[string]bool      // dir -> we've attempted .zarray detection
}

func newZarrState() *zarrState {
	return &zarrState{grid: map[string]*zarrGrid{}, last: map[string][]int{}, checked: map[string]bool{}}
}

// chunkCoords parses a Zarr v2 chunk basename ("0.0.5") into integer grid
// coordinates. Returns false for metadata (".zarray") or non-chunk names.
func chunkCoords(base string) ([]int, bool) {
	if base == "" || strings.HasPrefix(base, ".") {
		return nil, false
	}
	parts := strings.Split(base, ".")
	c := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		c[i] = n
	}
	return c, true
}

// parseZarray reads shape/chunks from a .zarray JSON body and returns the grid
// (number of chunks per axis). It is lenient: any structural problem yields
// (nil,false) so the caller falls back to key-order readahead silently.
func parseZarray(body []byte) (*zarrGrid, bool) {
	var z struct {
		Shape  []int64 `json:"shape"`
		Chunks []int64 `json:"chunks"`
	}
	if json.Unmarshal(body, &z) != nil {
		return nil, false
	}
	if len(z.Shape) == 0 || len(z.Shape) != len(z.Chunks) {
		return nil, false
	}
	dims := make([]int, len(z.Shape))
	for i := range z.Shape {
		if z.Chunks[i] <= 0 || z.Shape[i] < 0 {
			return nil, false
		}
		dims[i] = int((z.Shape[i] + z.Chunks[i] - 1) / z.Chunks[i]) // ceil
	}
	return &zarrGrid{dims: dims}, true
}

// gridFor returns the cached grid for dir, reading and parsing <dir>/.zarray on
// first use. A nil grid (cached) means "not a Zarr variable" — detection is by
// the Index alone (no S3) except for the one-time .zarray read.
func (f *rawFS) gridFor(dir string) *zarrGrid {
	f.zarr.mu.Lock()
	if f.zarr.checked[dir] {
		g := f.zarr.grid[dir]
		f.zarr.mu.Unlock()
		return g
	}
	f.zarr.mu.Unlock()

	var g *zarrGrid
	rel := path.Join(dir, ".zarray") // "" dir -> ".zarray"
	if fi, err := f.ix.Stat("/" + rel); err == nil && !fi.IsDir && fi.Size > 0 && fi.Size < 1<<20 {
		f.met.FormatDetect("zarr")
		key := blockstore.Key{Key: f.objectKey(rel), ETagHash: f.ix.ETagHashOf("/" + rel)}
		if body, err := f.store.GetRange(f.ctx, key, 0, fi.Size, fi.Size); err == nil {
			if parsed, ok := parseZarray(body); ok {
				g = parsed
			}
		}
	}
	f.zarr.mu.Lock()
	f.zarr.checked[dir] = true
	f.zarr.grid[dir] = g
	f.zarr.mu.Unlock()
	return g
}

// maybeZarrReadahead handles the open of a Zarr chunk with grid-aware readahead.
// It returns true when the directory is a Zarr variable (so the caller must NOT
// also run key-order sibling readahead, which is wrong for a chunk grid), and
// false when the dir is not Zarr (fall back to key-order).
func (f *rawFS) maybeZarrReadahead(relPath string) bool {
	if f.cfg.SiblingReadahead <= 0 || f.cfg.Limits == nil {
		return false
	}
	base := path.Base(relPath)
	coords, ok := chunkCoords(base)
	if !ok {
		return false // not a chunk name
	}
	dir := dirOf(relPath)
	grid := f.gridFor(dir)
	if grid == nil || len(grid.dims) != len(coords) {
		return false // not a parseable Zarr variable -> key-order fallback
	}

	// Infer the walked axis from the last two opens in this dir: the single axis
	// whose coordinate advanced. Ambiguous (0 or >1 axes changed) -> prefetch
	// nothing this open, but still claim it (don't do key-order for a Zarr dir).
	f.zarr.mu.Lock()
	last := f.zarr.last[dir]
	f.zarr.last[dir] = coords
	f.zarr.mu.Unlock()
	f.sibMu.Lock()
	f.markSiblingUsedLocked(relPath)
	f.sibMu.Unlock()

	// Prefetch the next N chunks along the walked axis, in grid order.
	for _, rel := range planZarrChunks(dir, coords, last, grid, f.cfg.SiblingReadahead) {
		fi, err := f.ix.Stat("/" + rel)
		if err != nil || fi.IsDir || fi.Size <= 0 || fi.Size > f.cfg.SmallFile {
			continue // missing or too large to whole-fetch
		}
		key := blockstore.Key{Key: f.objectKey(rel), ETagHash: f.ix.ETagHashOf("/" + rel)}
		if !f.prefetchWhole(key, fi.Size) {
			break // budget exhausted
		}
		f.noteSiblingIssued(rel)
	}
	return true
}

// walkedAxis returns the single axis whose coordinate advanced from last to
// coords, or -1 if that is ambiguous (no previous open, a backward step, or
// more than one axis changed — the app isn't cleanly walking one axis yet).
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
// along the axis the app is walking (inferred from last), in grid order,
// clamped to the grid bounds. Empty when the walked axis is ambiguous.
func planZarrChunks(dir string, coords, last []int, grid *zarrGrid, n int) []string {
	axis := walkedAxis(coords, last)
	if axis == -1 {
		return nil
	}
	var out []string
	for d := 1; d <= n; d++ {
		v := coords[axis] + d
		if v >= grid.dims[axis] {
			break // walked off the grid
		}
		parts := make([]string, len(coords))
		for i, c := range coords {
			if i == axis {
				c = v
			}
			parts[i] = strconv.Itoa(c)
		}
		out = append(out, path.Join(dir, strings.Join(parts, ".")))
	}
	return out
}
