// SPDX-License-Identifier: Apache-2.0

package cargoship

import (
	"os"
	"path/filepath"
	"testing"
)

func fixtureManifest(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestParseAndResolveFixture(t *testing.T) {
	m, err := Parse(fixtureManifest(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Version != "2.1" {
		t.Fatalf("version %q", m.Version)
	}
	arch, err := m.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(arch.Chunks) != 1 {
		t.Fatalf("chunks=%d, want 1", len(arch.Chunks))
	}
	c := arch.Chunks[0]
	if c.Key != "lith-bench/cargoship/fixture-fix/uploads/20260911-f7fef25e/shard-0/chunk-0.tar.zst" {
		t.Fatalf("chunk key = %q", c.Key)
	}
	// frames contiguous and summing to UncompTotal
	var u int64
	for i, fr := range c.Frames {
		if fr.UncompOff != u {
			t.Fatalf("frame %d not contiguous: off=%d want=%d", i, fr.UncompOff, u)
		}
		if !fr.HasSum {
			t.Fatalf("frame %d missing checksum", i)
		}
		u += fr.UncompLen
	}
	if u != c.UncompTotal {
		t.Fatalf("UncompTotal=%d, frames sum=%d", c.UncompTotal, u)
	}

	// Expected virtual tree (paths relativized against source_path).
	want := map[string]struct {
		size int64
		off  int64
	}{
		"alpha.txt":     {390000, 512},
		"readme.txt":    {70, 391168},
		"sub/beta.txt":  {510000, 392192},
		"sub/gamma.txt": {390000, 903168},
	}
	if len(arch.Files) != len(want) {
		t.Fatalf("files=%d, want %d", len(arch.Files), len(want))
	}
	for _, f := range arch.Files {
		w, ok := want[f.Path]
		if !ok {
			t.Fatalf("unexpected virtual path %q", f.Path)
		}
		if f.Size != w.size {
			t.Errorf("%s size=%d, want %d", f.Path, f.Size, w.size)
		}
		if len(f.Parts) != 1 {
			t.Fatalf("%s parts=%d, want 1", f.Path, len(f.Parts))
		}
		if f.Parts[0].ArchiveOffset != w.off {
			t.Errorf("%s archiveOffset=%d, want %d", f.Path, f.Parts[0].ArchiveOffset, w.off)
		}
		if f.Sum == "" {
			t.Errorf("%s missing per-file checksum", f.Path)
		}
	}
}

func TestFramesForCoversRange(t *testing.T) {
	m, _ := Parse(fixtureManifest(t))
	arch, _ := m.Resolve()
	c := arch.Chunks[0]
	// A read of alpha.txt (off 512, len 390000) must be covered by the returned
	// frames (contiguous, spanning the whole request).
	fs := c.FramesFor(512, 390000)
	if len(fs) == 0 {
		t.Fatal("no frames for alpha.txt range")
	}
	if fs[0].UncompOff > 512 {
		t.Fatalf("first frame starts after the read: %d", fs[0].UncompOff)
	}
	last := fs[len(fs)-1]
	if last.UncompOff+last.UncompLen < 512+390000 {
		t.Fatalf("frames do not cover the read end")
	}
}

func TestParseRejects(t *testing.T) {
	base := string(fixtureManifest(t))
	cases := []struct {
		name string
		body string
	}{
		{"version-2.0", replaceFirst(base, `"version": "2.1"`, `"version": "2.0"`)},
		{"encrypted", replaceFirst(base, `"format_features"`, `"encryption":{"enabled":true},"format_features"`)},
		{"not-json", "{ this is not json"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.body)); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestParseSizeCap(t *testing.T) {
	big := make([]byte, MaxManifestBytes+1)
	if _, err := Parse(big); err == nil {
		t.Fatal("expected size-cap rejection")
	}
}

// replaceFirst replaces the first occurrence of old with new (test helper; note
// the feature list appears once as `"frames"`).
func replaceFirst(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// FuzzParse ensures the parser never panics on hostile input (lith#101): the
// real fixture, truncated and bit-flipped, plus free-form bytes.
func FuzzParse(f *testing.F) {
	if b, err := os.ReadFile(filepath.Join("testdata", "fixture", "manifest.json")); err == nil {
		f.Add(b)
	}
	f.Add([]byte(`{"version":"2.1","format_features":["frames"],"chunks":[{"id":0,"frames":[{"compressed_offset":0,"compressed_size":1,"uncompressed_offset":0,"uncompressed_size":1}]}]}`))
	f.Add([]byte("{}"))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Parse(b)
		if err != nil {
			return
		}
		// If it parsed, resolving must also not panic.
		_, _ = m.Resolve()
	})
}

// TestResolveKeepsCompleteStagingSnapshot: CargoShip may record intermediate
// staging snapshots of a chunk under one s3_key (smaller compressed_size); the
// resolver must keep the complete entry (largest compressed_size) and drop the
// partials.
func TestResolveKeepsCompleteStagingSnapshot(t *testing.T) {
	m := &Manifest{
		Version: "2.1", Prefix: "p",
		Chunks: []rawChunkEntry{
			{ID: 0, S3Key: "c0.tar.zst", CompressedSize: 100, Frames: []rawFrameEntry{{CompressedOffset: 0, CompressedSize: 100, UncompressedOffset: 0, UncompressedSize: 200}}},
			{ID: 0, S3Key: "c0.tar.zst", CompressedSize: 300, Frames: []rawFrameEntry{{CompressedOffset: 0, CompressedSize: 150, UncompressedOffset: 0, UncompressedSize: 300}, {CompressedOffset: 150, CompressedSize: 150, UncompressedOffset: 300, UncompressedSize: 300}}},
		},
		Files: []rawFileEntry{{Path: "p/f", Size: 50, ChunkID: 0, S3Key: "c0.tar.zst", ArchiveOffset: p64(0)}},
	}
	arch, err := m.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(arch.Chunks) != 1 {
		t.Fatalf("chunks=%d, want 1 (partial snapshot dropped)", len(arch.Chunks))
	}
	if len(arch.Chunks[0].Frames) != 2 || arch.Chunks[0].UncompTotal != 600 {
		t.Fatalf("kept the wrong entry: frames=%d total=%d", len(arch.Chunks[0].Frames), arch.Chunks[0].UncompTotal)
	}
}

func p64(v int64) *int64 { return &v }

// TestResolveFramelessChunk: a plain (unframed) .tar chunk resolves to a
// frameless Chunk (UncompTotal = object size); its files carry archive_offset.
func TestResolveFramelessChunk(t *testing.T) {
	m := &Manifest{
		Version: "2.1", Prefix: "p",
		Chunks: []rawChunkEntry{
			{ID: 0, S3Key: "plain.tar", CompressedSize: 4096, UncompressedSize: 4096}, // no frames
		},
		Files: []rawFileEntry{{Path: "p/blob", Size: 1000, ChunkID: 0, S3Key: "plain.tar", ArchiveOffset: p64(512)}},
	}
	arch, err := m.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(arch.Chunks) != 1 || len(arch.Chunks[0].Frames) != 0 {
		t.Fatalf("want 1 frameless chunk, got %d chunks / %d frames", len(arch.Chunks), len(arch.Chunks[0].Frames))
	}
	if arch.Chunks[0].UncompTotal != 4096 {
		t.Fatalf("frameless UncompTotal=%d, want 4096 (object size)", arch.Chunks[0].UncompTotal)
	}
	if arch.Files[0].Parts[0].ArchiveOffset != 512 {
		t.Fatalf("blob archive_offset=%d, want 512", arch.Files[0].Parts[0].ArchiveOffset)
	}
}

// TestResolveRejectsNullArchiveOffset: a v0.24.2-style null offset (nil pointer)
// on a frameless file is rejected with the upgrade message.
func TestResolveRejectsNullArchiveOffset(t *testing.T) {
	m := &Manifest{
		Version: "2.1", Prefix: "p",
		Chunks: []rawChunkEntry{{ID: 0, S3Key: "plain.tar", CompressedSize: 4096}},
		Files:  []rawFileEntry{{Path: "p/blob", Size: 1000, ChunkID: 0, S3Key: "plain.tar", ArchiveOffset: nil}},
	}
	if _, err := m.Resolve(); err == nil {
		t.Fatal("expected rejection for a null archive_offset")
	}
}
