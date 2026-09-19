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
		"fh,pid,key,off,len,blk,gap,path,state_before,state_after,window,dispatched,peak_window\n" + rows
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// contiguous emits a handle streaming nbytes from 0 in 128 KiB reads. `dispatched`
// is left at 0, which is deliberately WRONG for a streaming handle — the fidelity
// check must notice, which is what proves the check works.
func contiguous(fh int, nbytes int64) string {
	var b strings.Builder
	var prevEnd int64
	for off := int64(0); off < nbytes; off += kib {
		gap := off - prevEnd
		fmt.Fprintf(&b, "%d,7,obj,%d,%d,%d,%d,window,cold,cold,0,0,0\n", fh, off, kib, off/8388608, gap)
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
		rows += fmt.Sprintf("1,7,obj,%d,%d,%d,%d,window,cold,cold,0,0,0\n", i*kib, kib, 0, kib)
		rows += fmt.Sprintf("2,9,obj,%d,%d,%d,%d,window,cold,cold,0,0,0\n", i*kib, kib, 0, kib)
	}
	cfg, parsed, err := readTrace(writeTrace(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.blockSize != 8388608 || cfg.maxReadahead != 64 || cfg.coverageWindow != 16 || cfg.coverageMin != 0.5 {
		t.Errorf("config not parsed: %+v", cfg)
	}
	scores := scoreTrace("x", cfg, parsed, 8, 1<<20)
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
	cfg, parsed, err := readTrace(writeTrace(t, contiguous(1, 24<<20)))
	if err != nil {
		t.Fatal(err)
	}
	scores := scoreTrace("x", cfg, parsed, 8, 1<<20)
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

// TestFollowThroughIsBytesNotTouches is the core of the agreed rule: a handle that
// prefetches a lot and then reads only a sliver must score LOW, where lith's live
// used/issued would call those chunks "used".
func TestFollowThroughIsBytesNotTouches(t *testing.T) {
	// Establish by streaming the first 24 MiB (so blocks get dispatched ahead), then
	// stop reading. Later reads cover almost none of what was dispatched.
	cfg, parsed, err := readTrace(writeTrace(t, contiguous(1, 24<<20)))
	if err != nil {
		t.Fatal(err)
	}
	s := scoreTrace("x", cfg, parsed, 8, 1<<20)[0]
	if s.dispatchedBytes == 0 {
		t.Fatal("nothing dispatched")
	}
	if s.followThrough < 0 || s.followThrough > 1.0001 {
		t.Errorf("follow-through %.3f out of range", s.followThrough)
	}
	// The handle stops at 24 MiB while the window reaches far past it, so most
	// dispatched bytes are never read: a low score is the correct answer.
	if s.followThrough > 0.5 {
		t.Errorf("follow-through %.3f: a handle that stops reading should score low", s.followThrough)
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
		rows += fmt.Sprintf("1,7,obj,%d,65536,%d,7340032,window,cold,cold,0,0,0\n", i*7340032, i)
	}
	// One read after classification: must NOT be counted.
	rows += "1,7,obj,99999744,65536,11,7340032,window,random,random,0,0,0\n"
	cfg, parsed, err := readTrace(writeTrace(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	s := scoreTrace("x", cfg, parsed, 8, 1<<20)[0]
	if s.coldSmallReads != 5 {
		t.Errorf("coldSmallReads = %d, want 5 (only the cold-state rows)", s.coldSmallReads)
	}
	wantWaste := int64(5) * (1<<20 - 65536)
	if s.coldSmallReadWasteBytes != wantWaste {
		t.Errorf("waste = %d, want %d", s.coldSmallReadWasteBytes, wantWaste)
	}
}

// TestOmittedRowsAreNotReplayedButAreRead: `parts`/`footer` rows were real reads the
// live code deliberately did not drive the prefetcher with. Replay must skip them as
// decisions, yet still count them as reads for follow-through — otherwise it scores a
// different program than the one that ran.
func TestOmittedRowsAreNotReplayedButAreRead(t *testing.T) {
	rows := contiguous(1, 16<<20)
	// A later read, on the parts path, covering block 3's bytes.
	rows += "1,7,obj,25165824,1048576,3,0,parts,sequential,sequential,64,0,64\n"
	cfg, parsed, err := readTrace(writeTrace(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	s := scoreTrace("x", cfg, parsed, 8, 1<<20)[0]
	if s.rows != len(strings.Split(strings.TrimSpace(rows), "\n")) {
		t.Errorf("rows=%d; every read must be counted, including omitted ones", s.rows)
	}
	// The parts row contributed read bytes, so some dispatched block should show use.
	if s.usedBytes == 0 {
		t.Error("a later `parts` read covering a dispatched block contributed no used bytes; " +
			"omitted rows must still count as reads")
	}
}
