// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const kib = 128 << 10

// writeTrace builds a trace file in the #262 format. The header matters: without it
// the tool must refuse, because a replay that guesses the block size is replaying a
// different program than the one that ran.
func writeTrace(t *testing.T, rows string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pf.csv")
	body := "# lith prefetch trace; block_size=8388608 max_readahead=64 parts_max=67108864 small_file=4194304 coverage_window=16 coverage_min=0.5 evidence_ratio=0\n" +
		"fh,pid,key,size,off,len,blk,gap,path,state_before,state_after,window,dispatched,peak_window\n" + rows
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// contiguous emits a handle streaming nbytes from 0 in 128 KiB reads. `dispatched`
// is left at 0, which is deliberately WRONG for a streaming handle — the fidelity
// check must notice, which is what proves the check works.
func contiguous(fh int, nbytes, objSize int64) string {
	var b strings.Builder
	var prevEnd int64
	for off := int64(0); off < nbytes; off += kib {
		gap := off - prevEnd
		fmt.Fprintf(&b, "%d,7,obj,%d,%d,%d,%d,%d,window,cold,cold,0,0,0\n", fh, objSize, off, kib, off/8388608, gap)
		prevEnd = off + kib
	}
	return b.String()
}

func TestReplayRejectsTraceWithoutConfigHeader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.csv")
	if err := os.WriteFile(p, []byte("key,off,len,blk,gap,state_before,state_after,peak_window\nobj,0,1024,0,0,cold,cold,0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readTrace(p); err == nil {
		t.Fatal("a pre-#262 trace must be refused, not silently replayed against guessed config")
	}
}

func TestReplayParsesConfigAndGroupsByHandle(t *testing.T) {
	// Two handles interleaved on one key — the shape that was unreadable before the
	// trace carried `fh`.
	rows := ""
	for i := int64(0); i < 4; i++ {
		rows += fmt.Sprintf("1,7,obj,%d,%d,%d,%d,%d,window,cold,cold,0,0,0\n", 64<<20, i*kib, kib, 0, kib)
		rows += fmt.Sprintf("2,9,obj,%d,%d,%d,%d,%d,window,cold,cold,0,0,0\n", 64<<20, i*kib, kib, 0, kib)
	}
	cfg, parsed, err := readTrace(writeTrace(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.blockSize != 8388608 || cfg.maxReadahead != 64 || cfg.coverageWindow != 16 || cfg.coverageMin != 0.5 {
		t.Errorf("config not parsed: %+v", cfg)
	}
	scores := scoreTrace("x", "a", cfg, parsed, 8, 1<<20)
	if len(scores) != 2 {
		t.Fatalf("got %d handles, want 2 (rows must group by fh)", len(scores))
	}
	for _, s := range scores {
		if s.rows != 4 {
			t.Errorf("handle %d has %d rows, want 4", s.fh, s.rows)
		}
	}
}

// TestReplayFidelityDetectsMismatch: the tool's first duty is to notice when its own
// replay disagrees with what the live mount recorded, because every verdict after
// that is void. The synthetic trace claims dispatched=0 everywhere while a streaming
// handle really does dispatch, so mismatches must be non-zero.
func TestReplayFidelityDetectsMismatch(t *testing.T) {
	cfg, parsed, err := readTrace(writeTrace(t, contiguous(1, 24<<20, 24<<20)))
	if err != nil {
		t.Fatal(err)
	}
	scores := scoreTrace("x", "a", cfg, parsed, 8, 1<<20)
	if len(scores) != 1 {
		t.Fatalf("got %d handles, want 1", len(scores))
	}
	if scores[0].mismatches == 0 {
		t.Error("replay reported perfect fidelity against a trace whose dispatch counts are all 0; " +
			"the fidelity check is not working, and without it no verdict is trustworthy")
	}
	// A streaming handle must actually establish and dispatch under replay.
	if scores[0].dispatchedBytes == 0 {
		t.Error("a 24 MiB contiguous stream dispatched nothing under replay; the replay is not driving the detector")
	}
}

// TestFollowThroughIsBytesNotTouches is the core of the agreed rule, and it also
// pins the EOF clamp. A handle that streams an object to its end consumes what it
// prefetched and must score HIGH; one that prefetches deep into a large object and
// then stops must score LOW. Before the clamp, both came out near zero, because
// blocks dispatched past EOF — which the live store.Prefetch declines to fetch at
// all — were counted in the denominator. That artifact read as "prefetch never pays
// off" and is what made an earlier version of this tool unusable.
func TestFollowThroughIsBytesNotTouches(t *testing.T) {
	// Reads the whole 24 MiB object: everything dispatched inside the object is read.
	cfg, parsed, err := readTrace(writeTrace(t, contiguous(1, 24<<20, 24<<20)))
	if err != nil {
		t.Fatal(err)
	}
	full := scoreTrace("x", "a", cfg, parsed, 8, 1<<20)[0]
	if full.dispatchedBytes == 0 {
		t.Fatal("nothing dispatched")
	}
	if full.followThrough < 0.9 {
		t.Errorf("a handle that streams its whole object scored %.3f, want >= 0.9 "+
			"(a low score here means past-EOF blocks are polluting the denominator)", full.followThrough)
	}
	if full.dispatchedBytes > full.objSize {
		t.Errorf("dispatched %d bytes against a %d-byte object: the EOF clamp is not working",
			full.dispatchedBytes, full.objSize)
	}

	// Same reads, but the object is 512 MiB: the window runs far past what the handle
	// ever reads, so most dispatched bytes inside the object are wasted.
	cfg2, parsed2, err := readTrace(writeTrace(t, contiguous(1, 24<<20, 512<<20)))
	if err != nil {
		t.Fatal(err)
	}
	stops := scoreTrace("x", "a", cfg2, parsed2, 8, 1<<20)[0]
	if stops.followThrough >= full.followThrough {
		t.Errorf("a handle that stops early (%.3f) must score below one that reads to EOF (%.3f)",
			stops.followThrough, full.followThrough)
	}
	if stops.followThrough > 0.6 {
		t.Errorf("handle that stops early scored %.3f, want clearly low", stops.followThrough)
	}
}

// TestColdWasteIsNetOfLaterReads: a contiguous reader consumes the chunks it
// cold-fetched, so its true cold-start waste is ~0. Charging chunk-minus-read-length
// credited a streaming arm with hundreds of MiB of fiction, which would have
// swamped the reported floor on the healthy mount.
func TestColdWasteIsNetOfLaterReads(t *testing.T) {
	cfg, parsed, err := readTrace(writeTrace(t, contiguous(1, 24<<20, 24<<20)))
	if err != nil {
		t.Fatal(err)
	}
	s := scoreTrace("x", "a", cfg, parsed, 8, 1<<20)[0]
	if s.coldFirstRunReads == 0 {
		t.Fatal("expected some pre-decision cold reads")
	}
	if s.coldNetWasteBytes != 0 {
		t.Errorf("net cold waste = %d on a handle that reads every chunk it fetched, want 0", s.coldNetWasteBytes)
	}
	if s.coldGrossWasteByte == 0 {
		t.Error("gross waste should be non-zero: it is the uncorrected figure, kept for comparison")
	}
}

// TestColdReentriesExcluded: `state_before == cold` also matches RE-ENTRIES into
// cold (seq->cold is a real transition), which are not the pre-decision phase the
// floor mechanism describes. They must be counted separately.
func TestColdReentriesExcluded(t *testing.T) {
	rows := ""
	// First cold run: two pre-decision small reads.
	for i := 0; i < 2; i++ {
		rows += fmt.Sprintf("1,7,obj,%d,%d,65536,%d,7340032,window,cold,cold,0,0,0\n", 128<<20, int64(i)*7340032, i)
	}
	// Classified.
	rows += fmt.Sprintf("1,7,obj,%d,%d,65536,5,7340032,window,sequential,sequential,4,0,4\n", 128<<20, int64(5)*7340032)
	// Back to cold: a re-entry, not the pre-decision phase.
	for i := 6; i < 9; i++ {
		rows += fmt.Sprintf("1,7,obj,%d,%d,65536,%d,7340032,window,cold,cold,0,0,0\n", 128<<20, int64(i)*7340032, i)
	}
	cfg, parsed, err := readTrace(writeTrace(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	s := scoreTrace("x", "a", cfg, parsed, 8, 1<<20)[0]
	if s.coldFirstRunReads != 2 {
		t.Errorf("first-run cold reads = %d, want 2", s.coldFirstRunReads)
	}
	if s.coldReentryReads != 3 {
		t.Errorf("cold re-entry reads = %d, want 3 (counted, but not charged to the floor)", s.coldReentryReads)
	}
}

func TestCoveredBytesUnionsOverlaps(t *testing.T) {
	// Overlapping reads must count each byte once, or follow-through can exceed 1.
	reads := []row{{off: 0, length: 100}, {off: 50, length: 100}, {off: 500, length: 10}}
	if got := coveredBytes(reads, 0, 1000); got != 160 {
		t.Errorf("coveredBytes = %d, want 160 (150 union + 10)", got)
	}
	// Clipping to the block.
	if got := coveredBytes([]row{{off: 0, length: 1000}}, 100, 200); got != 100 {
		t.Errorf("clipped coveredBytes = %d, want 100", got)
	}
	if got := coveredBytes(nil, 0, 100); got != 0 {
		t.Errorf("no reads = %d, want 0", got)
	}
}

// TestColdTaxCountsPreDecisionSmallReads pins the #256 floor estimator: reads served
// while the handle is still `cold` pay a whole 1 MiB chunk for a small request.
func TestColdTaxCountsPreDecisionSmallReads(t *testing.T) {
	rows := ""
	for i := 0; i < 5; i++ {
		rows += fmt.Sprintf("1,7,obj,%d,%d,65536,%d,7340032,window,cold,cold,0,0,0\n", 128<<20, i*7340032, i)
	}
	// One read after classification: must NOT be counted.
	rows += "1,7,obj,134217728,99999744,65536,11,7340032,window,random,random,0,0,0\n"
	cfg, parsed, err := readTrace(writeTrace(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	s := scoreTrace("x", "a", cfg, parsed, 8, 1<<20)[0]
	if s.coldFirstRunReads != 5 {
		t.Errorf("coldFirstRunReads = %d, want 5 (only first-run cold rows)", s.coldFirstRunReads)
	}
	wantWaste := int64(5) * (1<<20 - 65536)
	if s.coldGrossWasteByte != wantWaste {
		t.Errorf("gross waste = %d, want %d", s.coldGrossWasteByte, wantWaste)
	}
}

// TestOmittedRowsAreNotReplayedButAreRead: `parts`/`footer` rows were real reads the
// live code deliberately did not drive the prefetcher with. Replay must skip them as
// decisions, yet still count them as reads for follow-through — otherwise it scores a
// different program than the one that ran.
func TestOmittedRowsAreNotReplayedButAreRead(t *testing.T) {
	rows := contiguous(1, 16<<20, 64<<20)
	// A later read, on the parts path, covering block 3's bytes.
	rows += "1,7,obj,67108864,25165824,1048576,3,0,parts,sequential,sequential,64,0,64\n"
	cfg, parsed, err := readTrace(writeTrace(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	s := scoreTrace("x", "a", cfg, parsed, 8, 1<<20)[0]
	if s.rows != len(strings.Split(strings.TrimSpace(rows), "\n")) {
		t.Errorf("rows=%d; every read must be counted, including omitted ones", s.rows)
	}
	// The parts row contributed read bytes, so some dispatched block should show use.
	if s.usedBytes == 0 {
		t.Error("a later `parts` read covering a dispatched block contributed no used bytes; " +
			"omitted rows must still count as reads")
	}
}
