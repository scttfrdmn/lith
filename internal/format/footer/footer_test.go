// SPDX-License-Identifier: Apache-2.0

package footer

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

// --- Thrift-compact encoder (test-only) used to hand-craft a Parquet footer.

func putUvarint(dst []byte, x uint64) []byte {
	for x >= 0x80 {
		dst = append(dst, byte(x)|0x80)
		x >>= 7
	}
	return append(dst, byte(x))
}

func zigzagEnc(v int64) uint64 { return uint64((v << 1) ^ (v >> 63)) }

func putSvarint(dst []byte, v int64) []byte { return putUvarint(dst, zigzagEnc(v)) }

func fieldHdr(delta, typ byte) byte { return (delta << 4) | typ }

func listHdr(size int, elemType byte) byte { return (byte(size) << 4) | elemType }

// buildColumnMeta emits a ColumnMetaData struct.
func buildColumnMeta(name string, dataOff, dictOff, length int64, hasDict bool) []byte {
	var b []byte
	// field 1: type (i32) — unknown/skipped by the parser.
	b = append(b, fieldHdr(1, ctI32))
	b = putSvarint(b, 0)
	// field 3: path_in_schema list<string> (single element = name).
	b = append(b, fieldHdr(2, ctList))
	b = append(b, listHdr(1, ctBinary))
	b = putUvarint(b, uint64(len(name)))
	b = append(b, name...)
	// field 7: total_compressed_size (i64).
	b = append(b, fieldHdr(4, ctI64))
	b = putSvarint(b, length)
	// field 9: data_page_offset (i64).
	b = append(b, fieldHdr(2, ctI64))
	b = putSvarint(b, dataOff)
	if hasDict {
		// field 11: dictionary_page_offset (i64).
		b = append(b, fieldHdr(2, ctI64))
		b = putSvarint(b, dictOff)
	}
	b = append(b, 0) // STOP
	return b
}

func buildColumnChunk(meta []byte) []byte {
	var b []byte
	b = append(b, fieldHdr(3, ctStruct)) // field 3: meta_data
	b = append(b, meta...)
	b = append(b, 0) // STOP
	return b
}

func buildRowGroup(columns [][]byte) []byte {
	var b []byte
	b = append(b, fieldHdr(1, ctList)) // field 1: columns list<struct>
	b = append(b, listHdr(len(columns), ctStruct))
	for _, c := range columns {
		b = append(b, c...)
	}
	b = append(b, 0) // STOP
	return b
}

func buildFileMeta(rowGroups [][]byte) []byte {
	var b []byte
	b = append(b, fieldHdr(1, ctI32)) // field 1: version (skipped)
	b = putSvarint(b, 1)
	b = append(b, fieldHdr(3, ctList)) // field 4: row_groups list<struct>
	b = append(b, listHdr(len(rowGroups), ctStruct))
	for _, rg := range rowGroups {
		b = append(b, rg...)
	}
	b = append(b, 0) // STOP
	return b
}

// base offset and length helpers for the 4x6 fixture.
func base(rg, c int) int64 { return int64(rg)*1_000_000 + int64(c)*10_000 + 100 }
func clen(c int) int64     { return 200 + int64(c) }
func colName(c int) string { return fmt.Sprintf("c%d", c) }

// build4x6Footer builds a FileMetaData with 4 row groups x 6 columns.
// Even columns carry a dictionary page (Start = dictOff); odd columns do not
// (Start = dataOff). In both cases the effective Start equals base(rg,c).
func build4x6Footer() []byte {
	var rgs [][]byte
	for rg := 0; rg < 4; rg++ {
		var cols [][]byte
		for c := 0; c < 6; c++ {
			b := base(rg, c)
			var meta []byte
			if c%2 == 0 {
				// dict present, dictOff < dataOff => Start = dictOff = b.
				meta = buildColumnMeta(colName(c), b+50, b, clen(c), true)
			} else {
				meta = buildColumnMeta(colName(c), b, 0, clen(c), false)
			}
			cols = append(cols, buildColumnChunk(meta))
		}
		rgs = append(rgs, buildRowGroup(cols))
	}
	return buildFileMeta(rgs)
}

func TestDetect(t *testing.T) {
	cases := []struct {
		key  string
		want Format
	}{
		{"data.parquet", FormatParquet},
		{"DATA.PARQUET", FormatParquet},
		{"a/b/c.orc", FormatORC},
		{"x.arrow", FormatArrow},
		{"x.feather", FormatArrow},
		{"bundle.zip", FormatZip},
		{"noext", FormatNone},
		{"dir.zip/inner.txt", FormatNone},
		{"", FormatNone},
	}
	for _, tc := range cases {
		if got := Detect(tc.key); got != tc.want {
			t.Errorf("Detect(%q)=%v want %v", tc.key, got, tc.want)
		}
	}
}

func TestFormatString(t *testing.T) {
	cases := map[Format]string{
		FormatNone: "none", FormatParquet: "parquet", FormatORC: "orc",
		FormatArrow: "arrow", FormatZip: "zip", Format(99): "none",
	}
	for f, want := range cases {
		if got := f.String(); got != want {
			t.Errorf("Format(%d).String()=%q want %q", f, got, want)
		}
	}
}

func TestConfirmMagic(t *testing.T) {
	// Parquet.
	if !ConfirmMagic(FormatParquet, []byte("PAR1xxxx"), []byte("yyyyPAR1"), 100) {
		t.Error("parquet positive failed")
	}
	if ConfirmMagic(FormatParquet, []byte("XXXXxxxx"), []byte("yyyyPAR1"), 100) {
		t.Error("parquet bad head accepted")
	}
	if ConfirmMagic(FormatParquet, []byte("PAR1xxxx"), []byte("yyyyXXXX"), 100) {
		t.Error("parquet bad tail accepted")
	}

	// ORC: [ORC magic][postscript(5)][len=5] at tail; head starts with "ORC".
	orcTail := append([]byte("ORC"), 1, 2, 3, 4, 5)
	orcTail = append(orcTail, 5)
	if !ConfirmMagic(FormatORC, []byte("ORCabcde"), orcTail, 100) {
		t.Error("orc positive failed")
	}
	badORC := append([]byte("ORC"), 1, 2, 3, 4, 5, 9) // wrong length byte
	if ConfirmMagic(FormatORC, []byte("ORCabcde"), badORC, 100) {
		t.Error("orc bad postscript length accepted")
	}
	if ConfirmMagic(FormatORC, []byte("XXXabcde"), orcTail, 100) {
		t.Error("orc bad head accepted")
	}

	// Arrow.
	if !ConfirmMagic(FormatArrow, []byte("ARROW1ab"), []byte("zzARROW1"), 100) {
		t.Error("arrow positive failed")
	}
	if ConfirmMagic(FormatArrow, []byte("ARROW1ab"), []byte("zzARROWX"), 100) {
		t.Error("arrow bad tail accepted")
	}

	// Zip: EOCD signature somewhere in tail.
	zt := make([]byte, 40)
	binary.LittleEndian.PutUint32(zt[10:], eocdSignature)
	if !ConfirmMagic(FormatZip, nil, zt, 100) {
		t.Error("zip positive failed")
	}
	if ConfirmMagic(FormatZip, nil, make([]byte, 40), 100) {
		t.Error("zip without signature accepted")
	}

	if ConfirmMagic(FormatNone, []byte("PAR1xxxx"), []byte("yyyyPAR1"), 100) {
		t.Error("FormatNone accepted")
	}
	if ConfirmMagic(FormatParquet, []byte("PAR1xxxx"), []byte("yyyyPAR1"), -1) {
		t.Error("negative size accepted")
	}
}

func TestParquetFooterStart(t *testing.T) {
	meta := build4x6Footer()
	var tail []byte
	tail = append(tail, meta...)
	lb := make([]byte, 4)
	binary.LittleEndian.PutUint32(lb, uint32(len(meta)))
	tail = append(tail, lb...)
	tail = append(tail, "PAR1"...)
	size := int64(len(tail))

	start, whole, ok := ParquetFooterStart(tail, size)
	if !ok {
		t.Fatal("ParquetFooterStart ok=false")
	}
	if want := size - 8 - int64(len(meta)); start != want {
		t.Errorf("footerStart=%d want %d", start, want)
	}
	if !whole {
		t.Error("wholeInTail=false, expected true")
	}

	// Implausible length field.
	bad := append([]byte(nil), tail...)
	binary.LittleEndian.PutUint32(bad[len(bad)-8:len(bad)-4], 0xffffffff)
	if _, _, ok := ParquetFooterStart(bad, size); ok {
		t.Error("oversize footer length accepted")
	}
	// footerLen+8 > size.
	if _, _, ok := ParquetFooterStart(tail, 4); ok {
		t.Error("footer larger than file accepted")
	}
	// Too short.
	if _, _, ok := ParquetFooterStart([]byte("PAR1"), 4); ok {
		t.Error("short tail accepted")
	}
	// Missing trailing magic.
	nomagic := append([]byte(nil), tail...)
	copy(nomagic[len(nomagic)-4:], "XXXX")
	if _, _, ok := ParquetFooterStart(nomagic, size); ok {
		t.Error("missing magic accepted")
	}
}

func TestParseParquetFooter(t *testing.T) {
	meta := build4x6Footer()
	m, ok := ParseParquetFooter(meta)
	if !ok {
		t.Fatal("ParseParquetFooter ok=false")
	}
	if len(m.RowGroups) != 4 {
		t.Fatalf("row groups=%d want 4", len(m.RowGroups))
	}
	for rg := 0; rg < 4; rg++ {
		cols := m.RowGroups[rg].Columns
		if len(cols) != 6 {
			t.Fatalf("rg%d cols=%d want 6", rg, len(cols))
		}
		for c := 0; c < 6; c++ {
			cc := cols[c]
			if cc.Path != colName(c) {
				t.Errorf("rg%d col%d path=%q want %q", rg, c, cc.Path, colName(c))
			}
			if cc.Start != base(rg, c) {
				t.Errorf("rg%d col%d start=%d want %d", rg, c, cc.Start, base(rg, c))
			}
			if cc.Length != clen(c) {
				t.Errorf("rg%d col%d length=%d want %d", rg, c, cc.Length, clen(c))
			}
		}
	}
}

func TestLocate(t *testing.T) {
	m, ok := ParseParquetFooter(build4x6Footer())
	if !ok {
		t.Fatal("parse failed")
	}
	// Inside RG1.c2.
	off := base(1, 2) + 10
	rg, path, ok := m.Locate(off)
	if !ok || rg != 1 || path != "c2" {
		t.Errorf("Locate(%d)=(%d,%q,%v) want (1,c2,true)", off, rg, path, ok)
	}
	// A gap (before any chunk in RG0; chunks start at offset >=100).
	if _, _, ok := m.Locate(50); ok {
		t.Error("Locate in gap returned ok=true")
	}
}

func TestProjectionChunks(t *testing.T) {
	m, ok := ParseParquetFooter(build4x6Footer())
	if !ok {
		t.Fatal("parse failed")
	}
	got := m.ProjectionChunks([]string{"c0", "c2"}, 1, 4)
	var want []Range
	for rg := 1; rg < 4; rg++ {
		for _, c := range []int{0, 2} {
			want = append(want, Range{Start: base(rg, c), End: base(rg, c) + clen(c)})
		}
	}
	// want must be sorted by Start; build in rg-then-col order which is
	// already ascending here.
	if len(got) != len(want) {
		t.Fatalf("got %d ranges want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("range[%d]=%+v want %+v", i, got[i], want[i])
		}
	}
	// Clamp rgTo beyond len.
	if r := m.ProjectionChunks([]string{"c0"}, 0, 100); len(r) != 4 {
		t.Errorf("clamped projection returned %d want 4", len(r))
	}
	// Empty range.
	if r := m.ProjectionChunks([]string{"c0"}, 3, 1); r != nil {
		t.Errorf("inverted range returned %v want nil", r)
	}
}

// buildZip builds a 20-entry zip using archive/zip; no *testing.T so it can
// also seed the fuzz corpus.
func buildZip() ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("dir/file_%02d.txt", i)
		fw, err := w.Create(name)
		if err != nil {
			return nil, err
		}
		payload := bytes.Repeat([]byte{byte('a' + i)}, i+1)
		if _, err := fw.Write(payload); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildZipFixture returns the 20-entry zip bytes and a stdlib reader for
// cross-checking (tail = whole file).
func buildZipFixture(t *testing.T) ([]byte, *zip.Reader) {
	t.Helper()
	data, err := buildZip()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return data, zr
}

func TestParseZipCentralDir(t *testing.T) {
	data, zr := buildZipFixture(t)
	entries, ok := ParseZipCentralDir(data, int64(len(data)))
	if !ok {
		t.Fatal("ParseZipCentralDir ok=false")
	}
	if len(entries) != len(zr.File) {
		t.Fatalf("entries=%d want %d", len(entries), len(zr.File))
	}
	for i, e := range entries {
		f := zr.File[i]
		if e.Name != f.Name {
			t.Errorf("entry %d name=%q want %q", i, e.Name, f.Name)
		}
		if e.CompressedSize != int64(f.CompressedSize64) {
			t.Errorf("entry %d csize=%d want %d", i, e.CompressedSize, f.CompressedSize64)
		}
		off, err := f.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		// LocalHeaderOffset should be <= data offset for that member.
		if e.LocalHeaderOffset < 0 || e.LocalHeaderOffset > off {
			t.Errorf("entry %d localOff=%d not before data off %d", i, e.LocalHeaderOffset, off)
		}
	}
}

func TestParseZipCentralDirMalformed(t *testing.T) {
	data, _ := buildZipFixture(t)
	// Central dir before tail: pretend the file is much bigger than the tail.
	if _, ok := ParseZipCentralDir(data, int64(len(data))+1_000_000); ok {
		t.Error("central dir before tail accepted")
	}
	// Truncated (drop EOCD).
	if _, ok := ParseZipCentralDir(data[:len(data)-10], int64(len(data)-10)); ok {
		t.Error("truncated zip accepted")
	}
	// Empty.
	if _, ok := ParseZipCentralDir(nil, 0); ok {
		t.Error("empty accepted")
	}
	// Oversize tail.
	if _, ok := ParseZipCentralDir(make([]byte, MaxFooterBytes+1), MaxFooterBytes+1); ok {
		t.Error("oversize tail accepted")
	}
}

func TestParseParquetFooterMalformed(t *testing.T) {
	meta := build4x6Footer()
	cases := map[string][]byte{
		"empty":     nil,
		"truncated": meta[:len(meta)/2],
		"oversize":  make([]byte, MaxFooterBytes+1),
		"garbage":   {0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	}
	for name, in := range cases {
		if _, ok := ParseParquetFooter(in); ok && name != "garbage" {
			// garbage may or may not parse to an empty meta; only assert no panic.
			t.Errorf("%s: expected ok=false", name)
		}
	}
	// Every 1-byte truncation must not panic.
	for i := 0; i < len(meta); i++ {
		_, _ = ParseParquetFooter(meta[:i])
	}
}

// --- Fuzz targets.

func FuzzParseParquetFooter(f *testing.F) {
	f.Add(build4x6Footer())
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		m, ok := ParseParquetFooter(data)
		if ok {
			// Exercise the accessors; must not panic on parsed output.
			_, _, _ = m.Locate(0)
			_ = m.ProjectionChunks([]string{"c0"}, 0, len(m.RowGroups))
		}
	})
}

func FuzzParseZipCentralDir(f *testing.F) {
	if data, err := buildZip(); err == nil {
		f.Add(data, int64(len(data)))
	}
	f.Add([]byte{}, int64(0))
	f.Add([]byte("PK\x05\x06"), int64(4))
	f.Fuzz(func(t *testing.T, data []byte, size int64) {
		_, _ = ParseZipCentralDir(data, size)
	})
}
