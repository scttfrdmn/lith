// SPDX-License-Identifier: Apache-2.0

// Package zarr implements the Zarr chunk-grid access plan (#70). It is pure —
// no FUSE, blockstore, or index imports — so it is exhaustively testable and
// fuzzable in isolation. The FUSE layer feeds it observed chunk opens and gets
// back the set of chunks to prefetch.
//
// Tier 1 (#87) predicts the next chunk along the walked axis and fails at every
// grid-row boundary. Tier 2 (this package) stops predicting the walk and
// prefetches the **selection**: after K opens it classifies each array axis as
// *fixed* (same coordinate in every recent open) or *varying*, and the plane is
// every chunk with the fixed axes pinned and all coordinates on the varying
// axes. An open that falls outside the current plane triggers a re-plan.
//
// The metadata parsers (.zarray, consolidated .zmetadata) treat their input as
// attacker-controlled bytes from a bucket lith does not own (the #101 rule):
// every field is bounds-checked against sane caps and no input panics; a
// malformed document yields (nil,false) and the caller falls back to tier 1.
package zarr

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Caps bound what a hostile metadata document can cost. A real store is far
// under these; anything over is rejected rather than trusted.
const (
	// MaxConsolidatedBytes caps the .zmetadata body the caller will read and
	// hand to ParseConsolidated (a consolidated index of a huge store is still
	// small — NWM chrtout.zarr's is a few hundred KiB).
	MaxConsolidatedBytes = 8 << 20
	maxArrays            = 65536 // arrays in one consolidated document
	maxDims              = 32    // axes in one array (Zarr/NumPy practical limit)
	maxChunksPerAxis     = 1 << 40
)

// Grid is the chunk-grid shape of one array: Dims[i] is the number of chunks
// along axis i (ceil(shape[i]/chunks[i])).
type Grid struct {
	Dims []int
}

// ParseArray parses a `.zarray` JSON body into a Grid. Lenient and bounds-safe:
// any structural problem or out-of-cap value yields (nil,false).
func ParseArray(body []byte) (*Grid, bool) {
	if len(body) == 0 || len(body) > MaxConsolidatedBytes {
		return nil, false
	}
	var z struct {
		Shape  []int64 `json:"shape"`
		Chunks []int64 `json:"chunks"`
	}
	if json.Unmarshal(body, &z) != nil {
		return nil, false
	}
	return gridFrom(z.Shape, z.Chunks)
}

// gridFrom builds a Grid from shape/chunks with full bounds-checking.
func gridFrom(shape, chunks []int64) (*Grid, bool) {
	n := len(shape)
	if n == 0 || n > maxDims || n != len(chunks) {
		return nil, false
	}
	dims := make([]int, n)
	for i := range shape {
		if shape[i] < 0 || chunks[i] <= 0 {
			return nil, false
		}
		d := (shape[i] + chunks[i] - 1) / chunks[i] // ceil
		if d < 0 || d > maxChunksPerAxis {
			return nil, false
		}
		dims[i] = int(d)
	}
	return &Grid{Dims: dims}, true
}

// ParseConsolidated parses a Zarr v2 consolidated `.zmetadata` body into a map
// from array directory (the key with the trailing "/.zarray" stripped; "" for a
// root-level array) to its Grid. Bounds-safe: caps the document size and array
// count, never panics, and returns (nil,false) on any structural problem so the
// caller falls back to per-array `.zarray` or tier 1.
func ParseConsolidated(body []byte) (map[string]*Grid, bool) {
	if len(body) == 0 || len(body) > MaxConsolidatedBytes {
		return nil, false
	}
	// The values under "metadata" are heterogeneous (.zarray, .zattrs, .zgroup),
	// so decode each .zarray entry lazily as json.RawMessage rather than forcing
	// one schema on all of them.
	var doc struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil, false
	}
	if len(doc.Metadata) == 0 || len(doc.Metadata) > maxArrays {
		return nil, false
	}
	out := make(map[string]*Grid)
	for k, raw := range doc.Metadata {
		if !strings.HasSuffix(k, ".zarray") {
			continue
		}
		if len(raw) > MaxConsolidatedBytes {
			return nil, false
		}
		var z struct {
			Shape  []int64 `json:"shape"`
			Chunks []int64 `json:"chunks"`
		}
		if json.Unmarshal(raw, &z) != nil {
			continue // skip a single malformed array; don't fail the whole store
		}
		g, ok := gridFrom(z.Shape, z.Chunks)
		if !ok {
			continue
		}
		// "streamflow/.zarray" -> "streamflow"; ".zarray" -> "".
		dir := strings.TrimSuffix(k, ".zarray")
		dir = strings.TrimSuffix(dir, "/")
		out[dir] = g
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// ParseCoords parses a Zarr v2 chunk basename ("0.0.5") into integer grid
// coordinates. Returns false for metadata (".zarray") or non-chunk names.
func ParseCoords(base string) ([]int, bool) {
	if base == "" || strings.HasPrefix(base, ".") {
		return nil, false
	}
	parts := strings.Split(base, ".")
	if len(parts) > maxDims {
		return nil, false
	}
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

// FormatCoords renders grid coordinates as a Zarr v2 chunk basename.
func FormatCoords(coords []int) string {
	parts := make([]string, len(coords))
	for i, c := range coords {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, ".")
}
