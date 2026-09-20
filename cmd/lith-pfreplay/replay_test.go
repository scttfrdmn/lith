// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
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

// captureStdout runs f and returns what it printed.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// TestDegenerateArmCannotProduceSeparation pins the wiring, not just the helper:
// the reported failure was an arm with n=3 and a two-way tie on both axes printing
// "VERDICT: SEPARATION" off |rho| = 1.000, on a feature that was a restatement of
// which handle had enough rows to be scored. The guard must keep that out of the
// verdict even though the correlation is real arithmetic.
func TestDegenerateArmCannotProduceSeparation(t *testing.T) {
	// The exact reported numbers, as two arms of one class (replication of a
	// degenerate fit must not rescue it).
	mk := func(arm string) []handleScore {
		return []handleScore{
			{label: "met", arm: arm, fh: 1, followThrough: 0.155852, fracLargeGap: 0.000},
			{label: "met", arm: arm, fh: 2, followThrough: 0.000000, fracLargeGap: 0.125},
			{label: "met", arm: arm, fh: 6, followThrough: 0.000000, fracLargeGap: 0.125},
		}
	}
	all := append(mk("a"), mk("b")...)

	out := captureStdout(t, func() { reportSeparation(all, []string{"met"}, 8) })

	if strings.Contains(out, "VERDICT: SEPARATION") {
		t.Errorf("a degenerate arm (n=3, two-way ties) produced a SEPARATION verdict:\n%s", out)
	}
	if !strings.Contains(out, "n=3<8") {
		t.Errorf("the rejection reason should be shown next to the value, so the reader sees why it was not counted:\n%s", out)
	}
	if strings.Contains(out, "QUALIFYING arms") {
		t.Errorf("a degenerate fit replicated across arms must not be annotated as surviving out-of-sample:\n%s", out)
	}

	// Control: the same shape with enough spread DOES count, so the guard is not
	// simply suppressing everything.
	var spread []handleScore
	for i := 0; i < 12; i++ {
		spread = append(spread,
			handleScore{label: "met", arm: "a", fh: uint64(i), followThrough: float64(i) / 12, fracLargeGap: float64(i) * 0.01},
			handleScore{label: "met", arm: "b", fh: uint64(100 + i), followThrough: float64(i) / 12, fracLargeGap: float64(i) * 0.01})
	}
	out2 := captureStdout(t, func() { reportSeparation(spread, []string{"met"}, 8) })
	if strings.Contains(out2, "n=12<8") {
		t.Errorf("a 12-handle arm was wrongly rejected:\n%s", out2)
	}
	if !strings.Contains(out2, "best |rho| = 1.000") {
		t.Errorf("a genuine monotone relationship over 12 handles should count:\n%s", out2)
	}
}

// TestVerdictRespectsFidelityGate is the defect the real capture exposed: the tool
// printed "everything below is void" from the fidelity gate and then, eleven lines
// later, "VERDICT: SEPARATION" — disagreeing with itself on one page. --min-n did not
// help, because it counts SCORED handles and a diverged handle is scored; it is just
// scored wrong. Nothing connected the verdict to the gate.
func TestVerdictRespectsFidelityGate(t *testing.T) {
	// Twelve handles per class with a strong correlation — but every one unfaithful.
	var all []handleScore
	for i := 0; i < 12; i++ {
		for _, lab := range []string{"met", "hemco"} {
			for _, arm := range []string{"a", "b"} {
				all = append(all, handleScore{
					label: lab, arm: arm, fh: uint64(i), mismatches: 1, // <- diverged
					followThrough: float64(i) / 12, fracLargeGap: float64(i) * 0.01,
				})
			}
		}
	}
	out := captureStdout(t, func() { reportSeparation(all, []string{"met", "hemco"}, 8) })
	if strings.Contains(out, "VERDICT: SEPARATION") {
		t.Errorf("a verdict was declared entirely on handles whose replay diverged:\n%s", out)
	}
	if !strings.Contains(out, "fidelity filter") {
		t.Errorf("the exclusion must be stated, not silent:\n%s", out)
	}
	if !strings.Contains(out, "UNEVALUABLE") {
		t.Errorf("with no faithful handles the answer is UNEVALUABLE, not a measured absence:\n%s", out)
	}

	// Control: the same data, faithful, does reach a verdict — so the filter is not
	// simply suppressing everything.
	for i := range all {
		all[i].mismatches = 0
	}
	out2 := captureStdout(t, func() { reportSeparation(all, []string{"met", "hemco"}, 8) })
	if strings.Contains(out2, "fidelity filter") {
		t.Errorf("faithful handles must not be filtered:\n%s", out2)
	}
	if !strings.Contains(out2, "VERDICT: SEPARATION") {
		t.Errorf("faithful handles with a strong correlation should reach a verdict:\n%s", out2)
	}
}

// TestThinClassIsUnevaluableNotNoSeparation: after the fidelity filter a class can be
// left with a handful of handles. |rho| < 0.5 from n=3 is an absence of data, not a
// measured absence of relationship, and must not be reported as the latter.
func TestThinClassIsUnevaluableNotNoSeparation(t *testing.T) {
	var all []handleScore
	for i := 0; i < 12; i++ { // met is well-populated
		all = append(all, handleScore{label: "met", arm: "a", fh: uint64(i),
			followThrough: float64(i%5) / 5, fracLargeGap: float64(i) * 0.02})
	}
	for i := 0; i < 3; i++ { // hemco is thin
		all = append(all, handleScore{label: "hemco", arm: "a", fh: uint64(100 + i),
			followThrough: 0, fracLargeGap: 0.1})
	}
	out := captureStdout(t, func() { reportSeparation(all, []string{"met", "hemco"}, 8) })
	if !strings.Contains(out, "UNEVALUABLE") {
		t.Errorf("a thin class must yield UNEVALUABLE:\n%s", out)
	}
	if strings.Contains(out, "NO SEPARATION") {
		t.Errorf("n=3 must not be reported as a measured absence of relationship:\n%s", out)
	}
}

// TestReplayIsInvariantToRowOrder is the guard #272 asked for, and it is the property
// my own verification lacked: I checked a fresh-format trace round-tripped at 1.00x
// using a SINGLE-THREADED generator, where file order *is* decision order, so the
// defect was invisible. Shuffling a known-good trace and requiring identical results
// tests the property unconditionally — a replay that depends on file order fails it,
// however clean it looks on an uncontended trace.
func TestReplayIsInvariantToRowOrder(t *testing.T) {
	// One handle, contiguous, with explicit decision numbers.
	var b strings.Builder
	var prevEnd int64
	for i := 0; i < 160; i++ {
		off := int64(i) * kib
		fmt.Fprintf(&b, "%d,1,7,obj,%d,%d,%d,%d,%d,window,cold,cold,32,0,0,0\n",
			i+1, 64<<20, off, kib, off/(1<<20), off-prevEnd)
		prevEnd = off + kib
	}
	rows := b.String()

	cfg, inOrder, err := readTrace(writeTraceSeq(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	want := scoreTrace("x", "a", cfg, inOrder, 8, 1<<20)[0]
	// The property under test is invariance, not fidelity: this fixture's `dispatched`
	// column is hand-written and does not match what the detector decides, which is
	// fine — a shuffled copy must still score identically. Only require that the
	// replay actually did something, so the assertions below are not vacuous.
	if want.dispatchDecisions == 0 {
		t.Fatal("precondition: the fixture must cause dispatches, or invariance is trivial")
	}

	// Deterministic shuffles: reversed, and a deterministic interleave. Both must
	// produce identical scores, because `seq` carries the real order.
	for name, mangle := range map[string]func([]row) []row{
		"reversed": func(in []row) []row {
			out := append([]row(nil), in...)
			for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
				out[i], out[j] = out[j], out[i]
			}
			return out
		},
		"interleaved": func(in []row) []row {
			out := make([]row, 0, len(in))
			for i := 0; i < len(in); i += 2 {
				out = append(out, in[i])
			}
			for i := 1; i < len(in); i += 2 {
				out = append(out, in[i])
			}
			return out
		},
	} {
		got := scoreTrace("x", "a", cfg, mangle(inOrder), 8, 1<<20)[0]
		if got.mismatches != want.mismatches {
			t.Errorf("%s: mismatches %d, want %d — the replay depends on file order", name, got.mismatches, want.mismatches)
		}
		if got.dispatchDecisions != want.dispatchDecisions {
			t.Errorf("%s: dispatch decisions %d, want %d", name, got.dispatchDecisions, want.dispatchDecisions)
		}
		if got.dispatchedBytes != want.dispatchedBytes {
			t.Errorf("%s: dispatched bytes %d, want %d", name, got.dispatchedBytes, want.dispatchedBytes)
		}
		if got.followThrough != want.followThrough {
			t.Errorf("%s: follow-through %v, want %v", name, got.followThrough, want.followThrough)
		}
	}
}

// writeTraceSeq writes a trace in the #271 format (seq + max_window columns).
func writeTraceSeq(t *testing.T, rows string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pf.csv")
	body := "# lith prefetch trace; block_size=1048576 max_readahead=32 parts_max=0 small_file=0 coverage_window=16 coverage_min=0.5 evidence_ratio=0\n" +
		"seq,fh,pid,key,size,off,len,blk,gap,path,state_before,state_after,max_window,window,dispatched,peak_window\n" + rows
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeTraceCfg writes a #271-format trace with an explicit parts_max, so the
// conditional-Open behaviour below can be exercised on both sides of the threshold.
func writeTraceCfg(t *testing.T, partsMax int64, rows string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pf.csv")
	body := fmt.Sprintf("# lith prefetch trace; block_size=8388608 max_readahead=223 parts_max=%d small_file=4194304 coverage_window=16 coverage_min=0.5 evidence_ratio=0\n", partsMax) +
		"seq,fh,pid,key,size,off,len,blk,gap,path,state_before,state_after,max_window,window,dispatched,peak_window\n" + rows
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOpenIsConditionalOnPartsThreshold pins the last source of replay divergence on
// a real GCHP trace (10 of 6,229 HEMCO handles). The mount calls pf.open() ONLY for
// objects larger than partsThreshold; for a smaller object the prefetcher is never
// Open()ed, so its first Observe takes the !haveLast path — lastBlock = blockIdx
// instead of -1, which leaves lastDelta at 0 and makes the strided branch unreachable
// on read 2. Replaying Open() unconditionally dispatched a block the mount did not,
// and only ever on handles that never reach sequential/strided, which is exactly the
// population the report isolated.
func TestOpenIsConditionalOnPartsThreshold(t *testing.T) {
	const blk = 8 << 20
	// Two reads two blocks apart, each with a byte gap larger than one block, so
	// neither is contiguous. This is the shape that makes read 2's delta equal read
	// 1's — the strided trigger — but only if Open() set lastBlock to -1.
	rows := fmt.Sprintf(
		"116,35,17362,obj,%d,%d,131072,1,%d,window,cold,cold,39,0,0,0\n"+
			"247,35,17362,obj,%d,%d,86016,3,%d,window,cold,cold,39,0,0,0\n",
		30230325, 12845056, 12845056,
		30230325, 28487680, 15511552)

	// Object is 30.2 MB. With parts_max 64 MiB it is BELOW the threshold, so the
	// mount never called Open() and dispatched nothing — the trace says so.
	below := writeTraceCfg(t, 64<<20, rows)
	cfg, parsed, err := readTrace(below)
	if err != nil {
		t.Fatal(err)
	}
	if got := scoreTrace("x", "a", cfg, parsed, 8, 1<<20)[0]; got.mismatches != 0 {
		t.Errorf("object below parts-max: %d mismatches, want 0 — the replay must skip Open() "+
			"exactly as the mount does, or it reaches the strided branch the mount could not",
			got.mismatches)
	}

	// With parts_max 16 MiB the same object is ABOVE the threshold, so the mount
	// would have called Open() — and then read 2 does reach strided and dispatches,
	// so a trace claiming 0 dispatches is inconsistent. That asymmetry is the
	// behaviour under test: the condition must be read from the config, not assumed.
	above := writeTraceCfg(t, 16<<20, rows)
	cfg2, parsed2, err := readTrace(above)
	if err != nil {
		t.Fatal(err)
	}
	if got := scoreTrace("x", "a", cfg2, parsed2, 8, 1<<20)[0]; got.mismatches == 0 {
		t.Error("object above parts-max: expected the Open()-ed replay to diverge from a trace " +
			"recorded without it; if this passes, the threshold is not being consulted")
	}
	_ = blk
}
