// SPDX-License-Identifier: Apache-2.0

package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scttfrdmn/lith/internal/cargoship"
)

func loadFixtureArchive(t *testing.T) *cargoship.Archive {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "cargoship", "testdata", "fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	m, err := cargoship.Parse(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	arch, err := m.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return arch
}

func TestBuildFromCargoshipNamespaceAndBacking(t *testing.T) {
	arch := loadFixtureArchive(t)
	etags := make([]uint64, len(arch.Chunks))
	for i := range etags {
		etags[i] = uint64(0x1000 + i)
	}
	var sha [32]byte
	sha[0] = 0xab
	ix, err := BuildFromCargoship(arch, etags, sha, "20260911-f7fef25e", "2.1", "frames", Options{Bucket: "b"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	check := func(ix *Index, tag string) {
		if !ix.IsCargoship() {
			t.Fatalf("%s: not cargoship", tag)
		}
		// Namespace: the four files, directories derived.
		if _, err := ix.Stat("/alpha.txt"); err != nil {
			t.Fatalf("%s: stat alpha.txt: %v", tag, err)
		}
		if fi, err := ix.Stat("/sub/beta.txt"); err != nil || fi.Size != 510000 {
			t.Fatalf("%s: stat sub/beta.txt: %v size=%d", tag, err, fi.Size)
		}
		if _, err := ix.Stat("/sub"); err != nil {
			t.Fatalf("%s: /sub should be a directory: %v", tag, err)
		}
		// Backing for alpha.txt.
		b, ok := ix.BackingOf("/alpha.txt")
		if !ok || len(b.Parts) != 1 {
			t.Fatalf("%s: backing alpha.txt ok=%v parts=%d", tag, ok, len(b.Parts))
		}
		p := b.Parts[0]
		if p.ArchiveOffset != 512 || p.Length != 390000 {
			t.Errorf("%s: alpha part off=%d len=%d, want 512/390000", tag, p.ArchiveOffset, p.Length)
		}
		if p.ChunkKey == "" || len(p.Frames) == 0 || p.ChunkUncompTotal == 0 {
			t.Errorf("%s: alpha part chunk unresolved: key=%q frames=%d total=%d", tag, p.ChunkKey, len(p.Frames), p.ChunkUncompTotal)
		}
		if p.ChunkETagHash != 0x1000 {
			t.Errorf("%s: alpha chunk etag=%x, want 0x1000", tag, p.ChunkETagHash)
		}
		// A directory has no backing.
		if _, ok := ix.BackingOf("/sub"); ok {
			t.Errorf("%s: /sub should have no backing", tag)
		}
	}
	check(ix, "fresh")

	// Round-trip through Save/Open (validates the v5 backing blob).
	dir := t.TempDir()
	path := filepath.Join(dir, "cargo.idx")
	if err := ix.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	ix2, closeFn, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = closeFn() }()
	check(ix2, "reopened")

	sha2, up, ver, feat, nch, nfr := ix2.CargoProvenance()
	if up != "20260911-f7fef25e" || ver != "2.1" || feat != "frames" || nch != 1 || nfr == 0 || sha2 == "" {
		t.Fatalf("provenance mismatch: sha=%s up=%s ver=%s feat=%s ch=%d fr=%d", sha2, up, ver, feat, nch, nfr)
	}
}

func TestParseCargoBackingRejectsTruncation(t *testing.T) {
	arch := loadFixtureArchive(t)
	etags := make([]uint64, len(arch.Chunks))
	ix, err := BuildFromCargoship(arch, etags, [32]byte{}, "u", "2.1", "frames", Options{Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	blob := ix.marshalCargoBacking()
	// Truncating the blob at any point must error, never panic.
	for cut := 0; cut < len(blob); cut += 7 {
		if _, err := parseCargoBacking(blob[:cut], ix.Len()); err == nil {
			t.Fatalf("truncation at %d did not error", cut)
		}
	}
}
