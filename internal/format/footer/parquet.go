// SPDX-License-Identifier: Apache-2.0

package footer

import "sort"

// ColumnChunk locates one column's compressed bytes within a row group.
type ColumnChunk struct {
	Path   string // dotted path_in_schema, e.g. "url" or "payload.href"
	Start  int64  // first byte of the chunk
	Length int64  // total_compressed_size
}

// RowGroup is a set of column chunks that share the same rows.
type RowGroup struct{ Columns []ColumnChunk }

// ParquetMeta is the subset of Parquet FileMetaData lith needs to map a
// demand read to a (row group, column) and to prefetch a projection.
type ParquetMeta struct{ RowGroups []RowGroup }

// Compact protocol field types.
const (
	ctBoolTrue  = 1
	ctBoolFalse = 2
	ctByte      = 3
	ctI16       = 4
	ctI32       = 5
	ctI64       = 6
	ctDouble    = 7
	ctBinary    = 8
	ctList      = 9
	ctSet       = 10
	ctMap       = 11
	ctStruct    = 12
)

// tcompact is a fail-safe, bounds-checked reader over Thrift-compact bytes.
// Every read checks the remaining length; on underflow it sets err and all
// subsequent reads are no-ops. It never panics and never allocates sized by
// an unchecked field.
type tcompact struct {
	b   []byte
	pos int
	err bool
}

func (r *tcompact) fail() { r.err = true }

// byteAt reads one byte, advancing pos.
func (r *tcompact) byte() byte {
	if r.err || r.pos >= len(r.b) {
		r.fail()
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

// uvarint reads an unsigned LEB128 varint (max 10 bytes for a 64-bit value).
func (r *tcompact) uvarint() uint64 {
	var x uint64
	var shift uint
	for i := 0; i < 10; i++ {
		if r.err || r.pos >= len(r.b) {
			r.fail()
			return 0
		}
		b := r.b[r.pos]
		r.pos++
		if shift >= 64 {
			r.fail()
			return 0
		}
		x |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return x
		}
		shift += 7
	}
	// More than 10 bytes without a terminator: malformed.
	r.fail()
	return 0
}

// zigzag decodes a zigzag-encoded signed integer.
func zigzag(u uint64) int64 {
	return int64(u>>1) ^ -int64(u&1)
}

func (r *tcompact) svarint() int64 { return zigzag(r.uvarint()) }

// binaryLen reads the varint length of a BINARY field and returns it as a
// bounds-checked int. It does not read the payload; callers decide whether
// to copy or skip.
func (r *tcompact) binaryLen() (int, bool) {
	n := r.uvarint()
	if r.err {
		return 0, false
	}
	// Cap by both the security limit and the actual remaining bytes.
	if n > MaxFooterBytes || int64(n) > int64(len(r.b)-r.pos) {
		r.fail()
		return 0, false
	}
	return int(n), true
}

// binaryBytes returns a sub-slice of the next BINARY value.
func (r *tcompact) binaryBytes() []byte {
	n, ok := r.binaryLen()
	if !ok {
		return nil
	}
	s := r.b[r.pos : r.pos+n]
	r.pos += n
	return s
}

// skipBinary consumes a BINARY value without materializing it.
func (r *tcompact) skipBinary() {
	n, ok := r.binaryLen()
	if !ok {
		return
	}
	r.pos += n
}

// fieldHeader reads a compact struct field header, returning the new field id
// and compact type. typ==0 signals STOP.
func (r *tcompact) fieldHeader(prevID int16) (id int16, typ byte) {
	h := r.byte()
	if r.err {
		return 0, 0
	}
	typ = h & 0x0f
	if typ == 0 {
		return 0, 0 // STOP
	}
	delta := (h & 0xf0) >> 4
	if delta == 0 {
		// Long form: an explicit zigzag i16 field id follows.
		id = int16(r.svarint())
	} else {
		id = prevID + int16(delta)
	}
	return id, typ
}

// listHeader reads a compact list/set header: element type and size.
func (r *tcompact) listHeader() (elemType byte, size int) {
	h := r.byte()
	if r.err {
		return 0, 0
	}
	elemType = h & 0x0f
	sz := (h & 0xf0) >> 4
	if sz == 0x0f {
		n := r.uvarint()
		if r.err {
			return 0, 0
		}
		// A list cannot have more elements than there are remaining bytes.
		if n > uint64(len(r.b)) {
			r.fail()
			return 0, 0
		}
		size = int(n)
	} else {
		size = int(sz)
	}
	return elemType, size
}

// skip consumes a value of the given compact type without interpreting it.
// It recurses for containers, guarding depth to avoid stack exhaustion.
func (r *tcompact) skip(typ byte, depth int) {
	if r.err {
		return
	}
	if depth > 128 {
		r.fail()
		return
	}
	switch typ {
	case ctBoolTrue, ctBoolFalse:
		// Value carried in the type nibble; nothing to consume.
	case ctByte:
		r.byte()
	case ctI16, ctI32, ctI64:
		r.svarint()
	case ctDouble:
		for i := 0; i < 8; i++ {
			r.byte()
		}
	case ctBinary:
		r.skipBinary()
	case ctList, ctSet:
		et, n := r.listHeader()
		for i := 0; i < n && !r.err; i++ {
			r.skip(et, depth+1)
		}
	case ctMap:
		r.skipMap(depth)
	case ctStruct:
		r.skipStruct(depth)
	default:
		r.fail()
	}
}

// skipMap consumes a compact map value.
func (r *tcompact) skipMap(depth int) {
	n := r.uvarint()
	if r.err {
		return
	}
	if n == 0 {
		return // empty map has no key/value type byte
	}
	if n > uint64(len(r.b)) {
		r.fail()
		return
	}
	kv := r.byte()
	if r.err {
		return
	}
	keyType := (kv & 0xf0) >> 4
	valType := kv & 0x0f
	for i := uint64(0); i < n && !r.err; i++ {
		r.skip(keyType, depth+1)
		r.skip(valType, depth+1)
	}
}

// skipStruct consumes a struct to its STOP field.
func (r *tcompact) skipStruct(depth int) {
	if depth > 128 {
		r.fail()
		return
	}
	var id int16
	for {
		fid, typ := r.fieldHeader(id)
		if r.err || typ == 0 {
			return
		}
		id = fid
		r.skip(typ, depth+1)
	}
}

// ParseParquetFooter parses the Thrift-compact FileMetaData (the metadata
// bytes only, without the trailing length+magic). It returns row groups in
// file order, each with its column chunks. Unknown fields are skipped
// gracefully. It returns (nil,false) on any problem or if len>MaxFooterBytes.
func ParseParquetFooter(meta []byte) (*ParquetMeta, bool) {
	if len(meta) == 0 || len(meta) > MaxFooterBytes {
		return nil, false
	}
	r := &tcompact{b: meta}
	m := &ParquetMeta{}

	// FileMetaData struct. We want field 4 = list<RowGroup>.
	var id int16
	for {
		fid, typ := r.fieldHeader(id)
		if r.err {
			return nil, false
		}
		if typ == 0 { // STOP
			break
		}
		id = fid
		if fid == 4 && typ == ctList {
			if !parseRowGroupList(r, m) {
				return nil, false
			}
			continue
		}
		r.skip(typ, 0)
	}
	if r.err {
		return nil, false
	}
	return m, true
}

// parseRowGroupList reads field 4's list<RowGroup> into m.
func parseRowGroupList(r *tcompact, m *ParquetMeta) bool {
	et, n := r.listHeader()
	if r.err || et != ctStruct {
		return false
	}
	for i := 0; i < n; i++ {
		rg, ok := parseRowGroup(r)
		if !ok {
			return false
		}
		m.RowGroups = append(m.RowGroups, rg)
	}
	return !r.err
}

// parseRowGroup reads one RowGroup struct; field 1 = list<ColumnChunk>.
func parseRowGroup(r *tcompact) (RowGroup, bool) {
	var rg RowGroup
	var id int16
	for {
		fid, typ := r.fieldHeader(id)
		if r.err {
			return rg, false
		}
		if typ == 0 {
			return rg, true
		}
		id = fid
		if fid == 1 && typ == ctList {
			if !parseColumnList(r, &rg) {
				return rg, false
			}
			continue
		}
		r.skip(typ, 0)
	}
}

// parseColumnList reads a RowGroup's list<ColumnChunk>.
func parseColumnList(r *tcompact, rg *RowGroup) bool {
	et, n := r.listHeader()
	if r.err || et != ctStruct {
		return false
	}
	for i := 0; i < n; i++ {
		cc, ok := parseColumnChunk(r)
		if !ok {
			return false
		}
		rg.Columns = append(rg.Columns, cc)
	}
	return !r.err
}

// parseColumnChunk reads a ColumnChunk struct; field 3 = ColumnMetaData.
func parseColumnChunk(r *tcompact) (ColumnChunk, bool) {
	var cc ColumnChunk
	var id int16
	for {
		fid, typ := r.fieldHeader(id)
		if r.err {
			return cc, false
		}
		if typ == 0 {
			return cc, true
		}
		id = fid
		if fid == 3 && typ == ctStruct {
			if !parseColumnMeta(r, &cc) {
				return cc, false
			}
			continue
		}
		r.skip(typ, 0)
	}
}

// parseColumnMeta reads ColumnMetaData and fills Path/Start/Length.
// Fields: 3 = list<string> path_in_schema, 7 = i64 total_compressed_size,
// 9 = i64 data_page_offset, 11 = i64 dictionary_page_offset (optional).
func parseColumnMeta(r *tcompact, cc *ColumnChunk) bool {
	var (
		id          int16
		dataOff     int64
		dictOff     int64
		haveDict    bool
		haveDataOff bool
	)
	for {
		fid, typ := r.fieldHeader(id)
		if r.err {
			return false
		}
		if typ == 0 {
			break
		}
		id = fid
		switch {
		case fid == 3 && typ == ctList:
			path, ok := parsePathInSchema(r)
			if !ok {
				return false
			}
			cc.Path = path
		case fid == 7 && typ == ctI64:
			cc.Length = r.svarint()
		case fid == 9 && typ == ctI64:
			dataOff = r.svarint()
			haveDataOff = true
		case fid == 11 && typ == ctI64:
			dictOff = r.svarint()
			haveDict = true
		default:
			r.skip(typ, 0)
		}
		if r.err {
			return false
		}
	}
	if !haveDataOff {
		return false
	}
	// Start = dictionary_page_offset if present and >0 and < data_page_offset,
	// else data_page_offset.
	if haveDict && dictOff > 0 && dictOff < dataOff {
		cc.Start = dictOff
	} else {
		cc.Start = dataOff
	}
	return true
}

// parsePathInSchema reads a list<string> and joins it with '.'.
func parsePathInSchema(r *tcompact) (string, bool) {
	et, n := r.listHeader()
	if r.err || et != ctBinary {
		return "", false
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b := r.binaryBytes()
		if r.err {
			return "", false
		}
		parts = append(parts, string(b))
	}
	// join without importing strings twice; simple manual join.
	return joinDot(parts), true
}

// joinDot joins path parts with '.'.
func joinDot(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	n := len(parts) - 1
	for _, p := range parts {
		n += len(p)
	}
	buf := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, '.')
		}
		buf = append(buf, p...)
	}
	return string(buf)
}

// Locate returns the index of the row group and the column Path whose byte
// range [Start,Start+Length) contains off, and ok.
func (m *ParquetMeta) Locate(off int64) (rowGroup int, path string, ok bool) {
	for rgi := range m.RowGroups {
		for _, c := range m.RowGroups[rgi].Columns {
			if c.Length <= 0 {
				continue
			}
			if off >= c.Start && off < c.Start+c.Length {
				return rgi, c.Path, true
			}
		}
	}
	return 0, "", false
}

// ProjectionChunks returns, sorted by Start, the column chunks for the given
// column paths across row groups in the half-open index range [rgFrom, rgTo)
// (rgTo is clamped to len(RowGroups)).
func (m *ParquetMeta) ProjectionChunks(paths []string, rgFrom, rgTo int) []Range {
	if rgFrom < 0 {
		rgFrom = 0
	}
	if rgTo > len(m.RowGroups) {
		rgTo = len(m.RowGroups)
	}
	if rgFrom >= rgTo {
		return nil
	}
	want := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		want[p] = struct{}{}
	}
	var out []Range
	for rgi := rgFrom; rgi < rgTo; rgi++ {
		for _, c := range m.RowGroups[rgi].Columns {
			if _, ok := want[c.Path]; !ok {
				continue
			}
			out = append(out, Range{Start: c.Start, End: c.Start + c.Length})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}
