// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/cargoship"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// cargoGateway serves the real CargoShip fixture archive over the NFS gateway's roFS. The
// FUSE path has had a byte-exact test for this since #94 (fuse.TestCargoMountReadsFilesByteExact);
// the gateway had none, which is how the defect below survived.
func cargoGateway(t *testing.T) (*roFS, *cargoship.Archive) {
	t.Helper()
	dir := filepath.Join("..", "cargoship", "testdata", "fixture")
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := cargoship.Parse(mb)
	if err != nil {
		t.Fatal(err)
	}
	arch, err := m.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	chunkBytes, err := os.ReadFile(filepath.Join(dir, "chunk-0.tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	srv := fake.New()
	srv.Put(arch.Chunks[0].Key, chunkBytes, time.Unix(1_700_000_000, 0))
	o, err := srv.HeadObject(context.Background(), arch.Chunks[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.BuildFromCargoship(arch, []uint64{index.HashETag(o.ETag)}, [32]byte{}, "u", "2.1", "frames", index.Options{Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := ix.Root("")
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 1 << 20, MemCache: 1 << 30, MaxRange: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bs.Close)
	cfg := Config{Index: reader, Store: bs, RootID: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, ClientIdle: time.Minute}
	s := &server{cfg: cfg, clients: map[string]time.Time{}}
	return &roFS{cfg: cfg, srv: s, ctx: context.Background(), states: map[string]*seqState{}}, arch
}

// The NFS gateway must serve a CargoShip-backed file byte-exact, like the FUSE mount does.
//
// `roFS.key` resolves a cargo-backed path to its chunk key and frame table, but roFile kept
// no ARCHIVE OFFSET and no part length, and `ReadAt` passed the FILE's offset straight to
// `Store.Chunk` against the CHUNK's key. A file at `archive_offset: 512` — every file in a
// real archive, since a tar header precedes the data — was therefore served from the chunk's
// uncompressed offset 0: the tar header and the previous file's tail, not the file's bytes.
//
// It also passed the FILE's size as the object size, so the frame lookup was bounded to the
// first `size` bytes of a multi-megabyte chunk, and `off >= r.size` returned EOF at the
// file's length measured from the WRONG origin.
//
// A "serves wrong" verdict with no adversarial input at all — the honest fixture is enough.
// It survived because the gateway had no CargoShip test; the FUSE path has had one since #94.
func TestGatewayServesCargoBackedFilesByteExact(t *testing.T) {
	fs, arch := cargoGateway(t)

	// Reference bytes straight from the archive, via the same decode path but with the
	// offsets the FUSE mount uses, so this compares the gateway against the format rather
	// than against itself.
	for _, name := range []string{"alpha.txt", "readme.txt"} {
		var vf cargoship.VFile
		for _, f := range arch.Files {
			if f.Path == name {
				vf = f
			}
		}
		if vf.Path == "" {
			t.Fatalf("fixture: %q not in the archive", name)
		}
		if len(vf.Parts) != 1 {
			t.Fatalf("fixture: %q has %d parts; this test covers the single-part path", name, len(vf.Parts))
		}
		if vf.Parts[0].ArchiveOffset == 0 {
			t.Fatalf("fixture: %q sits at archive_offset 0, so an ignored offset would be "+
				"invisible and this test would prove nothing", name)
		}

		fh, err := fs.Open("/" + name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		buf := make([]byte, vf.Size)
		n, err := fh.ReadAt(buf, 0)
		if err != nil && n == 0 {
			t.Fatalf("read %s: %v", name, err)
		}
		if int64(n) != vf.Size {
			t.Errorf("%s: read %d bytes, want %d", name, n, vf.Size)
		}

		// The authoritative bytes: the chunk's uncompressed stream at the file's
		// archive_offset, which is exactly what the FUSE path asks for.
		b, ok := fs.cfg.Index.BackingOf("/" + name)
		if !ok {
			t.Fatalf("fixture: %q has no cargo backing", name)
		}
		want, err := fs.cfg.Store.GetRange(context.Background(), blockstore.Key{
			Key:      arch.Chunks[0].Key,
			ETagHash: b.Parts[0].ChunkETagHash,
			Cargo: &blockstore.CargoChunk{
				Frames: arch.Chunks[0].Frames, UncompTotal: arch.Chunks[0].UncompTotal,
			},
		}, vf.Parts[0].ArchiveOffset, vf.Size, arch.Chunks[0].UncompTotal)
		if err != nil {
			t.Fatalf("reference read of %s: %v", name, err)
		}
		if !bytes.Equal(buf[:n], want) {
			off := -1
			for i := range want {
				if i >= n || buf[i] != want[i] {
					off = i
					break
				}
			}
			t.Errorf("verdict SERVES WRONG: %s differs from the archive at byte %d "+
				"(archive_offset %d ignored?)", name, off, vf.Parts[0].ArchiveOffset)
		}
	}
}

// A sequential whole-file read must end at the FILE's end, not at the chunk's. r.size is in
// file coordinates and r.space in the key's, and conflating them is what the bug above did:
// EOF was computed from the file's length measured from the wrong origin, so the last file in
// a chunk read past its own end and an early one stopped short of the chunk.
func TestGatewayCargoSequentialReadEndsAtTheFileNotTheChunk(t *testing.T) {
	fs, arch := cargoGateway(t)
	var vf cargoship.VFile
	for _, f := range arch.Files {
		if f.Path == "readme.txt" {
			vf = f
		}
	}
	// readme.txt is 70 bytes at archive_offset 391168 inside a 1.29 MB chunk, so a read
	// bounded by the chunk instead of the file would run 1.2 MB long.
	if vf.Size >= arch.Chunks[0].UncompTotal/2 {
		t.Fatalf("fixture: %q is %d bytes of a %d-byte chunk; too close to distinguish "+
			"a file-bounded read from a chunk-bounded one", vf.Path, vf.Size, arch.Chunks[0].UncompTotal)
	}

	fh, err := fs.Open("/readme.txt")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var total int64
	buf := make([]byte, 32) // smaller than the file, so several Read calls are needed
	for i := 0; i < 100; i++ {
		n, err := fh.Read(buf)
		total += int64(n)
		if err != nil {
			break
		}
	}
	if total != vf.Size {
		t.Errorf("a sequential read returned %d bytes, want the file's %d", total, vf.Size)
	}

	// A read starting past the file's end is EOF, even though the chunk has plenty of
	// bytes there.
	if n, err := fh.ReadAt(buf, vf.Size); err != io.EOF || n != 0 {
		t.Errorf("read at the file's size returned (%d, %v), want (0, EOF) — the chunk's "+
			"bytes past this file are not part of it", n, err)
	}
}

// A CargoShip file split across several chunks must FAIL CLOSED at open, naming the reason.
//
// The gateway's read path holds one key and one origin, so it cannot assemble parts the way
// FUSE's readCargo does. Before the fix it silently fell through to an OBJECT key —
// prefix+path — which does not exist in a packed archive, so the failure surfaced as a bare
// NoSuchKey on first read with nothing to connect it to splitting (#141's fail-closed law).
func TestGatewaySplitCargoFileFailsClosedAtOpen(t *testing.T) {
	fs, arch := cargoGateway(t)

	// Synthesize a two-part file over the real chunk. Only the part COUNT matters here:
	// open must refuse before any byte is read.
	p0 := arch.Files[0].Parts[0]
	split := cargoship.VFile{
		Path: "split.bin", Size: 2000, ModTime: arch.Files[0].ModTime,
		Parts: []cargoship.Part{
			{ChunkIndex: p0.ChunkIndex, ArchiveOffset: p0.ArchiveOffset, FileOffset: 0, Length: 1000},
			{ChunkIndex: p0.ChunkIndex, ArchiveOffset: p0.ArchiveOffset + 1000, FileOffset: 1000, Length: 1000},
		},
	}
	arch.Files = append(arch.Files, split)
	ix, err := index.BuildFromCargoship(arch, []uint64{1}, [32]byte{}, "u", "2.1", "frames", index.Options{Bucket: "bkt"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	reader, err := ix.Root("")
	if err != nil {
		t.Fatal(err)
	}
	fs.cfg.Index = reader

	// It must still be VISIBLE — hiding it would be a different wrong answer, a file the
	// FUSE mount serves and the gateway denies the existence of.
	if _, err := fs.Stat("/split.bin"); err != nil {
		t.Errorf("stat of a split file failed (%v); it exists and FUSE serves it", err)
	}

	_, err = fs.Open("/split.bin")
	if err == nil {
		t.Fatal("verdict SERVES WRONG: opening a split cargo file succeeded; its reads " +
			"would be served from one chunk at one origin")
	}
	if !strings.Contains(err.Error(), "split") {
		t.Errorf("open failed, but not with a reason naming the split: %v", err)
	}

	// Single-chunk files in the same archive must be unaffected.
	if _, err := fs.Open("/alpha.txt"); err != nil {
		t.Errorf("a single-chunk file in the same archive failed to open: %v", err)
	}
}
