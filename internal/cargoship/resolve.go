// SPDX-License-Identifier: Apache-2.0

package cargoship

import (
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// Frame is one independently-decodable zstd frame of a chunk's compressed
// `.tar.zst` object. CompOff/CompLen is the byte range to GET; UncompOff/
// UncompLen is the span it decompresses to in the chunk's uncompressed tar
// stream. Sum is the sha256 of the compressed frame bytes (#439), verified on
// fetch.
type Frame struct {
	CompOff, CompLen     int64
	UncompOff, UncompLen int64
	Sum                  [32]byte
	HasSum               bool
}

// Chunk is one packed `.tar.zst` archive object and its frame table.
type Chunk struct {
	Key         string // full bucket-relative S3 key
	UncompTotal int64  // total uncompressed tar-stream bytes (sum of frame UncompLen)
	Frames      []Frame
}

// Part is one contiguous run of a virtual file's bytes living in one chunk.
// Most files have exactly one part; a split file has several, in file order.
type Part struct {
	ChunkIndex    int   // index into Archive.Chunks
	ArchiveOffset int64 // start in the chunk's uncompressed tar stream
	FileOffset    int64 // start offset within the virtual file
	Length        int64 // bytes of this part
}

// VFile is a virtual file in the archive's original tree.
type VFile struct {
	Path    string // relative virtual path (no leading slash)
	Size    int64
	ModTime time.Time
	Sum     string // per-file sha256 hex, "" if absent
	Parts   []Part // ≥1, sorted by FileOffset
}

// Archive is a resolved 2.1 archive: the virtual files and the chunk frame
// tables their reads map onto.
type Archive struct {
	Bucket string
	Chunks []Chunk
	Files  []VFile
}

// Resolve turns a validated manifest into the virtual tree + chunk frame
// tables, handling deduplicated and split files. Every part's byte span is
// bounds-checked against its chunk's uncompressed stream (lith#101).
func (m *Manifest) Resolve() (*Archive, error) {
	// Chunks, indexed by their S3 key. Chunk IDs are only unique within a shard
	// (each shard restarts at 0), so the object key is the stable identity.
	// CargoShip may record intermediate staging snapshots of a chunk under one
	// key (partial writes with a smaller compressed_size); the complete entry is
	// the one whose compressed_size matches the final object — the maximum. Keep
	// that one and drop the partial snapshots.
	best := make(map[string]int) // s3_key -> index into m.Chunks of the winning entry
	var keyOrder []string
	for i, rc := range m.Chunks {
		if rc.S3Key == "" {
			return nil, fmt.Errorf("cargoship: chunk %d has no s3_key", i)
		}
		if j, seen := best[rc.S3Key]; seen {
			if rc.CompressedSize > m.Chunks[j].CompressedSize {
				best[rc.S3Key] = i
			}
			continue
		}
		best[rc.S3Key] = i
		keyOrder = append(keyOrder, rc.S3Key)
	}
	chunks := make([]Chunk, len(keyOrder))
	keyToIdx := make(map[string]int, len(keyOrder))
	for idx, key := range keyOrder {
		rc := m.Chunks[best[key]]
		frames := make([]Frame, len(rc.Frames))
		for fi, rf := range rc.Frames {
			fr := Frame{
				CompOff: rf.CompressedOffset, CompLen: rf.CompressedSize,
				UncompOff: rf.UncompressedOffset, UncompLen: rf.UncompressedSize,
			}
			if rf.Checksum != "" {
				sum, err := hex.DecodeString(rf.Checksum)
				if err != nil || len(sum) != 32 {
					return nil, fmt.Errorf("cargoship: chunk %q frame %d has a malformed checksum", rc.S3Key, fi)
				}
				copy(fr.Sum[:], sum)
				fr.HasSum = true
			}
			frames[fi] = fr
		}
		sortFramesByUncomp(frames)
		var total int64
		if len(frames) == 0 {
			// Frameless (plain .tar) chunk: the object IS the uncompressed tar stream
			// that archive_offset indexes; its size is the object size.
			total = rc.CompressedSize
			if total == 0 {
				total = rc.UncompressedSize
			}
		} else {
			for _, fr := range frames {
				total += fr.UncompLen
			}
		}
		chunks[idx] = Chunk{Key: resolveChunkKey(m.Prefix, rc.S3Key), UncompTotal: total, Frames: frames}
		keyToIdx[rc.S3Key] = idx
	}

	// Group file entries by virtual path so split parts assemble into one file.
	// Also index non-duplicate files by checksum so a duplicate resolves to the
	// original's backing.
	type pathGroup struct {
		rel     string
		entries []rawFileEntry
	}
	order := []string{}
	groups := map[string]*pathGroup{}
	byHash := map[string]rawFileEntry{}
	for _, fe := range m.Files {
		rel := relPath(fe.Path, m.SourcePath)
		if rel == "" {
			continue
		}
		g := groups[rel]
		if g == nil {
			g = &pathGroup{rel: rel}
			groups[rel] = g
			order = append(order, rel)
		}
		g.entries = append(g.entries, fe)
		if !fe.IsDuplicate && fe.Checksum != "" {
			if _, seen := byHash[fe.Checksum]; !seen {
				byHash[fe.Checksum] = fe
			}
		}
	}

	partFor := func(fe rawFileEntry) (Part, error) {
		src := fe
		if fe.IsDuplicate {
			h := fe.DupOfHash
			if h == "" {
				h = fe.Checksum
			}
			orig, ok := byHash[h]
			if !ok {
				return Part{}, fmt.Errorf("cargoship: duplicate file %q references missing original (hash %s)", fe.Path, h)
			}
			src = orig
		}
		ci, ok := keyToIdx[src.S3Key]
		if !ok {
			return Part{}, fmt.Errorf("cargoship: file %q references unknown chunk %q", fe.Path, src.S3Key)
		}
		if src.ArchiveOffset == nil {
			return Part{}, fmt.Errorf("cargoship: file %q has a null archive_offset — CargoShip v0.24.0–.2 omitted it for files in plain (unframed) .tar chunks; re-pack the archive with cargoship v0.24.3+ (which records archive_offset for every file)", fe.Path)
		}
		length := fe.Length
		if length == 0 { // full file
			length = fe.Size
		}
		p := Part{ChunkIndex: ci, ArchiveOffset: *src.ArchiveOffset, FileOffset: fe.Offset, Length: length}
		if p.ArchiveOffset < 0 || p.Length < 0 || p.FileOffset < 0 {
			return Part{}, fmt.Errorf("cargoship: file %q has a negative offset/length", fe.Path)
		}
		if end := p.ArchiveOffset + p.Length; end > chunks[ci].UncompTotal {
			return Part{}, fmt.Errorf("cargoship: file %q part [%d,%d) exceeds chunk %d uncompressed size %d", fe.Path, p.ArchiveOffset, end, src.ChunkID, chunks[ci].UncompTotal)
		}
		return p, nil
	}

	files := make([]VFile, 0, len(order))
	for _, rel := range order {
		g := groups[rel]
		vf := VFile{Path: rel, ModTime: g.entries[0].ModTime, Sum: g.entries[0].Checksum}
		for _, fe := range g.entries {
			p, err := partFor(fe)
			if err != nil {
				return nil, err
			}
			vf.Parts = append(vf.Parts, p)
		}
		sort.Slice(vf.Parts, func(i, j int) bool { return vf.Parts[i].FileOffset < vf.Parts[j].FileOffset })
		// Size is the file's true size: for a single entry it is Size; for split
		// parts it is the max end. Verify the parts tile [0,size) with no gap.
		vf.Size = g.entries[0].Size
		if len(vf.Parts) > 1 || vf.Parts[0].FileOffset != 0 {
			var end int64
			for _, p := range vf.Parts {
				if p.FileOffset != end {
					return nil, fmt.Errorf("cargoship: split file %q has a gap/overlap at offset %d (want %d)", rel, p.FileOffset, end)
				}
				end += p.Length
			}
			vf.Size = end
		}
		files = append(files, vf)
	}

	return &Archive{Bucket: m.Bucket, Chunks: chunks, Files: files}, nil
}

// FramesFor returns the frames of chunk ci overlapping the uncompressed byte
// range [off, off+length), in order. It is the read-path mapping: a virtual
// file read at file offset o becomes chunk-uncompressed range
// [ArchiveOffset+o, ...), which this maps to the covering frame(s).
func (c *Chunk) FramesFor(off, length int64) []Frame {
	if length <= 0 || off < 0 {
		return nil
	}
	end := off + length
	lo := sort.Search(len(c.Frames), func(i int) bool {
		return c.Frames[i].UncompOff+c.Frames[i].UncompLen > off
	})
	var out []Frame
	for i := lo; i < len(c.Frames) && c.Frames[i].UncompOff < end; i++ {
		out = append(out, c.Frames[i])
	}
	return out
}
