// SPDX-License-Identifier: Apache-2.0

package footer

import "encoding/binary"

// ZipEntry is one central-directory entry lith needs to map a member to its
// compressed bytes.
type ZipEntry struct {
	Name              string
	LocalHeaderOffset int64 // offset of the local file header
	CompressedSize    int64
}

const (
	cdHeaderSignature = 0x02014b50 // central directory file header
	cdHeaderFixed     = 46         // fixed-size prefix of a central dir header
	eocdFixed         = 22         // fixed-size EOCD record

	zip64U16 = 0xffff
	zip64U32 = 0xffffffff
)

// ParseZipCentralDir finds the EOCD record in tail and parses the central
// directory. If the central directory begins before tail, it returns
// (nil,false) so the caller can re-fetch. It only handles the non-ZIP64
// case, is fully bounds-checked, and returns (nil,false) on any problem.
func ParseZipCentralDir(tail []byte, size int64) ([]ZipEntry, bool) {
	if size < 0 || len(tail) == 0 || int64(len(tail)) > MaxFooterBytes {
		return nil, false
	}
	eocdOff, ok := findValidEOCD(tail)
	if !ok {
		return nil, false
	}
	e := tail[eocdOff:]
	// e is guaranteed >= eocdFixed by findValidEOCD.
	totalEntries := binary.LittleEndian.Uint16(e[10:12])
	cdSize := binary.LittleEndian.Uint32(e[12:16])
	cdOffset := binary.LittleEndian.Uint32(e[16:20])

	// ZIP64 sentinels: refuse.
	if totalEntries == zip64U16 || cdSize == zip64U32 || cdOffset == zip64U32 {
		return nil, false
	}

	// The central directory must lie entirely within tail. tail covers the
	// file's last len(tail) bytes, i.e. absolute offsets [size-len(tail), size).
	tailStart := size - int64(len(tail))
	if int64(cdOffset) < tailStart {
		return nil, false // central dir starts before tail; caller re-fetches
	}
	cdStartInTail := int64(cdOffset) - tailStart
	cdEndInTail := cdStartInTail + int64(cdSize)
	if cdStartInTail < 0 || cdEndInTail > int64(len(tail)) || cdEndInTail < cdStartInTail {
		return nil, false
	}
	// The central directory must not run past where the EOCD begins.
	if cdEndInTail > int64(eocdOff) {
		return nil, false
	}

	cd := tail[cdStartInTail:cdEndInTail]
	entries := make([]ZipEntry, 0, totalEntries)
	pos := 0
	for i := 0; i < int(totalEntries); i++ {
		ent, next, ok := parseCDEntry(cd, pos)
		if !ok {
			return nil, false
		}
		entries = append(entries, ent)
		pos = next
	}
	return entries, true
}

// parseCDEntry parses one central directory header starting at cd[pos] and
// returns the entry and the offset of the next header.
func parseCDEntry(cd []byte, pos int) (ZipEntry, int, bool) {
	if pos < 0 || pos+cdHeaderFixed > len(cd) {
		return ZipEntry{}, 0, false
	}
	h := cd[pos:]
	if binary.LittleEndian.Uint32(h[0:4]) != cdHeaderSignature {
		return ZipEntry{}, 0, false
	}
	compressedSize := binary.LittleEndian.Uint32(h[20:24])
	nameLen := int(binary.LittleEndian.Uint16(h[28:30]))
	extraLen := int(binary.LittleEndian.Uint16(h[30:32]))
	commentLen := int(binary.LittleEndian.Uint16(h[32:34]))
	localOff := binary.LittleEndian.Uint32(h[42:46])

	// ZIP64 sentinels in the per-entry fields: refuse.
	if compressedSize == zip64U32 || localOff == zip64U32 {
		return ZipEntry{}, 0, false
	}

	total := cdHeaderFixed + nameLen + extraLen + commentLen
	if pos+total > len(cd) || total < cdHeaderFixed {
		return ZipEntry{}, 0, false
	}
	name := string(cd[pos+cdHeaderFixed : pos+cdHeaderFixed+nameLen])

	return ZipEntry{
		Name:              name,
		LocalHeaderOffset: int64(localOff),
		CompressedSize:    int64(compressedSize),
	}, pos + total, true
}

// findValidEOCD scans backward for an EOCD signature whose fixed record and
// declared comment length fit within tail, and returns its offset.
func findValidEOCD(tail []byte) (int, bool) {
	if len(tail) < eocdFixed {
		return 0, false
	}
	for i := len(tail) - eocdFixed; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:i+4]) != eocdSignature {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(tail[i+20 : i+22]))
		if i+eocdFixed+commentLen <= len(tail) {
			return i, true
		}
	}
	return 0, false
}
