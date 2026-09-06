// SPDX-License-Identifier: Apache-2.0

package index

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"
)

// The on-disk index format is lith-private and versioned. It carries no
// compatibility promise before v1 (see the pinned Design issue, §10). Numeric
// arrays are stored in host byte order so a built index can be memory-mapped
// and queried without copying; a mismatched byte order is rejected on load.

const (
	magic       = "LITHIDX1"
	formatVer   = uint32(1)
	headerSize  = 48 // magic(8)+ver(4)+flags(4)+keyCount(8)+dropped(8)+shadowed(8)+collisions(8)
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

// Marshal serializes the index to a byte slice in the v1 format.
func (ix *Index) Marshal() []byte {
	n := ix.Len()
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
	off = align8(offsAt + (n+1)*4)
	sizesAt := off
	off += n * 8
	mtimesAt := off
	off += n * 8
	etagsAt := off
	off += n * 8
	inosAt := off
	off += n * 8

	buf := make([]byte, off)
	copy(buf[0:8], magic)
	ne.PutUint32(buf[8:], formatVer)
	ne.PutUint32(buf[12:], flags)
	ne.PutUint64(buf[16:], uint64(n))
	ne.PutUint64(buf[24:], ix.dropped)
	ne.PutUint64(buf[32:], ix.shadowed)
	ne.PutUint64(buf[40:], ix.collisions)

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

	copyU32(buf[offsAt:], ix.offs)
	copyU64(buf[sizesAt:], ix.sizes)
	copyI64(buf[mtimesAt:], ix.mtimes)
	copyU64(buf[etagsAt:], ix.etags)
	copyU64(buf[inosAt:], ix.inos)
	return buf
}

// Unmarshal parses a v1 index image into a new, heap-owned Index (the arrays
// are copied out of b).
func Unmarshal(b []byte) (*Index, error) {
	ix, err := parse(b, true)
	if err != nil {
		return nil, err
	}
	return ix, nil
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

// parse reads a v1 image. When copyOut is true the arrays are copied so they
// do not alias b; otherwise they point directly into b (used by the mmap
// loader).
func parse(b []byte, copyOut bool) (*Index, error) {
	if len(b) < headerSize {
		return nil, fmt.Errorf("index: image too small (%d bytes)", len(b))
	}
	if string(b[0:8]) != magic {
		return nil, fmt.Errorf("index: bad magic")
	}
	ne := binary.NativeEndian
	if v := ne.Uint32(b[8:]); v != formatVer {
		return nil, fmt.Errorf("index: unsupported format version %d", v)
	}
	flags := ne.Uint32(b[12:])
	if (flags>>1)&1 != hostByteOrder() {
		return nil, fmt.Errorf("index: image byte order does not match host")
	}
	n := int(ne.Uint64(b[16:]))

	ix := &Index{
		execMode:   flags&flagExec != 0,
		dropped:    ne.Uint64(b[24:]),
		shadowed:   ne.Uint64(b[32:]),
		collisions: ne.Uint64(b[40:]),
	}

	p := headerSize
	blen := int(ne.Uint32(b[p:]))
	p += 4
	ix.bucket = string(b[p : p+blen])
	p += blen
	plen := int(ne.Uint32(b[p:]))
	p += 4
	ix.prefix = string(b[p : p+plen])
	p += plen
	p = align8(p)

	arenaLen := int(ne.Uint64(b[p:]))
	arenaAt := p + 8
	p = align8(arenaAt + arenaLen)
	offsAt := p
	p = align8(offsAt + (n+1)*4)
	sizesAt := p
	p += n * 8
	mtimesAt := p
	p += n * 8
	etagsAt := p
	p += n * 8
	inosAt := p
	p += n * 8
	if len(b) < p {
		return nil, fmt.Errorf("index: image truncated (want %d, have %d)", p, len(b))
	}

	if copyOut {
		ix.arena = append([]byte(nil), b[arenaAt:arenaAt+arenaLen]...)
		ix.offs = append([]uint32(nil), asU32(b[offsAt:], n+1)...)
		ix.sizes = append([]uint64(nil), asU64(b[sizesAt:], n)...)
		ix.mtimes = append([]int64(nil), asI64(b[mtimesAt:], n)...)
		ix.etags = append([]uint64(nil), asU64(b[etagsAt:], n)...)
		ix.inos = append([]uint64(nil), asU64(b[inosAt:], n)...)
	} else {
		ix.arena = b[arenaAt : arenaAt+arenaLen]
		ix.offs = asU32(b[offsAt:], n+1)
		ix.sizes = asU64(b[sizesAt:], n)
		ix.mtimes = asI64(b[mtimesAt:], n)
		ix.etags = asU64(b[etagsAt:], n)
		ix.inos = asU64(b[inosAt:], n)
	}
	return ix, nil
}

// --- unsafe reinterpretation helpers (host byte order) ---

func asU32(b []byte, n int) []uint32 {
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&b[0])), n)
}

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

func copyU32(dst []byte, src []uint32) {
	if len(src) == 0 {
		return
	}
	copy(dst, unsafe.Slice((*byte)(unsafe.Pointer(&src[0])), len(src)*4))
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
