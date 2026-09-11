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
	// Chunks, indexed by their manifest ID.
	chunks := make([]Chunk, len(m.Chunks))
	idToIdx := make(map[int]int, len(m.Chunks))
	for i, rc := range m.Chunks {
		frames := make([]Frame, len(rc.Frames))
		for fi, rf := range rc.Frames {
			fr := Frame{
				CompOff: rf.CompressedOffset, CompLen: rf.CompressedSize,
				UncompOff: rf.UncompressedOffset, UncompLen: rf.UncompressedSize,
			}
			if rf.Checksum != "" {
				sum, err := hex.DecodeString(rf.Checksum)
				if err != nil || len(sum) != 32 {
					return nil, fmt.Errorf("cargoship: chunk %d frame %d has a malformed checksum", rc.ID, fi)
				}
				copy(fr.Sum[:], sum)
				fr.HasSum = true
			}
			frames[fi] = fr
		}
		sortFramesByUncomp(frames)
		var total int64
		for _, fr := range frames {
			total += fr.UncompLen
		}
		chunks[i] = Chunk{Key: resolveChunkKey(m.Prefix, rc.S3Key), UncompTotal: total, Frames: frames}
		if _, dup := idToIdx[rc.ID]; dup {
			return nil, fmt.Errorf("cargoship: duplicate chunk id %d", rc.ID)
		}
		idToIdx[rc.ID] = i
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
		ci, ok := idToIdx[src.ChunkID]
		if !ok {
			return Part{}, fmt.Errorf("cargoship: file %q references unknown chunk id %d", fe.Path, src.ChunkID)
		}
		length := fe.Length
		if length == 0 { // full file
			length = fe.Size
		}
		p := Part{ChunkIndex: ci, ArchiveOffset: src.ArchiveOffset, FileOffset: fe.Offset, Length: length}
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
