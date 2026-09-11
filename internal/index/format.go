// SPDX-License-Identifier: Apache-2.0

package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"
)

// ErrCorruptIndex is returned when an index image is malformed — a length or
// offset in the header points outside the image, or an arena offset is out of
// range. Parsing a corrupt image returns this error (wrapped, naming the byte
// offset at fault); it never panics.
var ErrCorruptIndex = errors.New("index: corrupt image")

func corruptf(off int, format string, a ...any) error {
	return fmt.Errorf("%w: offset %d: %s", ErrCorruptIndex, off, fmt.Sprintf(format, a...))
}

// inBounds reports whether [at, at+size) lies within a slice of length blen.
// Overflow-safe for any int at, size (size <= blen-at avoids at+size wrap).
func inBounds(blen, at, size int) bool {
	return at >= 0 && size >= 0 && at <= blen && size <= blen-at
}

// The on-disk index format is lith-private and versioned. It carries no
// compatibility promise before v1 (see the pinned Design issue, §10). Numeric
// arrays are stored in host byte order so a built index can be memory-mapped
// and queried without copying; a mismatched byte order is rejected on load.
//
// v2 adds the directory table (sorted dir paths, per-directory inodes and
// mtimes). v3 widens the arena offset arrays (offs/dirOffs) from uint32 to
// uint64 so the pathname arena is not capped at ~4 GiB. v4 appends a provenance
// trailer at the very end of the image (after the dir arrays, so the
// mmap-reinterpreted arrays are undisturbed): a length-prefixed `source` string
// (e.g. "list", "keys", "manifest", "inventory") followed by 32 raw sha256
// bytes recording the key file/manifest the index was built from. Older files
// are rejected with a message to rebuild.

// FormatVersion is the current on-disk index format version.
const FormatVersion = 4

const (
	magic       = "LITHIDX1"
	formatVer   = uint32(FormatVersion)
	headerSize  = 56 // magic(8)+ver(4)+flags(4)+keyCount(8)+dropped(8)+shadowed(8)+collisions(8)+dirCount(8)
	flagExec    = uint32(1) << 0
	byteOrderLE = 0
	byteOrderBE = 1
)

// hostByteOrder reports 0 for little-endian, 1 for big-endian hosts.
func hostByteOrder() uint32 {
	var x uint16 = 1
	if *(*byte)(unsafe.Pointer(&x)) == 1 {
		return byteOrderLE
	}
	return byteOrderBE
}

func align8(n int) int { return (n + 7) &^ 7 }

// Marshal serializes the index to a byte slice in the current format.
func (ix *Index) Marshal() []byte {
	n := ix.Len()
	m := ix.dirCount()
	ne := binary.NativeEndian

	flags := hostByteOrder() << 1
	if ix.execMode {
		flags |= flagExec
	}

	// Compute the layout so we can allocate once.
	off := headerSize
	off += 4 + len(ix.bucket)
	off += 4 + len(ix.prefix)
	off = align8(off)
	arenaAt := off + 8 // arenaLen(8) precedes the bytes
	off = align8(arenaAt + len(ix.arena))
	offsAt := off
	off = align8(offsAt + (n+1)*8)
	sizesAt := off
	off += n * 8
	mtimesAt := off
	off += n * 8
	etagsAt := off
	off += n * 8
	inosAt := off
	off = align8(inosAt + n*8)
	dirArenaAt := off + 8 // dirArenaLen(8) precedes the bytes
	off = align8(dirArenaAt + len(ix.dirArena))
	dirOffsAt := off
	off = align8(dirOffsAt + (m+1)*8)
	dirMtimesAt := off
	off += m * 8
	dirInosAt := off
	off += m * 8

	// Provenance trailer (v4): srcLen(4) + source bytes + 32 raw sha bytes. It
	// lives after the dir arrays so the mmap-reinterpreted arrays are undisturbed.
	srcLenAt := off
	off += 4
	srcAt := off
	off += len(ix.source)
	shaAt := off
	off += 32

	buf := make([]byte, off)
	copy(buf[0:8], magic)
	ne.PutUint32(buf[8:], formatVer)
	ne.PutUint32(buf[12:], flags)
	ne.PutUint64(buf[16:], uint64(n))
	ne.PutUint64(buf[24:], ix.dropped)
	ne.PutUint64(buf[32:], ix.shadowed)
	ne.PutUint64(buf[40:], ix.collisions)
	ne.PutUint64(buf[48:], uint64(m))

	p := headerSize
	ne.PutUint32(buf[p:], uint32(len(ix.bucket)))
	p += 4
	copy(buf[p:], ix.bucket)
	p += len(ix.bucket)
	ne.PutUint32(buf[p:], uint32(len(ix.prefix)))
	p += 4
	copy(buf[p:], ix.prefix)

	ne.PutUint64(buf[arenaAt-8:], uint64(len(ix.arena)))
	copy(buf[arenaAt:], ix.arena)
	copyU64(buf[offsAt:], ix.offs)
	copyU64(buf[sizesAt:], ix.sizes)
	copyI64(buf[mtimesAt:], ix.mtimes)
	copyU64(buf[etagsAt:], ix.etags)
	copyU64(buf[inosAt:], ix.inos)

	ne.PutUint64(buf[dirArenaAt-8:], uint64(len(ix.dirArena)))
	copy(buf[dirArenaAt:], ix.dirArena)
	copyU64(buf[dirOffsAt:], ix.dirOffs)
	copyI64(buf[dirMtimesAt:], ix.dirMtimes)
	copyU64(buf[dirInosAt:], ix.dirInos)

	ne.PutUint32(buf[srcLenAt:], uint32(len(ix.source)))
	copy(buf[srcAt:], ix.source)
	copy(buf[shaAt:], ix.keysSHA[:])
	return buf
}

// Unmarshal parses an index image into a new, heap-owned Index (the arrays are
// copied out of b).
func Unmarshal(b []byte) (*Index, error) {
	return parse(b, true)
}

// Save writes the index to path atomically (temp file + rename).
func (ix *Index) Save(path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lith-index-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(ix.Marshal()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// parse reads an index image. When copyOut is true the arrays are copied so
// they do not alias b; otherwise they point directly into b (mmap loader).
func parse(b []byte, copyOut bool) (*Index, error) {
	if len(b) < headerSize {
		return nil, corruptf(0, "image too small: %d bytes, header needs %d", len(b), headerSize)
	}
	if string(b[0:8]) != magic {
		return nil, corruptf(0, "bad magic")
	}
	ne := binary.NativeEndian
	if v := ne.Uint32(b[8:]); v != formatVer {
		return nil, fmt.Errorf("index: unsupported format version %d (this build writes v%d); rebuild the index with `lith index build`", v, formatVer)
	}
	flags := ne.Uint32(b[12:])
	if (flags>>1)&1 != hostByteOrder() {
		return nil, fmt.Errorf("index: image byte order does not match host; rebuild the index")
	}
	blen := len(b)
	// Element counts come from the (attacker-controllable) header. Each key
	// costs >= 8 bytes (offs) and each dir >= 8 (dirOffs), so a count larger
	// than the whole image is impossible; reject early so (n+1)*8 below cannot
	// overflow int.
	nRaw := ne.Uint64(b[16:])
	mRaw := ne.Uint64(b[48:])
	if nRaw > uint64(blen) {
		return nil, corruptf(16, "key count %d exceeds image size %d", nRaw, blen)
	}
	if mRaw > uint64(blen) {
		return nil, corruptf(48, "dir count %d exceeds image size %d", mRaw, blen)
	}
	n, m := int(nRaw), int(mRaw)

	ix := &Index{
		execMode:   flags&flagExec != 0,
		dropped:    ne.Uint64(b[24:]),
		shadowed:   ne.Uint64(b[32:]),
		collisions: ne.Uint64(b[40:]),
	}

	// bucket + prefix: length-prefixed strings.
	p := headerSize
	if !inBounds(blen, p, 4) {
		return nil, corruptf(p, "truncated before bucket length")
	}
	bkLen := int(ne.Uint32(b[p:]))
	p += 4
	if !inBounds(blen, p, bkLen) {
		return nil, corruptf(p, "bucket length %d runs past end of image (%d left)", bkLen, blen-p)
	}
	ix.bucket = string(b[p : p+bkLen])
	p += bkLen
	if !inBounds(blen, p, 4) {
		return nil, corruptf(p, "truncated before prefix length")
	}
	plen := int(ne.Uint32(b[p:]))
	p += 4
	if !inBounds(blen, p, plen) {
		return nil, corruptf(p, "prefix length %d runs past end of image (%d left)", plen, blen-p)
	}
	ix.prefix = string(b[p : p+plen])
	p += plen
	p = align8(p)

	// arena (length-prefixed).
	if !inBounds(blen, p, 8) {
		return nil, corruptf(p, "truncated before arena length")
	}
	arenaLen := int(ne.Uint64(b[p:]))
	arenaAt := p + 8
	if arenaLen < 0 || !inBounds(blen, arenaAt, arenaLen) {
		return nil, corruptf(p, "arena length %d runs past end of image (%d left)", arenaLen, blen-arenaAt)
	}
	p = align8(arenaAt + arenaLen)

	// Per-key arrays. Validate every region against the image before slicing.
	offsAt := p
	if !inBounds(blen, offsAt, (n+1)*8) {
		return nil, corruptf(offsAt, "offs array (%d entries) runs past end of image", n+1)
	}
	p = align8(offsAt + (n+1)*8)
	sizesAt := p
	if !inBounds(blen, sizesAt, n*8) {
		return nil, corruptf(sizesAt, "sizes array runs past end of image")
	}
	p += n * 8
	mtimesAt := p
	if !inBounds(blen, mtimesAt, n*8) {
		return nil, corruptf(mtimesAt, "mtimes array runs past end of image")
	}
	p += n * 8
	etagsAt := p
	if !inBounds(blen, etagsAt, n*8) {
		return nil, corruptf(etagsAt, "etags array runs past end of image")
	}
	p += n * 8
	inosAt := p
	if !inBounds(blen, inosAt, n*8) {
		return nil, corruptf(inosAt, "inos array runs past end of image")
	}
	p = align8(inosAt + n*8)

	// dir arena (length-prefixed).
	if !inBounds(blen, p, 8) {
		return nil, corruptf(p, "truncated before dir arena length")
	}
	dirArenaLen := int(ne.Uint64(b[p:]))
	dirArenaAt := p + 8
	if dirArenaLen < 0 || !inBounds(blen, dirArenaAt, dirArenaLen) {
		return nil, corruptf(p, "dir arena length %d runs past end of image (%d left)", dirArenaLen, blen-dirArenaAt)
	}
	p = align8(dirArenaAt + dirArenaLen)

	dirOffsAt := p
	if !inBounds(blen, dirOffsAt, (m+1)*8) {
		return nil, corruptf(dirOffsAt, "dirOffs array (%d entries) runs past end of image", m+1)
	}
	p = align8(dirOffsAt + (m+1)*8)
	dirMtimesAt := p
	if !inBounds(blen, dirMtimesAt, m*8) {
		return nil, corruptf(dirMtimesAt, "dir mtimes array runs past end of image")
	}
	p += m * 8
	dirInosAt := p
	if !inBounds(blen, dirInosAt, m*8) {
		return nil, corruptf(dirInosAt, "dir inos array runs past end of image")
	}
	p = dirInosAt + m*8

	// Provenance trailer (v4): length-prefixed source string + 32 sha bytes.
	// Read defensively so a truncated or forged trailer errors (never panics).
	if !inBounds(blen, p, 4) {
		return nil, corruptf(p, "truncated before source length")
	}
	srcLen := int(ne.Uint32(b[p:]))
	p += 4
	if !inBounds(blen, p, srcLen) {
		return nil, corruptf(p, "source length %d runs past end of image (%d left)", srcLen, blen-p)
	}
	ix.source = string(b[p : p+srcLen])
	p += srcLen
	if !inBounds(blen, p, 32) {
		return nil, corruptf(p, "truncated before keys sha256")
	}
	copy(ix.keysSHA[:], b[p:p+32])

	if copyOut {
		ix.arena = append([]byte(nil), b[arenaAt:arenaAt+arenaLen]...)
		ix.offs = append([]uint64(nil), asU64(b[offsAt:], n+1)...)
		ix.sizes = append([]uint64(nil), asU64(b[sizesAt:], n)...)
		ix.mtimes = append([]int64(nil), asI64(b[mtimesAt:], n)...)
		ix.etags = append([]uint64(nil), asU64(b[etagsAt:], n)...)
		ix.inos = append([]uint64(nil), asU64(b[inosAt:], n)...)
		ix.dirArena = append([]byte(nil), b[dirArenaAt:dirArenaAt+dirArenaLen]...)
		ix.dirOffs = append([]uint64(nil), asU64(b[dirOffsAt:], m+1)...)
		ix.dirMtimes = append([]int64(nil), asI64(b[dirMtimesAt:], m)...)
		ix.dirInos = append([]uint64(nil), asU64(b[dirInosAt:], m)...)
	} else {
		ix.arena = b[arenaAt : arenaAt+arenaLen]
		ix.offs = asU64(b[offsAt:], n+1)
		ix.sizes = asU64(b[sizesAt:], n)
		ix.mtimes = asI64(b[mtimesAt:], n)
		ix.etags = asU64(b[etagsAt:], n)
		ix.inos = asU64(b[inosAt:], n)
		ix.dirArena = b[dirArenaAt : dirArenaAt+dirArenaLen]
		ix.dirOffs = asU64(b[dirOffsAt:], m+1)
		ix.dirMtimes = asI64(b[dirMtimesAt:], m)
		ix.dirInos = asU64(b[dirInosAt:], m)
	}

	// Validate arena offsets so later arena[offs[i]:offs[i+1]] slicing (Key,
	// dirName, Readdir) cannot panic on a bit-flipped offset: each must be
	// monotonic non-decreasing and bounded by its arena length.
	if err := validateOffsets(ix.offs, arenaLen, offsAt); err != nil {
		return nil, err
	}
	if err := validateOffsets(ix.dirOffs, dirArenaLen, dirOffsAt); err != nil {
		return nil, err
	}
	return ix, nil
}

// validateOffsets checks that offs is monotonic non-decreasing and every entry
// is <= arenaLen (so arena[offs[i]:offs[i+1]] is always a valid sub-slice).
func validateOffsets(offs []uint64, arenaLen, baseAt int) error {
	prev := uint64(0)
	for i, o := range offs {
		if o > uint64(arenaLen) {
			return corruptf(baseAt+i*8, "arena offset %d exceeds arena length %d", o, arenaLen)
		}
		if o < prev {
			return corruptf(baseAt+i*8, "arena offset %d not monotonic (previous %d)", o, prev)
		}
		prev = o
	}
	return nil
}

// --- unsafe reinterpretation helpers (host byte order) ---

func asU64(b []byte, n int) []uint64 {
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&b[0])), n)
}

func asI64(b []byte, n int) []int64 {
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*int64)(unsafe.Pointer(&b[0])), n)
}

func copyU64(dst []byte, src []uint64) {
	if len(src) == 0 {
		return
	}
	copy(dst, unsafe.Slice((*byte)(unsafe.Pointer(&src[0])), len(src)*8))
}

func copyI64(dst []byte, src []int64) {
	if len(src) == 0 {
		return
	}
	copy(dst, unsafe.Slice((*byte)(unsafe.Pointer(&src[0])), len(src)*8))
}
