// SPDX-License-Identifier: Apache-2.0

// Package bgzf implements the access plan for the bgzf + external-index family
// (#107): BAM/CRAM, VCF/BCF, bgzipped FASTA. bgzf is concatenated gzip blocks;
// a companion index maps genomic coordinates to bgzf *virtual offsets*
// (compressed-block-offset<<16 | within-block-offset). A region query reads the
// index, resolves the region to a handful of compressed byte ranges, and seeks
// there.
//
// Tier 1: on index-file open, fetch the whole (small) index and the data file's
// header block, which the tool reads next every time. Tier 2: parse the index
// and resolve a region to the compressed byte ranges to prefetch before the
// seek.
//
// The index bytes are attacker-controlled (a bucket lith does not own), so every
// parser here takes the #101 treatment: a size cap, every length bounds-checked,
// no panics on any input, and a committed fuzz corpus. A malformed index yields
// (nil,false) and the caller falls back to tier 1.
package bgzf

import (
	"sort"
	"strings"
)

// MaxIndexBytes caps the index body a parser will accept. An index larger than
// this is refused (the caller logs it and uses tier 1 only). Real .bai/.tbi/.csi
// are at most tens of MiB; 256 MiB is a generous ceiling.
const MaxIndexBytes = 256 << 20

// A bgzf block holds at most 64 KiB uncompressed and 64 KiB compressed.
const maxBlockSize = 1 << 16

// IndexKind identifies the external-index format.
type IndexKind int

const (
	KindNone IndexKind = iota
	KindBAI            // BAM .bai (also .bam.bai)
	KindTBI            // tabix .tbi (gzipped)
	KindCSI            // .csi (gzipped)
	KindCRAI           // CRAM .crai (gzipped TSV)
)

func (k IndexKind) String() string {
	switch k {
	case KindBAI:
		return "bai"
	case KindTBI:
		return "tbi"
	case KindCSI:
		return "csi"
	case KindCRAI:
		return "crai"
	default:
		return "none"
	}
}

// Range is a half-open compressed byte range [Start, End) of the data file to
// prefetch. Ranges from a plan are sorted and coalesced.
type Range struct {
	Start int64
	End   int64
}

// dataIndexSuffixes maps a data-file extension to the index suffixes that index
// it, in preference order. Detection asks the Index whether any of these sibling
// keys exists — no S3 call.
//
// The index key is the data key plus the suffix (htslib convention:
// `x.bam` -> `x.bam.bai` or `x.bai`; `x.vcf.gz` -> `x.vcf.gz.tbi` or `.csi`).
var dataIndexSuffixes = []struct {
	dataExt string
	index   []struct {
		suffix string
		kind   IndexKind
	}
}{
	{".cram", []struct {
		suffix string
		kind   IndexKind
	}{{".crai", KindCRAI}}},
	{".bam", []struct {
		suffix string
		kind   IndexKind
	}{{".bai", KindBAI}, {".csi", KindCSI}}},
	{".bcf", []struct {
		suffix string
		kind   IndexKind
	}{{".csi", KindCSI}}},
	{".vcf.gz", []struct {
		suffix string
		kind   IndexKind
	}{{".tbi", KindTBI}, {".csi", KindCSI}}},
	{".fa.gz", []struct {
		suffix string
		kind   IndexKind
	}{{".fai", KindNone}}}, // FASTA: header/whole-index tier-1 only (no coordinate R-tree)
	{".fasta.gz", []struct {
		suffix string
		kind   IndexKind
	}{{".fai", KindNone}}},
}

// IndexCandidate is a possible index sibling for a data file: the key to test in
// the Index and the parser kind it would be.
type IndexCandidate struct {
	Key  string
	Kind IndexKind
}

// IndexCandidates returns, for a data-file key, the candidate index sibling keys
// (both `data+suffix` and `base+suffix` conventions) in preference order. The
// caller checks each against the Index and uses the first that exists. Returns
// nil when the key is not a recognized bgzf data file.
func IndexCandidates(dataKey string) []IndexCandidate {
	lower := strings.ToLower(dataKey)
	for _, de := range dataIndexSuffixes {
		if !strings.HasSuffix(lower, de.dataExt) {
			continue
		}
		var out []IndexCandidate
		base := dataKey[:len(dataKey)-len(de.dataExt)] // strip the data extension
		for _, ix := range de.index {
			out = append(out,
				IndexCandidate{Key: dataKey + ix.suffix, Kind: ix.kind}, // x.bam.bai
				IndexCandidate{Key: base + ix.suffix, Kind: ix.kind},    // x.bai
			)
		}
		return out
	}
	return nil
}

// IsDataFile reports whether key looks like a bgzf-family data file lith should
// give the header/random-protection treatment.
func IsDataFile(key string) bool { return IndexCandidates(key) != nil }

// coalesce sorts ranges and merges overlapping/adjacent ones (within slack
// bytes), so a region's many small chunk ranges become a few GETs. It also
// clamps to [0, fileSize] and drops empty ranges.
func coalesce(rs []Range, slack, fileSize int64) []Range {
	if len(rs) == 0 {
		return nil
	}
	cl := rs[:0]
	for _, r := range rs {
		if r.Start < 0 {
			r.Start = 0
		}
		if fileSize > 0 && r.End > fileSize {
			r.End = fileSize
		}
		if r.End > r.Start {
			cl = append(cl, r)
		}
	}
	if len(cl) == 0 {
		return nil
	}
	sort.Slice(cl, func(i, j int) bool { return cl[i].Start < cl[j].Start })
	out := []Range{cl[0]}
	for _, r := range cl[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End+slack {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// cvo splits a bgzf virtual offset into its compressed block offset.
func cvo(voffset uint64) int64 { return int64(voffset >> 16) }
