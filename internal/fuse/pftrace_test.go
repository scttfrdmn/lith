// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// The prefetch trace is the instrument for answering what predicts prefetch
// follow-through offline, instead of buying a 35-minute cluster run per candidate
// policy (#262). Two properties make it usable, and both are asserted here
// because both were absent and the absences were invisible:
//
//   - rows carry the FILE HANDLE, since lith builds one prefetcher per Open, so
//     the handle is the unit that makes decisions; without it concurrent handles
//     on one key interleave indistinguishably;
//   - reads the prefetcher was NOT driven for are still recorded and MARKED, since
//     they are a large non-random share of the bytes wherever #229 whole-fetches
//     large objects, and dropping them silently yields a biased sample that looks
//     complete.
func newTraceFS(t *testing.T, trace string, objBytes int64, partsMax int64) (*rawFS, *fake.Server) {
	t.Helper()
	srv := fake.New()
	srv.Put("big.bin", bytes.Repeat([]byte("A"), int(objBytes)), time.Unix(1_700_000_000, 0))
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	bs, err := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 1 << 20, MemCache: 512 << 20, MaxRange: 64 << 20})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	lim := prefetch.NewPolicy(256<<20, prefetch.DeviceLimits{NICBytesPerSec: 100 << 20, TTFB: 10 * time.Millisecond}, nil)
	raw := NewRawFileSystem(Config{
		Index: ix, Store: bs, Metrics: metrics.New(), UID: 1000, GID: 1000, Limits: lim,
		PartsMax: partsMax, PFTracePath: trace,
	}).(*rawFS)
	return raw, srv
}

func traceRows(t *testing.T, path string) (header string, cols []string, rows [][]string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			header = line
		case cols == nil:
			cols = strings.Split(line, ",")
		default:
			rows = append(rows, strings.Split(line, ","))
		}
	}
	return header, cols, rows
}

func colIdx(t *testing.T, cols []string, name string) int {
	t.Helper()
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	t.Fatalf("trace has no %q column; columns = %v", name, cols)
	return -1
}

// TestPFTraceGroupsByHandleAndCarriesConfig: two concurrent handles on the SAME key
// must be separable by `fh`, and the trace must state the config that produced it
// so a replay can construct the same prefetcher.
func TestPFTraceGroupsByHandleAndCarriesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pf.csv")
	// parts-max 0 so the whole-file path never fires and every read drives the
	// prefetcher; the omission case gets its own test below.
	raw, _ := newTraceFS(t, path, 8<<20, 0)

	var eo fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "big.bin", &eo); s != fuse.OK {
		t.Fatalf("lookup: %v", s)
	}
	// Two handles on one object, interleaved — the shape that is unreadable without
	// a handle id.
	var a, b fuse.OpenOut
	for _, oo := range []*fuse.OpenOut{&a, &b} {
		if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: eo.NodeId}}, oo); s != fuse.OK {
			t.Fatalf("open: %v", s)
		}
	}
	buf := make([]byte, 128<<10)
	for i := int64(0); i < 6; i++ {
		for _, fh := range []uint64{a.Fh, b.Fh} {
			if _, st := raw.Read(nil, &fuse.ReadIn{
				InHeader: fuse.InHeader{NodeId: eo.NodeId, Pid: 4242},
				Fh:       fh, Offset: uint64(i * (128 << 10)), Size: uint32(len(buf)),
			}, buf); st != fuse.OK {
				t.Fatalf("read: %v", st)
			}
		}
	}

	hdr, cols, rows := traceRows(t, path)
	for _, want := range []string{"block_size=", "max_readahead=", "parts_max=", "coverage_window=", "coverage_min=", "evidence_ratio="} {
		if !strings.Contains(hdr, want) {
			t.Errorf("trace header is missing %q; a replay cannot reconstruct the prefetcher. header=%q", want, hdr)
		}
	}
	if len(rows) == 0 {
		t.Fatal("trace recorded no rows")
	}
	fhCol := colIdx(t, cols, "fh")
	seen := map[string]int{}
	for _, r := range rows {
		seen[r[fhCol]]++
	}
	if len(seen) != 2 {
		t.Errorf("rows carry %d distinct fh values, want 2 (the two handles must be separable): %v", len(seen), seen)
	}
	for fh, n := range seen {
		if n == 0 {
			t.Errorf("handle %s has no rows", fh)
		}
	}
	// pid is present so a multi-rank trace can be split by reader.
	pidCol := colIdx(t, cols, "pid")
	if rows[0][pidCol] != "4242" {
		t.Errorf("pid = %q, want 4242", rows[0][pidCol])
	}
	// window and dispatched are present so a replay's fidelity can be checked
	// before its verdict on a new policy is trusted.
	for _, c := range []string{"window", "dispatched", "path"} {
		colIdx(t, cols, c)
	}
}

// TestPFTraceMarksOmittedReads: once a whole-file parts fetch is in flight the
// prefetcher is deliberately not driven — but the reads must still appear, marked
// `parts`, or the trace is a biased sample that looks complete.
func TestPFTraceMarksOmittedReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pf.csv")
	// A small object under parts-max: once the handle establishes, the whole-file
	// parts fetch dispatches and subsequent reads stop driving the prefetcher.
	raw, _ := newTraceFS(t, path, 4<<20, 8<<20)

	var eo fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "big.bin", &eo); s != fuse.OK {
		t.Fatalf("lookup: %v", s)
	}
	var oo fuse.OpenOut
	if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: eo.NodeId}}, &oo); s != fuse.OK {
		t.Fatalf("open: %v", s)
	}
	buf := make([]byte, 128<<10)
	for i := int64(0); i < 24; i++ {
		off := uint64(i * (128 << 10))
		if off >= 4<<20 {
			break
		}
		if _, st := raw.Read(nil, &fuse.ReadIn{
			InHeader: fuse.InHeader{NodeId: eo.NodeId, Pid: 7}, Fh: oo.Fh,
			Offset: off, Size: uint32(len(buf)),
		}, buf); st != fuse.OK {
			t.Fatalf("read: %v", st)
		}
	}

	_, cols, rows := traceRows(t, path)
	pathCol := colIdx(t, cols, "path")
	kinds := map[string]int{}
	for _, r := range rows {
		kinds[r[pathCol]]++
	}
	if kinds["window"] == 0 {
		t.Errorf("no rows marked `window`; kinds=%v", kinds)
	}
	if kinds["parts"] == 0 {
		t.Errorf("no rows marked `parts`: reads omitted while a whole-file fetch is in flight must still be recorded and marked, "+
			"or the trace silently under-samples exactly the objects #229 whole-fetches. kinds=%v", kinds)
	}
	// Every row's path must be one of the three known values — an unknown marker
	// would make an analysis silently mis-bucket.
	for _, r := range rows {
		switch r[pathCol] {
		case "window", "parts", "footer":
		default:
			t.Errorf("unknown path marker %q", r[pathCol])
		}
	}
	// Sanity: dispatched is 0 on every omitted row, since nothing was dispatched.
	dCol := colIdx(t, cols, "dispatched")
	for _, r := range rows {
		if r[pathCol] == "parts" {
			if n, _ := strconv.Atoi(r[dCol]); n != 0 {
				t.Errorf("a `parts` row reports %d dispatched blocks, want 0", n)
			}
		}
	}
}

// TestPFTraceBadPathIsLoud: a typo'd path must not yield an empty trace and a
// successful-looking run (#262). The mount still comes up — this is a diagnostic —
// but tracing is off and the failure is logged rather than swallowed.
func TestPFTraceBadPathIsLoud(t *testing.T) {
	raw, _ := newTraceFS(t, filepath.Join(t.TempDir(), "no-such-dir", "pf.csv"), 1<<20, 0)
	if raw.pfTrace != nil {
		t.Error("trace file was somehow created under a nonexistent directory")
	}
	// And reads still work with tracing disabled.
	var eo fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "big.bin", &eo); s != fuse.OK {
		t.Fatalf("lookup: %v", s)
	}
}
