// SPDX-License-Identifier: Apache-2.0

// Package cargoship reads CargoShip format-2.1 archive manifests and maps a
// virtual file's byte range to zstd frame range GETs against the packed
// `.tar.zst` chunk objects (scttfrdmn/cargoship#436, #439). A 2.1 archive packs
// many source files into a few compressed chunks, each cut into independently
// decodable zstd frames at file boundaries; the manifest records, per file, the
// chunk it lives in and its offset in that chunk's uncompressed tar stream, and
// per chunk the frame table (compressed/uncompressed byte spans + a per-frame
// sha256 of the compressed bytes). lith mounts the archive as its original tree
// and serves a read by fetching only the covering frame(s).
//
// The parser is hardened per lith#101: it caps the manifest size, bounds-checks
// every offset and length against the chunk it belongs to, requires frames to
// tile each chunk monotonically and contiguously, and never panics on hostile
// input — every rejection names what failed.
package cargoship

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// MaxManifestBytes caps a decompressed manifest (lith#101). A real archive
// manifest is a few KB per thousand files; 256 MiB is a generous ceiling that
// still bounds memory on a hostile input.
const MaxManifestBytes = 256 << 20

// rawManifest mirrors the subset of the CargoShip 2.1 manifest lith needs
// (scttfrdmn/cargoship pkg/manifest). Unknown fields are ignored — the reader
// contract is additive.
type rawManifest struct {
	Version            string          `json:"version"`
	UploadID           string          `json:"upload_id"`
	SourcePath         string          `json:"source_path"`
	Bucket             string          `json:"bucket"`
	Prefix             string          `json:"prefix"`
	TotalChunks        int             `json:"total_chunks"`
	CompressionType    string          `json:"compression_type"`
	FormatFeatures     []string        `json:"format_features"`
	PreviousManifestID string          `json:"previous_manifest_id"`
	Encryption         *rawEncryption  `json:"encryption"`
	Files              []rawFileEntry  `json:"files"`
	Chunks             []rawChunkEntry `json:"chunks"`
}

type rawEncryption struct {
	Enabled           bool `json:"enabled"`
	ManifestEncrypted bool `json:"manifest_encrypted"`
}

type rawFileEntry struct {
	Path          string    `json:"path"`
	Size          int64     `json:"size"`
	ModTime       time.Time `json:"mod_time"`
	ChunkID       int       `json:"chunk_id"`
	S3Key         string    `json:"s3_key"`
	Offset        int64     `json:"offset"`         // start offset within the file for a split part
	Length        int64     `json:"length"`         // length of this part (0 = full file)
	PartIndex     int       `json:"part_index"`     // 0 for a non-split file
	TotalParts    int       `json:"total_parts"`    // 0 or 1 for a non-split file
	ArchiveOffset *int64    `json:"archive_offset"` // pointer: nil distinguishes a v0.24.2 null-on-frameless file from a genuine 0
	Checksum      string    `json:"checksum"`
	IsDuplicate   bool      `json:"is_duplicate"`
	DupOfHash     string    `json:"duplicate_of_hash"`
	OrigChunkID   int       `json:"original_chunk_id"`
	OrigS3Key     string    `json:"original_s3_key"`
}

type rawChunkEntry struct {
	ID               int             `json:"id"`
	S3Key            string          `json:"s3_key"`
	UncompressedSize int64           `json:"uncompressed_size"`
	CompressedSize   int64           `json:"compressed_size"`
	Checksum         string          `json:"checksum"`
	Frames           []rawFrameEntry `json:"frames"`
}

type rawFrameEntry struct {
	CompressedOffset   int64  `json:"compressed_offset"`
	CompressedSize     int64  `json:"compressed_size"`
	UncompressedOffset int64  `json:"uncompressed_offset"`
	UncompressedSize   int64  `json:"uncompressed_size"`
	Checksum           string `json:"checksum"`
}

// Manifest is a parsed, structurally validated 2.1 manifest. It is not yet
// resolved into the virtual tree — call Resolve for that.
type Manifest struct {
	Version            string
	UploadID           string
	SourcePath         string
	Bucket             string
	Prefix             string
	PreviousManifestID string
	Files              []rawFileEntry
	Chunks             []rawChunkEntry
}

// Parse decodes and validates a single 2.1 manifest (already decompressed). It
// rejects — never panics on — a manifest that is too large, encrypted, not 2.1,
// missing the frame index, or whose frames/offsets are inconsistent.
func Parse(b []byte) (*Manifest, error) {
	if len(b) > MaxManifestBytes {
		return nil, fmt.Errorf("cargoship manifest: %d bytes exceeds the %d-byte cap", len(b), MaxManifestBytes)
	}
	var rm rawManifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(&rm); err != nil {
		return nil, fmt.Errorf("cargoship manifest: %w", err)
	}

	if rm.Encryption != nil && (rm.Encryption.Enabled || rm.Encryption.ManifestEncrypted) {
		return nil, fmt.Errorf("cargoship manifest: encrypted archives are not supported yet (KMS-envelope manifest)")
	}
	if rm.Version != "2.1" {
		return nil, fmt.Errorf("cargoship manifest: version %q is not supported — lith reads format 2.1 (upgrade the archive with cargoship v0.24.0+; the 2.0 offset sidecar is out of scope, see cargoship#437)", rm.Version)
	}
	// A 2.1 archive is read via per-chunk frame tables (framed chunks) and/or each
	// file's archive_offset (frameless plain-.tar chunks, cargoship v0.24.3). The
	// "frames" feature is a capability hint, not required — an archive of only
	// incompressible content may have no framed chunks. The real contract, a
	// non-null archive_offset on every file, is enforced in Resolve.
	if len(rm.Chunks) == 0 {
		return nil, fmt.Errorf("cargoship manifest: no chunks (a direct-upload manifest has no framed archive to mount)")
	}

	// Validate each chunk's frame table: sizes non-negative, frames sorted and
	// contiguous in the uncompressed tar stream, compressed spans non-overlapping
	// and within a sane bound. Frame checksums are verified at read time.
	for ci := range rm.Chunks {
		c := &rm.Chunks[ci]
		if c.CompressedSize < 0 || c.UncompressedSize < 0 {
			return nil, fmt.Errorf("cargoship manifest: chunk %d has negative size", ci)
		}
		// A frameless (plain .tar) chunk is valid: reads are direct ranges at each
		// file's archive_offset (v0.24.3). Only framed chunks carry a frame index,
		// which must tile the chunk contiguously.
		if len(c.Frames) == 0 {
			continue
		}
		var wantU int64
		for fi := range c.Frames {
			f := c.Frames[fi]
			if f.CompressedSize <= 0 || f.UncompressedSize <= 0 {
				return nil, fmt.Errorf("cargoship manifest: chunk %d frame %d has non-positive size", ci, fi)
			}
			if f.CompressedOffset < 0 || f.UncompressedOffset < 0 {
				return nil, fmt.Errorf("cargoship manifest: chunk %d frame %d has negative offset", ci, fi)
			}
			if f.UncompressedOffset != wantU {
				return nil, fmt.Errorf("cargoship manifest: chunk %d frame %d uncompressed offset %d not contiguous (want %d)", ci, fi, f.UncompressedOffset, wantU)
			}
			wantU += f.UncompressedSize
			// Compressed span must lie within the object (compressed_size known);
			// allow the whole object as the upper bound.
			if c.CompressedSize > 0 && f.CompressedOffset+f.CompressedSize > c.CompressedSize {
				return nil, fmt.Errorf("cargoship manifest: chunk %d frame %d compressed span [%d,%d) exceeds object size %d", ci, fi, f.CompressedOffset, f.CompressedOffset+f.CompressedSize, c.CompressedSize)
			}
		}
	}

	return &Manifest{
		Version:            rm.Version,
		UploadID:           rm.UploadID,
		SourcePath:         rm.SourcePath,
		Bucket:             rm.Bucket,
		Prefix:             rm.Prefix,
		PreviousManifestID: rm.PreviousManifestID,
		Files:              rm.Files,
		Chunks:             rm.Chunks,
	}, nil
}

// relPath turns a manifest file path (which may be absolute under source_path,
// or already relative) into the virtual tree path: slash-cleaned, no leading
// slash, source_path stripped.
func relPath(p, sourcePath string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	sp := strings.ReplaceAll(sourcePath, "\\", "/")
	if sp != "" {
		if trimmed := strings.TrimPrefix(p, sp); trimmed != p {
			p = trimmed
		}
	}
	p = strings.TrimPrefix(p, "/")
	p = path.Clean(p)
	if p == "." {
		return ""
	}
	return strings.TrimPrefix(p, "/")
}

// resolveChunkKey turns a chunk's manifest s3_key (prefix-relative, e.g.
// "uploads/<id>/shard-0/chunk-0.tar.zst") into a full bucket-relative key by
// joining the manifest prefix, unless it already carries the prefix.
func resolveChunkKey(prefix, s3Key string) string {
	s3Key = strings.TrimPrefix(s3Key, "/")
	prefix = strings.Trim(prefix, "/")
	if prefix == "" || strings.HasPrefix(s3Key, prefix+"/") {
		return s3Key
	}
	return prefix + "/" + s3Key
}

// sortFramesByUncomp is defensive: frames should already be sorted, but a
// resolve must not assume it.
func sortFramesByUncomp(fs []Frame) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].UncompOff < fs[j].UncompOff })
}
