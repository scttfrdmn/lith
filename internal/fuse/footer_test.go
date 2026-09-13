// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
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

// TestFooterParquetFullProjectionPlan (#124/session 30, #125): the plan fires ONCE
// the projection is confirmed — the recurring column set, observed across two full
// row groups, seen again as the app enters a third — and then plans the projection
// for all remaining row groups (RG3..n) in one batch. Nothing is planned after only
// one or two row groups, and the touched row groups are not planned (demand-served).
func TestFooterParquetFullProjectionPlan(t *testing.T) {
	srv := fake.New()
	obj := buildParquetObject(5, 2) // 5 row groups, 2 columns
	srv.Put("t.parquet", obj, time.Unix(1_700_000_000, 0))
	met := metrics.New()
	raw := mkFS(t, srv, Config{SmallFile: 4 << 10, PartsMax: 4 << 10, Metrics: met})

	h, fh := openHandle(t, raw, "t.parquet")
	readAt := func(off int64) {
		buf := make([]byte, 4096)
		raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: uint64(off), Size: 4096}, buf)
	}
	planRanges := func() int {
		s := scrapeMetric(met, `lith_format_plan_ranges_total{format="parquet"}`)
		if s == "" {
			return 0
		}
		n := 0
		_, _ = fmt.Sscanf(s[strings.LastIndex(s, " ")+1:], "%d", &n)
		return n
	}
	// Read row groups 0 and 1 in full — only two row groups observed: no plan yet
	// (the recurring set is not confirmed until the app moves past the second).
	readAt(parquetColStart(0, 0))
	readAt(parquetColStart(0, 1))
	readAt(parquetColStart(1, 0))
	readAt(parquetColStart(1, 1))
	waitStableGets(srv)
	if h.footerProj == nil || h.footerProj.planned {
		t.Fatal("plan fired after only two row groups")
	}
	if got := planRanges(); got != 0 {
		t.Fatalf("plan dispatched %d ranges after two row groups, want 0", got)
	}
	// Read into row group 2 — a third distinct row group → projection confirmed →
	// plan fires once for RG3,RG4 (2 cols × 2 remaining row groups = 4 ranges).
	readAt(parquetColStart(2, 0))
	waitStableGets(srv)
	if !h.footerProj.planned {
		t.Fatal("plan did not fire after the projection was confirmed in three row groups")
	}
	if got := planRanges(); got != 4 {
		t.Fatalf("plan dispatched %d ranges, want 4 (both cols for RG3,RG4 — nothing in touched RG0,RG1,RG2)", got)
	}
}

// TestFooterParquetExcludesSingleRGColumn (#125): a column located in only one
// row group — as happens when pyarrow's coalesced pre_buffer read's offset lands
// in a non-projected column — is NOT added to the projection plan. Only columns
// that recur across ≥2 row groups (the true projection) are planned. This is the
// fix for the plan over-enumeration that fetched ~2.3× the projection.
func TestFooterParquetExcludesSingleRGColumn(t *testing.T) {
	srv := fake.New()
	obj := buildParquetObject(5, 2) // 5 row groups, 2 columns
	srv.Put("t.parquet", obj, time.Unix(1_700_000_000, 0))
	met := metrics.New()
	raw := mkFS(t, srv, Config{SmallFile: 4 << 10, PartsMax: 4 << 10, Metrics: met})
	h, fh := openHandle(t, raw, "t.parquet")
	readAt := func(off int64) {
		buf := make([]byte, 4096)
		raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: uint64(off), Size: 4096}, buf)
	}
	planRanges := func() int {
		s := scrapeMetric(met, `lith_format_plan_ranges_total{format="parquet"}`)
		if s == "" {
			return 0
		}
		n := 0
		_, _ = fmt.Sscanf(s[strings.LastIndex(s, " ")+1:], "%d", &n)
		return n
	}
	// col0 recurs (RG0, RG1, RG2 — the real projection); col1 is touched only once
	// (RG0), the coalesced-read noise. After two full row groups plus the move into a
	// third, the recurrence filter confirms col0 and excludes col1.
	readAt(parquetColStart(0, 0)) // col0 in RG0
	readAt(parquetColStart(0, 1)) // col1 in RG0 (spurious, single row group)
	readAt(parquetColStart(1, 0)) // col0 in RG1
	readAt(parquetColStart(2, 0)) // col0 in RG2 → third distinct row group → plan fires
	waitStableGets(srv)
	if !h.footerProj.planned {
		t.Fatal("plan did not fire after three row groups")
	}
	// col0 only, for RG3+RG4 = 2 ranges. If col1 (single-RG noise) leaked in, it
	// would be 4.
	if got := planRanges(); got != 2 {
		t.Fatalf("plan dispatched %d ranges, want 2 (col0 for RG3,RG4; the single-RG col1 excluded)", got)
	}
}

// TestProjectedColsRecurrence unit-tests the recurrence filter directly.
func TestProjectedColsRecurrence(t *testing.T) {
	p := newFooterProjection()
	p.colRGs = map[string]map[int]bool{
		"keep":   {0: true, 1: true, 5: true}, // 3 row groups
		"keep2":  {0: true, 3: true},          // 2 row groups
		"noise":  {0: true},                   // 1 row group → excluded at minRGs=2
		"noise2": {7: true},                   // 1 row group → excluded
	}
	got := map[string]bool{}
	for _, c := range p.projectedCols(2) {
		got[c] = true
	}
	if !got["keep"] || !got["keep2"] {
		t.Errorf("recurring columns dropped: %v", got)
	}
	if got["noise"] || got["noise2"] {
		t.Errorf("single-row-group columns included: %v", got)
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

// TestFooterTier2OffStreams: with tier 2 disabled (the v0.3.0 default), a footer
// handle must stream like a plain handle — footerStream set at open, no projection
// plan fires even after the projection would be confirmed, and reads are whole-chunk
// (not byte-precise). Guards the readahead-suppression coupling: tier-2 detection must
// not disable streaming when the byte-precise path is off (else a sequential scan
// demand-fills 64 KiB extents, ~100× slower than base — see #108, session 32).
func TestFooterTier2OffStreams(t *testing.T) {
	srv := fake.New()
	srv.Put("s.parquet", buildParquetObject(4, 2), time.Unix(1_700_000_000, 0))
	met := metrics.New()
	raw := mkFS(t, srv, Config{SmallFile: 4 << 10, PartsMax: 4 << 10, Metrics: met, DisableFooterTier2: true})

	h, fh := openHandle(t, raw, "s.parquet")
	if !h.footerStream {
		t.Fatal("tier 2 off: footer handle should stream (footerStream=true) so readahead stays on")
	}
	readAt := func(off int64) {
		buf := make([]byte, 4096)
		raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: uint64(off), Size: 4096}, buf)
	}
	// Read into two distinct row groups — enough to confirm a projection if tier 2
	// were on. With it off, no plan must fire.
	readAt(parquetColStart(0, 0))
	readAt(parquetColStart(1, 0))
	waitStableGets(srv)
	if got := scrapeMetric(met, `lith_format_plan_ranges_total{format="parquet"}`); got != "" {
		t.Fatalf("tier 2 off but a projection plan dispatched ranges: %s", got)
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
	// The first read's directory-order readahead prefetches the following entries
	// asynchronously (footerPrefetch → go FillBatch). Assert the *outcome* — the
	// next entry's data becomes cached — rather than re-reading it and counting
	// GETs: re-reading through the FUSE path re-fires the readahead, which under a
	// loaded runner can add a straggler GET for a not-yet-covered tail and flake
	// the count. Coverage of the next entry is the readahead signal; if readahead
	// were broken it never becomes covered and this times out.
	prefetched := false
	for i := 0; i < 1000; i++ {
		if raw.store.Covered(h.key, offsets[1]+40, 4096, h.size) {
			prefetched = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !prefetched {
		t.Fatal("directory-order readahead did not prefetch the next zip entry")
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
