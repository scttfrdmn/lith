// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// --- minimal Thrift-compact writers (mirror the footer package test) so we can
// build a full valid Parquet object in-tree. ---

func tcUvarint(dst []byte, x uint64) []byte {
	for x >= 0x80 {
		dst = append(dst, byte(x)|0x80)
		x >>= 7
	}
	return append(dst, byte(x))
}
func tcSvarint(dst []byte, v int64) []byte { return tcUvarint(dst, uint64((v<<1)^(v>>63))) }
func tcField(delta, typ byte) byte         { return (delta << 4) | typ }
func tcList(size int, elem byte) byte      { return (byte(size) << 4) | elem }

const (
	tI32    = 5
	tI64    = 6
	tBinary = 8
	tList   = 9
	tStruct = 12
)

func tcColMeta(name string, start, length int64) []byte {
	var b []byte
	b = append(b, tcField(1, tI32)) // type (skipped)
	b = tcSvarint(b, 0)
	b = append(b, tcField(2, tList)) // field 3: path_in_schema
	b = append(b, tcList(1, tBinary))
	b = tcUvarint(b, uint64(len(name)))
	b = append(b, name...)
	b = append(b, tcField(4, tI64)) // field 7: total_compressed_size
	b = tcSvarint(b, length)
	b = append(b, tcField(2, tI64)) // field 9: data_page_offset = start (no dict)
	b = tcSvarint(b, start)
	return append(b, 0) // STOP
}

func tcColChunk(meta []byte) []byte {
	b := append([]byte{tcField(3, tStruct)}, meta...) // field 3: meta_data
	return append(b, 0)
}

func tcRowGroup(cols [][]byte) []byte {
	b := []byte{tcField(1, tList)}
	b = append(b, tcList(len(cols), tStruct))
	for _, c := range cols {
		b = append(b, c...)
	}
	return append(b, 0)
}

func tcFileMeta(rgs [][]byte) []byte {
	b := []byte{tcField(1, tI32)}
	b = tcSvarint(b, 1)
	b = append(b, tcField(3, tList)) // field 4: row_groups
	b = append(b, tcList(len(rgs), tStruct))
	for _, rg := range rgs {
		b = append(b, rg...)
	}
	return append(b, 0)
}

// parquetColStart is the file offset of row group rg, column c in the test
// fixture: one row group per MiB, columns 100 KiB apart.
func parquetColStart(rg, c int) int64 { return 100 + int64(rg)*(1<<20) + int64(c)*(100<<10) }

const parquetColLen = 50 << 10

// buildParquetObject builds a valid Parquet object with nRG row groups × nCol
// columns (named c0..). PAR1 at head and tail; footer metadata + 4-byte length
// + PAR1 at the end. Column chunk offsets follow parquetColStart.
func buildParquetObject(nRG, nCol int) []byte {
	var rgs [][]byte
	for rg := 0; rg < nRG; rg++ {
		var cols [][]byte
		for c := 0; c < nCol; c++ {
			cols = append(cols, tcColChunk(tcColMeta(colName(c), parquetColStart(rg, c), parquetColLen)))
		}
		rgs = append(rgs, tcRowGroup(cols))
	}
	meta := tcFileMeta(rgs)
	// data region large enough to hold every column chunk.
	dataSize := parquetColStart(nRG-1, nCol-1) + parquetColLen + 4096
	obj := make([]byte, dataSize)
	copy(obj[0:4], "PAR1")
	obj = append(obj, meta...)
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(len(meta)))
	obj = append(obj, l[:]...)
	obj = append(obj, "PAR1"...)
	return obj
}

func colName(c int) string { return "c" + string(rune('0'+c)) }

func scrapeMetric(m *metrics.Metrics, substr string) string {
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var out []string
	for _, ln := range strings.Split(rec.Body.String(), "\n") {
		if strings.Contains(ln, substr) {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// TestFooterParquetProjectionNoSpeculation: reading a column parses the footer,
// learns the projection, and prefetches the current row group's projection
// columns — but NEVER a row group the app has not touched (predicate-safe).
func TestFooterParquetProjectionNoSpeculation(t *testing.T) {
	srv := fake.New()
	obj := buildParquetObject(4, 2)
	srv.Put("t.parquet", obj, time.Unix(1_700_000_000, 0))
	met := metrics.New()
	raw := mkFS(t, srv, Config{SmallFile: 4 << 10, PartsMax: 4 << 10, Metrics: met})

	h, fh := openHandle(t, raw, "t.parquet")
	if h.footerKind.String() != "parquet" {
		t.Fatalf("footerKind = %v, want parquet", h.footerKind)
	}
	readAt := func(off int64) {
		buf := make([]byte, 4096)
		raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: uint64(off), Size: 4096}, buf)
	}
	// Read row group 0's two columns.
	readAt(parquetColStart(0, 0))
	readAt(parquetColStart(0, 1))
	waitStableGets(srv)
	if h.footerMeta == nil || len(h.footerMeta.RowGroups) != 4 {
		t.Fatalf("footer not parsed into 4 row groups: %+v", h.footerMeta)
	}
	// The plan must not have dispatched anything at/after row group 1's offset.
	rg1 := parquetColStart(1, 0)
	for start := range h.footerProj.dispatched {
		if start >= rg1 {
			t.Fatalf("plan speculated into an untouched row group: dispatched start %d >= rg1 %d", start, rg1)
		}
	}
	if got := scrapeMetric(met, `lith_format_plan_ranges_total{format="parquet"}`); got == "" {
		t.Fatal("no parquet plan ranges recorded")
	}

	// Now touch row group 1; its projection is planned, still nothing for RG2/RG3.
	readAt(parquetColStart(1, 0))
	waitStableGets(srv)
	rg2 := parquetColStart(2, 0)
	for start := range h.footerProj.dispatched {
		if start >= rg2 {
			t.Fatalf("plan reached row group 2+ (start %d) after touching only RG0,RG1", start)
		}
	}
}

// TestFooterParquetConcurrentReads: many concurrent reads on one handle (as FUSE
// serves them) must not race on the per-handle projection state.
func TestFooterParquetConcurrentReads(t *testing.T) {
	srv := fake.New()
	srv.Put("c.parquet", buildParquetObject(4, 2), time.Unix(1_700_000_000, 0))
	raw := mkFS(t, srv, Config{SmallFile: 4 << 10, PartsMax: 4 << 10, Metrics: metrics.New()})
	_, fh := openHandle(t, raw, "c.parquet")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := parquetColStart(i%4, i%2)
			buf := make([]byte, 4096)
			raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: uint64(off), Size: 4096}, buf)
		}(i)
	}
	wg.Wait()
}

// TestFooterParquetMalformedTier1: a .parquet whose footer does not parse falls
// back to tier 1 (no plan, no panic).
func TestFooterParquetMalformedTier1(t *testing.T) {
	srv := fake.New()
	obj := make([]byte, 5<<20)
	copy(obj[0:4], "PAR1")
	copy(obj[len(obj)-4:], "PAR1") // tail magic, but the 4-byte length points at garbage
	srv.Put("bad.parquet", obj, time.Unix(1_700_000_000, 0))
	met := metrics.New()
	raw := mkFS(t, srv, Config{SmallFile: 4 << 10, PartsMax: 4 << 10, Metrics: met})

	h, fh := openHandle(t, raw, "bad.parquet")
	buf := make([]byte, 4096)
	raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: 1 << 20, Size: 4096}, buf)
	waitStableGets(srv)
	if !h.footerParsed || h.footerMeta != nil {
		t.Fatalf("malformed parquet should be parsed-and-nil: parsed=%v meta=%v", h.footerParsed, h.footerMeta)
	}
	if got := scrapeMetric(met, `lith_format_plan_ranges_total{format="parquet"}`); got != "" {
		t.Fatalf("malformed parquet planned ranges: %s", got)
	}
}

// TestFooterZipDirectoryReadahead: reading an entry parses the central directory
// and prefetches that entry plus the next few (directory-order readahead), so a
// follow-on read of the next entry is a cache hit.
func TestFooterZipDirectoryReadahead(t *testing.T) {
	srv := fake.New()
	obj, offsets := buildZipObject(t, 4, 2<<20)
	srv.Put("a.zip", obj, time.Unix(1_700_000_000, 0))
	met := metrics.New()
	raw := mkFS(t, srv, Config{SmallFile: 4 << 10, PartsMax: 4 << 10, Metrics: met})

	h, fh := openHandle(t, raw, "a.zip")
	if h.footerKind.String() != "zip" {
		t.Fatalf("footerKind = %v, want zip", h.footerKind)
	}
	readAt := func(off int64) {
		buf := make([]byte, 4096)
		raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: uint64(off), Size: 4096}, buf)
	}
	readAt(offsets[0] + 40) // into entry 0's data → parse + readahead
	waitStableGets(srv)
	if len(h.footerZip) != 4 {
		t.Fatalf("zip central dir not parsed into 4 entries: %d", len(h.footerZip))
	}
	before := srv.GetCallCount()
	readAt(offsets[1] + 40) // entry 1 was prefetched by directory-order readahead
	waitStableGets(srv)
	if after := srv.GetCallCount(); after != before {
		t.Fatalf("next zip entry not prefetched: %d new GET(s)", after-before)
	}
	if got := scrapeMetric(met, `lith_format_plan_ranges_total{format="zip"}`); got == "" {
		t.Fatal("no zip plan ranges recorded")
	}
}

// buildZipObject builds an uncompressed (stored) zip with n entries of the given
// size and returns the bytes and each entry's local-header offset.
func buildZipObject(t *testing.T, n int, entrySize int) ([]byte, []int64) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	payload := make([]byte, entrySize)
	for i := 0; i < n; i++ {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "e" + string(rune('0'+i)) + ".bin", Method: zip.Store})
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	data := buf.Bytes()
	// Recover local-header offsets from the archive's own reader.
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip reopen: %v", err)
	}
	offs := make([]int64, len(zr.File))
	for i, fh := range zr.File {
		o, _ := fh.DataOffset() // data offset; local header is a bit before, close enough for a chunk hit
		offs[i] = o
	}
	return data, offs
}
