// SPDX-License-Identifier: Apache-2.0

package bgzf

import "encoding/binary"

// rdr is a bounds-checked little-endian cursor over an index image. Every read
// checks remaining bytes first; on underrun it sets ok=false and subsequent
// reads are no-ops, so a parser can read optimistically and check ok once at the
// end without ever indexing out of range (the #101 no-panic rule).
type rdr struct {
	b  []byte
	p  int
	ok bool
}

func newRdr(b []byte) *rdr { return &rdr{b: b, ok: true} }

func (r *rdr) need(n int) bool {
	if !r.ok || n < 0 || r.p+n > len(r.b) {
		r.ok = false
		return false
	}
	return true
}

func (r *rdr) i32() int32 {
	if !r.need(4) {
		return 0
	}
	v := int32(binary.LittleEndian.Uint32(r.b[r.p:]))
	r.p += 4
	return v
}

func (r *rdr) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.p:])
	r.p += 4
	return v
}

func (r *rdr) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.p:])
	r.p += 8
	return v
}

// magic reads and compares a 4-byte magic without advancing on mismatch beyond
// the 4 bytes.
func (r *rdr) magic(want string) bool {
	if !r.need(4) {
		return false
	}
	got := string(r.b[r.p : r.p+4])
	r.p += 4
	return got == want
}

// skip advances p by n, bounds-checked.
func (r *rdr) skip(n int) {
	if r.need(n) {
		r.p += n
	}
}
