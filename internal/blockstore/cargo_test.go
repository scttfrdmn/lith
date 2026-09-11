// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/cargoship"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

func loadFixtureChunk(t *testing.T) (*cargoship.Archive, []byte) {
	t.Helper()
	dir := filepath.Join("..", "cargoship", "testdata", "fixture")
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	m, err := cargoship.Parse(mb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	arch, err := m.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	cb, err := os.ReadFile(filepath.Join(dir, "chunk-0.tar.zst"))
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	return arch, cb
}

// cargoKey puts the fixture chunk in the fake and returns a Cargo-backed Key.
func cargoKey(t *testing.T, srv *fake.Server, arch *cargoship.Archive, chunkBytes []byte) Key {
	t.Helper()
	c := arch.Chunks[0]
	srv.Put(c.Key, chunkBytes, time.Unix(1_700_000_000, 0))
	k := keyFor(t, srv, c.Key)
	k.Cargo = &CargoChunk{Frames: c.Frames, UncompTotal: c.UncompTotal}
	return k
}

// TestCargoReadFilesByteExact: every virtual file reads back byte-exact (checked
// against the manifest's per-file sha256) through the frame-decode path.
func TestCargoReadFilesByteExact(t *testing.T) {
	arch, chunkBytes := loadFixtureChunk(t)
	srv := fake.New()
	k := cargoKey(t, srv, arch, chunkBytes)
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})
	total := arch.Chunks[0].UncompTotal

	for _, f := range arch.Files {
		p := f.Parts[0]
		data, err := bs.GetRange(context.Background(), k, p.ArchiveOffset, f.Size, total)
		if err != nil {
			t.Fatalf("%s: GetRange: %v", f.Path, err)
		}
		if int64(len(data)) != f.Size {
			t.Fatalf("%s: got %d bytes, want %d", f.Path, len(data), f.Size)
		}
		if sum := hex.EncodeToString(sha256sum(data)); sum != f.Sum {
			t.Fatalf("%s: checksum mismatch", f.Path)
		}
	}
}

// TestCargoReadSpansTwoFrames: a read straddling a 64 KiB frame boundary
// assembles correctly (verified against the whole-file read).
func TestCargoReadSpansTwoFrames(t *testing.T) {
	arch, chunkBytes := loadFixtureChunk(t)
	srv := fake.New()
	k := cargoKey(t, srv, arch, chunkBytes)
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})
	total := arch.Chunks[0].UncompTotal

	// alpha.txt is 390000 bytes over ~6 frames of 64 KiB. Read a window straddling
	// the first frame boundary (~65536) and compare to the full-file read.
	var alpha cargoship.VFile
	for _, f := range arch.Files {
		if f.Path == "alpha.txt" {
			alpha = f
		}
	}
	full, err := bs.GetRange(context.Background(), k, alpha.Parts[0].ArchiveOffset, alpha.Size, total)
	if err != nil {
		t.Fatal(err)
	}
	// window [60000, 70000) within the file
	win, err := bs.GetRange(context.Background(), k, alpha.Parts[0].ArchiveOffset+60000, 10000, total)
	if err != nil {
		t.Fatal(err)
	}
	if string(win) != string(full[60000:70000]) {
		t.Fatal("cross-frame window did not match the whole-file read")
	}
}

// TestCargoChecksumCorruptionIsError: a corrupted chunk object fails the read
// (frame checksum / decode), never returning silent bad bytes.
func TestCargoChecksumCorruptionIsError(t *testing.T) {
	arch, chunkBytes := loadFixtureChunk(t)
	bad := append([]byte(nil), chunkBytes...)
	// Flip a byte inside the first frame's compressed region.
	f0 := arch.Chunks[0].Frames[0]
	bad[f0.CompOff+10] ^= 0xff
	srv := fake.New()
	k := cargoKey(t, srv, arch, bad)
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})
	total := arch.Chunks[0].UncompTotal
	if _, err := bs.GetRange(context.Background(), k, 512, 1000, total); err == nil {
		t.Fatal("expected an error reading a corrupted frame, got nil")
	}
}

// TestCargoSharedChunkNoDuplicateGET: two files whose bytes fall in the same
// cached 1 MiB uncompressed chunk are served from cache — the second read
// issues no new S3 GET (the small-files streaming win).
func TestCargoSharedChunkNoDuplicateGET(t *testing.T) {
	arch, chunkBytes := loadFixtureChunk(t)
	srv := fake.New()
	k := cargoKey(t, srv, arch, chunkBytes)
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})
	total := arch.Chunks[0].UncompTotal

	// alpha.txt (off 512, ~390 KB) and readme.txt (off 391168, 70 B) both live in
	// the first 1 MiB of the uncompressed stream. Reading alpha caches that chunk;
	// reading readme must add no GET.
	var alpha, readme cargoship.VFile
	for _, f := range arch.Files {
		switch f.Path {
		case "alpha.txt":
			alpha = f
		case "readme.txt":
			readme = f
		}
	}
	if _, err := bs.GetRange(context.Background(), k, alpha.Parts[0].ArchiveOffset, alpha.Size, total); err != nil {
		t.Fatal(err)
	}
	before := srv.GetCallCount()
	data, err := bs.GetRange(context.Background(), k, readme.Parts[0].ArchiveOffset, readme.Size, total)
	if err != nil {
		t.Fatal(err)
	}
	if after := srv.GetCallCount(); after != before {
		t.Fatalf("readme read issued %d new GET(s); expected a cache hit in the shared chunk", after-before)
	}
	if sum := hex.EncodeToString(sha256sum(data)); sum != readme.Sum {
		t.Fatal("readme checksum mismatch")
	}
}

// TestCargoConcurrentReadsRace exercises the frame path under -race.
func TestCargoConcurrentReadsRace(t *testing.T) {
	arch, chunkBytes := loadFixtureChunk(t)
	srv := fake.New()
	k := cargoKey(t, srv, arch, chunkBytes)
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})
	total := arch.Chunks[0].UncompTotal
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := arch.Files[i%len(arch.Files)]
			_, _ = bs.GetRange(context.Background(), k, f.Parts[0].ArchiveOffset, f.Size, total)
		}(i)
	}
	wg.Wait()
}

func sha256sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }
