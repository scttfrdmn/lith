// SPDX-License-Identifier: Apache-2.0

// Package footer holds the pure parsing and detection logic for lith's
// "footer family" access plan (issue #108): columnar/archive formats whose
// layout is described by a metadata block at the tail of the object
// (Parquet, ORC, Arrow/Feather, zip). It maps a demand read to the
// compressed byte ranges that must be fetched, so the FUSE layer can
// prefetch a projection instead of the whole object.
//
// Everything here is pure byte parsing: no S3, no FUSE, no I/O. The input
// bytes come from a bucket lith does not own and are therefore treated as
// hostile (see issue #101): every length and count read from the bytes is
// bounds-checked before use, inputs larger than MaxFooterBytes are refused,
// and no input may cause a panic. On any malformed, short, or oversize
// input the parsers return (nil,false) / false rather than partial results.
package footer

import (
	"encoding/binary"
	"strings"
)

// Format is a candidate footer-family container format.
type Format int

const (
	// FormatNone means the key does not name a known footer-family format.
	FormatNone Format = iota
	// FormatParquet is Apache Parquet.
	FormatParquet
	// FormatORC is Apache ORC.
	FormatORC
	// FormatArrow is Arrow IPC file / Feather v2.
	FormatArrow
	// FormatZip is a zip archive.
	FormatZip
)

// String returns the lower-case name of the format.
func (f Format) String() string {
	switch f {
	case FormatParquet:
		return "parquet"
	case FormatORC:
		return "orc"
	case FormatArrow:
		return "arrow"
	case FormatZip:
		return "zip"
	case FormatNone:
		return "none"
	default:
		return "none"
	}
}

const (
	// TailBytes is how many bytes lith fetches from the tail of the object
	// on open to try to satisfy the footer parse in a single range GET.
	TailBytes = 64 << 10
	// MaxFooterBytes caps the footer / central-directory input size. Any
	// length field or input exceeding this is refused (#101).
	MaxFooterBytes = 64 << 20
)

// Magic byte sequences.
var (
	magicParquet = []byte("PAR1")
	magicORC     = []byte("ORC")
	magicArrow   = []byte("ARROW1")
)

// eocdSignature is the zip End-Of-Central-Directory record signature.
const eocdSignature = 0x06054b50

// Range is a half-open compressed byte range [Start,End) in the data file.
type Range struct{ Start, End int64 }

// Detect returns the candidate format from the key's extension only, doing
// no I/O. Unknown extensions return FormatNone.
func Detect(key string) Format {
	// Use the portion after the last '/' so a directory named ".zip/" or a
	// dotted path segment cannot fool the extension match.
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		key = key[i+1:]
	}
	lower := strings.ToLower(key)
	switch {
	case strings.HasSuffix(lower, ".parquet"):
		return FormatParquet
	case strings.HasSuffix(lower, ".orc"):
		return FormatORC
	case strings.HasSuffix(lower, ".arrow"), strings.HasSuffix(lower, ".feather"):
		return FormatArrow
	case strings.HasSuffix(lower, ".zip"):
		return FormatZip
	default:
		return FormatNone
	}
}

// ConfirmMagic confirms format f from the object's head (first >=8 bytes)
// and tail (last up to TailBytes bytes) and total size. It returns false if
// the magic does not match.
func ConfirmMagic(f Format, head, tail []byte, size int64) bool {
	if size < 0 {
		return false
	}
	switch f {
	case FormatParquet:
		// "PAR1" at the very start of head AND at the very end of tail.
		return hasPrefix(head, magicParquet) && hasSuffix(tail, magicParquet)
	case FormatORC:
		// Head starts with "ORC"; the file ends with the ORC magic that the
		// PostScript's final length byte points at.
		if !hasPrefix(head, magicORC) {
			return false
		}
		return orcTailHasMagic(tail)
	case FormatArrow:
		// Arrow IPC file / Feather v2: "ARROW1" at both ends.
		return hasPrefix(head, magicArrow) && hasSuffix(tail, magicArrow)
	case FormatZip:
		// EOCD signature somewhere in the last 64 KiB.
		_, ok := findEOCD(tail)
		return ok
	default:
		return false
	}
}

// orcTailHasMagic confirms the ORC PostScript trailer. The last byte of the
// file is the PostScript length; immediately before the PostScript sits the
// 3-byte "ORC" magic. So the bytes at offset (end-1-psLen-3 .. end-1-psLen)
// must equal "ORC".
func orcTailHasMagic(tail []byte) bool {
	if len(tail) < 1 {
		return false
	}
	psLen := int(tail[len(tail)-1])
	// Need: [ORC magic (3)][PostScript (psLen)][length byte (1)] at the end.
	need := 3 + psLen + 1
	if need > len(tail) {
		return false
	}
	magicPos := len(tail) - need
	return string(tail[magicPos:magicPos+3]) == string(magicORC)
}

// hasPrefix reports whether b begins with p.
func hasPrefix(b, p []byte) bool {
	return len(b) >= len(p) && string(b[:len(p)]) == string(p)
}

// hasSuffix reports whether b ends with s.
func hasSuffix(b, s []byte) bool {
	return len(b) >= len(s) && string(b[len(b)-len(s):]) == string(s)
}

// ParquetFooterStart parses the trailing 8 bytes of tail (a 4-byte
// little-endian footer length immediately before the final "PAR1") and
// returns the file offset where the footer metadata begins
// (= size - 8 - footerLen) and whether the entire footer is already
// contained in tail. It returns ok=false if tail is too short or the length
// is implausible.
func ParquetFooterStart(tail []byte, size int64) (footerStart int64, wholeInTail bool, ok bool) {
	if size < 8 || len(tail) < 8 {
		return 0, false, false
	}
	if !hasSuffix(tail, magicParquet) {
		return 0, false, false
	}
	// footer length is the 4 bytes immediately before the trailing magic.
	lenOff := len(tail) - 8
	footerLen := int64(binary.LittleEndian.Uint32(tail[lenOff : lenOff+4]))
	if footerLen < 0 || footerLen > MaxFooterBytes {
		return 0, false, false
	}
	if footerLen+8 > size {
		return 0, false, false
	}
	footerStart = size - 8 - footerLen
	tailStart := size - int64(len(tail))
	wholeInTail = footerStart >= tailStart
	return footerStart, wholeInTail, true
}

// findEOCD scans tail backward for the EOCD signature and returns its offset
// within tail. The comment length field bounds a valid record, but for
// magic confirmation any signature match in the last 64 KiB is enough.
func findEOCD(tail []byte) (int, bool) {
	// EOCD is at least 22 bytes. Scan from the latest possible start.
	if len(tail) < 4 {
		return 0, false
	}
	for i := len(tail) - 4; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:i+4]) == eocdSignature {
			return i, true
		}
	}
	return 0, false
}
