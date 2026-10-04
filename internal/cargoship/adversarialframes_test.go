// SPDX-License-Identifier: Apache-2.0

package cargoship

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, rm rawManifest) []byte {
	t.Helper()
	b, err := json.Marshal(rm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// M17-B case 2 (#217): a manifest whose FRAMES OVERLAP, or whose archive_offset disagrees
// with the frame table. These parse cleanly and describe an inconsistent world, which is the
// class fuzzing cannot generate — the parser is already fuzzed for crashes.
//
// Verdicts are the three #217 asks for: detected (rejected with a clear error), fail-closed
// (serves nothing), or SERVES WRONG. Each case below names which it got.

// framedChunk builds a frame table that tiles a chunk validly, as a baseline every case
// below mutates by exactly one field.
func framedChunk(nFrames int) rawChunkEntry {
	const compLen, uncompLen = int64(1000), int64(4096)
	c := rawChunkEntry{ID: 0, S3Key: "c0.tar.zst",
		UncompressedSize: int64(nFrames) * uncompLen,
		CompressedSize:   int64(nFrames) * compLen,
	}
	for i := range nFrames {
		c.Frames = append(c.Frames, rawFrameEntry{
			CompressedOffset:   int64(i) * compLen,
			CompressedSize:     compLen,
			UncompressedOffset: int64(i) * uncompLen,
			UncompressedSize:   uncompLen,
		})
	}
	return c
}

func parseChunks(t *testing.T, c rawChunkEntry) error {
	t.Helper()
	rm := rawManifest{Version: "2.1", Prefix: "p", Chunks: []rawChunkEntry{c},
		Files: []rawFileEntry{{Path: "p/f", Size: 100, ChunkID: 0, S3Key: c.S3Key, ArchiveOffset: p64(0)}},
	}
	_, err := Parse(mustJSON(t, rm))
	return err
}

// CASE 2a: the UNCOMPRESSED spans overlap. Already DETECTED — the contiguity check that
// requires each frame to start where the last ended rejects it, and this test pins that,
// because it is the reason case 2b below is the only live half.
func TestFrameTableRejectsOverlappingUncompressedSpans(t *testing.T) {
	c := framedChunk(3)
	c.Frames[2].UncompressedOffset -= 1024 // overlaps frame 1's tail
	err := parseChunks(t, c)
	if err == nil {
		t.Fatal("verdict SERVES WRONG: a frame table whose uncompressed spans overlap parsed clean")
	}
	if !strings.Contains(err.Error(), "not contiguous") {
		t.Errorf("verdict detected, but not by the contiguity check: %v", err)
	}
}

// CASE 2b: the COMPRESSED spans overlap, and lith's own comment says they must not.
//
// manifest.go documents the frame-table validation as "frames sorted and contiguous in the
// uncompressed tar stream, COMPRESSED SPANS NON-OVERLAPPING and within a sane bound". Two of
// those three were enforced. The compressed check only ever bounded each span against the
// object size, so nothing stopped two frames from claiming the SAME compressed bytes.
//
// That is not a cosmetic gap. A frame's compressed span is the GET; its uncompressed span is
// where the bytes land in the tar stream. Point frame 1's compressed span at frame 0's bytes
// and frame 1 decodes successfully, to exactly the declared length, with FRAME 0'S CONTENT —
// so a read of the file at frame 1's offset returns another file's bytes. The per-frame
// checksum is no defence: it covers the COMPRESSED bytes, which really are the bytes fetched.
// See TestOverlappingCompressedFramesServeAnotherFramesBytes in internal/blockstore for the
// bytes themselves.
func TestFrameTableRejectsOverlappingCompressedSpans(t *testing.T) {
	// The full alias: frame 1 claims frame 0's compressed bytes outright. Every other field
	// stays valid — uncompressed spans still tile the chunk contiguously, every span is
	// inside the object, every size is positive.
	c := framedChunk(3)
	c.Frames[1].CompressedOffset = c.Frames[0].CompressedOffset
	c.Frames[1].CompressedSize = c.Frames[0].CompressedSize
	if err := parseChunks(t, c); err == nil {
		t.Error("verdict SERVES WRONG: frame 1 claiming frame 0's compressed bytes parsed clean")
	} else if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("rejected, but not as an overlap: %v", err)
	}

	// The partial overlap, which is the one a corrupt writer produces rather than an
	// attacker: frame 2 starts inside frame 1.
	c = framedChunk(3)
	c.Frames[2].CompressedOffset -= 1
	if err := parseChunks(t, c); err == nil {
		t.Error("verdict SERVES WRONG: a one-byte compressed overlap parsed clean")
	}

	// And overlap must be judged independently of array order, since the array is ordered by
	// UNCOMPRESSED offset: a chunk may legitimately store its frames' compressed spans in a
	// different order, so a check that only compares neighbours in order would miss this.
	c = framedChunk(3)
	c.Frames[0].CompressedOffset, c.Frames[2].CompressedOffset =
		c.Frames[2].CompressedOffset, c.Frames[0].CompressedOffset
	c.Frames[1].CompressedOffset = c.Frames[0].CompressedOffset - 1 // now overlaps frame 0
	if err := parseChunks(t, c); err == nil {
		t.Error("verdict SERVES WRONG: an out-of-order compressed overlap parsed clean")
	} else if !strings.Contains(err.Error(), "overlap") {
		// It must be the overlap check that catches this, not the object-size bound
		// catching it by accident -- a span that happens to run past compressed_size
		// would make this case pass while order-independence went untested.
		t.Errorf("rejected, but not as an overlap: %v", err)
	}
}

// What must STILL parse. A gap between compressed spans is legal and load-bearing: zstd
// skippable frames (which is where a frame index itself lives) sit between data frames, so a
// non-overlap check that demanded contiguity would reject real archives. This is the
// regression that a stricter check could cause, so it is asserted beside it.
func TestFrameTableAcceptsGapsBetweenCompressedSpans(t *testing.T) {
	c := framedChunk(3)
	// A 64-byte skippable frame between each pair of data frames.
	for i := 1; i < len(c.Frames); i++ {
		c.Frames[i].CompressedOffset += int64(i) * 64
	}
	c.CompressedSize += 3 * 64
	if err := parseChunks(t, c); err != nil {
		t.Fatalf("a legal compressed gap (skippable frames) was rejected: %v", err)
	}

	// Frames stored out of compressed order, non-overlapping. Nothing in the format forbids
	// it, so the non-overlap check must not have smuggled in an ordering requirement.
	c = framedChunk(3)
	c.Frames[0].CompressedOffset, c.Frames[2].CompressedOffset =
		c.Frames[2].CompressedOffset, c.Frames[0].CompressedOffset
	if err := parseChunks(t, c); err != nil {
		t.Fatalf("frames out of compressed order but non-overlapping were rejected: %v", err)
	}
}

// CASE 2c: archive_offset disagrees with the frame table.
//
// The honest verdict here is SERVES WRONG AND UNDETECTABLE, and it is not a defect: for a
// file inside a chunk, archive_offset IS the authority on where the bytes are. There is no
// second record to cross-check it against — the frame table says where frames boundaries are,
// not which file lives at which offset. An offset that points at another file's bytes inside
// the same chunk produces wrong bytes, and lith cannot know.
//
// What IS checkable is the bound, and that is enforced: a part running past the chunk's
// uncompressed total is rejected rather than clamped or served short. This pins that line,
// since it is the whole of the defence.
func TestArchiveOffsetPastTheChunkIsDetectedNotClamped(t *testing.T) {
	c := framedChunk(2) // 8192 uncompressed bytes
	rm := rawManifest{Version: "2.1", Prefix: "p", Chunks: []rawChunkEntry{c},
		Files: []rawFileEntry{{Path: "p/f", Size: 100, ChunkID: 0, S3Key: c.S3Key, ArchiveOffset: p64(8150)}},
	}
	m, err := Parse(mustJSON(t, rm))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 8150+100 = 8250 > 8192. Must be rejected at resolve, not truncated to 42 bytes: a
	// short file is a wrong answer that looks like a real one.
	if _, err := m.Resolve(); err == nil {
		t.Error("verdict SERVES WRONG: a part running 58 bytes past the chunk resolved clean")
	} else if !strings.Contains(err.Error(), "exceeds chunk") {
		t.Errorf("rejected, but not by the bound: %v", err)
	}

	// And exactly at the boundary must still work, or the bound is off by one and a
	// legitimate last-file-in-chunk read fails.
	rm.Files[0].ArchiveOffset = p64(8092)
	m, err = Parse(mustJSON(t, rm))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := m.Resolve(); err != nil {
		t.Errorf("a part ending exactly at the chunk's uncompressed size was rejected: %v", err)
	}
}
