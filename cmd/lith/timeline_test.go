// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
)

func TestMountTimelineCorrelatesDispatchAndJoin(t *testing.T) {
	m := newMountTimeline()
	base := m.t0

	// A prefetch for (k, chunk 0) is dispatched 10ms in; the app opens it 60ms in
	// and joins the in-flight fill for 30ms → lag 50ms, join_wait 30ms.
	m.PrefetchDispatch("obj/a", 0, base.Add(10*time.Millisecond))
	m.ChunkRead(blockstore.ChunkEvent{
		At: base.Add(60 * time.Millisecond), Key: "obj/a", Chunk: 0,
		Kind: "join", Prefetched: true, Wait: 30 * time.Millisecond, Inflight: 12,
	})
	// A hit with no prior dispatch → lag -1.
	m.ChunkRead(blockstore.ChunkEvent{
		At: base.Add(70 * time.Millisecond), Key: "obj/b", Chunk: 3, Kind: "hit",
	})

	if len(m.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(m.rows))
	}
	r0 := m.rows[0]
	if r0.kind != "join" || r0.waitMs != 30 || r0.inflight != 12 {
		t.Fatalf("join row = %+v", r0)
	}
	if r0.lagMs < 49.9 || r0.lagMs > 50.1 {
		t.Fatalf("lag = %.3f ms, want ~50", r0.lagMs)
	}
	if m.rows[1].lagMs != -1 {
		t.Fatalf("uncorrelated hit lag = %.3f, want -1", m.rows[1].lagMs)
	}

	// CSV round-trips with a header + one row per event.
	path := filepath.Join(t.TempDir(), "tl.csv")
	n, err := m.writeCSV(path)
	if err != nil || n != 2 {
		t.Fatalf("writeCSV = %d,%v", n, err)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ms,kind,") {
		t.Fatalf("csv =\n%s", b)
	}
	if s := m.summary(); !strings.Contains(s, "join_wait ms") || !strings.Contains(s, "dispatch->open lag") {
		t.Fatalf("summary missing distributions:\n%s", s)
	}
}

// TestWriteCSVResistsInjection verifies that attacker-controlled S3 keys cannot
// corrupt CSV structure (comma/quote/newline) or inject live spreadsheet
// formulas (= + - @ \t \r) via the key column (finding F4).
func TestWriteCSVResistsInjection(t *testing.T) {
	m := newMountTimeline()
	base := m.t0

	// keys[i] is the key we feed; want[i] is what we expect to read back.
	keys := []string{
		"obj/normal",        // plain — must be unchanged
		"=cmd|'/C calc'!A0", // formula (=)
		"+1+2",              // formula (+)
		"-2+3",              // formula (-)
		"@SUM(1)",           // formula (@)
		"a,b\"c",            // structural: comma + double-quote
		"line1\nline2",      // structural: embedded newline
	}
	want := []string{
		"obj/normal",
		"'=cmd|'/C calc'!A0",
		"'+1+2",
		"'-2+3",
		"'@SUM(1)",
		"a,b\"c",
		"line1\nline2",
	}
	for i, k := range keys {
		m.ChunkRead(blockstore.ChunkEvent{
			At: base.Add(time.Duration(i) * time.Millisecond), Key: k, Chunk: int64(i), Kind: "hit",
		})
	}

	path := filepath.Join(t.TempDir(), "inj.csv")
	n, err := m.writeCSV(path)
	if err != nil || n != len(keys) {
		t.Fatalf("writeCSV = %d,%v", n, err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}

	// Header + one record per event: no forged rows, no column corruption.
	if len(recs) != len(keys)+1 {
		t.Fatalf("records = %d, want %d (structure injection / forged rows)", len(recs), len(keys)+1)
	}
	header := recs[0]
	if len(header) != 8 || header[0] != "ms" || header[7] != "key" {
		t.Fatalf("header = %v", header)
	}
	const keyCol = 7
	for i, w := range want {
		rec := recs[i+1]
		if len(rec) != 8 {
			t.Fatalf("record %d has %d columns, want 8: %v", i, len(rec), rec)
		}
		if got := rec[keyCol]; got != w {
			t.Fatalf("record %d key = %q, want %q", i, got, w)
		}
	}
}

// #284: an open must appear on the timeline, and the gap to the first chunk read for the same
// key must be readable from the CSV.
//
// That interval is the per-open cost an external deployment fitted at 0.47 s — 72% of a 260 MB
// read's wall — and no instrument could show where it went. `--pf-trace` has no timestamp
// column at all; `--timeline-csv` had timestamps but nothing marked the open, so the gap could
// only be inferred from process start.
func TestTimelineRecordsHandleOpen(t *testing.T) {
	m := newMountTimeline()
	base := m.t0

	m.HandleOpen("obj/big", 260<<20, base.Add(5*time.Millisecond))
	m.ChunkRead(blockstore.ChunkEvent{
		At: base.Add(475 * time.Millisecond), Key: "obj/big", Chunk: 0,
		Kind: "uncovered", Wait: 2 * time.Millisecond,
	})

	dir := t.TempDir()
	path := dir + "/t.csv"
	n, err := m.writeCSV(path)
	if err != nil {
		t.Fatalf("writeCSV: %v", err)
	}
	if n != 2 {
		t.Fatalf("wrote %d rows, want 2 (the open and the chunk)", n)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(b)

	// The open is a row, with its own kind, so no CSV schema change was needed and an
	// existing consumer sees one more kind value rather than a different file.
	if !strings.Contains(out, ",open,") {
		t.Errorf("no open row in the CSV:\n%s", out)
	}
	// Ordered before the chunk, and at its own timestamp, so the interval is a subtraction.
	iOpen, iChunk := strings.Index(out, ",open,"), strings.Index(out, ",uncovered,")
	if iOpen < 0 || iChunk < 0 || iOpen > iChunk {
		t.Errorf("open row is not ordered before the chunk row:\n%s", out)
	}
	if !strings.Contains(out, "5.000,open,") {
		t.Errorf("open row is not at its own timestamp:\n%s", out)
	}
	if !strings.Contains(out, "475.000,uncovered,") {
		t.Errorf("chunk row is not at its own timestamp:\n%s", out)
	}
	// The object size rides in the chunk column, which is the one numeric field an open has.
	if !strings.Contains(out, strconv.FormatInt(260<<20, 10)) {
		t.Errorf("open row does not carry the object size:\n%s", out)
	}
}
