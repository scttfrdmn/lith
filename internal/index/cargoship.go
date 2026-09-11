// SPDX-License-Identifier: Apache-2.0

package index

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/scttfrdmn/lith/internal/cargoship"
)

// cargoChunk is one packed `.tar.zst` archive object and its frame table, held
// in memory for a CargoShip-backed index (format v5).
type cargoChunk struct {
	Key         string
	ETagHash    uint64
	UncompTotal int64
	Frames      []cargoship.Frame
}

// cargoPart is one contiguous run of a virtual file's bytes in one chunk.
type cargoPart struct {
	ChunkIdx      int32
	ArchiveOffset int64
	FileOffset    int64
	Length        int64
}

// cargoBacking is the in-memory CargoShip backing table: archive provenance,
// the per-chunk frame tables, and the per-entry parts (parallel to the index's
// n entries; an entry with no parts is object-backed).
type cargoBacking struct {
	manifestSHA [32]byte
	uploadID    string
	version     string
	features    string
	chunks      []cargoChunk
	parts       [][]cargoPart
}

// Backing is the read-mapping for one virtual file, handed to the mount layer.
// A read of the file at [off,off+len) is served from these parts: each part
// names the packed chunk, the file's start in that chunk's uncompressed tar
// stream, and the byte span. The chunk's frame table maps an uncompressed span
// to the zstd frame range GETs that cover it.
type Backing struct {
	Parts []BackingPart
}

// BackingPart is one part's full read descriptor (the chunk resolved).
type BackingPart struct {
	ChunkKey         string
	ChunkETagHash    uint64
	ChunkUncompTotal int64
	ArchiveOffset    int64
	FileOffset       int64
	Length           int64
	Frames           []cargoship.Frame
}

// BuildFromCargoship builds an index whose namespace is a resolved CargoShip
// archive's original tree. chunkETags[i] is the xxh3 hash of chunk i's S3 ETag
// (HeadObject'd at build), used for staleness detection on the packed object.
func BuildFromCargoship(arch *cargoship.Archive, chunkETags []uint64, manifestSHA [32]byte, uploadID, version, features string, opts Options) (*Index, error) {
	if len(chunkETags) != len(arch.Chunks) {
		return nil, fmt.Errorf("index: cargoship build has %d chunk etags for %d chunks", len(chunkETags), len(arch.Chunks))
	}
	// Namespace entries — one per virtual file. The stored per-entry ETag is the
	// packed chunk's etag (the object a read actually fetches), so a re-uploaded
	// chunk is detected as stale.
	entries := make([]Entry, 0, len(arch.Files))
	byPath := make(map[string][]cargoPart, len(arch.Files))
	for _, vf := range arch.Files {
		var etag uint64
		if len(vf.Parts) > 0 {
			etag = chunkETags[vf.Parts[0].ChunkIndex]
		}
		entries = append(entries, Entry{Key: vf.Path, Size: vf.Size, MTime: vf.ModTime.UnixNano(), ETagHash: etag})
		parts := make([]cargoPart, len(vf.Parts))
		for i, p := range vf.Parts {
			parts[i] = cargoPart{ChunkIdx: int32(p.ChunkIndex), ArchiveOffset: p.ArchiveOffset, FileOffset: p.FileOffset, Length: p.Length}
		}
		byPath[vf.Path] = parts
	}

	opts.Source = "cargoship"
	if opts.BuildTime.IsZero() {
		opts.BuildTime = time.Now()
	}
	ix := Build(entries, opts)

	cb := &cargoBacking{manifestSHA: manifestSHA, uploadID: uploadID, version: version, features: features}
	cb.chunks = make([]cargoChunk, len(arch.Chunks))
	for i, c := range arch.Chunks {
		cb.chunks[i] = cargoChunk{Key: c.Key, ETagHash: chunkETags[i], UncompTotal: c.UncompTotal, Frames: c.Frames}
	}
	// Parts parallel to the FINAL (sorted/deduped) entries.
	n := ix.Len()
	cb.parts = make([][]cargoPart, n)
	for i := 0; i < n; i++ {
		cb.parts[i] = byPath[ix.key(i)]
	}
	ix.cargo = cb
	return ix, nil
}

// IsCargoship reports whether the index is CargoShip-backed.
func (ix *Index) IsCargoship() bool { return ix.cargo != nil }

// CargoProvenance returns the archive provenance for `inspect` (empty when not
// a CargoShip index).
func (ix *Index) CargoProvenance() (manifestSHA, uploadID, version, features string, chunks, frames int) {
	if ix.cargo == nil {
		return "", "", "", "", 0, 0
	}
	c := ix.cargo
	for _, ch := range c.chunks {
		frames += len(ch.Frames)
	}
	sha := ""
	if c.manifestSHA != ([32]byte{}) {
		sha = hex.EncodeToString(c.manifestSHA[:])
	}
	return sha, c.uploadID, c.version, c.features, len(c.chunks), frames
}

// BackingOf returns the read-mapping for the virtual file at path (leading "/"
// accepted), resolving each part's chunk. ok is false for an object-backed
// index or a path with no parts (a directory or unknown path).
func (ix *Index) BackingOf(path string) (Backing, bool) {
	if ix.cargo == nil {
		return Backing{}, false
	}
	pos, ok := ix.Position(path)
	if !ok || pos < 0 || pos >= len(ix.cargo.parts) {
		return Backing{}, false
	}
	parts := ix.cargo.parts[pos]
	if len(parts) == 0 {
		return Backing{}, false
	}
	out := Backing{Parts: make([]BackingPart, len(parts))}
	for i, p := range parts {
		if int(p.ChunkIdx) >= len(ix.cargo.chunks) {
			return Backing{}, false
		}
		ch := ix.cargo.chunks[p.ChunkIdx]
		out.Parts[i] = BackingPart{
			ChunkKey: ch.Key, ChunkETagHash: ch.ETagHash, ChunkUncompTotal: ch.UncompTotal,
			ArchiveOffset: p.ArchiveOffset, FileOffset: p.FileOffset, Length: p.Length, Frames: ch.Frames,
		}
	}
	return out, true
}

// --- v5 backing blob (de)serialization; bounds-checked, never panics (lith#101) ---

func (ix *Index) marshalCargoBacking() []byte {
	if ix.cargo == nil {
		return nil
	}
	ne := binary.NativeEndian
	var buf []byte
	putU32 := func(v uint32) { buf = ne.AppendUint32(buf, v) }
	putU64 := func(v uint64) { buf = ne.AppendUint64(buf, v) }
	putStr := func(s string) { putU32(uint32(len(s))); buf = append(buf, s...) }

	c := ix.cargo
	buf = append(buf, c.manifestSHA[:]...)
	putStr(c.uploadID)
	putStr(c.version)
	putStr(c.features)
	putU32(uint32(len(c.chunks)))
	for _, ch := range c.chunks {
		putStr(ch.Key)
		putU64(ch.ETagHash)
		putU64(uint64(ch.UncompTotal))
		putU32(uint32(len(ch.Frames)))
		for _, f := range ch.Frames {
			putU64(uint64(f.CompOff))
			putU64(uint64(f.CompLen))
			putU64(uint64(f.UncompOff))
			putU64(uint64(f.UncompLen))
			if f.HasSum {
				buf = append(buf, 1)
				buf = append(buf, f.Sum[:]...)
			} else {
				buf = append(buf, 0)
			}
		}
	}
	putU32(uint32(len(c.parts)))
	for _, ps := range c.parts {
		putU32(uint32(len(ps)))
		for _, p := range ps {
			putU32(uint32(p.ChunkIdx))
			putU64(uint64(p.ArchiveOffset))
			putU64(uint64(p.FileOffset))
			putU64(uint64(p.Length))
		}
	}
	return buf
}

// blobReader is a bounds-checked cursor over the backing blob.
type blobReader struct {
	b   []byte
	p   int
	err error
}

func (r *blobReader) need(k int) bool {
	if r.err != nil {
		return false
	}
	if r.p < 0 || k < 0 || k > len(r.b)-r.p {
		r.err = corruptf(r.p, "cargoship backing: read of %d bytes runs past blob end (%d left)", k, len(r.b)-r.p)
		return false
	}
	return true
}
func (r *blobReader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.NativeEndian.Uint32(r.b[r.p:])
	r.p += 4
	return v
}
func (r *blobReader) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.NativeEndian.Uint64(r.b[r.p:])
	r.p += 8
	return v
}
func (r *blobReader) byte1() byte {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.p]
	r.p++
	return v
}
func (r *blobReader) bytes(k int) []byte {
	if !r.need(k) {
		return nil
	}
	v := r.b[r.p : r.p+k]
	r.p += k
	return v
}
func (r *blobReader) str() string {
	n := int(r.u32())
	return string(r.bytes(n))
}

func parseCargoBacking(b []byte, n int) (*cargoBacking, error) {
	r := &blobReader{b: b}
	cb := &cargoBacking{}
	copy(cb.manifestSHA[:], r.bytes(32))
	cb.uploadID = r.str()
	cb.version = r.str()
	cb.features = r.str()
	nchunks := int(r.u32())
	if r.err != nil {
		return nil, r.err
	}
	if nchunks < 0 || nchunks > len(b) {
		return nil, corruptf(0, "cargoship backing: implausible chunk count %d", nchunks)
	}
	cb.chunks = make([]cargoChunk, nchunks)
	for i := 0; i < nchunks; i++ {
		ch := cargoChunk{}
		ch.Key = r.str()
		ch.ETagHash = r.u64()
		ch.UncompTotal = int64(r.u64())
		nf := int(r.u32())
		if r.err != nil {
			return nil, r.err
		}
		if nf < 0 || nf > len(b) {
			return nil, corruptf(r.p, "cargoship backing: implausible frame count %d", nf)
		}
		ch.Frames = make([]cargoship.Frame, nf)
		for j := 0; j < nf; j++ {
			f := cargoship.Frame{
				CompOff:   int64(r.u64()),
				CompLen:   int64(r.u64()),
				UncompOff: int64(r.u64()),
				UncompLen: int64(r.u64()),
			}
			if r.byte1() == 1 {
				copy(f.Sum[:], r.bytes(32))
				f.HasSum = true
			}
			ch.Frames[j] = f
		}
		cb.chunks[i] = ch
	}
	nparts := int(r.u32())
	if r.err != nil {
		return nil, r.err
	}
	if nparts != n {
		return nil, corruptf(r.p, "cargoship backing: parts count %d != entry count %d", nparts, n)
	}
	cb.parts = make([][]cargoPart, nparts)
	for i := 0; i < nparts; i++ {
		pc := int(r.u32())
		if r.err != nil {
			return nil, r.err
		}
		if pc < 0 || pc > len(b) {
			return nil, corruptf(r.p, "cargoship backing: implausible part count %d", pc)
		}
		if pc == 0 {
			continue
		}
		ps := make([]cargoPart, pc)
		for j := 0; j < pc; j++ {
			ps[j] = cargoPart{
				ChunkIdx:      int32(r.u32()),
				ArchiveOffset: int64(r.u64()),
				FileOffset:    int64(r.u64()),
				Length:        int64(r.u64()),
			}
		}
		cb.parts[i] = ps
	}
	if r.err != nil {
		return nil, r.err
	}
	// Validate chunk references so BackingOf can trust them.
	for i, ps := range cb.parts {
		for _, p := range ps {
			if p.ChunkIdx < 0 || int(p.ChunkIdx) >= len(cb.chunks) {
				return nil, corruptf(0, "cargoship backing: entry %d part references chunk %d of %d", i, p.ChunkIdx, len(cb.chunks))
			}
		}
	}
	return cb, nil
}
