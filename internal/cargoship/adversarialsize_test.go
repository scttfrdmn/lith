// SPDX-License-Identifier: Apache-2.0

package cargoship

import (
	"strings"
	"testing"
)

// M17-B case 3 (#217): a published dataset where the index and the manifest disagree about a
// file's SIZE.
//
// The index's per-file size comes from the manifest's `size`; its read mapping comes from the
// same entry's `length`/`archive_offset`. Those are two different fields, and for a file that
// is NOT split, nothing compared them: `Resolve` runs its parts-tile-[0,size) check only when
// a file has several parts or its single part does not start at 0.
//
//	if len(vf.Parts) > 1 || vf.Parts[0].FileOffset != 0 { ... verify tiling ... }
//
// So a single-entry file could declare `size: 1000` and `length: 500` and resolve to a
// 1000-byte file with 500 bytes of mapping. The mount then reports 1000 bytes and serves 500,
// with no error — a silently truncated file whose metadata looks right. See
// fuse.TestCargoSizeExceedingItsPartsIsNotSilentlyTruncated for the bytes.
func TestResolveRejectsASizeItsPartsCannotCover(t *testing.T) {
	// One chunk, 4096 uncompressed bytes, so the part is comfortably inside it and the
	// existing bound check cannot be what fires.
	c := framedChunk(1)
	mk := func(size, length int64, totalParts int) *Manifest {
		return &Manifest{Version: "2.1", Prefix: "p", Chunks: []rawChunkEntry{c},
			Files: []rawFileEntry{{
				Path: "p/f", Size: size, Length: length, TotalParts: totalParts,
				ChunkID: 0, S3Key: c.S3Key, ArchiveOffset: p64(0),
			}},
		}
	}

	// THE LIE: declared 1000, mapped 500. One field apart from a valid manifest.
	if _, err := mk(1000, 500, 1).Resolve(); err == nil {
		t.Error("verdict SERVES WRONG: a file declaring 1000 bytes with a 500-byte part " +
			"resolved clean — the mount reports 1000 and can serve only 500")
	} else if !strings.Contains(err.Error(), "size") {
		t.Errorf("rejected, but not on the size disagreement: %v", err)
	}

	// The other direction. A part LONGER than the declared size is a different bug with the
	// same cause: the extra bytes are unreachable, so it is not a wrong answer, but it means
	// the two records were built from different states and nothing else about them can be
	// trusted. Rejected too.
	if _, err := mk(500, 1000, 1).Resolve(); err == nil {
		t.Error("a 1000-byte part under a 500-byte declared size resolved clean")
	}

	// WHAT MUST STILL RESOLVE, because these are the shapes real manifests use:
	//   - length 0 meaning "the whole file" (the common case: Length is only set on splits)
	//   - length == size, written explicitly
	for _, tc := range []struct {
		name         string
		size, length int64
		totalParts   int
	}{
		{"length omitted means whole file", 1000, 0, 0},
		{"length omitted, total_parts 1", 1000, 0, 1},
		{"length written explicitly", 1000, 1000, 1},
		{"zero-length file", 0, 0, 1},
	} {
		if _, err := mk(tc.size, tc.length, tc.totalParts).Resolve(); err != nil {
			t.Errorf("%s: a legal manifest was rejected: %v", tc.name, err)
		}
	}
}

// And the split case must keep working: a real split file's entries carry per-part `length`
// values that sum to `size`, which is exactly the shape a naive "length must equal size"
// check would break. This is the regression the fix above could cause, so it is asserted
// beside it.
func TestResolveStillAcceptsASplitFileWhosePartsSumToItsSize(t *testing.T) {
	c := framedChunk(1) // 4096 uncompressed bytes
	m := &Manifest{Version: "2.1", Prefix: "p", Chunks: []rawChunkEntry{c},
		Files: []rawFileEntry{
			{Path: "p/big", Size: 3000, Offset: 0, Length: 1000, PartIndex: 0, TotalParts: 3,
				ChunkID: 0, S3Key: c.S3Key, ArchiveOffset: p64(0)},
			{Path: "p/big", Size: 3000, Offset: 1000, Length: 1000, PartIndex: 1, TotalParts: 3,
				ChunkID: 0, S3Key: c.S3Key, ArchiveOffset: p64(1000)},
			{Path: "p/big", Size: 3000, Offset: 2000, Length: 1000, PartIndex: 2, TotalParts: 3,
				ChunkID: 0, S3Key: c.S3Key, ArchiveOffset: p64(2000)},
		},
	}
	arch, err := m.Resolve()
	if err != nil {
		t.Fatalf("a valid 3-part split file was rejected: %v", err)
	}
	if len(arch.Files) != 1 {
		t.Fatalf("files=%d, want 1", len(arch.Files))
	}
	if got := arch.Files[0].Size; got != 3000 {
		t.Errorf("split size=%d, want 3000", got)
	}
	if n := len(arch.Files[0].Parts); n != 3 {
		t.Errorf("parts=%d, want 3", n)
	}

	// A split whose parts do NOT sum to the declared size. THE RULING: the parts win, and
	// the declared size is ignored rather than checked — recorded here so the asymmetry
	// with the non-split case is deliberate and visible, not an oversight.
	//
	// It cannot be checked, because `size` on a split entry is ambiguous: it may mean the
	// whole file or just that part. Under the per-part reading the declared value is
	// SUPPOSED to differ from the total, so a disagreement carries no information. The
	// tiled total is the same number under either reading, which is why the parts are
	// authoritative. The non-split case has no such ambiguity — one entry, one size, one
	// length — which is exactly why it is checkable and now checked.
	m.Files[2].Length = 500 // parts now tile [0,2500), size says 3000
	arch, err = m.Resolve()
	if err != nil {
		t.Fatalf("a split file whose parts disagree with its declared size must resolve "+
			"to the parts, not be rejected: %v", err)
	}
	if got := arch.Files[0].Size; got != 2500 {
		t.Errorf("split size=%d, want 2500 (the tiled total, not the declared 3000)", got)
	}
	// And the mount is self-consistent, which is the property that matters: the size it
	// reports is the size its mapping covers, so no reader can get a short read inside it.
	var covered int64
	for _, p := range arch.Files[0].Parts {
		covered += p.Length
	}
	if covered != arch.Files[0].Size {
		t.Errorf("mapping covers %d but size is %d; a split must stay self-consistent "+
			"even when the manifest's declared size does not match", covered, arch.Files[0].Size)
	}
}
